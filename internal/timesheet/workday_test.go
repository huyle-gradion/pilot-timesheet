package timesheet

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

const vnTZ = "Asia/Ho_Chi_Minh"

// powDay builds one day's durable inputs: its calendar meetings plus one topic
// per proof-of-work instant, so each stamp is addressable on its own.
func powDay(date string, meetings []ProposedMeeting, stamps ...string) Review {
	topics := make([]TopicCandidate, 0, len(stamps))
	for i, at := range stamps {
		topics = append(topics, TopicCandidate{
			Ref:     fmt.Sprintf("tc-%d", i),
			Title:   fmt.Sprintf("PoW %02d", i),
			FirstAt: at, LastAt: at,
			Items: []EvidenceItem{{
				Ref: fmt.Sprintf("ev-%d", i), Kind: "commit", Repo: "acme/api",
				Title: fmt.Sprintf("note %d", i), Time: at,
			}},
		})
	}
	r := draftReview(meetings, nil, topics, nil)
	r.Date = date
	r.Timezone = vnTZ
	return r
}

func draftBlockIDs(d Draft) []string {
	out := make([]string, 0, len(d.Blocks))
	for _, b := range d.Blocks {
		out = append(out, b.BlockID)
	}
	return out
}

// day20261104 is the reported defect: two disjoint calendar meetings, one
// morning PR, a mid-afternoon burst inside the second meeting, and an evening
// burst 7h41m after the previous commit.
func day20261104() Review {
	return powDay("2026-11-04",
		[]ProposedMeeting{
			{Ref: "m1", Start: "2026-11-04T10:00:00+07:00", End: "2026-11-04T11:00:00+07:00",
				Title: "Intern Academy - Chat Bridge Project Idea"},
			{Ref: "m2", Start: "2026-11-04T14:10:00+07:00", End: "2026-11-04T15:00:00+07:00",
				Title: "Chat Bridge Group A Meeting"},
		},
		"2026-11-04T09:30:56+07:00",
		"2026-11-04T14:00:55+07:00",
		"2026-11-04T14:07:12+07:00",
		"2026-11-04T14:17:47+07:00",
		"2026-11-04T14:19:21+07:00",
		"2026-11-04T22:00:46+07:00",
		"2026-11-04T22:10:20+07:00",
	)
}

// recordDraft records the draft against a state carrying the same durable
// inputs and returns the entries by block id.
func recordDraft(t *testing.T, review Review, d Draft) map[string]EntryRecord {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "state.json")
	ev := DayEvidence{}
	if review.Evidence != nil {
		ev = DayEvidence{Topics: review.Evidence.Topics, Meetings: review.Evidence.Meetings}
	}
	writeStateDay(t, statePath, review.Date, review.Timezone, ev, review.Confirmed.Segments)
	if _, err := RecordEntries(statePath, "test", draftAsReviewed(t, d), false, time.Now()); err != nil {
		t.Fatalf("record: %v", err)
	}
	f, err := ReadEntries(statePath, review.Date)
	if err != nil || f == nil {
		t.Fatalf("read entries: %+v %v", f, err)
	}
	byID := make(map[string]EntryRecord, len(f.Entries))
	for _, e := range f.Entries {
		byID[e.BlockID] = e
	}
	return byID
}

