package timesheet

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func workHoursPolicy() *WorkHoursConfig {
	return &WorkHoursConfig{
		Days: []string{"mon", "tue", "wed", "thu", "fri"},
		Windows: []WorkWindow{
			{Start: "09:00", End: "12:00"},
			{Start: "13:00", End: "18:00"},
		},
	}
}

func vnLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	return loc
}

func TestAnnotateWorkHoursClassification(t *testing.T) {
	d := Draft{Date: "2026-10-26", Blocks: []DraftBlock{
		{BlockID: "inside", Kind: "work", Start: "09:00", End: "10:30"},
		{BlockID: "before", Kind: "work", Start: "07:00", End: "08:00"},
		{BlockID: "lunch", Kind: "work", Start: "12:00", End: "13:00"},
		{BlockID: "after", Kind: "work", Start: "18:30", End: "19:00"},
		{BlockID: "partial-start", Kind: "meeting", Start: "08:30", End: "09:30"},
		{BlockID: "partial-end", Kind: "meeting", Start: "17:30", End: "18:30"},
		{BlockID: "across-lunch", Kind: "work", Start: "11:30", End: "13:30"},
		{BlockID: "day", Kind: "day"},
	}}
	AnnotateWorkHours(&d, workHoursPolicy(), vnLoc(t))

	want := map[string]string{
		"inside":        "",
		"before":        "before_window",
		"lunch":         "between_windows",
		"after":         "after_window",
		"partial-start": "partial",
		"partial-end":   "partial",
		"across-lunch":  "partial",
		"day":           "",
	}
	for _, b := range d.Blocks {
		if b.WorkHoursReason != want[b.BlockID] {
			t.Fatalf("%s reason = %q, want %q", b.BlockID, b.WorkHoursReason, want[b.BlockID])
		}
		if b.OutsideWorkHours != (want[b.BlockID] != "") {
			t.Fatalf("%s outside = %v, reason = %q, want reason %q", b.BlockID, b.OutsideWorkHours, b.WorkHoursReason, want[b.BlockID])
		}
	}
	if d.Blocks[4].Start != "08:30" || d.Blocks[4].End != "09:30" {
		t.Fatalf("annotation changed the block span: %+v", d.Blocks[4])
	}
}

func TestAnnotateWorkHoursWeekend(t *testing.T) {
	d := Draft{Date: "2026-10-31", Blocks: []DraftBlock{
		{BlockID: "sat", Kind: "work", Start: "10:00", End: "11:00"},
	}}
	AnnotateWorkHours(&d, workHoursPolicy(), vnLoc(t))
	if !d.Blocks[0].OutsideWorkHours || d.Blocks[0].WorkHoursReason != "day_not_in_policy" {
		t.Fatalf("weekend block = %+v, want day_not_in_policy", d.Blocks[0])
	}
}

func TestAnnotateWorkHoursNilPolicyByteIdentical(t *testing.T) {
	review := draftReview(
		[]ProposedMeeting{{Ref: "m1", Start: "2026-10-26T09:00:00+07:00", End: "2026-10-26T10:00:00+07:00", Title: "Standup"}},
		[]Segment{{IntervalID: "i1", Date: "2026-10-26", Start: "10:30", End: "12:00", Description: "focus"}},
		nil, nil,
	)
	d := BuildDraft(review, "test")
	before, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal before: %v", err)
	}
	AnnotateWorkHours(&d, nil, vnLoc(t))
	after, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal after: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("nil policy changed the draft:\nbefore %s\nafter  %s", before, after)
	}
	if bytes.Contains(after, []byte("outside_work_hours")) || bytes.Contains(after, []byte("work_hours_reason")) {
		t.Fatalf("annotation fields leaked into a no-policy draft: %s", after)
	}
}

