package timesheet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func noOverlapReview() Review {
	return draftReview(
		[]ProposedMeeting{
			{Ref: "m1", Start: "2026-10-26T09:00:00+07:00", End: "2026-10-26T09:30:00+07:00", Title: "Standup"},
			{Ref: "m2", Start: "2026-10-26T14:00:00+07:00", End: "2026-10-26T15:00:00+07:00", Title: "Design review"},
		},
		[]Segment{
			{IntervalID: "i1", Date: "2026-10-26", Start: "10:00", End: "11:00", Description: "Bridge PRs"},
			{IntervalID: "i2", Date: "2026-10-26", Start: "16:00", End: "17:30", Description: "Deep work"},
		},
		[]TopicCandidate{
			{Title: "Calendar filtering", FirstAt: "2026-10-26T09:05:00+07:00", LastAt: "2026-10-26T09:25:00+07:00",
				Items: []EvidenceItem{
					{Kind: "commit", Repo: "acme/api", Title: "fix window", Time: "2026-10-26T09:10:00+07:00"},
					{Kind: "pull_request", Repo: "acme/api", Resource: "pr:acme/api:12", Title: "Add filter", Time: "2026-10-26T09:20:00+07:00"},
				}},
			{Title: "Pull request #21 (acme/web)", FirstAt: "2026-10-26T10:10:00+07:00", LastAt: "2026-10-26T10:40:00+07:00",
				Items: []EvidenceItem{{Kind: "pull_request", Repo: "acme/web", Resource: "pr:acme/web:21", Title: "Ship table", Time: "2026-10-26T10:20:00+07:00"}}},
			{Title: "Evening research", FirstAt: "2026-10-26T19:00:00+07:00", LastAt: "2026-10-26T20:00:00+07:00",
				Items: []EvidenceItem{{Kind: "commit", Repo: "acme/api", Title: "read notes", Time: "2026-10-26T19:30:00+07:00"}}},
		},
		[]ComposedSourceStatus{
			{Source: "github", Enabled: true, Status: StatusComplete},
			{Source: "calendar", Enabled: true, Status: StatusComplete},
		},
	)
}