// TestWorkdayPartitionLive20261104 is the acceptance test for the reported
// defect: meetings keep their calendar spans, the two evening bursts stay
// apart, the empty lunch hour is not logged, and the slot between the morning
// meeting and lunch still names the work it sits in.
func TestWorkdayPartitionLive20261104(t *testing.T) {
	review := day20261104()
	d := BuildDraft(review, "test")

	wantIDs := []string{
		"evidence-09:00-10:00",
		"meeting-10:00-11:00",
		"evidence-11:00-12:00",
		"evidence-13:00-14:10",
		"meeting-14:10-15:00",
		"evidence-22:00-22:10",
	}
	if got := draftBlockIDs(d); !sameStrings(got, wantIDs) {
		t.Fatalf("blocks = %v, want %v", got, wantIDs)
	}

	meetingMinutes := 0
	for _, b := range d.Blocks {
		start, okS := draftClock(b.Start)
		end, okE := draftClock(b.End)
		if !okS || !okE || end <= start {
			t.Fatalf("block %s has no positive span: %+v", b.BlockID, b)
		}
		if b.Kind == "meeting" {
			meetingMinutes += end - start
		}
		if start < lunchEndMinute && end > lunchStartMinute {
			t.Fatalf("block %s crosses the empty lunch hour: %s-%s", b.BlockID, b.Start, b.End)
		}
	}
	if meetingMinutes != 110 {
		t.Fatalf("meeting minutes = %d, want 110 (1h50m of real calendar time)", meetingMinutes)
	}

	titles := []string{d.Blocks[1].Title, d.Blocks[4].Title}
	wantTitles := []string{"Intern Academy - Chat Bridge Project Idea", "Chat Bridge Group A Meeting"}
	if !sameStrings(titles, wantTitles) {
		t.Fatalf("meeting titles = %v, want both calendar titles %v", titles, wantTitles)
	}
	if len(d.Blocks[2].TopicCandidates) == 0 {
		t.Fatalf("evidence-11:00-12:00 carries no topic: %+v", d.Blocks[2])
	}
	if len(d.Blocks[4].TopicCandidates) != 2 {
		t.Fatalf("the afternoon meeting did not absorb the proof of work inside it: %+v", d.Blocks[4])
	}

	recorded := recordDraft(t, review, d)
	for _, b := range d.Blocks {
		got, ok := recorded[b.BlockID]
		if !ok {
			t.Fatalf("block %s was not recorded", b.BlockID)
		}
		recordedSourcesEqualDraft(t, got, b)
	}
}

// TestWorkdayPartitionLunchEvidencedByPOW: a published item inside 12:00-13:00
// proves the person worked through lunch, so the hour is not carved.
func TestWorkdayPartitionLunchEvidencedByPOW(t *testing.T) {
	review := powDay("2026-11-04", nil,
		"2026-11-04T09:30:00+07:00",
		"2026-11-04T12:25:00+07:00",
		"2026-11-04T17:00:00+07:00",
	)
	d := BuildDraft(review, "test")
	if got := draftBlockIDs(d); !sameStrings(got, []string{"evidence-09:00-17:00"}) {
		t.Fatalf("blocks = %v, want one unbroken span through lunch", got)
	}
}

// TestWorkdayPartitionMeetingAcrossLunch: a meeting keeps its own 11:30-12:30
// span, and the evidence around it is carved at both the meeting and the
// un-evidenced lunch hour.
func TestWorkdayPartitionMeetingAcrossLunch(t *testing.T) {
	review := powDay("2026-11-04",
		[]ProposedMeeting{{Ref: "m1", Start: "2026-11-04T11:30:00+07:00", End: "2026-11-04T12:30:00+07:00",
			Title: "Design sync"}},
		"2026-11-04T09:30:00+07:00",
		"2026-11-04T14:00:00+07:00",
	)
	d := BuildDraft(review, "test")
	want := []string{"evidence-09:00-11:30", "meeting-11:30-12:30", "evidence-13:00-14:00"}
	if got := draftBlockIDs(d); !sameStrings(got, want) {
		t.Fatalf("blocks = %v, want %v", got, want)
	}
}

// TestWorkdayPartitionSinglePOWBackfillsWorkdayStart: a lone instant records
// when the work was published, so work starts at 09:00 and ends at the proof.
func TestWorkdayPartitionSinglePOWBackfillsWorkdayStart(t *testing.T) {
	early := powDay("2026-11-04", nil, "2026-11-04T09:30:00+07:00")
	if got := draftBlockIDs(BuildDraft(early, "test")); !sameStrings(got, []string{"evidence-09:00-09:30"}) {
		t.Fatalf("blocks = %v, want the backfilled 09:00-09:30 span", got)
	}

	afterMeeting := powDay("2026-11-04",
		[]ProposedMeeting{{Ref: "m1", Start: "2026-11-04T09:00:00+07:00", End: "2026-11-04T10:00:00+07:00",
			Title: "Standup"}},
		"2026-11-04T11:10:00+07:00",
	)
	if got := draftBlockIDs(BuildDraft(afterMeeting, "test")); !sameStrings(got, []string{"meeting-09:00-10:00", "evidence-10:00-11:10"}) {
		t.Fatalf("blocks = %v, want the meeting then 10:00-11:10", got)
	}
}

