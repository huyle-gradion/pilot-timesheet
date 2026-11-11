package timesheet

import (
	"slices"
	"sort"
	"time"
)

// collectStamps stamps every timed item, including a topic's retained
// overflow: the draft only summarizes overflow on the topic line, but its
// instants are still work and must shape the evidence blocks (#309). An
// overflow stamp carries overflowItemIdx(oi), so a block it lands in gains
// the topic and its span, never a listed item.
func collectStamps(topics []TopicCandidate, loc *time.Location) []minuteStamp {
	var out []minuteStamp
	for ti := range topics {
		for ii, item := range topics[ti].Items {
			if m, ok := anchorClock(item.Time, loc); ok && m >= 0 && m <= dayEndMinutes {
				out = append(out, minuteStamp{m, ti, ii})
			}
		}
		for oi, item := range topics[ti].OverflowItems {
			if m, ok := anchorClock(item.Time, loc); ok && m >= 0 && m <= dayEndMinutes {
				out = append(out, minuteStamp{m, ti, overflowItemIdx(oi)})
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
