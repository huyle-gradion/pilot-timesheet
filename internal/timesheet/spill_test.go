package timesheet

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// spillState saves one cross-midnight interval (23:00-01:00) on the day before
// requested through the real store operations, so the requested day's review
// renders the previous day's 00:00-01:00 allocation.
func spillState(t *testing.T, requested string) (*Session, string) {
	t.Helper()
	cfg := &Config{
		Timezone:  "Asia/Ho_Chi_Minh",
		StatePath: filepath.Join(t.TempDir(), "state.json"),
	}
	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := NewSession(cfg, nil, store)

	previous := "2026-10-25"
	opened, err := s.openDay(Request{Day: previous})
	if err != nil {
		t.Fatal(err)
	}
	added, err := s.addInterval(Request{Day: previous, Revision: &opened.Revision,
		Text: previous + " 23:00-01:00 Overnight release watch"})
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := s.confirm(Request{Day: previous, Revision: &added.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.save(Request{Day: previous, Revision: &confirmed.Revision}); err != nil {
		t.Fatal(err)
	}
	return s, cfg.StatePath
}

// TestRecordSourcesIncludePreviousDaySpill is the record-parity case for a
// cross-midnight interval: the draft seeds work-00:00-01:00 from the previous
// day's spill, so record must recompute over the same rendered segments and
// attach that block's sources. Reading the raw days[date].confirmed projection
// cannot see the spill and records an empty source list.
func TestRecordSourcesIncludePreviousDaySpill(t *testing.T) {
	date := "2026-10-26"
	s, statePath := spillState(t, date)
	store := s.store
	if err := store.CommitEvidence(date, &DayEvidence{Topics: []TopicCandidate{{
		Ref: "tc:spill", Provider: "github", Title: "Spill evidence",
		FirstAt: date + "T00:30:00+07:00", LastAt: date + "T00:40:00+07:00",
		Items: []EvidenceItem{{
			Ref: "gh:acme/api:commit:spill", Kind: "commit", Repo: "acme/api",
			Title: "spill evidence", Time: date + "T00:30:00+07:00",
		}},
	}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.openDay(Request{Day: date}); err != nil {
		t.Fatal(err)
	}
	review, err := s.review(Request{Day: date})
	if err != nil {
		t.Fatal(err)
	}

	draft := BuildDraft(review, "test")
	if len(draft.Blocks) != 1 {
		t.Fatalf("draft blocks = %+v, want the spill block only", draft.Blocks)
	}
	want := draft.Blocks[0]
	if want.BlockID != "work-00:00-01:00" || len(want.Sources) != 1 {
		t.Fatalf("draft block = %s sources=%+v, want work-00:00-01:00 with its source", want.BlockID, want.Sources)
	}

	// The reviewed input is the serialized draft, which carries no sources.
	raw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	var fileDraft Draft
	if err := json.Unmarshal(raw, &fileDraft); err != nil {
		t.Fatal(err)
	}
	reviewed, err := ComposeReviewed(fileDraft, DescriptionsFile{
		Descriptions: map[string]string{want.BlockID: "Overnight release watch"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(reviewed.Blocks[0].Sources) != 0 {
		t.Fatalf("serialized draft carried sources: %+v", reviewed.Blocks[0].Sources)
	}

	if _, err := RecordEntries(statePath, "test", reviewed, false, time.Now()); err != nil {
		t.Fatalf("record: %v", err)
	}
	f, err := ReadEntries(statePath, date)
	if err != nil || f == nil || len(f.Entries) != 1 {
		t.Fatalf("read entries: %+v %v", f, err)
	}
	got := f.Entries[0]
	if len(got.Sources) == 0 {
		t.Fatalf("recorded sources = [], want the draft's %+v", want.Sources)
	}
	if len(got.Sources) != len(want.Sources) {
		t.Fatalf("recorded sources = %+v, want the draft's %+v", got.Sources, want.Sources)
	}
	for i := range want.Sources {
		if got.Sources[i] != want.Sources[i] {
			t.Fatalf("recorded sources = %+v, want the draft's %+v", got.Sources, want.Sources)
		}
	}
}