// TestWorkdayPartitionPreDawnPOW: work that was published before the workday
// starts keeps its own early span instead of being pulled forward to 09:00.
func TestWorkdayPartitionPreDawnPOW(t *testing.T) {
	review := powDay("2026-10-28", nil, "2026-10-28T05:52:00+07:00")
	d := BuildDraft(review, "test")
	if got := draftBlockIDs(d); !sameStrings(got, []string{"evidence-05:37-05:52"}) {
		t.Fatalf("blocks = %v, want a span ending at the 05:52 proof", got)
	}
}

// TestWorkdayPartitionLateEveningPOW: an evening instant is never backfilled to
// the morning; it looks backward for its positive span instead.
func TestWorkdayPartitionLateEveningPOW(t *testing.T) {
	review := powDay("2026-11-04", nil, "2026-11-04T20:10:00+07:00")
	d := BuildDraft(review, "test")
	if got := draftBlockIDs(d); !sameStrings(got, []string{"evidence-19:55-20:10"}) {
		t.Fatalf("blocks = %v, want 19:55-20:10", got)
	}
}

// TestWorkdayPartitionStampOnConfirmedClosingMinute: a commitment published at
// the exact minute confirmed work closes proves that work's tail, and must not
// grow a one-minute evidence block after it (the guard this pins once fell back
// to [end, end+1)).
func TestWorkdayPartitionStampOnConfirmedClosingMinute(t *testing.T) {
	review := draftReview(nil,
		[]Segment{{IntervalID: "i1", Date: "2026-11-04", Start: "08:00", End: "09:00", Description: "planned stretch"}},
		[]TopicCandidate{{
			Ref: "tc-close", Title: "Closing push",
			FirstAt: "2026-11-04T09:00:00+07:00", LastAt: "2026-11-04T09:00:00+07:00",
			Items: []EvidenceItem{{
				Ref: "ev-close", Kind: "commit", Repo: "acme/api",
				Title: "closing note", Time: "2026-11-04T09:00:00+07:00",
			}},
		}},
		nil,
	)
	d := BuildDraft(review, "test")
	if got := draftBlockIDs(d); !sameStrings(got, []string{"work-08:00-09:00"}) {
		t.Fatalf("blocks = %v, want only the confirmed work block", got)
	}
	work := d.Blocks[0]
	if len(work.TopicCandidates) != 1 || work.TopicCandidates[0] != "Closing push" {
		t.Fatalf("work topics = %v, want the 09:00 stamp landing on the block it closes", work.TopicCandidates)
	}
}

// TestWorkdayPartitionOverlappingMeetings: two calendar entries that overlap
// each other merge into one meeting over their union, and both titles survive.
func TestWorkdayPartitionOverlappingMeetings(t *testing.T) {
	review := powDay("2026-11-04",
		[]ProposedMeeting{
			{Ref: "m1", Start: "2026-11-04T10:00:00+07:00", End: "2026-11-04T11:00:00+07:00", Title: "Sync"},
			{Ref: "m2", Start: "2026-11-04T10:30:00+07:00", End: "2026-11-04T11:30:00+07:00", Title: "Review"},
		},
		"2026-11-04T09:30:00+07:00",
	)
	d := BuildDraft(review, "test")
	if got := draftBlockIDs(d); !sameStrings(got, []string{"evidence-09:00-09:30", "meeting-10:00-11:30"}) {
		t.Fatalf("blocks = %v, want the union meeting and the carved evidence", got)
	}
	if want := "Sync + Review"; d.Blocks[1].Title != want {
		t.Fatalf("meeting title = %q, want %q", d.Blocks[1].Title, want)
	}

	touching := powDay("2026-11-04",
		[]ProposedMeeting{
			{Ref: "m1", Start: "2026-11-04T10:00:00+07:00", End: "2026-11-04T10:30:00+07:00", Title: "First"},
			{Ref: "m2", Start: "2026-11-04T10:30:00+07:00", End: "2026-11-04T11:00:00+07:00", Title: "Second"},
		},
	)
	td := BuildDraft(touching, "test")
	want := []string{"meeting-10:00-10:30", "meeting-10:30-11:00"}
	if got := draftBlockIDs(td); !sameStrings(got, want) {
		t.Fatalf("touching meetings = %v, want them kept apart as %v", got, want)
	}
}

