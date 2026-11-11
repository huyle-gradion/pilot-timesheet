package timesheet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func draftReview(meetings []ProposedMeeting, segments []Segment, topics []TopicCandidate, sources []ComposedSourceStatus) Review {
	r := Review{Date: "2026-10-26", Timezone: "Asia/Ho_Chi_Minh"}
	if meetings != nil || segments != nil || topics != nil || sources != nil {
		r.Evidence = &EvidenceReview{Sources: sources, Topics: topics, Meetings: meetings}
	}
	r.Confirmed = View{Segments: segments}
	return r
}

func TestBuildDraftMeetingBlockCarriesTopicsAndPRs(t *testing.T) {
	review := draftReview(
		[]ProposedMeeting{{Ref: "m1", Start: "2026-10-26T09:00:00+07:00", End: "2026-10-26T10:00:00+07:00", Title: "Standup"}},
		nil,
		[]TopicCandidate{{
			Title:   "Calendar filtering",
			FirstAt: "2026-10-26T09:20:00+07:00", LastAt: "2026-10-26T09:40:00+07:00",
			Items: []EvidenceItem{
				{Kind: "commit", Repo: "acme/api", Title: "fix window", Time: "2026-10-26T09:25:00+07:00"},
				{Kind: "pull_request", Repo: "acme/api", Resource: "pr:acme/api:12", Title: "Add filter", Time: "2026-10-26T09:30:00+07:00", URL: "https://example.com/pr/12"},
			},
		}},
		nil,
	)
	d := BuildDraft(review, "test")
	if len(d.Blocks) != 1 {
		t.Fatalf("blocks = %d, want 1: %+v", len(d.Blocks), d.Blocks)
	}
	b := d.Blocks[0]
	if b.Kind != "meeting" || b.Start != "09:00" || b.End != "10:00" || b.Title != "Standup" {
		t.Fatalf("meeting block shape wrong: %+v", b)
	}
	if len(b.TopicCandidates) != 1 || b.TopicCandidates[0] != "Calendar filtering" {
		t.Fatalf("topic candidates = %+v", b.TopicCandidates)
	}
	if len(b.PRs) != 1 || b.PRs[0] != "#12 acme/api" {
		t.Fatalf("prs = %+v", b.PRs)
	}
	if len(b.Sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(b.Sources))
	}
	if d.DayNote != "" {
		t.Fatalf("day note should be empty when meetings exist: %q", d.DayNote)
	}
}

func TestBuildDraftDeclinedAndAllDayMeetingsSkipped(t *testing.T) {
	review := draftReview([]ProposedMeeting{
		{Ref: "m1", Start: "2026-10-26T09:00:00+07:00", End: "2026-10-26T09:30:00+07:00", Title: "Skipped", Decision: "no"},
		{Ref: "m2", Start: "2026-10-26T00:00:00+07:00", End: "2026-10-26T00:00:00+07:00", Title: "Away", AllDay: true},
		{Ref: "m3", Start: "2026-10-26T11:00:00+07:00", End: "2026-10-26T11:30:00+07:00", Title: "Kept", Status: "cancelled"},
	}, nil, nil, nil)
	d := BuildDraft(review, "test")
	if len(d.Blocks) != 1 || d.Blocks[0].Kind != "day" {
		t.Fatalf("expected one fallback day block, got %+v", d.Blocks)
	}
	if d.DayNote != "no calendar events" {
		t.Fatalf("day note = %q", d.DayNote)
	}
}

func TestBuildDraftIntervalOutsideMeetingsBecomesWorkBlock(t *testing.T) {
	review := draftReview(
		[]ProposedMeeting{{Ref: "m1", Start: "2026-10-26T09:00:00+07:00", End: "2026-10-26T10:00:00+07:00", Title: "Standup"}},
		[]Segment{
			{IntervalID: "i1", Date: "2026-10-26", Start: "09:15", End: "09:45", Description: "joined standup"},
			{IntervalID: "i2", Date: "2026-10-26", Start: "13:00", End: "15:00", Description: "deep work"},
		},
		nil, nil,
	)
	d := BuildDraft(review, "test")
	if len(d.Blocks) != 2 {
		t.Fatalf("blocks = %d, want 2: %+v", len(d.Blocks), d.Blocks)
	}
	kinds := map[string]DraftBlock{}
	for _, b := range d.Blocks {
		kinds[b.Kind] = b
	}
	if w, ok := kinds["work"]; !ok || w.Start != "13:00" || w.End != "15:00" || w.Title != "deep work" {
		t.Fatalf("work block wrong: %+v", w)
	}
	if _, ok := kinds["meeting"]; !ok {
		t.Fatalf("meeting block missing: %+v", d.Blocks)
	}
	if containsString(d.Flags, "no_confirmed_intervals") {
		t.Fatalf("no_confirmed_intervals flag must be absent when intervals exist: %+v", d.Flags)
	}
}

// TestBuildDraftUnallocatedTopicsFormEvidenceBlock: an evening-only topic that
// no meeting and no confirmed interval owns becomes its own evidence block over
// the span ending at its proof of work.
func TestBuildDraftUnallocatedTopicsFormEvidenceBlock(t *testing.T) {
	review := draftReview(
		[]ProposedMeeting{{Ref: "m1", Start: "2026-10-26T09:00:00+07:00", End: "2026-10-26T09:30:00+07:00", Title: "Standup"}},
		nil,
		[]TopicCandidate{
			{Title: "Evening research", FirstAt: "2026-10-26T20:00:00+07:00", LastAt: "2026-10-26T21:00:00+07:00",
				Items: []EvidenceItem{{Kind: "commit", Repo: "acme/api", Title: "read notes", Time: "2026-10-26T20:10:00+07:00"}}},
		},
		nil,
	)
	d := BuildDraft(review, "test")
	var evidence *DraftBlock
	for i := range d.Blocks {
		if d.Blocks[i].Kind == "evidence" {
			evidence = &d.Blocks[i]
		}
	}
	if evidence == nil {
		t.Fatalf("evidence block missing: %+v", d.Blocks)
	}
	// The instant proves work happened at 20:10, not that it began then, so the
	// span looks back for its minimum burst and still ends at the proof.
	if evidence.Start != "19:55" || evidence.End != "20:10" {
		t.Fatalf("evidence span = %s-%s, want 19:55-20:10", evidence.Start, evidence.End)
	}
	if len(evidence.TopicCandidates) != 1 || evidence.TopicCandidates[0] != "Evening research" {
		t.Fatalf("evidence topics = %+v", evidence.TopicCandidates)
	}
}

func TestBuildDraftFlagsIncompleteSources(t *testing.T) {
	review := draftReview(nil, nil, nil, []ComposedSourceStatus{
		{Source: "github", Enabled: true, Status: StatusComplete},
		{Source: "calendar", Enabled: true, Status: "incomplete"},
	})
	d := BuildDraft(review, "test")
	if !containsString(d.Flags, "source_incomplete:calendar") {
		t.Fatalf("flags = %+v", d.Flags)
	}
	if !containsString(d.Flags, "no_confirmed_intervals") {
		t.Fatalf("flags = %+v", d.Flags)
	}
}

func reviewed(blocks ...ReviewedBlock) ReviewedDraft {
	return ReviewedDraft{Date: "2026-10-26", Blocks: blocks}
}

func TestRecordEntriesWritesMergesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	now := time.Now()

	first := reviewed(ReviewedBlock{
		DraftBlock:  DraftBlock{BlockID: "meeting-09:00-10:00", Kind: "meeting", Start: "09:00", End: "10:00"},
		Description: "Standup and reviews",
	})
	r1, err := RecordEntries(statePath, "test", first, false, now)
	if err != nil {
		t.Fatalf("first record: %v", err)
	}
	if r1.Written != 1 || r1.EntriesTotal != 1 {
		t.Fatalf("receipt = %+v", r1)
	}
	f1, err := ReadEntries(statePath, "2026-10-26")
	if err != nil || f1 == nil || len(f1.Entries) != 1 {
		t.Fatalf("read after first: %+v %v", f1, err)
	}

	// Same block with a new description updates in place; a new block adds.
	second := reviewed(
		ReviewedBlock{
			DraftBlock:  DraftBlock{BlockID: "meeting-09:00-10:00", Kind: "meeting", Start: "09:00", End: "10:00"},
			Description: "Standup, filter fix, and reviews",
		},
		ReviewedBlock{
			DraftBlock:  DraftBlock{BlockID: "work-13:00-15:00", Kind: "work", Start: "13:00", End: "15:00"},
			Description: "Deep work on the draft builder",
		},
	)
	r2, err := RecordEntries(statePath, "test", second, false, now)
	if err != nil {
		t.Fatalf("second record: %v", err)
	}
	if r2.EntriesTotal != 2 || r2.Written != 2 {
		t.Fatalf("receipt = %+v", r2)
	}
	f2, _ := ReadEntries(statePath, "2026-10-26")
	if len(f2.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(f2.Entries))
	}
	found := map[string]string{}
	for _, e := range f2.Entries {
		found[e.BlockID] = e.Description
	}
	if found["meeting-09:00-10:00"] != "Standup, filter fix, and reviews" {
		t.Fatalf("updated description missing: %+v", found)
	}
}

func TestRecordEntriesValidation(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	cases := []struct {
		name     string
		draft    ReviewedDraft
		wantCode string
		wantMsg  string
	}{
		{"bad date", ReviewedDraft{Date: "26-10-2026", Blocks: []ReviewedBlock{{DraftBlock: DraftBlock{BlockID: "b"}, Description: "x"}}}, CodeInvalidDate, ""},
		{"no blocks", ReviewedDraft{Date: "2026-10-26"}, CodeBadRequest, "at least one block"},
		{"empty description", reviewed(ReviewedBlock{DraftBlock: DraftBlock{BlockID: "b"}, Description: "   "}), CodeBadRequest, "empty description"},
		{"duplicate block", reviewed(
			ReviewedBlock{DraftBlock: DraftBlock{BlockID: "b"}, Description: "x"},
			ReviewedBlock{DraftBlock: DraftBlock{BlockID: "b"}, Description: "y"},
		), CodeBadRequest, "duplicate block_id"},
		{"missing prs list", reviewed(ReviewedBlock{
			DraftBlock:  DraftBlock{BlockID: "b", PRs: []string{"#12 acme/api"}},
			Description: "work without the list",
		}), CodeBadRequest, "PRs:"},
	}
	for _, tc := range cases {
		_, err := RecordEntries(statePath, "test", tc.draft, false, time.Now())
		if err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
		if tc.wantCode != "" && !strings.Contains(err.Error(), tc.wantCode) {
			t.Fatalf("%s: error %q lacks code %q", tc.name, err, tc.wantCode)
		}
		if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
			t.Fatalf("%s: error %q lacks %q", tc.name, err, tc.wantMsg)
		}
	}
}

func TestRecordEntriesRejectsMismatchedExistingFile(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	if err := os.MkdirAll(filepath.Dir(EntriesPath(statePath, "2026-10-26")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(EntriesPath(statePath, "2026-10-26"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := RecordEntries(statePath, "test", reviewed(ReviewedBlock{
		DraftBlock: DraftBlock{BlockID: "b"}, Description: "x",
	}), false, time.Now())
	if err == nil || !strings.Contains(err.Error(), CodeStateInvalid) {
		t.Fatalf("expected state invalid error, got %v", err)
	}
}

func TestDraftJSONRoundTripThroughReviewedSchema(t *testing.T) {
	review := draftReview(
		[]ProposedMeeting{{Ref: "m1", Start: "2026-10-26T09:00:00+07:00", End: "2026-10-26T10:00:00+07:00", Title: "Standup"}},
		nil,
		[]TopicCandidate{{Title: "T", FirstAt: "2026-10-26T09:10:00+07:00", LastAt: "2026-10-26T09:20:00+07:00",
			Items: []EvidenceItem{{Kind: "commit", Repo: "r", Title: "i", Time: "2026-10-26T09:11:00+07:00"}}}},
		nil,
	)
	d := BuildDraft(review, "test")
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var reviewed ReviewedDraft
	if err := json.Unmarshal(raw, &reviewed); err != nil {
		t.Fatalf("draft JSON does not fit the reviewed schema: %v", err)
	}
	if reviewed.Date != "2026-10-26" || len(reviewed.Blocks) != 1 {
		t.Fatalf("reviewed = %+v", reviewed)
	}
	reviewed.Blocks[0].Description = "Standup PRs: none"
	if _, err := RecordEntries(filepath.Join(t.TempDir(), "state.json"), "test", reviewed, false, time.Now()); err != nil {
		t.Fatalf("record from round-tripped draft: %v", err)
	}
}

// --- DRAFT-COMPACT-1: compacted AI-visible draft (issue #148) ----------------

// busyDayReview is a synthetic mechanical day: 1 meeting, 20 per-PR topics
// across two repositories (acme/api #10–#19, acme/web #20–#29), 3 evidence
// items per topic — 60 items in total, all outside the meeting.
func busyDayReview() Review {
	var topics []TopicCandidate
	for repoIdx, repo := range []string{"acme/api", "acme/web"} {
		base := 10 + repoIdx*10
		for k := 0; k < 10; k++ {
			num := base + k
			ts := fmt.Sprintf("2026-10-20T%02d:%02d:00+07:00", 10+k/2, (k%2)*30)
			topics = append(topics, TopicCandidate{
				Title:   fmt.Sprintf("Pull request #%d (%s)", num, repo),
				FirstAt: ts, LastAt: ts,
				Items: []EvidenceItem{
					{Kind: "commit", Repo: repo, Title: fmt.Sprintf("fix %d", num), Time: ts},
					{Kind: "commit", Repo: repo, Title: fmt.Sprintf("address review %d", num), Time: ts},
					{Kind: "pull_request", Repo: repo, Resource: fmt.Sprintf("pr:%s:%d", repo, num),
						Title: "Add filter", Time: ts, URL: "https://github.com/acme/" + repo + "/pull/" + fmt.Sprint(num)},
				},
			})
		}
	}
	r := draftReview(
		[]ProposedMeeting{{Ref: "m1", Start: "2026-10-20T09:00:00+07:00", End: "2026-10-20T09:30:00+07:00", Title: "Standup"}},
		nil, topics, nil,
	)
	r.Date = "2026-10-20"
	return r
}

func evidenceBlock(t *testing.T, d Draft) DraftBlock {
	t.Helper()
	for _, b := range d.Blocks {
		if b.Kind == "evidence" {
			return b
		}
	}
	t.Fatalf("no evidence block: %+v", d.Blocks)
	return DraftBlock{}
}

// Synthetic busy day: the draft stays under the 3,000-byte budget, per-PR
// topics collapse into one grouped line per repository, prs_inline carries
// every PR label exactly once, and no source URLs leak into the AI draft.
func TestBuildDraftBusyDayCompactBoundedAndURLFree(t *testing.T) {
	d := BuildDraft(busyDayReview(), "test")
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(raw); got > 3000 {
		t.Fatalf("busy-day draft = %d bytes, want <= 3000: %s", got, raw)
	}
	if d.Date != "2026-10-20" {
		t.Fatalf("draft date = %q, want the requested --date 2026-10-20", d.Date)
	}
	for _, want := range []string{"10 PRs (#10–#19) in acme/api", "10 PRs (#20–#29) in acme/web"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("draft lacks grouped line %q: %s", want, raw)
		}
	}
	eb := evidenceBlock(t, d)
	if len(eb.TopicCandidates) != 2 {
		t.Fatalf("topic candidates = %+v, want 2 grouped lines", eb.TopicCandidates)
	}
	// PRS-COMPACT-1: the inline list states each repository once and every PR
	// number exactly once, as contiguous numeric ranges per repository.
	if eb.PRsInline != "PRs: acme/api: #10–#19; acme/web: #20–#29" {
		t.Fatalf("prs_inline = %q, want the compact per-repo ranges", eb.PRsInline)
	}
	rangeRE := regexp.MustCompile(`#(\d+)(?:–#(\d+))?`)
	seen := map[int]int{}
	for _, m := range rangeRE.FindAllStringSubmatch(eb.PRsInline, -1) {
		lo, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatal(err)
		}
		hi := lo
		if m[2] != "" {
			if hi, err = strconv.Atoi(m[2]); err != nil {
				t.Fatal(err)
			}
		}
		for n := lo; n <= hi; n++ {
			seen[n]++
		}
	}
	if len(seen) != 20 {
		t.Fatalf("prs_inline carries %d distinct PR numbers, want 20: %q", len(seen), eb.PRsInline)
	}
	for n, count := range seen {
		if count != 1 {
			t.Fatalf("prs_inline carries #%d %d times: %q", n, count, eb.PRsInline)
		}
	}
	for _, forbidden := range []string{"http", `"sources"`, "sources_truncated", `"url"`} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("draft output contains %q: %s", forbidden, raw)
		}
	}
}