func TestBuildDraftNoOverlapGolden(t *testing.T) {
	raw, err := json.Marshal(BuildDraft(noOverlapReview(), "test"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"date":"2026-10-26","timezone":"Asia/Ho_Chi_Minh","app_version":"test","blocks":[{"block_id":"meeting-09:00-09:30","kind":"meeting","start":"09:00","end":"09:30","title":"Standup","topic_candidates":["Calendar filtering"],"prs":["#12 acme/api"],"prs_inline":"PRs: acme/api: #12","completeness":{"calendar":"complete","github":"complete"}},{"block_id":"work-10:00-11:00","kind":"work","start":"10:00","end":"11:00","title":"Bridge PRs","topic_candidates":["Pull request #21 (acme/web)"],"prs":["#21 acme/web"],"prs_inline":"PRs: acme/web: #21","completeness":{"calendar":"complete","github":"complete"}},{"block_id":"meeting-14:00-15:00","kind":"meeting","start":"14:00","end":"15:00","title":"Design review","completeness":{"calendar":"complete","github":"complete"}},{"block_id":"work-16:00-17:30","kind":"work","start":"16:00","end":"17:30","title":"Deep work","completeness":{"calendar":"complete","github":"complete"}},{"block_id":"evidence-19:15-19:30","kind":"evidence","start":"19:15","end":"19:30","topic_candidates":["Evening research"],"completeness":{"calendar":"complete","github":"complete"}}]}`
	if string(raw) != want {
		t.Fatalf("no-overlap draft changed:\n got %s\nwant %s", raw, want)
	}
}

func blockByID(t *testing.T, d Draft, id string) DraftBlock {
	t.Helper()
	for _, b := range d.Blocks {
		if b.BlockID == id {
			return b
		}
	}
	t.Fatalf("no block %q in %+v", id, d.Blocks)
	return DraftBlock{}
}

func sourceKeys(b DraftBlock) []string {
	out := make([]string, 0, len(b.Sources))
	for _, s := range b.Sources {
		out = append(out, s.Kind+"|"+s.Title)
	}
	return out
}

func segment(desc, start, end string) Segment {
	return Segment{IntervalID: "i-" + start, Date: "2026-10-26", Start: start, End: end, Description: desc}
}

func topic(title, first, last string, items ...EvidenceItem) TopicCandidate {
	return TopicCandidate{
		Title:   title,
		FirstAt: "2026-10-26T" + first + ":00+07:00",
		LastAt:  "2026-10-26T" + last + ":00+07:00",
		Items:   items,
	}
}

func item(kind, title, at string) EvidenceItem {
	return EvidenceItem{Kind: kind, Repo: "acme/api", Title: title, Time: "2026-10-26T" + at + ":00+07:00"}
}

func TestBuildDraftOverlappingWorkBlocksMerge(t *testing.T) {
	review := draftReview(nil,
		[]Segment{segment("Bridge PRs", "09:00", "11:00"), segment("Ops deploy", "10:00", "12:00")},
		[]TopicCandidate{
			topic("API bridge", "09:10", "09:20", item("commit", "bridge handler", "09:15")),
			topic("Ops deploy", "11:00", "11:30", item("pull_request", "deploy runbook", "11:15")),
		},
		nil,
	)
	d := BuildDraft(review, "test")
	if len(d.Blocks) != 1 {
		t.Fatalf("blocks = %+v, want one merged block", d.Blocks)
	}
	b := d.Blocks[0]
	if b.BlockID != "work-09:00-12:00" || b.Kind != "work" || b.Start != "09:00" || b.End != "12:00" {
		t.Fatalf("merged block = %s %s-%s, want work-09:00-12:00 over the union span", b.BlockID, b.Start, b.End)
	}
	if b.Title != "Bridge PRs + Ops deploy" {
		t.Fatalf("merged title = %q, want both titles", b.Title)
	}
	wantTopics := []string{"API bridge", "Ops deploy"}
	if len(b.TopicCandidates) != len(wantTopics) {
		t.Fatalf("topic candidates = %+v, want %+v", b.TopicCandidates, wantTopics)
	}
	for i, want := range wantTopics {
		if b.TopicCandidates[i] != want {
			t.Fatalf("topic candidates = %+v, want %+v", b.TopicCandidates, wantTopics)
		}
	}
	wantSources := []string{"commit|bridge handler", "pull_request|deploy runbook"}
	gotSources := sourceKeys(b)
	if len(gotSources) != len(wantSources) {
		t.Fatalf("sources = %+v, want %+v", gotSources, wantSources)
	}
	for i, want := range wantSources {
		if gotSources[i] != want {
			t.Fatalf("sources = %+v, want %+v", gotSources, wantSources)
		}
	}
	dAgain := BuildDraft(review, "test")
	first, _ := json.Marshal(d)
	second, _ := json.Marshal(dAgain)
	if !bytes.Equal(first, second) {
		t.Fatalf("same input produced different drafts:\n%s\n%s", first, second)
	}
}

func TestBuildDraftChainedOverlapIsOneBlock(t *testing.T) {
	review := draftReview(nil,
		[]Segment{
			segment("A", "09:00", "10:00"),
			segment("B", "09:30", "11:00"),
			segment("C", "10:30", "12:00"),
		},
		nil, nil,
	)
	d := BuildDraft(review, "test")
	if len(d.Blocks) != 1 {
		t.Fatalf("blocks = %+v, want one chained cluster", d.Blocks)
	}
	if b := d.Blocks[0]; b.Start != "09:00" || b.End != "12:00" || b.Title != "A + B + C" {
		t.Fatalf("chained block = %+v, want 09:00-12:00 naming all three", b)
	}
}

func TestBuildDraftTouchingBlocksStaySeparate(t *testing.T) {
	review := draftReview(nil,
		[]Segment{segment("First", "09:00", "10:00"), segment("Second", "10:00", "11:00")},
		nil, nil,
	)
	d := BuildDraft(review, "test")
	if len(d.Blocks) != 2 {
		t.Fatalf("blocks = %+v, want two touching blocks kept apart", d.Blocks)
	}
	if first := blockByID(t, d, "work-09:00-10:00"); first.Title != "First" {
		t.Fatalf("first block = %+v, want its own title", first)
	}
	if second := blockByID(t, d, "work-10:00-11:00"); second.Title != "Second" {
		t.Fatalf("second block = %+v, want its own title", second)
	}
}

func TestBuildDraftMeetingKeepsItsSpanAndCarvesWork(t *testing.T) {
	review := draftReview(
		[]ProposedMeeting{{Ref: "m1", Start: "2026-10-26T09:00:00+07:00", End: "2026-10-26T10:00:00+07:00", Title: "Standup"}},
		[]Segment{segment("Joined standup", "09:30", "10:30")},
		[]TopicCandidate{
			topic("Calendar filtering", "09:10", "09:20", item("commit", "fix window", "09:15")),
			topic("Pipeline fix", "10:00", "10:20", item("pull_request", "green build", "10:10")),
		},
		nil,
	)
	d := BuildDraft(review, "test")
	if len(d.Blocks) != 2 {
		t.Fatalf("blocks = %+v, want the meeting and the carved work", d.Blocks)
	}
	b := blockByID(t, d, "meeting-09:00-10:00")
	if b.Kind != "meeting" || b.Title != "Standup" {
		t.Fatalf("meeting block = %+v, want the calendar title over its own span", b)
	}
	if len(b.TopicCandidates) != 1 || b.TopicCandidates[0] != "Calendar filtering" {
		t.Fatalf("meeting topics = %+v, want the item that happened inside it", b.TopicCandidates)
	}
	if len(b.Sources) != 1 {
		t.Fatalf("meeting sources = %+v, want only its own contributor", b.Sources)
	}
	w := blockByID(t, d, "work-10:00-10:30")
	if w.Kind != "work" || w.Title != "Joined standup" {
		t.Fatalf("work block = %+v, want the carved remainder with its description", w)
	}
	if len(w.TopicCandidates) != 1 || w.TopicCandidates[0] != "Pipeline fix" {
		t.Fatalf("work topics = %+v", w.TopicCandidates)
	}
}

func TestBuildDraftCarvesEvidenceAroundMeetings(t *testing.T) {
	review := draftReview(
		[]ProposedMeeting{
			{Ref: "m1", Start: "2026-10-26T06:40:00+07:00", End: "2026-10-26T07:15:00+07:00", Title: "Workflow training"},
			{Ref: "m2", Start: "2026-10-26T09:00:00+07:00", End: "2026-10-26T09:30:00+07:00", Title: "Standup"},
		},
		nil,
		[]TopicCandidate{
			topic("Early commit", "02:16", "02:16", item("commit", "early", "02:16")),
			topic("Between meetings", "08:00", "08:15", item("commit", "mid", "08:00")),
			topic("After standup", "10:00", "10:00", item("pull_request", "late", "10:00")),
			topic("Last item", "13:00", "13:00", item("commit", "last", "13:00")),
		},
		nil,
	)
	d := BuildDraft(review, "test")
	want := []string{
		"evidence-02:16-06:40",
		"meeting-06:40-07:15",
		"evidence-07:15-08:00",
		"meeting-09:00-09:30",
		"evidence-10:00-12:00",
	}
	if got := draftBlockIDs(d); !sameStrings(got, want) {
		t.Fatalf("blocks = %v, want %v", got, want)
	}
	for _, b := range d.Blocks {
		if b.Kind == "meeting" && b.Start != "06:40" && b.Start != "09:00" {
			t.Fatalf("meeting block = %+v, want a calendar start", b)
		}
	}
	if b := blockByID(t, d, "evidence-10:00-12:00"); len(b.TopicCandidates) != 2 {
		t.Fatalf("evidence topics = %+v, want both contributors", b.TopicCandidates)
	}
}

func TestRecordEntriesMergesOverlappingBlocks(t *testing.T) {
	review := draftReview(nil,
		[]Segment{segment("Bridge PRs", "09:00", "11:00"), segment("Ops deploy", "10:00", "12:00")},
		[]TopicCandidate{
			topic("API bridge", "09:10", "09:20", item("commit", "bridge handler", "09:15")),
			topic("Ops deploy", "11:00", "11:30", item("pull_request", "deploy runbook", "11:15")),
		},
		nil,
	)
	draft := BuildDraft(review, "test")
	if len(draft.Blocks) != 1 {
		t.Fatalf("draft blocks = %+v, want one merged block", draft.Blocks)
	}
	want := draft.Blocks[0]

	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	writeStateDay(t, statePath, "2026-10-26", "Asia/Ho_Chi_Minh",
		DayEvidence{Topics: review.Evidence.Topics}, review.Confirmed.Segments)
	stale := ReviewedDraft{Date: "2026-10-26", Timezone: "Asia/Ho_Chi_Minh", Blocks: []ReviewedBlock{
		{DraftBlock: DraftBlock{BlockID: "work-09:00-11:00", Kind: "work", Start: "09:00", End: "11:00",
			TopicCandidates: []string{"API bridge"}}, Description: "Bridge PRs"},
		{DraftBlock: DraftBlock{BlockID: "work-10:00-12:00", Kind: "work", Start: "10:00", End: "12:00",
			TopicCandidates: []string{"Ops deploy"}}, Description: "Ops deploy"},
	}}
	if _, err := RecordEntries(statePath, "test", stale, false, time.Now()); err != nil {
		t.Fatalf("record: %v", err)
	}
	f, err := ReadEntries(statePath, "2026-10-26")
	if err != nil || f == nil || len(f.Entries) != 1 {
		t.Fatalf("read entries: %+v %v", f, err)
	}
	got := f.Entries[0]
	if got.BlockID != want.BlockID || got.Kind != "work" || got.Start != want.Start || got.End != want.End {
		t.Fatalf("recorded block = %s %s %s-%s, want the draft's id %s over its union span %s-%s",
			got.BlockID, got.Kind, got.Start, got.End, want.BlockID, want.Start, want.End)
	}
	if !strings.Contains(got.Description, "Bridge PRs") || !strings.Contains(got.Description, "Ops deploy") {
		t.Fatalf("recorded description = %q, want both contributors", got.Description)
	}
	if len(got.TopicCandidates) != len(want.TopicCandidates) {
		t.Fatalf("recorded topics = %+v, want the draft's %+v", got.TopicCandidates, want.TopicCandidates)
	}
	for i := range want.TopicCandidates {
		if got.TopicCandidates[i] != want.TopicCandidates[i] {
			t.Fatalf("recorded topics = %+v, want the draft's %+v", got.TopicCandidates, want.TopicCandidates)
		}
	}
	if len(got.Sources) != len(want.Sources) {
		t.Fatalf("recorded sources = %+v, want the draft's %+v", got.Sources, want.Sources)
	}
	for i := range want.Sources {
		if got.Sources[i] != want.Sources[i] {
			t.Fatalf("recorded sources = %+v, want the draft's %+v", got.Sources, want.Sources)
		}
	}
	merged := ReviewedDraft{Date: "2026-10-26", Timezone: "Asia/Ho_Chi_Minh", Blocks: []ReviewedBlock{
		{DraftBlock: DraftBlock{BlockID: got.BlockID, Kind: got.Kind, Start: got.Start, End: got.End}, Description: got.Description},
	}}
	if _, err := RecordEntries(statePath, "test", merged, false, time.Now()); err != nil {
		t.Fatalf("re-record: %v", err)
	}
	f2, err := ReadEntries(statePath, "2026-10-26")
	if err != nil || f2 == nil || len(f2.Entries) != 1 {
		t.Fatalf("after re-record: %+v %v, want one entry", f2, err)
	}
}

func absorbedEvidenceReview() Review {
	untimed := []EvidenceItem{}
	for i := 0; i < 7; i++ {
		untimed = append(untimed, item("commit", fmt.Sprintf("note %d", i), fmt.Sprintf("09:%02d", 45+i)))
	}
	return draftReview(
		[]ProposedMeeting{{Ref: "m1", Start: "2026-10-26T09:00:00+07:00", End: "2026-10-26T10:00:00+07:00", Title: "Standup"}},
		nil,
		[]TopicCandidate{
			topic("Endpoint research", "08:00", "08:30", item("commit", "read docs", "09:40")),
			topic("Later review", "11:00", "11:30", item("pull_request", "review deck", "10:10")),
			{Title: "Untimed notes", Items: untimed},
		},
		nil,
	)
}

func draftAsReviewed(t *testing.T, d Draft) ReviewedDraft {
	t.Helper()
	out := ReviewedDraft{Date: d.Date, Timezone: d.Timezone}
	for _, b := range d.Blocks {
		desc := b.Title
		if desc == "" {
			desc = "Work"
		}
		if b.PRsInline != "" {
			desc += " " + b.PRsInline
		}
		b.Sources = nil
		b.SourcesTruncated = false
		out.Blocks = append(out.Blocks, ReviewedBlock{DraftBlock: b, Description: desc})
	}
	return out
}

func recordedSourcesEqualDraft(t *testing.T, got EntryRecord, want DraftBlock) {
	t.Helper()
	if len(got.Sources) != len(want.Sources) {
		t.Fatalf("recorded sources = %d, draft = %d: %+v", len(got.Sources), len(want.Sources), got.Sources)
	}
	for i := range want.Sources {
		if got.Sources[i] != want.Sources[i] {
			t.Fatalf("recorded source[%d] = %+v, draft = %+v", i, got.Sources[i], want.Sources[i])
		}
	}
	if got.SourcesTruncated != want.SourcesTruncated {
		t.Fatalf("recorded sources_truncated = %v, draft = %v", got.SourcesTruncated, want.SourcesTruncated)
	}
}

func TestRecordSourcesMatchDraftWhenEvidenceStraddlesMeetingEdge(t *testing.T) {
	review := absorbedEvidenceReview()
	draft := BuildDraft(review, "test")
	if len(draft.Blocks) != 2 {
		t.Fatalf("draft blocks = %+v, want the meeting and the carved evidence", draft.Blocks)
	}
	meeting := blockByID(t, draft, "meeting-09:00-10:00")
	if meeting.Kind != "meeting" || len(meeting.Sources) != draftSourcesPerBlock || meeting.SourcesTruncated {
		t.Fatalf("meeting = %s sources=%d truncated=%v, want the 8 items that happened inside it",
			meeting.Kind, len(meeting.Sources), meeting.SourcesTruncated)
	}
	evidence := blockByID(t, draft, "evidence-10:00-10:10")
	if len(evidence.Sources) != 1 {
		t.Fatalf("evidence sources = %+v, want the one item past the meeting", evidence.Sources)
	}

	shapes := []struct {
		name string
		file ReviewedDraft
	}{
		{"hand-authored blocks", ReviewedDraft{Date: "2026-10-26", Timezone: "Asia/Ho_Chi_Minh", Blocks: []ReviewedBlock{
			{DraftBlock: DraftBlock{BlockID: "meeting-09:00-10:00", Kind: "meeting", Start: "09:00", End: "10:00"}, Description: "Standup"},
			{DraftBlock: DraftBlock{BlockID: "evidence-10:00-10:10", Kind: "evidence", Start: "10:00", End: "10:10"}, Description: "Evidence"},
		}}},
		{"draft", draftAsReviewed(t, draft)},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			dir := t.TempDir()
			statePath := filepath.Join(dir, "state.json")
			writeStateDay(t, statePath, "2026-10-26", "Asia/Ho_Chi_Minh",
				DayEvidence{Topics: review.Evidence.Topics, Meetings: review.Evidence.Meetings},
				review.Confirmed.Segments)
			if _, err := RecordEntries(statePath, "test", shape.file, false, time.Now()); err != nil {
				t.Fatalf("record: %v", err)
			}
			f, err := ReadEntries(statePath, "2026-10-26")
			if err != nil || f == nil || len(f.Entries) != 2 {
				t.Fatalf("read entries: %+v %v", f, err)
			}
			byID := map[string]EntryRecord{}
			for _, e := range f.Entries {
				byID[e.BlockID] = e
			}
			recordedSourcesEqualDraft(t, byID["meeting-09:00-10:00"], meeting)
			recordedSourcesEqualDraft(t, byID["evidence-10:00-10:10"], evidence)
		})
	}
}

func TestBuildDraftMeetingKeepsItsSpanWithDescriptionOnlyWork(t *testing.T) {
	review := draftReview(
		[]ProposedMeeting{{Ref: "m1", Start: "2026-10-26T09:00:00+07:00", End: "2026-10-26T10:00:00+07:00", Title: "Standup"}},
		[]Segment{segment("Repair billing outage", "09:30", "10:30")},
		nil, nil,
	)
	d := BuildDraft(review, "test")
	if len(d.Blocks) != 2 {
		t.Fatalf("blocks = %+v, want the meeting and the carved work", d.Blocks)
	}
	if got := blockByID(t, d, "meeting-09:00-10:00").Title; got != "Standup" {
		t.Fatalf("meeting title = %q, want the calendar title", got)
	}
	work := blockByID(t, d, "work-10:00-10:30")
	if work.Title != "Repair billing outage" {
		t.Fatalf("work title = %q, want the confirmed description on its own piece", work.Title)
	}
}

func TestBuildDraftTopicBackedWorkKeepsItsOwnTopics(t *testing.T) {
	review := draftReview(
		[]ProposedMeeting{{Ref: "m1", Start: "2026-10-26T09:00:00+07:00", End: "2026-10-26T10:00:00+07:00", Title: "Standup"}},
		[]Segment{segment("Joined standup", "09:30", "10:30")},
		[]TopicCandidate{topic("Pipeline fix", "10:00", "10:20", item("pull_request", "green build", "10:10"))},
		nil,
	)
	d := BuildDraft(review, "test")
	if len(d.Blocks) != 2 {
		t.Fatalf("blocks = %+v, want the meeting and the carved work", d.Blocks)
	}
	meeting := blockByID(t, d, "meeting-09:00-10:00")
	if meeting.Title != "Standup" || len(meeting.TopicCandidates) != 0 {
		t.Fatalf("meeting = %+v, want a clean calendar title", meeting)
	}
	work := blockByID(t, d, "work-10:00-10:30")
	if len(work.TopicCandidates) != 1 || work.TopicCandidates[0] != "Pipeline fix" {
		t.Fatalf("work topics = %+v, want the item that landed past the meeting", work.TopicCandidates)
	}
}

func TestBuildDraftSplitsWorkTopicsAtTheCarve(t *testing.T) {
	review := draftReview(nil,
		[]Segment{segment("Project A work", "09:00", "10:00")},
		[]TopicCandidate{
			topic("Project B work", "08:00", "08:30", item("commit", "b1", "09:30")),
			topic("Project B followup", "11:00", "11:30", item("commit", "b2", "10:20")),
		},
		nil,
	)
	d := BuildDraft(review, "test")
	if got := draftBlockIDs(d); !sameStrings(got, []string{"work-09:00-10:00", "evidence-10:00-10:20"}) {
		t.Fatalf("blocks = %v, want the work block then the evidence", got)
	}
	work := blockByID(t, d, "work-09:00-10:00")
	if work.Title != "Project A work" || len(work.TopicCandidates) != 1 || work.TopicCandidates[0] != "Project B work" {
		t.Fatalf("work block = %+v, want its description and the item inside it", work)
	}
	evidence := blockByID(t, d, "evidence-10:00-10:20")
	if len(evidence.TopicCandidates) != 1 || evidence.TopicCandidates[0] != "Project B followup" {
		t.Fatalf("evidence topics = %+v, want only the later item", evidence.TopicCandidates)
	}
}

func TestFoldReviewedClusterNamesEveryMeetingTitle(t *testing.T) {
	got := MergeOverlappingBlocks([]ReviewedBlock{
		{DraftBlock: DraftBlock{BlockID: "meeting-09:00-10:00", Kind: "meeting", Start: "09:00", End: "10:00", Title: "Intern Academy"},
			Description: "Morning"},
		{DraftBlock: DraftBlock{BlockID: "meeting-09:30-10:30", Kind: "meeting", Start: "09:30", End: "10:30", Title: "Group A"},
			Description: "Afternoon"},
	})
	if len(got) != 1 {
		t.Fatalf("blocks = %+v, want one union meeting", got)
	}
	if want := "Intern Academy + Group A"; got[0].Title != want {
		t.Fatalf("title = %q, want every meeting title kept as %q", got[0].Title, want)
	}
	if got[0].BlockID != "meeting-09:00-10:30" {
		t.Fatalf("id = %q, want the union-derived id", got[0].BlockID)
	}
}

func TestRecordAndDraftUseOneMergedBlockID(t *testing.T) {
	review := draftReview(nil,
		[]Segment{segment("Bridge PRs", "09:00", "11:00"), segment("Ops deploy", "10:00", "12:00")},
		nil, nil,
	)
	draft := BuildDraft(review, "test")
	if len(draft.Blocks) != 1 || draft.Blocks[0].BlockID != "work-09:00-12:00" {
		t.Fatalf("draft = %+v, want work-09:00-12:00 over the union span", draft.Blocks)
	}
	preMerge := ReviewedDraft{Date: "2026-10-26", Blocks: []ReviewedBlock{
		{DraftBlock: DraftBlock{BlockID: "work-09:00-11:00", Kind: "work", Start: "09:00", End: "11:00"}, Description: "Bridge PRs"},
		{DraftBlock: DraftBlock{BlockID: "work-10:00-12:00", Kind: "work", Start: "10:00", End: "12:00"}, Description: "Ops deploy"},
	}}

	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	if _, err := RecordEntries(statePath, "test", preMerge, false, time.Now()); err != nil {
		t.Fatalf("record: %v", err)
	}
	f, _ := ReadEntries(statePath, "2026-10-26")
	if f == nil || len(f.Entries) != 1 || f.Entries[0].BlockID != draft.Blocks[0].BlockID {
		t.Fatalf("recorded = %+v, want the draft's id %s", f, draft.Blocks[0].BlockID)
	}
	if _, err := RecordEntries(statePath, "test", draftAsReviewed(t, draft), false, time.Now()); err != nil {
		t.Fatalf("re-record draft: %v", err)
	}
	f2, _ := ReadEntries(statePath, "2026-10-26")
	if f2 == nil || len(f2.Entries) != 1 || f2.Entries[0].BlockID != draft.Blocks[0].BlockID {
		t.Fatalf("after recording the draft = %+v, want one entry under %s", f2, draft.Blocks[0].BlockID)
	}
	if _, err := RecordEntries(statePath, "test", preMerge, false, time.Now()); err != nil {
		t.Fatalf("re-record same content: %v", err)
	}
	f3, _ := ReadEntries(statePath, "2026-10-26")
	if f3 == nil || len(f3.Entries) != 1 || f3.Entries[0].BlockID != draft.Blocks[0].BlockID ||
		f3.Entries[0].Start != "09:00" || f3.Entries[0].End != "12:00" {
		t.Fatalf("after re-recording = %+v, want one unchanged union entry", f3)
	}
}

func TestRecordReplaceReportsObsoleteMergedContributors(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	if err := os.MkdirAll(filepath.Dir(EntriesPath(statePath, "2026-10-26")), 0o700); err != nil {
		t.Fatal(err)
	}
	existing := EntriesFile{Date: "2026-10-26", AppVersion: "test", Entries: []EntryRecord{
		{BlockID: "work-09:00-11:00", Date: "2026-10-26", Start: "09:00", End: "11:00", Description: "Bridge PRs", Kind: "work"},
		{BlockID: "work-10:00-12:00", Date: "2026-10-26", Start: "10:00", End: "12:00", Description: "Ops deploy", Kind: "work"},
	}}
	data, err := json.Marshal(existing)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(EntriesPath(statePath, "2026-10-26"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	reviewed := ReviewedDraft{Date: "2026-10-26", Blocks: []ReviewedBlock{
		{DraftBlock: DraftBlock{BlockID: "work-09:00-11:00", Kind: "work", Start: "09:00", End: "11:00"}, Description: "Bridge PRs"},
		{DraftBlock: DraftBlock{BlockID: "work-10:00-12:00", Kind: "work", Start: "10:00", End: "12:00"}, Description: "Ops deploy"},
	}}
	receipt, err := RecordEntries(statePath, "test", reviewed, true, time.Now())
	if err != nil {
		t.Fatalf("record --replace: %v", err)
	}
	if receipt.Removed != 2 {
		t.Fatalf("removed = %d, want both obsolete contributors reported", receipt.Removed)
	}
	f, _ := ReadEntries(statePath, "2026-10-26")
	if f == nil || len(f.Entries) != 1 || f.Entries[0].BlockID != "work-09:00-12:00" {
		t.Fatalf("entries = %+v, want one entry under the union-derived id", f)
	}
}

func TestMergeOverlappingBlocksResolvesUnionIDCollisions(t *testing.T) {
	got := MergeOverlappingBlocks([]ReviewedBlock{
		{DraftBlock: DraftBlock{BlockID: "work-09:00-11:00", Kind: "work", Start: "09:00", End: "11:00"}, Description: "A"},
		{DraftBlock: DraftBlock{BlockID: "work-10:00-12:00", Kind: "work", Start: "10:00", End: "12:00"}, Description: "B"},
		{DraftBlock: DraftBlock{BlockID: "work-09:00-12:00", Kind: "work"}, Description: "C"},
	})
	if len(got) != 2 {
		t.Fatalf("blocks = %+v, want the merged cluster and the singleton", got)
	}
	if got[0].BlockID != "work-09:00-12:00-2" {
		t.Fatalf("merged id = %q, want the union id suffixed past the existing singleton", got[0].BlockID)
	}
	if got[1].BlockID != "work-09:00-12:00" {
		t.Fatalf("singleton id = %q, want it untouched", got[1].BlockID)
	}
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func interleavedOwnershipReview() Review {
	return draftReview(
		[]ProposedMeeting{{Ref: "m1", Start: "2026-10-26T09:00:00+07:00", End: "2026-10-26T10:00:00+07:00", Title: "Standup"}},
		nil,
		[]TopicCandidate{
			topic("Early endpoint", "08:00", "08:00", item("commit", "early", "09:20")),
			topic("Inside meeting", "09:15", "09:20", item("pull_request", "inside", "09:15")),
			topic("Late endpoint", "11:00", "11:00", item("commit", "late", "11:05")),
			{Title: "Untimed topic", Items: []EvidenceItem{{Kind: "commit", Repo: "acme/api", Title: "untimed"}}},
		},
		nil,
	)
}

func TestRecordSourcesFollowEvidenceOrderWhenOwnershipInterleaves(t *testing.T) {
	review := interleavedOwnershipReview()
	draft := BuildDraft(review, "test")
	if got := draftBlockIDs(draft); !sameStrings(got, []string{"meeting-09:00-10:00", "evidence-10:00-11:05"}) {
		t.Fatalf("draft blocks = %v, want the meeting and the carved evidence", got)
	}
	meeting := blockByID(t, draft, "meeting-09:00-10:00")
	wantInside := []string{"commit|early", "pull_request|inside"}
	if got := sourceKeys(meeting); !sameStrings(got, wantInside) {
		t.Fatalf("meeting sources = %v, want the items inside it %v", got, wantInside)
	}
	evidence := blockByID(t, draft, "evidence-10:00-11:05")
	wantOutside := []string{"commit|late", "commit|untimed"}
	if got := sourceKeys(evidence); !sameStrings(got, wantOutside) {
		t.Fatalf("evidence sources = %v, want the items outside the meeting %v", got, wantOutside)
	}

	shapes := []struct {
		name string
		file ReviewedDraft
	}{
		{"hand-authored blocks", ReviewedDraft{Date: "2026-10-26", Timezone: "Asia/Ho_Chi_Minh", Blocks: []ReviewedBlock{
			{DraftBlock: DraftBlock{BlockID: "meeting-09:00-10:00", Kind: "meeting", Start: "09:00", End: "10:00",
				TopicCandidates: []string{"Inside meeting"}}, Description: "Standup"},
			{DraftBlock: DraftBlock{BlockID: "evidence-10:00-11:05", Kind: "evidence", Start: "10:00", End: "11:05",
				TopicCandidates: []string{"Late endpoint", "Untimed topic"}}, Description: "Evidence"},
		}}},
		{"draft", draftAsReviewed(t, draft)},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			dir := t.TempDir()
			statePath := filepath.Join(dir, "state.json")
			writeStateDay(t, statePath, review.Date, review.Timezone,
				DayEvidence{Topics: review.Evidence.Topics, Meetings: review.Evidence.Meetings},
				review.Confirmed.Segments)
			if _, err := RecordEntries(statePath, "test", shape.file, false, time.Now()); err != nil {
				t.Fatalf("record: %v", err)
			}
			f, err := ReadEntries(statePath, review.Date)
			if err != nil || f == nil || len(f.Entries) != 2 {
				t.Fatalf("read entries: %+v %v", f, err)
			}
			byID := map[string]EntryRecord{}
			for _, e := range f.Entries {
				byID[e.BlockID] = e
			}
			recordedSourcesEqualDraft(t, byID["meeting-09:00-10:00"], meeting)
			recordedSourcesEqualDraft(t, byID["evidence-10:00-11:05"], evidence)
		})
	}
}

func collapsedEvidenceReview() Review {
	return draftReview(
		[]ProposedMeeting{{Ref: "m1", Start: "2026-10-26T09:00:00+07:00", End: "2026-10-26T10:00:00+07:00", Title: "Standup"}},
		nil,
		[]TopicCandidate{
			{Title: "A unanchored", Items: []EvidenceItem{
				{Kind: "commit", Repo: "acme/api", Title: "a", Time: "2026-10-26T09:40:00+07:00"},
			}},
			{Title: "B spanning", FirstAt: "2026-10-26T10:05:00+07:00", LastAt: "2026-10-26T10:10:00+07:00",
				Items: []EvidenceItem{{Kind: "commit", Repo: "acme/api", Title: "b", Time: "2026-10-26T10:10:00+07:00"}}},
			{Title: "C untimed", Items: []EvidenceItem{{Kind: "commit", Repo: "acme/api", Title: "c"}}},
		},
		nil,
	)
}

func TestRecordSourcesKeepEvidencePastTheMeeting(t *testing.T) {
	review := collapsedEvidenceReview()
	draft := BuildDraft(review, "test")
	if got := draftBlockIDs(draft); !sameStrings(got, []string{"meeting-09:00-10:00", "evidence-10:00-10:10"}) {
		t.Fatalf("draft blocks = %v, want the meeting and the carved evidence", got)
	}
	meeting := blockByID(t, draft, "meeting-09:00-10:00")
	wantMeeting := []string{"commit|a"}
	if got := sourceKeys(meeting); !sameStrings(got, wantMeeting) {
		t.Fatalf("meeting sources = %v, want %v", got, wantMeeting)
	}
	evidence := blockByID(t, draft, "evidence-10:00-10:10")
	wantEvidence := []string{"commit|b", "commit|c"}
	if got := sourceKeys(evidence); !sameStrings(got, wantEvidence) {
		t.Fatalf("evidence sources = %v, want %v", got, wantEvidence)
	}

	shapes := []struct {
		name string
		file ReviewedDraft
	}{
		{"hand-authored blocks", ReviewedDraft{Date: "2026-10-26", Timezone: "Asia/Ho_Chi_Minh", Blocks: []ReviewedBlock{
			{DraftBlock: DraftBlock{BlockID: "meeting-09:00-10:00", Kind: "meeting", Start: "09:00", End: "10:00"}, Description: "Standup"},
			{DraftBlock: DraftBlock{BlockID: "evidence-10:00-10:10", Kind: "evidence", Start: "10:00", End: "10:10",
				TopicCandidates: []string{"B spanning", "C untimed"}}, Description: "Evidence"},
		}}},
		{"draft", draftAsReviewed(t, draft)},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			dir := t.TempDir()
			statePath := filepath.Join(dir, "state.json")
			writeStateDay(t, statePath, review.Date, review.Timezone,
				DayEvidence{Topics: review.Evidence.Topics, Meetings: review.Evidence.Meetings},
				review.Confirmed.Segments)
			if _, err := RecordEntries(statePath, "test", shape.file, false, time.Now()); err != nil {
				t.Fatalf("record: %v", err)
			}
			f, err := ReadEntries(statePath, review.Date)
			if err != nil || f == nil || len(f.Entries) != 2 {
				t.Fatalf("read entries: %+v %v", f, err)
			}
			byID := map[string]EntryRecord{}
			for _, e := range f.Entries {
				byID[e.BlockID] = e
			}
			recordedSourcesEqualDraft(t, byID["meeting-09:00-10:00"], meeting)
			recordedSourcesEqualDraft(t, byID["evidence-10:00-10:10"], evidence)
		})
	}
}

func TestRecordReportsBlocksOutsideTheRecomputedDraft(t *testing.T) {
	topics := []TopicCandidate{{
		Title:   "Evening evidence",
		FirstAt: "2026-10-26T20:00:00+07:00", LastAt: "2026-10-26T21:00:00+07:00",
		Items: []EvidenceItem{{Kind: "commit", Repo: "acme/api", Title: "read notes", Time: "2026-10-26T20:10:00+07:00"}},
	}}
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	writeStateDay(t, statePath, "2026-10-26", "Asia/Ho_Chi_Minh", DayEvidence{Topics: topics}, nil)

	var logged bytes.Buffer
	previous := recordDiagnostics
	recordDiagnostics = &logged
	t.Cleanup(func() { recordDiagnostics = previous })

	unknown := ReviewedDraft{Date: "2026-10-26", Timezone: "Asia/Ho_Chi_Minh", Blocks: []ReviewedBlock{{
		DraftBlock:  DraftBlock{BlockID: "work-22:00-23:00", Kind: "work", Start: "22:00", End: "23:00"},
		Description: "Hand-edited block",
	}}}
	if _, err := RecordEntries(statePath, "test", unknown, false, time.Now()); err != nil {
		t.Fatalf("record: %v", err)
	}
	want := "record: block work-22:00-23:00 is not in the recomputed draft; " +
		"the state no longer matches the draft, so no sources were attached\n"
	if logged.String() != want {
		t.Fatalf("diagnostic = %q, want %q", logged.String(), want)
	}
	f, err := ReadEntries(statePath, "2026-10-26")
	if err != nil || f == nil || len(f.Entries) != 1 {
		t.Fatalf("entries = %+v %v, want the block still recorded", f, err)
	}
	if len(f.Entries[0].Sources) != 0 {
		t.Fatalf("recorded sources = %+v, want none", f.Entries[0].Sources)
	}

	draft := BuildDraft(draftReview(nil, nil, topics, nil), "test")
	if len(draft.Blocks) != 1 {
		t.Fatalf("draft blocks = %+v, want the evidence block", draft.Blocks)
	}
	wantBlock := draft.Blocks[0]
	logged.Reset()
	drafted := ReviewedDraft{Date: "2026-10-26", Timezone: "Asia/Ho_Chi_Minh", Blocks: []ReviewedBlock{{
		DraftBlock:  wantBlock,
		Description: "Evening notes",
	}}}
	if _, err := RecordEntries(statePath, "test", drafted, false, time.Now()); err != nil {
		t.Fatalf("record the drafted block: %v", err)
	}
	if logged.String() != "" {
		t.Fatalf("diagnostic = %q, want none for a resolving id", logged.String())
	}
	f2, err := ReadEntries(statePath, "2026-10-26")
	if err != nil || f2 == nil {
		t.Fatalf("entries = %+v %v", f2, err)
	}
	for _, entry := range f2.Entries {
		if entry.BlockID != wantBlock.BlockID {
			continue
		}
		recordedSourcesEqualDraft(t, entry, wantBlock)
		return
	}
	t.Fatalf("entries = %+v, want the drafted block %s", f2.Entries, wantBlock.BlockID)
}