func TestAnnotateWorkHoursSerializesReason(t *testing.T) {
	d := Draft{Date: "2026-10-26", Blocks: []DraftBlock{
		{BlockID: "in", Kind: "work", Start: "09:00", End: "10:00"},
		{BlockID: "out", Kind: "work", Start: "19:00", End: "20:00"},
	}}
	AnnotateWorkHours(&d, workHoursPolicy(), vnLoc(t))
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Count(data, []byte("outside_work_hours")) != 1 {
		t.Fatalf("expected exactly one annotated block: %s", data)
	}
	var fields struct {
		Blocks    []map[string]any `json:"blocks"`
		WorkHours *struct {
			Days    []string `json:"days"`
			Windows []struct {
				Start string `json:"start"`
				End   string `json:"end"`
			} `json:"windows"`
		} `json:"work_hours"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if fields.WorkHours == nil || len(fields.WorkHours.Days) != 5 || len(fields.WorkHours.Windows) != 2 {
		t.Fatalf("draft work_hours echo = %+v, want the policy that produced the annotation", fields.WorkHours)
	}
	if _, ok := fields.Blocks[0]["outside_work_hours"]; ok {
		t.Fatalf("inside block carries an annotation: %v", fields.Blocks[0])
	}
	if fields.Blocks[1]["outside_work_hours"] != true || fields.Blocks[1]["work_hours_reason"] != "after_window" {
		t.Fatalf("outside block annotations = %v, want true/after_window", fields.Blocks[1])
	}
	var echo struct {
		WorkHours *WorkHoursConfig `json:"work_hours"`
	}
	if err := json.Unmarshal(data, &echo); err != nil {
		t.Fatalf("unmarshal policy echo: %v", err)
	}
	if echo.WorkHours == nil || len(echo.WorkHours.Days) != 5 || len(echo.WorkHours.Windows) != 2 {
		t.Fatalf("work_hours echo = %+v, want the configured policy", echo.WorkHours)
	}
	if strings.Contains(string(data), `"work_hours":null`) {
		t.Fatalf("policy echo must be omitted, not null: %s", data)
	}
}

func TestBuildDraftConvertsMeetingInstantsToLocalWallClock(t *testing.T) {
	review := Review{
		Date:     "2026-10-20",
		Timezone: "Asia/Ho_Chi_Minh",
		Evidence: &EvidenceReview{
			Meetings: []ProposedMeeting{{
				Ref:   "cal:synthetic@20261020T070000Z",
				Title: "Daily sync",
				Start: "2026-10-20T07:00:00Z",
				End:   "2026-10-20T08:00:00Z",
			}},
		},
	}

	draft := BuildDraft(review, "test")
	if len(draft.Blocks) != 1 {
		t.Fatalf("blocks = %+v, want the one meeting", draft.Blocks)
	}
	if b := draft.Blocks[0]; b.Start != "14:00" || b.End != "15:00" {
		t.Fatalf("meeting block = %s-%s, want the local wall clock 14:00-15:00", b.Start, b.End)
	}

	AnnotateWorkHours(&draft, workHoursPolicy(), vnLoc(t))
	if b := draft.Blocks[0]; b.OutsideWorkHours || b.WorkHoursReason != "" {
		t.Fatalf("meeting block = %+v, want inside main work hours after local conversion", b)
	}
}

func TestBuildDraftKeepsBareLocalMeetingClock(t *testing.T) {
	review := Review{
		Date:     "2026-10-20",
		Timezone: "Asia/Ho_Chi_Minh",
		Evidence: &EvidenceReview{
			Meetings: []ProposedMeeting{{
				Ref:   "cal:synthetic@local",
				Title: "Already local",
				Start: "09:30",
				End:   "10:30",
			}},
		},
	}

	draft := BuildDraft(review, "test")
	if len(draft.Blocks) != 1 {
		t.Fatalf("blocks = %+v, want the one meeting", draft.Blocks)
	}
	if b := draft.Blocks[0]; b.Start != "09:30" || b.End != "10:30" {
		t.Fatalf("meeting block = %s-%s, want the bare local clock unchanged", b.Start, b.End)
	}
}

func TestBuildDraftResolvesTopicAnchorsInDayTimezone(t *testing.T) {
	review := Review{
		Date:     "2026-10-20",
		Timezone: "Asia/Ho_Chi_Minh",
		Confirmed: View{Segments: []Segment{{
			IntervalID: "iv-1", Start: "14:30", End: "15:30", Description: "overlapping work",
		}}},
		Evidence: &EvidenceReview{
			Meetings: []ProposedMeeting{{
				Ref: "cal:synthetic@20261020T070000Z", Title: "Daily sync",
				Start: "2026-10-20T07:00:00Z", End: "2026-10-20T08:00:00Z",
			}},
			Topics: []TopicCandidate{{
				Ref: "tc:pr-7", Title: "Pull request #7 (acme/api)",
				FirstAt: "2026-10-20T07:20:00Z", LastAt: "2026-10-20T07:40:00Z",
				Items: []EvidenceItem{{Ref: "gh:acme/api:pr:7", Kind: "authored_pull_request", Time: "2026-10-20T07:20:00Z"}},
			}},
		},
	}

	draft := BuildDraft(review, "test")
	if len(draft.Blocks) != 2 {
		t.Fatalf("blocks = %+v, want the local meeting and the carved work remainder", draft.Blocks)
	}
	b := blockByID(t, draft, "meeting-14:00-15:00")
	if b.Kind != "meeting" {
		t.Fatalf("block = %s %s-%s, want the local meeting keeping its own span", b.Kind, b.Start, b.End)
	}
	if len(b.TopicCandidates) != 1 || b.TopicCandidates[0] != "Pull request #7 (acme/api)" {
		t.Fatalf("topic candidates = %+v, want the 14:20-local topic attached to the meeting", b.TopicCandidates)
	}
	if work := blockByID(t, draft, "work-15:00-15:30"); work.Kind != "work" {
		t.Fatalf("work block = %+v, want the confirmed interval carved at the meeting", work)
	}

	AnnotateWorkHours(&draft, workHoursPolicy(), vnLoc(t))
	if draft.Blocks[0].OutsideWorkHours || draft.Blocks[0].WorkHoursReason != "" {
		t.Fatalf("annotated block = %+v, want inside main work hours", draft.Blocks[0])
	}
}

func TestBuildDraftMeetingCarvesWorkAtItsSpan(t *testing.T) {
	meeting := ProposedMeeting{
		Ref: "cal:synthetic@20261020T070000Z", Title: "Daily sync",
		Start: "2026-10-20T07:00:00Z", End: "2026-10-20T08:00:00Z",
	}
	build := func(segments []Segment) Draft {
		return BuildDraft(Review{
			Date: "2026-10-20", Timezone: "Asia/Ho_Chi_Minh",
			Confirmed: View{Segments: segments},
			Evidence:  &EvidenceReview{Meetings: []ProposedMeeting{meeting}},
		}, "test")
	}

	overlapping := build([]Segment{{IntervalID: "iv-over", Start: "14:30", End: "15:30", Description: "inside"}})
	if len(overlapping.Blocks) != 2 || overlapping.Blocks[0].BlockID != "meeting-14:00-15:00" {
		t.Fatalf("overlapping blocks = %+v, want the meeting plus the carved work", overlapping.Blocks)
	}
	if b := overlapping.Blocks[1]; b.Kind != "work" || b.Start != "15:00" || b.End != "15:30" {
		t.Fatalf("carved block = %+v, want 15:00-15:30", b)
	}

	touching := build([]Segment{{IntervalID: "iv-touch", Start: "15:00", End: "16:00", Description: "after"}})
	if len(touching.Blocks) != 2 {
		t.Fatalf("touching blocks = %+v, want meeting plus the touching work block", touching.Blocks)
	}
	if touching.Blocks[1].Kind != "work" || touching.Blocks[1].Start != "15:00" || touching.Blocks[1].End != "16:00" {
		t.Fatalf("touching work block = %+v, want 15:00-16:00 kept", touching.Blocks[1])
	}
}
