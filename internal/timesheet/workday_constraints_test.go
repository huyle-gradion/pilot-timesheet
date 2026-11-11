package timesheet

import (
	"slices"
	"testing"
)

func TestDayEndMinutesRoundTripsThroughClockString(t *testing.T) {
	if m, ok := draftClock(clockString(dayEndMinutes)); !ok || m != dayEndMinutes {
		t.Fatalf("draftClock(clockString(%d)) = %d, %v", dayEndMinutes, m, ok)
	}
}

func TestPartitionDayAttachesTopicOverflowOnce(t *testing.T) {
	topic := TopicCandidate{Title: "spread", DroppedItems: 3, Items: []EvidenceItem{
		{Ref: "a", Kind: "commit", Time: "10:00"}, {Ref: "b", Kind: "commit", Time: "15:00"},
	}}
	meetings := []ProposedMeeting{{Start: "09:00", End: "11:00"}, {Start: "14:00", End: "16:00"}}
	named, attached := 0, 0
	for _, b := range partitionDay([]TopicCandidate{topic}, meetings, nil, "UTC").Blocks {
		for _, ov := range b.overflow {
			named++
			if ov != nil {
				attached++
			}
		}
	}
	if named != 2 || attached != 1 {
		t.Fatalf("topic named on %d blocks with %d overflow summaries, want 2 and 1", named, attached)
	}
}

func TestSortBlocksPutsSpanlessBlockLast(t *testing.T) {
	blocks := []dayBlock{{blk: DraftBlock{Kind: "day"}}, {hasSpan: true, span: minuteSpan{60, 120}}}
	sortBlocks(blocks)
	if !blocks[0].hasSpan || blocks[1].hasSpan {
		t.Fatalf("sortBlocks = %+v, want the timed block first", blocks)
	}
}

func TestClusterStampsKeepsExactlyMaxSilenceInOneStretch(t *testing.T) {
	stamps := []minuteStamp{{minute: 600}, {minute: 600 + powMaxSilence}}
	if got := clusterStamps(stamps); len(got) != 1 {
		t.Fatalf("clusterStamps = %v, want one stretch", got)
	}
}

func TestPartitionDayHomesTopicWithoutAnyTimedItem(t *testing.T) {
	topic := TopicCandidate{Title: "untimed", Items: []EvidenceItem{{Ref: "u", Kind: "commit"}}}
	homes := 0
	for _, b := range partitionDay([]TopicCandidate{topic}, nil, nil, "UTC").Blocks {
		if len(b.topics) == 1 && b.blk.Kind == "evidence" {
			homes++
		}
	}
	if homes != 1 {
		t.Fatalf("untimed topic homed on %d evidence blocks, want 1", homes)
	}
}

func TestPartitionDayUntimedItemFollowsTopicsFirstBlock(t *testing.T) {
	topic := TopicCandidate{Title: "mixed", Items: []EvidenceItem{
		{Ref: "a", Kind: "commit", Time: "10:00"}, {Ref: "b", Kind: "commit", Time: "15:00"}, {Ref: "u", Kind: "commit"},
	}}
	meetings := []ProposedMeeting{{Start: "09:00", End: "11:00"}, {Start: "14:00", End: "16:00"}}
	var starts []int
	for _, b := range partitionDay([]TopicCandidate{topic}, meetings, nil, "UTC").Blocks {
		if slices.Contains(b.refs, itemRef{0, 2}) {
			starts = append(starts, b.span.start)
		}
	}
	if !slices.Equal(starts, []int{9 * 60}) {
		t.Fatalf("untimed item landed on blocks starting at %v, want [540]", starts)
	}
}

func TestPartitionWorkNeverCarvesLunch(t *testing.T) {
	got := partitionWork([]Segment{{Start: "11:00", End: "14:00", Description: "stated"}}, nil)
	if len(got) != 1 || got[0].span != (minuteSpan{11 * 60, 14 * 60}) {
		t.Fatalf("partitionWork = %+v, want one 11:00-14:00 block", got)
	}
}