// workdayFixtures is every day the partition is checked against: the four real
// days plus synthetic shapes that stress a carve boundary.
func workdayFixtures() map[string]Review {
	return map[string]Review{
		"2026-10-27": powDay("2026-10-27",
			[]ProposedMeeting{{Ref: "m1", Start: "2026-10-27T16:00:00+07:00", End: "2026-10-27T16:10:00+07:00", Title: "Check-in"}},
			"2026-10-27T11:49:00+07:00", "2026-10-27T13:54:00+07:00", "2026-10-27T18:26:00+07:00"),
		"2026-10-28": powDay("2026-10-28", nil,
			"2026-10-28T05:52:00+07:00", "2026-10-28T09:23:00+07:00", "2026-10-28T11:05:00+07:00",
			"2026-10-28T14:00:00+07:00", "2026-10-28T17:28:00+07:00"),
		"2026-11-03": powDay("2026-11-03",
			[]ProposedMeeting{{Ref: "m1", Start: "2026-11-03T16:15:00+07:00", End: "2026-11-03T16:30:00+07:00", Title: "1vs1"}},
			"2026-11-03T11:30:00+07:00", "2026-11-03T11:40:00+07:00", "2026-11-03T14:38:00+07:00",
			"2026-11-03T15:00:00+07:00", "2026-11-03T17:00:00+07:00", "2026-11-03T17:46:00+07:00"),
		"2026-11-04": day20261104(),
		"meeting-inside-work": powDay("2026-11-03",
			[]ProposedMeeting{{Ref: "m1", Start: "2026-11-03T13:30:00+07:00", End: "2026-11-03T14:30:00+07:00", Title: "All hands"}},
			"2026-11-03T09:10:00+07:00", "2026-11-03T17:20:00+07:00"),
		"no-evidence": powDay("2026-11-03",
			[]ProposedMeeting{{Ref: "m1", Start: "2026-11-03T09:00:00+07:00", End: "2026-11-03T10:00:00+07:00", Title: "Standup"}}),
	}
}

// TestWorkdayPartitionRegressionDays pins the three other real days from the
// report: each now starts at the workday, keeps its meeting exact, and never
// logs the empty lunch hour.
func TestWorkdayPartitionRegressionDays(t *testing.T) {
	want := map[string][]string{
		"2026-10-27": {
			"evidence-09:00-12:00", "evidence-13:00-16:00", "meeting-16:00-16:10", "evidence-16:10-18:26",
		},
		"2026-10-28": {"evidence-05:37-05:52", "evidence-09:23-12:00", "evidence-13:00-17:28"},
		"2026-11-03": {
			"evidence-09:00-12:00", "evidence-13:00-16:15", "meeting-16:15-16:30", "evidence-16:30-17:46",
		},
	}
	fixtures := workdayFixtures()
	for date, ids := range want {
		t.Run(date, func(t *testing.T) {
			if got := draftBlockIDs(BuildDraft(fixtures[date], "test")); !sameStrings(got, ids) {
				t.Fatalf("blocks = %v, want %v", got, ids)
			}
		})
	}
}

// TestWorkdayPartitionInvariants holds on every fixture: blocks are positive,
// pairwise non-overlapping, uniquely identified, and never drop a topic.
func TestWorkdayPartitionInvariants(t *testing.T) {
	for name, review := range workdayFixtures() {
		t.Run(name, func(t *testing.T) {
			d := BuildDraft(review, "test")
			if len(d.Blocks) == 0 {
				t.Fatal("no blocks at all")
			}
			seenID := map[string]bool{}
			type span struct{ start, end int }
			var spans []span
			for _, b := range d.Blocks {
				if seenID[b.BlockID] {
					t.Fatalf("duplicate block id %q", b.BlockID)
				}
				seenID[b.BlockID] = true
				if len(b.Start) == 0 {
					continue
				}
				start, okS := draftClock(b.Start)
				end, okE := draftClock(b.End)
				if !okS || !okE || end <= start {
					t.Fatalf("block %s has no positive span: %+v", b.BlockID, b)
				}
				for _, other := range spans {
					if start < other.end && other.start < end {
						t.Fatalf("block %s overlaps an earlier block: %+v vs %+v", b.BlockID, spans, other)
					}
				}
				spans = append(spans, span{start, end})
			}
			for _, topic := range review.Evidence.Topics {
				found := false
				for _, b := range d.Blocks {
					for _, title := range b.TopicCandidates {
						if title == topic.Title {
							found = true
						}
					}
				}
				if !found {
					t.Fatalf("topic %q landed on no block: %+v", topic.Title, d.Blocks)
				}
			}
		})
	}
}
