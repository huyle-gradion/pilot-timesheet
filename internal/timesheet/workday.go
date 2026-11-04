package timesheet

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// The draft partition is an interval algebra. Meetings keep the span the
// calendar gave them, confirmed work keeps its own, and the day's proof-of-work
// instants claim only the minutes neither of those already owns. Every span is
// half-open, so adjacency (a.end == b.start) is not an overlap, and the remote's
// non-overlap rule holds by construction instead of being repaired by a merge
// afterwards.
//
// partitionDay is the only place that decides which block owns a minute.
// BuildDraft renders its blocks and attachDraftSources recomputes them from the
// same durable inputs, so the draft and the recorded audit sources cannot drift
// apart.

const (
	// dayEndMinutes is the last representable minute: 24:00 does not round-trip
	// through clockString.
	dayEndMinutes = 23*60 + 59
	// workdayStartMinute is when work is assumed to begin when the day's earliest
	// evidence of it is a later published commit. A commit records when work was
	// pushed, not when it started.
	workdayStartMinute = 9 * 60
	// workdayBackfillMax stops that assumption from dragging an evening burst
	// back to the morning.
	workdayBackfillMax = 18 * 60
	lunchStartMinute   = 12 * 60
	lunchEndMinute     = 13 * 60
	// powMaxSilence is the longest publish silence one stretch of work may
	// contain: 14:19 -> 22:00 on 2026-11-04 is 7h41m and splits, while
	// 09:30 -> 14:00 is 4h30m and stays one stretch.
	powMaxSilence = 6 * 60
	// powMinBurst gives a lone off-hours instant a positive span, because the
	// remote refuses a zero-duration interval.
	powMinBurst = 15
)

// itemRef names one retained evidence item by the topic and item it came from.
type itemRef struct{ topicIdx, itemIdx int }

// minuteStamp is one proof-of-work instant on the recorded day's local clock.
type minuteStamp struct{ minute, topicIdx, itemIdx int }

// dayBlock is one block while it is being filled. cluster ties an evidence block
// back to the stamps it came from, so a carved slot can inherit their topics
// instead of rendering empty.
type dayBlock struct {
	blk      DraftBlock
	span     minuteSpan
	hasSpan  bool
	cluster  int
	refs     []itemRef
	topics   []int
	overflow []*topicOverflow
}

// partitionResult is everything the draft and the record path need: the blocks,
// their per-line overflow payloads and the count the day note reports.
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

// render fills one block's AI-visible fields from its evidence. Topics render in
// evidence order, and a topic's overflow summary is attached once per day, on the
// first block that names it, so a topic spread across blocks is not counted
// twice.
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

// sortBlocks puts timed blocks in start order and the whole-day fallback last.
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

// partitionMeetings converts the eligible calendar proposals to local minutes,
// because a proposal carries an absolute instant in the provider's offset.
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

// partitionWork folds the confirmed segments and carves each meeting out of them.
// Confirmed work is never lunch-carved: the human stated it, and the partition
// does not rewrite a statement.
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

// clusterParts folds each connected component of strictly overlapping parts into
// one span over its union, joins every contributor's title, and carves cut out of
// the result. Touching parts stay separate.
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

// mergeItem folds one evidence item's PR identity and bounded audit reference
// into a block, respecting the fixed per-block source cap.
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