// prs_inline is deterministic: the same input twice yields byte-identical
// draft JSON, and the inline list orders PRs numerically ("#2" before "#10").
func TestBuildDraftPRsInlineDeterministicAndNumeric(t *testing.T) {
	a, err := json.Marshal(BuildDraft(busyDayReview(), "test"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(BuildDraft(busyDayReview(), "test"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("same input produced different draft bytes:\n%s\n%s", a, b)
	}
	review := draftReview(nil, nil, []TopicCandidate{
		{Title: "Pull request #10 (acme/api)", FirstAt: "2026-10-26T10:00:00+07:00", LastAt: "2026-10-26T10:10:00+07:00",
			Items: []EvidenceItem{{Kind: "pull_request", Repo: "acme/api", Resource: "pr:acme/api:10", Time: "2026-10-26T10:05:00+07:00"}}},
		{Title: "Pull request #2 (acme/api)", FirstAt: "2026-10-26T11:00:00+07:00", LastAt: "2026-10-26T11:10:00+07:00",
			Items: []EvidenceItem{{Kind: "pull_request", Repo: "acme/api", Resource: "pr:acme/api:2", Time: "2026-10-26T11:05:00+07:00"}}},
	}, nil)
	d := BuildDraft(review, "test")
	eb := evidenceBlock(t, d)
	if eb.PRsInline != "PRs: acme/api: #2, #10" {
		t.Fatalf("prs_inline = %q, want compact numeric order with the gap comma-listed", eb.PRsInline)
	}
}

// PRS-COMPACT-1 acceptance: the worst realistic case — 20 PRs in one ~60-rune
// repository, contiguous #88–#107 — renders in well under 200 runes, so a
// ~95-rune prose description composed with it stays under the 500-rune
// interval limit and ParseInterval accepts the line. The old full-label
// rendering ("#N repo" repeated per PR) provably blew the budget: it is also
// asserted to exceed the limit for the same prose.
func TestPRsInlineCompactFitsIntervalDescriptionLimit(t *testing.T) {
	longRepo := "huyle-gradion/" + strings.Repeat("p", 45) // 60 runes
	var prs []string
	for n := 88; n <= 107; n++ {
		prs = append(prs, fmt.Sprintf("#%d %s", n, longRepo))
	}
	inline := prsInline(prs)
	if got := len([]rune(inline)); got > 200 {
		t.Fatalf("compact prs_inline = %d runes, want <= 200: %q", got, inline)
	}
	if want := "PRs: " + longRepo + ": #88–#107"; inline != want {
		t.Fatalf("prs_inline = %q, want %q", inline, want)
	}
	prose := strings.Repeat("Word ", 19) // 95 runes
	oldInline := "PRs: " + strings.Join(prs, ", ")
	if old := len([]rune(prose)) + 1 + len([]rune(oldInline)); old <= maxDescriptionRunes {
		t.Fatalf("old rendering = %d runes, want > %d so the pre-change failure is proven", old, maxDescriptionRunes)
	}
	composed := prose + " " + inline
	if got := len([]rune(composed)); got > maxDescriptionRunes {
		t.Fatalf("composed description = %d runes, want <= %d", got, maxDescriptionRunes)
	}
	loc := time.FixedZone("ICT", 7*3600)
	if _, err := ParseInterval("2026-10-20 10:00-11:00 "+composed, "2026-10-20", loc); err != nil {
		t.Fatalf("ParseInterval rejected the composed description: %v", err)
	}
	// Determinism: the same input twice is byte-identical.
	if again := prsInline(prs); again != inline {
		t.Fatalf("prs_inline not deterministic:\n%s\n%s", inline, again)
	}
}

// PRS-COMPACT-1 acceptance: every PR number appears exactly once, contiguous
// runs collapse to "#a–#b" while numeric gaps stay comma-listed, each
// repository is stated once in its own alphabetically ordered segment, and
// labels that do not parse as "#N repo" are carried verbatim and
// deterministically.
func TestPRsInlineRangesGapsAndMultiRepoSegments(t *testing.T) {
	got := prsInline([]string{
		"#14 acme/api", "#10 acme/web", "#10 acme/api", "#11 acme/api", "#11 acme/web", "#88 acme/api",
	})
	if want := "PRs: acme/api: #10–#11, #14, #88; acme/web: #10–#11"; got != want {
		t.Fatalf("prs_inline = %q, want %q", got, want)
	}
	numRE := regexp.MustCompile(`#(\d+)`)
	counts := map[int]int{}
	for _, m := range numRE.FindAllStringSubmatch(got, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatal(err)
		}
		counts[n]++
	}
	wantNums := map[int]int{10: 2, 11: 2, 14: 1, 88: 1} // per repo: api{10,11,14,88}, web{10,11}
	if len(counts) != len(wantNums) {
		t.Fatalf("distinct numbers = %v, want %v", counts, wantNums)
	}
	for n, want := range wantNums {
		if counts[n] != want {
			t.Fatalf("#%d appears %d times, want %d: %q", n, counts[n], want, got)
		}
	}
	if strings.Count(got, "acme/api") != 1 || strings.Count(got, "acme/web") != 1 {
		t.Fatalf("each repository must be stated exactly once: %q", got)
	}
	// Unparseable labels survive verbatim, deterministically sorted.
	if got := prsInline([]string{"weird", "#3 x/y", "alpha"}); got != "PRs: x/y: #3; alpha; weird" {
		t.Fatalf("prs_inline = %q, want parsed segments then sorted verbatim labels", got)
	}
	if got := prsInline(nil); got != "" {
		t.Fatalf("prs_inline(nil) = %q, want empty", got)
	}
}

// Simple day (1 meeting, 1 PR): the draft matches a hand-computed fixture. The
// meeting keeps its calendar span, and the evidence starts at the workday and is
// carved at the meeting, so one 11:10 commit renders as 10:00-11:10.
func TestBuildDraftSimpleDayMatchesHandComputedFixture(t *testing.T) {
	review := draftReview(
		[]ProposedMeeting{{Ref: "m1", Start: "2026-10-26T09:00:00+07:00", End: "2026-10-26T10:00:00+07:00", Title: "Standup"}},
		nil,
		[]TopicCandidate{{
			Title:   "Pull request #12 (acme/api)",
			FirstAt: "2026-10-26T11:00:00+07:00", LastAt: "2026-10-26T11:30:00+07:00",
			Items: []EvidenceItem{
				{Kind: "pull_request", Repo: "acme/api", Resource: "pr:acme/api:12", Title: "Add filter", Time: "2026-10-26T11:10:00+07:00"},
			},
		}},
		[]ComposedSourceStatus{
			{Source: "github", Enabled: true, Status: StatusComplete},
			{Source: "calendar", Enabled: true, Status: StatusComplete},
		},
	)
	raw, err := json.Marshal(BuildDraft(review, "test"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"date":"2026-10-26","timezone":"Asia/Ho_Chi_Minh","app_version":"test",` +
		`"flags":["no_confirmed_intervals"],` +
		`"blocks":[` +
		`{"block_id":"meeting-09:00-10:00","kind":"meeting","start":"09:00","end":"10:00","title":"Standup",` +
		`"completeness":{"calendar":"complete","github":"complete"}},` +
		`{"block_id":"evidence-10:00-11:10","kind":"evidence","start":"10:00","end":"11:10",` +
		`"topic_candidates":["Pull request #12 (acme/api)"],` +
		`"prs":["#12 acme/api"],"prs_inline":"PRs: acme/api: #12",` +
		`"completeness":{"calendar":"complete","github":"complete"}}]}`
	if string(raw) != want {
		t.Fatalf("simple-day draft mismatch:\n got %s\nwant %s", raw, want)
	}
}

// The draft's date is always the requested --date, past dates included.
func TestBuildDraftDateEqualsRequestedDate(t *testing.T) {
	for _, date := range []string{"2026-10-20", "2026-10-26", "2025-01-02"} {
		r := draftReview(nil, nil, nil, nil)
		r.Date = date
		if d := BuildDraft(r, "test"); d.Date != date {
			t.Fatalf("draft date = %q, want requested %q", d.Date, date)
		}
	}
}

// The topic_candidates cap is honest ("+N more" counts the omitted lines),
// repeated identical titles collapse, PR ranges stay per-repository and split
// on numeric gaps, and genuinely distinct titles are never collapsed.
func TestBuildDraftTopicCandidatesCollapseAndCap(t *testing.T) {
	var topics []TopicCandidate
	add := func(title string, prResource string) {
		ts := fmt.Sprintf("2026-10-26T%02d:00:00+07:00", 10+len(topics)%8)
		item := EvidenceItem{Kind: "commit", Repo: "acme/api", Title: "note", Time: ts}
		if prResource != "" {
			item.Kind = "pull_request"
			item.Resource = prResource
		}
		topics = append(topics, TopicCandidate{Title: title, FirstAt: ts, LastAt: ts, Items: []EvidenceItem{item}})
	}
	// Three identical mechanical commit titles collapse into one line.
	add("Commits (acme/api)", "")
	add("Commits (acme/api)", "")
	add("Commits (acme/api)", "")
	// PR topics: a numeric gap splits ranges; two repositories never merge.
	add("Pull request #10 (acme/api)", "pr:acme/api:10")
	add("Pull request #11 (acme/api)", "pr:acme/api:11")
	add("Pull request #14 (acme/api)", "pr:acme/api:14")
	add("Pull request #10 (acme/web)", "pr:acme/web:10")
	add("Pull request #11 (acme/web)", "pr:acme/web:11")
	// Fifteen genuinely distinct titles stay verbatim.
	for i := 0; i < 15; i++ {
		add(fmt.Sprintf("Distinct topic %d", i), "")
	}
	review := draftReview(nil, nil, topics, nil)
	d := BuildDraft(review, "test")
	eb := evidenceBlock(t, d)

	got := eb.TopicCandidates
	if len(got) != maxDraftTopicCandidates+1 {
		t.Fatalf("topic candidates length = %d, want %d (+1 overflow line): %+v",
			len(got), maxDraftTopicCandidates, got)
	}
	if got[0] != "3× Commits (acme/api)" {
		t.Fatalf("repeated identical titles did not collapse: %q", got[0])
	}
	if got[1] != "3 PRs (#10–#11, #14) in acme/api" {
		t.Fatalf("gapped PR ranges wrong: %q", got[1])
	}
	if got[2] != "2 PRs (#10–#11) in acme/web" {
		t.Fatalf("second repository collapsed into the first: %q", got[2])
	}
	for i := 0; i < 9; i++ {
		if got[3+i] != fmt.Sprintf("Distinct topic %d", i) {
			t.Fatalf("distinct title not verbatim at %d: %+v", 3+i, got)
		}
	}
	if got[len(got)-1] != "+6 more" {
		t.Fatalf("overflow line = %q, want \"+6 more\"", got[len(got)-1])
	}
	for _, line := range got {
		if strings.Contains(line, "Distinct topic 14") {
			t.Fatalf("overflow line not honest: %+v", got)
		}
	}
	if eb.PRsInline != "PRs: acme/api: #10–#11, #14; acme/web: #10–#11" {
		t.Fatalf("prs_inline = %q, want compact per-repo segments with gap-split ranges", eb.PRsInline)
	}
}

// A pathological day of hundreds of distinct titles keeps the draft bounded:
// the rendered list is capped at the fixed constant plus one honest line.
func TestBuildDraftPathologicalDistinctTitlesStayBounded(t *testing.T) {
	var topics []TopicCandidate
	for i := 0; i < 150; i++ {
		ts := fmt.Sprintf("2026-10-26T%02d:%02d:00+07:00", 8+i/60, i%60)
		topics = append(topics, TopicCandidate{
			Title: fmt.Sprintf("One-off exploration %03d", i), FirstAt: ts, LastAt: ts,
			Items: []EvidenceItem{{Kind: "commit", Repo: "acme/api", Title: fmt.Sprintf("note %d", i), Time: ts}},
		})
	}
	d := BuildDraft(draftReview(nil, nil, topics, nil), "test")
	eb := evidenceBlock(t, d)
	if len(eb.TopicCandidates) != maxDraftTopicCandidates+1 {
		t.Fatalf("topic candidates length = %d, want %d", len(eb.TopicCandidates), maxDraftTopicCandidates+1)
	}
	if eb.TopicCandidates[len(eb.TopicCandidates)-1] != "+138 more" {
		t.Fatalf("overflow line = %q, want \"+138 more\"", eb.TopicCandidates[len(eb.TopicCandidates)-1])
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 3000 {
		t.Fatalf("pathological-day draft = %d bytes, want <= 3000", len(raw))
	}
}

// record accepts a reviewed draft whose description ends with the exact
// prs_inline string; a dropped PRs list is still rejected (covered further by
// TestRecordEntriesValidation).
func TestRecordEntriesAcceptsVerbatimPRsInline(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	reviewed := reviewed(ReviewedBlock{
		DraftBlock: DraftBlock{
			BlockID: "evidence-10:00-14:30",
			Kind:    "evidence",
			Start:   "10:00", End: "14:30",
			PRs:       []string{"#10 acme/api", "#2 acme/web"},
			PRsInline: "PRs: #2 acme/web, #10 acme/api",
		},
		Description: "Filter and export work PRs: #2 acme/web, #10 acme/api",
	})
	if _, err := RecordEntries(statePath, "test", reviewed, false, time.Now()); err != nil {
		t.Fatalf("record with verbatim prs_inline rejected: %v", err)
	}
	f, err := ReadEntries(statePath, "2026-10-26")
	if err != nil || f == nil || len(f.Entries) != 1 {
		t.Fatalf("read entries: %+v %v", f, err)
	}
	if f.Entries[0].Description != "Filter and export work PRs: #2 acme/web, #10 acme/api" {
		t.Fatalf("recorded description = %q", f.Entries[0].Description)
	}
}

// --- DRAFT-COMPACT-1 amendment (orchestrator-authorized): record re-attaches
// audit-only source references from the day's durable evidence. ---------------

// writeStateDay writes the durable slices record recomputes the draft from: one
// day's collected evidence, its confirmed intervals projected to the
// {start, end, description} segments BuildDraft partitions, and its timezone.
// A caller that omits the timezone or the segments writes them absent, which is
// the pre-existing evidence-only shape.
func writeStateDay(t *testing.T, statePath, date, timezone string, ev DayEvidence, segments []Segment) {
	t.Helper()
	day := map[string]any{}
	if timezone != "" {
		day["timezone"] = timezone
	}
	if len(segments) > 0 {
		confirmed := make([]map[string]string, 0, len(segments))
		for _, s := range segments {
			confirmed = append(confirmed, map[string]string{
				"id": s.IntervalID, "date": date, "start": s.Start, "end": s.End,
				"description": s.Description,
			})
		}
		day["confirmed"] = confirmed
	}
	body := map[string]any{
		"schema":   2,
		"days":     map[string]any{date: day},
		"evidence": map[string]any{date: ev},
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeStateEvidence writes a minimal state file carrying one day's topics in
// the existing store schema shape. The day carries the timezone its anchors
// use, because every stored day has one and record renders every stored day.
func writeStateEvidence(t *testing.T, statePath, date string, topics []TopicCandidate) {
	t.Helper()
	writeStateDay(t, statePath, date, "Asia/Ho_Chi_Minh", DayEvidence{Topics: topics}, nil)
}

// record on a day whose durable evidence has items for a block: the recorded
// EntryRecord carries the same sources (kind/repo/title/time/URL) the
// pre-change draft would have echoed, with sources_truncated set past the
// 8-item cap and the topic→block span mapping respected.
func TestRecordEntriesReattachesSourcesFromDurableState(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	// Two items inside the 09:00–10:00 meeting span (URLs included) and ten
	// items outside it, so the evidence block folds 8 sources + truncation.
	topics := []TopicCandidate{
		{Title: "Pull request #12 (acme/api)", FirstAt: "2026-10-26T09:20:00+07:00", LastAt: "2026-10-26T09:40:00+07:00",
			Items: []EvidenceItem{
				{Kind: "commit", Repo: "acme/api", Title: "fix window", Time: "2026-10-26T09:25:00+07:00", URL: "https://github.com/acme/api/commit/a"},
				{Kind: "pull_request", Repo: "acme/api", Resource: "pr:acme/api:12", Title: "Add filter", Time: "2026-10-26T09:30:00+07:00", URL: "https://github.com/acme/api/pull/12"},
			}},
		{Title: "Evening evidence", FirstAt: "2026-10-26T20:00:00+07:00", LastAt: "2026-10-26T21:00:00+07:00",
			Items: func() []EvidenceItem {
				var items []EvidenceItem
				for i := 0; i < 10; i++ {
					items = append(items, EvidenceItem{Kind: "commit", Repo: "acme/web",
						Title: fmt.Sprintf("note %d", i), Time: fmt.Sprintf("2026-10-26T20:%02d:00+07:00", i),
						URL: fmt.Sprintf("https://github.com/acme/web/commit/%d", i)})
				}
				return items
			}()},
	}
	meetings := []ProposedMeeting{{Ref: "m1", Title: "Standup",
		Start: "2026-10-26T09:00:00+07:00", End: "2026-10-26T10:00:00+07:00"}}
	writeStateDay(t, statePath, "2026-10-26", "Asia/Ho_Chi_Minh", DayEvidence{Topics: topics, Meetings: meetings}, nil)
	reviewed := reviewed(
		ReviewedBlock{
			DraftBlock:  DraftBlock{BlockID: "meeting-09:00-10:00", Kind: "meeting", Start: "09:00", End: "10:00", PRs: []string{"#12 acme/api"}},
			Description: "Standup and filter work PRs: #12 acme/api",
		},
		ReviewedBlock{
			DraftBlock:  DraftBlock{BlockID: "evidence-20:00-20:09", Kind: "evidence", Start: "20:00", End: "20:09"},
			Description: "Evening notes",
		},
	)
	if _, err := RecordEntries(statePath, "test", reviewed, false, time.Now()); err != nil {
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
	m := byID["meeting-09:00-10:00"]
	if len(m.Sources) != 2 {
		t.Fatalf("meeting sources = %d, want 2: %+v", len(m.Sources), m.Sources)
	}
	if m.Sources[0].Kind != "commit" || m.Sources[0].Repo != "acme/api" ||
		m.Sources[0].Title != "fix window" || m.Sources[0].Time != "2026-10-26T09:25:00+07:00" ||
		m.Sources[0].URL != "https://github.com/acme/api/commit/a" {
		t.Fatalf("meeting source[0] = %+v", m.Sources[0])
	}
	if m.Sources[1].URL != "https://github.com/acme/api/pull/12" {
		t.Fatalf("meeting source[1] = %+v", m.Sources[1])
	}
	if m.SourcesTruncated {
		t.Fatalf("meeting sources must not be truncated")
	}
	e := byID["evidence-20:00-20:09"]
	if len(e.Sources) != draftSourcesPerBlock {
		t.Fatalf("evidence sources = %d, want %d", len(e.Sources), draftSourcesPerBlock)
	}
	if !e.SourcesTruncated {
		t.Fatalf("evidence sources_truncated must be set past the cap")
	}
	if e.Sources[0].URL != "https://github.com/acme/web/commit/0" {
		t.Fatalf("evidence source[0] = %+v", e.Sources[0])
	}
}

// The new normal (reviewed file without sources) records identically to the
// old echo style, because the durable evidence is authoritative, and
// pre-existing entries in the file are untouched by a later record.
func TestRecordEntriesSourcesIdenticalWithAndWithoutEcho(t *testing.T) {
	topics := []TopicCandidate{
		{Title: "Evening evidence", FirstAt: "2026-10-26T20:00:00+07:00", LastAt: "2026-10-26T21:00:00+07:00",
			Items: []EvidenceItem{
				{Kind: "commit", Repo: "acme/api", Title: "read notes", Time: "2026-10-26T20:10:00+07:00", URL: "https://github.com/acme/api/commit/1"},
			}},
	}
	newNormal := reviewed(ReviewedBlock{
		DraftBlock:  DraftBlock{BlockID: "evidence-19:55-20:10", Kind: "evidence", Start: "19:55", End: "20:10"},
		Description: "Evening notes",
	})
	echoed := reviewed(ReviewedBlock{
		DraftBlock: DraftBlock{
			BlockID: "evidence-19:55-20:10", Kind: "evidence", Start: "19:55", End: "20:10",
			Sources: []DraftSourceItem{{Kind: "stale", Repo: "wrong", Title: "echoed", Time: "00:00", URL: "https://stale.example/x"}},
		},
		Description: "Evening notes",
	})
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	writeStateEvidence(t, statePath, "2026-10-26", topics)
	if _, err := RecordEntries(statePath, "test", newNormal, false, time.Now()); err != nil {
		t.Fatalf("record without echoed sources: %v", err)
	}
	// A later record of the same block in the old echo style must produce the
	// same recorded sources (state wins over the stale echo).
	if _, err := RecordEntries(statePath, "test", echoed, false, time.Now()); err != nil {
		t.Fatalf("record with echoed sources: %v", err)
	}
	f, err := ReadEntries(statePath, "2026-10-26")
	if err != nil || f == nil || len(f.Entries) != 1 {
		t.Fatalf("read entries: %+v %v", f, err)
	}
	e := f.Entries[0]
	if len(e.Sources) != 1 || e.Sources[0].Kind != "commit" || e.Sources[0].Repo != "acme/api" ||
		e.Sources[0].Title != "read notes" || e.Sources[0].URL != "https://github.com/acme/api/commit/1" {
		t.Fatalf("recorded sources = %+v", e.Sources)
	}

	// Pre-existing entries are untouched by a record that writes other blocks.
	other := reviewed(ReviewedBlock{
		DraftBlock:  DraftBlock{BlockID: "work-13:00-15:00", Kind: "work", Start: "13:00", End: "15:00"},
		Description: "Deep work",
	})
	if _, err := RecordEntries(statePath, "test", other, false, time.Now()); err != nil {
		t.Fatalf("record of other block: %v", err)
	}
	f2, err := ReadEntries(statePath, "2026-10-26")
	if err != nil || f2 == nil || len(f2.Entries) != 2 {
		t.Fatalf("read entries after second record: %+v %v", f2, err)
	}
	for _, entry := range f2.Entries {
		if entry.BlockID != "evidence-19:55-20:10" {
			continue
		}
		if len(entry.Sources) != 1 || entry.Sources[0].Title != "read notes" {
			t.Fatalf("pre-existing entry mutated: %+v", entry)
		}
	}
}

// Determinism: two record runs with the same input and timestamp produce a
// byte-identical entries file.
func TestRecordEntriesSourcesFileDeterministic(t *testing.T) {
	record := func(statePath string, now time.Time) {
		t.Helper()
		reviewed := reviewed(ReviewedBlock{
			DraftBlock:  DraftBlock{BlockID: "evidence-19:55-20:10", Kind: "evidence", Start: "19:55", End: "20:10"},
			Description: "Evening notes",
		})
		if _, err := RecordEntries(statePath, "test", reviewed, false, now); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	writeStateEvidence(t, statePath, "2026-10-26", []TopicCandidate{
		{Title: "Evening evidence", FirstAt: "2026-10-26T20:00:00+07:00", LastAt: "2026-10-26T21:00:00+07:00",
			Items: []EvidenceItem{{Kind: "commit", Repo: "acme/api", Title: "read notes", Time: "2026-10-26T20:10:00+07:00", URL: "https://github.com/acme/api/commit/1"}}},
	})
	record(statePath, now)
	first, err := os.ReadFile(EntriesPath(statePath, "2026-10-26"))
	if err != nil {
		t.Fatal(err)
	}
	record(statePath, now)
	second, err := os.ReadFile(EntriesPath(statePath, "2026-10-26"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("two identical record runs produced different entries files:\n%s\n%s", first, second)
	}
}

// F1 pinning (review finding): record-time source re-attachment must use the
// exact same topic→block mapping as BuildDraft. Demonstrated divergence case:
// a work block that starts BEFORE a later meeting, with the evidence item inside
// the work block while the topic's own anchors span both blocks.
func TestRecordSourcesLandOnSameBlockBuildDraftAttached(t *testing.T) {
	topic := TopicCandidate{
		Title:   "Spanning topic",
		FirstAt: "2026-10-26T08:30:00+07:00", LastAt: "2026-10-26T10:30:00+07:00",
		Items: []EvidenceItem{
			{Kind: "commit", Repo: "acme/api", Title: "spanning note", Time: "2026-10-26T09:00:00+07:00", URL: "https://github.com/acme/api/commit/span"},
		},
	}
	review := draftReview(
		[]ProposedMeeting{{Ref: "m1", Start: "2026-10-26T10:00:00+07:00", End: "2026-10-26T11:00:00+07:00", Title: "Late sync"}},
		[]Segment{{IntervalID: "i1", Date: "2026-10-26", Start: "08:00", End: "09:00", Description: "early work"}},
		[]TopicCandidate{topic},
		nil,
	)
	d := BuildDraft(review, "test")

	// Draft side: the 08:00 work block holds the item's minute, so the topic and
	// its source attach to the work block, never to the later meeting.
	var work, meeting *DraftBlock
	for i := range d.Blocks {
		switch d.Blocks[i].BlockID {
		case "work-08:00-09:00":
			work = &d.Blocks[i]
		case "meeting-10:00-11:00":
			meeting = &d.Blocks[i]
		}
	}
	if work == nil || meeting == nil {
		t.Fatalf("expected work and meeting blocks: %+v", d.Blocks)
	}
	if len(work.TopicCandidates) != 1 || len(work.Sources) != 1 {
		t.Fatalf("BuildDraft did not attach the topic to the work block: work=%+v meeting=%+v", work, meeting)
	}
	if len(meeting.TopicCandidates) != 0 || len(meeting.Sources) != 0 {
		t.Fatalf("BuildDraft attached the topic to the meeting block: %+v", meeting)
	}

	// Record side: the same blocks reviewed and recorded against the same
	// durable evidence must land the source on the same block BuildDraft
	// attached it to (matched by block_id).
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	writeStateDay(t, statePath, "2026-10-26", "Asia/Ho_Chi_Minh",
		DayEvidence{Topics: []TopicCandidate{topic}, Meetings: review.Evidence.Meetings},
		review.Confirmed.Segments)
	var reviewed ReviewedDraft
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &reviewed); err != nil {
		t.Fatal(err)
	}
	for i := range reviewed.Blocks {
		reviewed.Blocks[i].Description = "recorded block"
	}
	if _, err := RecordEntries(statePath, "test", reviewed, false, time.Now()); err != nil {
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
	if len(byID["work-08:00-09:00"].Sources) != 1 ||
		byID["work-08:00-09:00"].Sources[0].URL != "https://github.com/acme/api/commit/span" {
		t.Fatalf("record did not attach the source to the work block BuildDraft used: %+v", byID)
	}
	if len(byID["meeting-10:00-11:00"].Sources) != 0 {
		t.Fatalf("record attached the source to the meeting block: %+v", byID["meeting-10:00-11:00"])
	}
}

// --- DESC-ONLY-1: descriptions-only judgment step (issue #152) ---------------

// composeDraft is a synthetic draft with one meeting block carrying PRs (so
// prs_inline applies) and one evidence block without them.
func composeDraft() Draft {
	return Draft{
		Date:     "2026-10-26",
		Timezone: "Asia/Ho_Chi_Minh",
		Flags:    []string{"source_incomplete:calendar"},
		Blocks: []DraftBlock{
			{
				BlockID: "meeting-09:00-10:00", Kind: "meeting", Start: "09:00", End: "10:00",
				Title: "Standup", TopicCandidates: []string{"Calendar filtering"},
				PRs: []string{"#12 acme/api"}, PRsInline: "PRs: #12 acme/api",
				Completeness: map[string]string{"github": "complete"},
			},
			{
				BlockID: "evidence-20:00-21:00", Kind: "evidence", Start: "20:00", End: "21:00",
				TopicCandidates: []string{"Evening research"},
			},
		},
	}
}

func TestComposeReviewedAppliesDescriptionsAndAppendsPRsInline(t *testing.T) {
	composed, err := ComposeReviewed(composeDraft(), DescriptionsFile{
		Notes: "quiet day, calendar source incomplete",
		Descriptions: map[string]string{
			"meeting-09:00-10:00":  "Standup and calendar filtering",
			"evidence-20:00-21:00": "Evening research notes",
		},
	})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if composed.Date != "2026-10-26" || composed.Timezone != "Asia/Ho_Chi_Minh" {
		t.Fatalf("date/timezone not carried: %+v", composed)
	}
	if composed.Notes != "quiet day, calendar source incomplete" {
		t.Fatalf("notes = %q", composed.Notes)
	}
	if len(composed.Flags) != 1 || composed.Flags[0] != "source_incomplete:calendar" {
		t.Fatalf("flags not carried: %+v", composed.Flags)
	}
	if len(composed.Blocks) != 2 {
		t.Fatalf("blocks = %d, want 2", len(composed.Blocks))
	}
	m := composed.Blocks[0]
	if m.BlockID != "meeting-09:00-10:00" || m.Title != "Standup" ||
		m.Start != "09:00" || m.End != "10:00" || len(m.PRs) != 1 {
		t.Fatalf("draft fields not carried: %+v", m)
	}
	if m.Description != "Standup and calendar filtering PRs: #12 acme/api" {
		t.Fatalf("meeting description = %q, want prs_inline appended once", m.Description)
	}
	e := composed.Blocks[1]
	if e.Description != "Evening research notes" {
		t.Fatalf("evidence description = %q, want unchanged prose", e.Description)
	}
	// The composed reviewed draft passes the exact existing record validation.
	if _, err := RecordEntries(filepath.Join(t.TempDir(), "state.json"), "test", composed, false, time.Now()); err != nil {
		t.Fatalf("composed reviewed draft rejected by record: %v", err)
	}
}

// A description that already ends with the block's prs_inline text is left
// untouched: the append is idempotent and never duplicates the PRs list. A
// description ending in a prefix-sharing PR list ("#123" contains "#12" as a
// substring) is NOT idempotent: the block's own "#12" list must be appended.
func TestComposeReviewedPRsInlineAppendIsIdempotent(t *testing.T) {
	composed, err := ComposeReviewed(composeDraft(), DescriptionsFile{
		Descriptions: map[string]string{
			"meeting-09:00-10:00":  "Standup and calendar filtering PRs: #12 acme/api",
			"evidence-20:00-21:00": "Evening research notes",
		},
	})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if got := composed.Blocks[0].Description; got != "Standup and calendar filtering PRs: #12 acme/api" {
		t.Fatalf("description = %q, prs_inline must not be duplicated", got)
	}

	// Prefix collision: "PRs: #123 acme/api" is not the block's "PRs: #12
	// acme/api" list — the correct "#12" list must be appended.
	collided, err := ComposeReviewed(composeDraft(), DescriptionsFile{
		Descriptions: map[string]string{
			"meeting-09:00-10:00":  "Reviews for PRs: #123 acme/api",
			"evidence-20:00-21:00": "Evening research notes",
		},
	})
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if got := collided.Blocks[0].Description; got != "Reviews for PRs: #123 acme/api PRs: #12 acme/api" {
		t.Fatalf("description = %q, want the block's own #12 list appended after the #123 text", got)
	}
}

func TestComposeReviewedValidation(t *testing.T) {
	cases := []struct {
		name    string
		desc    DescriptionsFile
		wantMsg []string
	}{
		{
			name: "missing description",
			desc: DescriptionsFile{Descriptions: map[string]string{
				"evidence-20:00-21:00": "Evening research notes",
			}},
			wantMsg: []string{"meeting-09:00-10:00", "missing or empty"},
		},
		{
			name: "empty description",
			desc: DescriptionsFile{Descriptions: map[string]string{
				"meeting-09:00-10:00":  "   ",
				"evidence-20:00-21:00": "Evening research notes",
			}},
			wantMsg: []string{"meeting-09:00-10:00", "missing or empty"},
		},
		{
			name: "unknown block id",
			desc: DescriptionsFile{Descriptions: map[string]string{
				"meeting-09:00-10:00":  "Standup",
				"evidence-20:00-21:00": "Evening research notes",
				"work-13:00-15:00":     "not in the draft",
			}},
			wantMsg: []string{"work-13:00-15:00", "does not contain"},
		},
	}
	for _, tc := range cases {
		_, err := ComposeReviewed(composeDraft(), tc.desc)
		if err == nil {
			t.Fatalf("%s: expected error", tc.name)
		}
		for _, want := range tc.wantMsg {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s: error %q lacks %q", tc.name, err, want)
			}
		}
	}
}

func TestParseDescriptionsNotesMustBeString(t *testing.T) {
	ok, err := ParseDescriptions([]byte(`{"notes": "one string", "descriptions": {"b": "x"}}`))
	if err != nil || ok.Notes != "one string" || ok.Descriptions["b"] != "x" {
		t.Fatalf("string notes rejected: %+v %v", ok, err)
	}
	absent, err := ParseDescriptions([]byte(`{"descriptions": {"b": "x"}}`))
	if err != nil || absent.Notes != "" {
		t.Fatalf("absent notes must be an empty string: %+v %v", absent, err)
	}
	for _, raw := range []string{
		`{"notes": ["a", "b"], "descriptions": {}}`,
		`{"notes": 123, "descriptions": {}}`,
		`{"notes": {}, "descriptions": {}}`,
	} {
		if _, err := ParseDescriptions([]byte(raw)); err == nil ||
			!strings.Contains(err.Error(), `"notes" must be one JSON string`) {
			t.Fatalf("notes %s = %v, want the string-only validation error", raw, err)
		}
	}
}

// The equivalence proof: recording via --draft + --descriptions composition
// and recording via --file with the equivalently hand-composed reviewed JSON
// write byte-identical entries files (same date, same clock).
func TestRecordComposedMatchesFileComposedByteForByte(t *testing.T) {
	draft := composeDraft()
	desc := DescriptionsFile{
		Notes: "day note",
		Descriptions: map[string]string{
			"meeting-09:00-10:00":  "Standup and calendar filtering",
			"evidence-20:00-21:00": "Evening research notes",
		},
	}
	composed, err := ComposeReviewed(draft, desc)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}

	// The equivalently hand-composed reviewed JSON the pre-change judgment
	// step would have written: every draft field copied, description prose
	// with the prs_inline suffix appended by hand, one notes string.
	handJSON := `{
  "date": "2026-10-26",
  "timezone": "Asia/Ho_Chi_Minh",
  "flags": ["source_incomplete:calendar"],
  "notes": "day note",
  "blocks": [
    {
      "block_id": "meeting-09:00-10:00",
      "kind": "meeting",
      "start": "09:00",
      "end": "10:00",
      "title": "Standup",
      "topic_candidates": ["Calendar filtering"],
      "prs": ["#12 acme/api"],
      "prs_inline": "PRs: #12 acme/api",
      "completeness": {"github": "complete"},
      "description": "Standup and calendar filtering PRs: #12 acme/api"
    },
    {
      "block_id": "evidence-20:00-21:00",
      "kind": "evidence",
      "start": "20:00",
      "end": "21:00",
      "topic_candidates": ["Evening research"],
      "description": "Evening research notes"
    }
  ]
}`
	var viaFile ReviewedDraft
	if err := json.Unmarshal([]byte(handJSON), &viaFile); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	composedPath := filepath.Join(dir, "composed", "state.json")
	filePath := filepath.Join(dir, "file", "state.json")
	if _, err := RecordEntries(composedPath, "test", composed, false, now); err != nil {
		t.Fatalf("record composed: %v", err)
	}
	if _, err := RecordEntries(filePath, "test", viaFile, false, now); err != nil {
		t.Fatalf("record via file: %v", err)
	}
	composedBytes, err := os.ReadFile(EntriesPath(composedPath, "2026-10-26"))
	if err != nil {
		t.Fatal(err)
	}
	fileBytes, err := os.ReadFile(EntriesPath(filePath, "2026-10-26"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(composedBytes, fileBytes) {
		t.Fatalf("entries files differ:\ncomposed:\n%s\nvia --file:\n%s", composedBytes, fileBytes)
	}
}

// --- FLOW-GAP-1: record --replace (issue #162) --------------------------------

// TestRecordEntriesReplaceSupersedesGeneration pins --replace semantics: the
// reviewed draft is the day's complete content — blocks absent from it are
// removed, present block_ids are replaced in place — while merge mode
// (replace=false) keeps preserving unmentioned blocks, and a merge run after a
// replace is still idempotent per block.
func TestRecordEntriesReplaceSupersedesGeneration(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	now := time.Now()

	seed := reviewed(
		ReviewedBlock{
			DraftBlock:  DraftBlock{BlockID: "meeting-06:00-07:30", Kind: "meeting", Start: "06:00", End: "07:30"},
			Description: "Old morning standup",
		},
		ReviewedBlock{
			DraftBlock:  DraftBlock{BlockID: "meeting-15:30-15:45", Kind: "meeting", Start: "15:30", End: "15:45"},
			Description: "Old sync",
		},
	)
	if _, err := RecordEntries(statePath, "test", seed, false, now); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Replace with B',C: B replaced per block_id, A removed.
	next := reviewed(
		ReviewedBlock{
			DraftBlock:  DraftBlock{BlockID: "meeting-15:30-15:45", Kind: "meeting", Start: "15:30", End: "15:45"},
			Description: "New sync",
		},
		ReviewedBlock{
			DraftBlock:  DraftBlock{BlockID: "work-16:00-17:00", Kind: "work", Start: "16:00", End: "17:00"},
			Description: "New deep work",
		},
	)
	rep, err := RecordEntries(statePath, "test", next, true, now)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if rep.EntriesTotal != 2 || rep.Written != 2 || rep.Removed != 1 {
		t.Fatalf("replace receipt = %+v", rep)
	}
	f, err := ReadEntries(statePath, "2026-10-26")
	if err != nil || f == nil || len(f.Entries) != 2 {
		t.Fatalf("read after replace: %+v %v", f, err)
	}
	byID := map[string]string{}
	for _, e := range f.Entries {
		byID[e.BlockID] = e.Description
	}
	if _, gone := byID["meeting-06:00-07:30"]; gone {
		t.Fatalf("old generation survived replace: %+v", byID)
	}
	if byID["meeting-15:30-15:45"] != "New sync" || byID["work-16:00-17:00"] != "New deep work" {
		t.Fatalf("replace content wrong: %+v", byID)
	}

	// Re-running the same replace changes nothing (idempotent within replace).
	again, err := RecordEntries(statePath, "test", next, true, now)
	if err != nil || again.EntriesTotal != 2 || again.Written != 2 || again.Removed != 0 {
		t.Fatalf("second replace = %+v err %v", again, err)
	}

	// Merge mode after a replace is unchanged: unmentioned blocks survive and
	// a second identical merge run stays idempotent.
	merge := reviewed(ReviewedBlock{
		DraftBlock:  DraftBlock{BlockID: "evidence-20:00-21:00", Kind: "evidence", Start: "20:00", End: "21:00"},
		Description: "Evening notes",
	})
	for i := 0; i < 2; i++ {
		if _, err := RecordEntries(statePath, "test", merge, false, now); err != nil {
			t.Fatalf("merge run %d: %v", i+1, err)
		}
	}
	f3, _ := ReadEntries(statePath, "2026-10-26")
	if f3 == nil || len(f3.Entries) != 3 {
		t.Fatalf("merge total = %+v, want 3 entries", f3)
	}
}

// TestRecordEntriesUsesStoredDayTimezoneWhenFileOmitsIt: a reviewed file may be
// recovered without its timezone key. The day's stored timezone is the
// authority, so topic anchors must still resolve on the local clock and attach
// to the same block the draft attached them to (review finding on #174).
func TestRecordEntriesUsesStoredDayTimezoneWhenFileOmitsIt(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	writeStateDay(t, statePath, "2026-10-20", "Asia/Ho_Chi_Minh", DayEvidence{
		Topics: []TopicCandidate{{
			Title:   "Pull request #7 (acme/api)",
			FirstAt: "2026-10-20T07:20:00Z", LastAt: "2026-10-20T07:40:00Z",
			Items: []EvidenceItem{{
				Kind: "authored_pull_request", Repo: "acme/api", Title: "PR 7", Time: "2026-10-20T07:20:00Z",
			}},
		}},
		Meetings: []ProposedMeeting{{
			Ref: "m1", Title: "Standup",
			Start: "2026-10-20T07:00:00Z", End: "2026-10-20T08:00:00Z",
		}},
	}, nil)

	// No Timezone on the reviewed file: 07:20Z is 14:20 local, inside the block.
	reviewed := ReviewedDraft{Date: "2026-10-20", Blocks: []ReviewedBlock{{
		DraftBlock:  DraftBlock{BlockID: "meeting-14:00-15:00", Kind: "meeting", Start: "14:00", End: "15:00"},
		Description: "Standup",
	}}}
	if _, err := RecordEntries(statePath, "test", reviewed, false, time.Now()); err != nil {
		t.Fatalf("record: %v", err)
	}
	f, err := ReadEntries(statePath, "2026-10-20")
	if err != nil || f == nil || len(f.Entries) != 1 {
		t.Fatalf("entries = %+v %v", f, err)
	}
	if got := len(f.Entries[0].Sources); got != 1 {
		t.Fatalf("sources = %d (%+v), want the 07:20Z topic resolved to 14:20 local and attached", got, f.Entries[0].Sources)
	}
}
