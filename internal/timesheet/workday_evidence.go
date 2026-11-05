package timesheet

import (
	"slices"
	"sort"
	"time"
)

// The evidence half of the draft partition: it turns the day's proof-of-work
// instants into the evidence spans the days' meetings and confirmed work leave
// unclaimed, and decides which block owns each instant. It never changes a
// meeting's or a confirmed interval's span; partitionDay in workday.go owns the
// pipeline that calls it.

func collectStamps(topics []TopicCandidate, loc *time.Location) []minuteStamp {
	var out []minuteStamp
	for ti := range topics {
		for ii, item := range topics[ti].Items {
			if m, ok := anchorClock(item.Time, loc); ok && m >= 0 && m <= dayEndMinutes {
				out = append(out, minuteStamp{m, ti, ii})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].minute != out[j].minute {
			return out[i].minute < out[j].minute
		}
		if out[i].topicIdx != out[j].topicIdx {
			return out[i].topicIdx < out[j].topicIdx
		}
		return out[i].itemIdx < out[j].itemIdx
	})
	return out
}

// clusterStamps groups the day's instants into stretches of work. A new stretch
// starts after more than powMaxSilence of silence, and none bridges morning sleep
// into the workday, so a 05:52 commit does not claim everything up to 09:23.
func clusterStamps(stamps []minuteStamp) [][]int {
	var out [][]int
	for i, s := range stamps {
		if len(out) > 0 {
			last := out[len(out)-1]
			prev := stamps[last[len(last)-1]].minute
			gap := s.minute - prev
			asleep := prev < workdayStartMinute && s.minute >= workdayStartMinute && gap > 60
			if gap <= powMaxSilence && !asleep {
				out[len(out)-1] = append(last, i)
				continue
			}
		}
		out = append(out, []int{i})
	}
	return out
}

// evidenceBlocks turns each unoccupied work stretch into non-overlapping evidence
// spans. A stretch whose every instant sits inside a meeting or confirmed work
// invents nothing: its stamps attach to those blocks instead.
func evidenceBlocks(stamps []minuteStamp, clusters [][]int, occupied []minuteSpan) []dayBlock {
	inLunch := slices.ContainsFunc(stamps, func(s minuteStamp) bool {
		return s.minute >= lunchStartMinute && s.minute < lunchEndMinute
	})
	var out []dayBlock
	var carved []minuteSpan
	for ci, cluster := range clusters {
		if !slices.ContainsFunc(cluster, func(i int) bool { return !containsMinute(occupied, stamps[i].minute) }) {
			continue
		}
		start, end := stamps[cluster[0]].minute, stamps[cluster[len(cluster)-1]].minute
		if cluster[0] == 0 && start > workdayStartMinute && start <= workdayBackfillMax {
			start = workdayStartMinute
		}
		if start == end {
			// A lone off-hours instant states that work happened, not when it
			// began, so it looks backward for its span.
			floor := 0
			for _, s := range append(append([]minuteSpan{}, occupied...), carved...) {
				if s.end <= end {
					floor = max(floor, s.end)
				}
			}
			start = max(floor, end-powMinBurst)
			if start == end {
				if end > 0 || containsMinute(occupied, end) {
					continue
				}
				start, end = 0, 1
			}
		}
		pieces := subtractSpans([]minuteSpan{{start, end}}, occupied)
		if !inLunch {
			pieces = subtractSpans(pieces, []minuteSpan{{lunchStartMinute, lunchEndMinute}})
		}
		for _, piece := range pieces {
			out = append(out, dayBlock{blk: DraftBlock{Kind: "evidence"}, span: piece, hasSpan: true, cluster: ci})
			carved = append(carved, piece)
		}
	}
	return out
}

// stampBlock picks the block that owns one minute. A stamp sitting exactly on a
// carve boundary belongs to the block it closes, which is where the work that
// produced it happened; a stamp no block can hold goes to the nearest one, so no
// proof of work is dropped.
func stampBlock(blocks []dayBlock, minute int) int {
	for i := range blocks {
		if blocks[i].hasSpan && blocks[i].span.contains(minute) {
			return i
		}
	}
	for i := range blocks {
		if blocks[i].hasSpan && blocks[i].span.end == minute {
			return i
		}
	}
	best, bestDist := -1, 0
	for i := range blocks {
		if !blocks[i].hasSpan {
			continue
		}
		s := blocks[i].span
		dist := max(s.start-minute, minute-s.end, 0)
		if best < 0 || dist < bestDist || (dist == bestDist && s.start < blocks[best].span.start) {
			best, bestDist = i, dist
		}
	}
	return best
}

// inheritSlotTopics gives a carved slot that owns no stamp of its own the
// cluster's unoccupied stamps nearest it, so a slot between a meeting and lunch
// carries the real topics, PRs and sources of the work around it.
func inheritSlotTopics(blocks []dayBlock, stamps []minuteStamp, clusters [][]int, occupied []minuteSpan) {
	for i := range blocks {
		b := &blocks[i]
		if b.blk.Kind != "evidence" || !b.hasSpan || len(b.refs) > 0 {
			continue
		}
		for _, forward := range []bool{true, false} {
			for _, si := range clusters[b.cluster] {
				s := stamps[si]
				near := s.minute >= b.span.end
				if !forward {
					near = s.minute <= b.span.start
				}
				if near && !containsMinute(occupied, s.minute) {
					b.addRef(itemRef{s.topicIdx, s.itemIdx})
				}
			}
			if len(b.refs) > 0 {
				break
			}
		}
	}
}

func hasAnchor(item EvidenceItem, loc *time.Location) bool {
	_, ok := anchorClock(item.Time, loc)
	return ok
}

// attachUntimedTopics homes every item without a parsable anchor. An untimed item
// follows its topic's first attributed block; a topic with no timed item at all
// lands on the first evidence block, or on one spanless evidence block when the
// day has none, so a topic is never silently dropped.
func attachUntimedTopics(blocks []dayBlock, topics []TopicCandidate, loc *time.Location) []dayBlock {
	timed, needHome := make([]bool, len(topics)), false
	for ti := range topics {
		for _, item := range topics[ti].Items {
			timed[ti] = timed[ti] || hasAnchor(item, loc)
		}
		needHome = needHome || !timed[ti]
	}
	home := -1
	for i := range blocks {
		if blocks[i].blk.Kind == "evidence" {
			home = i
			break
		}
	}
	if needHome && home < 0 {
		blocks = append(blocks, dayBlock{blk: DraftBlock{Kind: "evidence"}})
		home = len(blocks) - 1
	}
	for ti := range topics {
		target := home
		if timed[ti] {
			target = -1
			for i := range blocks {
				if slices.Contains(blocks[i].topics, ti) {
					target = i
					break
				}
			}
		}
		if target < 0 {
			continue
		}
		blocks[target].addTopic(ti)
		for ii, item := range topics[ti].Items {
			if !hasAnchor(item, loc) {
				blocks[target].addRef(itemRef{ti, ii})
			}
		}
	}
	return blocks
}
