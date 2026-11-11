package timesheet

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

const (
	dayEndMinutes      = 23*60 + 59
	workdayStartMinute = 9 * 60
	workdayBackfillMax = 18 * 60
	lunchStartMinute   = 12 * 60
	lunchEndMinute     = 13 * 60
	powMaxSilence      = 6 * 60
	// powMinBurst gives a lone off-hours instant a positive span, because the
	// remote refuses a zero-duration interval.
	powMinBurst = 15
)

type itemRef struct{ topicIdx, itemIdx int }

// minuteStamp is one proof-of-work instant on the recorded day's local clock.
type minuteStamp struct{ minute, topicIdx, itemIdx int }

type dayBlock struct {
	blk      DraftBlock
	span     minuteSpan
	hasSpan  bool
	cluster  int
	refs     []itemRef
	topics   []int
	overflow []*topicOverflow
}

type partitionResult struct {
	Blocks           []dayBlock
	EligibleMeetings int
}

func (b *dayBlock) addTopic(ti int) {
	if !slices.Contains(b.topics, ti) {
		b.topics = append(b.topics, ti)
	}
}

func (b *dayBlock) addRef(r itemRef) {
	if !slices.Contains(b.refs, r) {
		b.refs = append(b.refs, r)
	}
	b.addTopic(r.topicIdx)
}

func (b *dayBlock) render(topics []TopicCandidate, loc *time.Location, taken []bool) {
	if b.hasSpan {
		b.blk.Start, b.blk.End = clockString(b.span.start), clockString(b.span.end)
	}
	sort.Ints(b.topics)
	sort.Slice(b.refs, func(i, j int) bool {
		if b.refs[i].topicIdx != b.refs[j].topicIdx {
			return b.refs[i].topicIdx < b.refs[j].topicIdx
		}
		return b.refs[i].itemIdx < b.refs[j].itemIdx
	})
	for _, ti := range b.topics {
		b.blk.TopicCandidates = append(b.blk.TopicCandidates, topics[ti].Title)
		var ov *topicOverflow
		if !taken[ti] {
			ov, taken[ti] = topicOverflowOf(topics[ti], loc), true
		}
		b.overflow = append(b.overflow, ov)
	}
	for _, r := range b.refs {
		b.blk = mergeItem(b.blk, topics[r.topicIdx].Items[r.itemIdx])
	}
}

func partitionDay(topics []TopicCandidate, meetings []ProposedMeeting, segments []Segment, timezone string) partitionResult {
	loc := DayLocationOrNil(timezone)
	meetingBlocks, eligible := partitionMeetings(meetings, loc)
	workBlocks := partitionWork(segments, spansOf(meetingBlocks))
	occupied := unionSpans(append(spansOf(meetingBlocks), spansOf(workBlocks)...))
	stamps := collectStamps(topics, loc)
	clusters := clusterStamps(stamps)

	blocks := append(append(meetingBlocks, workBlocks...), evidenceBlocks(stamps, clusters, occupied)...)
	sortBlocks(blocks)
	for _, s := range stamps {
		if i := stampBlock(blocks, s.minute); i >= 0 {
			blocks[i].addRef(itemRef{s.topicIdx, s.itemIdx})
		}
	}
	inheritSlotTopics(blocks, stamps, clusters, occupied)
	blocks = attachUntimedTopics(blocks, topics, loc)

	taken := make([]bool, len(topics))
	for i := range blocks {
		blocks[i].render(topics, loc, taken)
	}
	if len(blocks) == 0 {
		blocks = append(blocks, dayBlock{blk: DraftBlock{Kind: "day"}})
	}
	for i, id := range blockIDs(blocks) {
		blocks[i].blk.BlockID = id
	}
	return partitionResult{Blocks: blocks, EligibleMeetings: eligible}
}

func sortBlocks(blocks []dayBlock) {
	sort.SliceStable(blocks, func(i, j int) bool {
		a, b := blocks[i], blocks[j]
		if a.hasSpan != b.hasSpan {
			return a.hasSpan
		}
		if a.hasSpan && a.span.start != b.span.start {
			return a.span.start < b.span.start
		}
		return a.blk.Kind < b.blk.Kind
	})
}

func blockIDs(blocks []dayBlock) []string {
	ids := make([]string, len(blocks))
	fixed := make([]bool, len(blocks))
	for i := range blocks {
		ids[i] = blocks[i].blk.Kind + "-" + blocks[i].blk.Start + "-" + blocks[i].blk.End
		fixed[i] = true
	}
	return resolveBlockIDs(ids, fixed)
}

func partitionMeetings(meetings []ProposedMeeting, loc *time.Location) ([]dayBlock, int) {
	var parts []dayBlock
	eligible := 0
	for _, m := range meetings {
		startM, okS := anchorClock(m.Start, loc)
		endM, okE := anchorClock(m.End, loc)
		if m.Decision == "no" || m.Status == "cancelled" || m.AllDay || !okS || !okE || endM <= startM {
			continue
		}
		eligible++
		parts = append(parts, dayBlock{span: minuteSpan{startM, endM}, hasSpan: true,
			blk: DraftBlock{Kind: "meeting", Title: m.Title}})
	}
	return clusterParts(parts, nil), eligible
}

func partitionWork(segments []Segment, meetings []minuteSpan) []dayBlock {
	var parts []dayBlock
	for _, seg := range segments {
		startM, okS := draftClock(seg.Start)
		endM, okE := draftClock(seg.End)
		if !okS || !okE || endM <= startM {
			continue
		}
		parts = append(parts, dayBlock{span: minuteSpan{startM, endM}, hasSpan: true,
			blk: DraftBlock{Kind: "work", Title: seg.Description}})
	}
	return clusterParts(parts, meetings)
}

func clusterParts(parts []dayBlock, cut []minuteSpan) []dayBlock {
	spans := make([]powSpan, len(parts))
	for i, p := range parts {
		spans[i] = powSpan{id: fmt.Sprintf("%04d", i), startM: p.span.start, endM: p.span.end}
	}
	var out []dayBlock
	for _, cluster := range mergeSpans(spans) {
		union, titles := parts[cluster[0]].span, make([]string, 0, len(cluster))
		for _, i := range cluster {
			union.end = max(union.end, parts[i].span.end)
			titles = append(titles, parts[i].blk.Title)
		}
		kind, title := parts[cluster[0]].blk.Kind, joinText(titles...)
		for _, piece := range subtractSpans([]minuteSpan{union}, cut) {
			out = append(out, dayBlock{span: piece, hasSpan: true, blk: DraftBlock{Kind: kind, Title: title}})
		}
	}
	return out
}

func spansOf(blocks []dayBlock) []minuteSpan {
	var out []minuteSpan
	for _, b := range blocks {
		if b.hasSpan {
			out = append(out, b.span)
		}
	}
	return out
}

func mergeItem(blk DraftBlock, item EvidenceItem) DraftBlock {
	if strings.HasPrefix(item.Resource, "pr:") {
		if pr := prLabel(item.Resource); pr != "" && !containsString(blk.PRs, pr) {
			blk.PRs = append(blk.PRs, pr)
		}
	}
	if len(blk.Sources) < draftSourcesPerBlock {
		blk.Sources = append(blk.Sources, DraftSourceItem{
			Kind: item.Kind, Repo: item.Repo, Title: item.Title, Time: item.Time, URL: item.URL,
		})
	} else {
		blk.SourcesTruncated = true
	}
	return blk
}
