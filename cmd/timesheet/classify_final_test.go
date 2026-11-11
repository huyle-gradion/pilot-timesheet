package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// --- CLASSIFY-FINAL-1 synthetic acceptance tests --------------------------------
//
// The deadlock this slice fixes: a day finalized (confirmed + saved) WITHOUT a
// classification could never become plan-ready — plan blocked on
// classification_unselected while classify refused finalized days via
// openFinalizeDay. These tests cover classify's finalized-day path: the
// selections key off the durable interval ids, the command re-finalizes the
// day at a new revision (confirm + save in-process), `{}` is an honest no-op,
// and a stale or unknown interval id fails closed with the revision unchanged.
// No network: the catalog is seeded directly into the store's persisted
// catalog cache and the plan observation runs against the SUBMIT-3 loopback
// stub; every command runs as its own one-shot process would against a fresh
// temporary store.

// readDurableDay opens the store read-only and returns the persisted day.
func readDurableDay(t *testing.T, cfg, date string) *timesheet.Day {
	t.Helper()
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatalf("read durable day: %v", err)
	}
	defer func() { _ = store.Close() }()
	day, ok := store.State().Days[date]
	if !ok {
		t.Fatalf("read durable day: day %s is not in the store", date)
	}
	return day
}

// finalizedUnclassifiedDay runs the SUBMIT-1 flow through confirm and save
// WITHOUT any classification: three timed blocks become three durable
// confirmed intervals with empty remote_classification_id, and a catalog with
// two options is seeded for the finalized-path selections. It returns the
// config path, the durable interval ids in block order and the saved revision.
func finalizedUnclassifiedDay(t *testing.T) (string, []string, int) {
	t.Helper()
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "Build the bridge"),
		timedBlock("work-2", "11:00", "12:00", "Review the deck"),
		timedBlock("work-3", "14:00", "15:00", "Write the report"),
	})
	seedCatalog(t, cfg, "cat-1",
		timesheet.ClassificationOption{ID: "cls-dev", Name: "Development", Code: "DEV", ActivityType: "engineering"},
		timesheet.ClassificationOption{ID: "cls-ops", Name: "Operations", Code: "OPS"},
	)
	stage := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	if len(stage.Staged) != 3 {
		t.Fatalf("stage = %+v, want 3 staged intervals", stage.Staged)
	}
	if err := runConfirm([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := runSave([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("save: %v", err)
	}
	confirmed := readDayConfirmed(t, cfg, testDate)
	if len(confirmed) != 3 {
		t.Fatalf("confirmed = %+v, want 3 intervals", confirmed)
	}
	ids := make([]string, 0, len(confirmed))
	for _, iv := range confirmed {
		if iv.RemoteClassificationID != "" {
			t.Fatalf("confirmed %s carries %q; the fixture day must be unclassified", iv.ID, iv.RemoteClassificationID)
		}
		ids = append(ids, iv.ID)
	}
	return cfg, ids, readDurableDay(t, cfg, testDate).Revision
}

// TestClassifyFinalizedDayRefinalizesAtNewRevision: a saved unclassified day
// classifies against its durable interval ids; the command confirms and saves
// in-process and the receipt reports the re-finalize at the new revision; the
// store's confirmed intervals at that revision carry the chosen remote
// classification and catalog ids, and no classification sidecar is written.
func TestClassifyFinalizedDayRefinalizesAtNewRevision(t *testing.T) {
	cfg, ids, before := finalizedUnclassifiedDay(t)
	choices := `{"selections": {` +
		`"` + ids[0] + `": {"catalog_id": "cat-1", "classification_id": "cls-dev"},` +
		`"` + ids[1] + `": {"catalog_id": "cat-1", "classification_id": "cls-ops"}}}`
	choicesPath := writeChoicesFile(t, cfg, choices)
	receipt := runClassifyCapture(t, []string{"--config", cfg, "--date", testDate, "--file", choicesPath})
	if receipt.Selections != 2 || len(receipt.Applied) != 2 {
		t.Fatalf("classify receipt = %+v, want 2 applied", receipt)
	}
	if !receipt.Refinalized {
		t.Fatalf("classify receipt = %+v, want refinalized", receipt)
	}
	if receipt.SidecarFile != "" {
		t.Fatalf("classify receipt sidecar = %q, want none on the finalized-day path", receipt.SidecarFile)
	}
	if receipt.Revision != before+1 {
		t.Fatalf("classify receipt revision = %d, want the new revision %d", receipt.Revision, before+1)
	}
	if !strings.Contains(receipt.Note, "re-finalized at revision "+itoa(receipt.Revision)) {
		t.Fatalf("classify receipt note = %q, want the re-finalize wording", receipt.Note)
	}
	byInterval := map[string]ClassifyApplied{}
	for _, a := range receipt.Applied {
		byInterval[a.IntervalID] = a
	}
	if a := byInterval[ids[0]]; a.ClassificationID != "cls-dev" || a.Classification != "Development" || a.Code != "DEV" || !a.CatalogComplete {
		t.Fatalf("applied %s = %+v, want cls-dev Development/DEV complete", ids[0], a)
	}
	if a := byInterval[ids[1]]; a.ClassificationID != "cls-ops" || a.Classification != "Operations" || a.Code != "OPS" {
		t.Fatalf("applied %s = %+v, want cls-ops Operations/OPS", ids[1], a)
	}
	day := readDurableDay(t, cfg, testDate)
	if day.Revision != before+1 {
		t.Fatalf("durable revision = %d, want the new revision %d", day.Revision, before+1)
	}
	if len(day.Confirmed) != 3 {
		t.Fatalf("confirmed = %+v, want the same 3 intervals", day.Confirmed)
	}
	byID := map[string]timesheet.Interval{}
	for _, iv := range day.Confirmed {
		byID[iv.ID] = iv
	}
	if iv := byID[ids[0]]; iv.RemoteClassificationID != "cls-dev" || iv.RemoteCatalogID != "cat-1" {
		t.Fatalf("confirmed %s = %+v, want cls-dev from cat-1", ids[0], iv)
	}
	if iv := byID[ids[1]]; iv.RemoteClassificationID != "cls-ops" || iv.RemoteCatalogID != "cat-1" {
		t.Fatalf("confirmed %s = %+v, want cls-ops from cat-1", ids[1], iv)
	}
	if iv := byID[ids[2]]; iv.RemoteClassificationID != "" || iv.RemoteCatalogID != "" {
		t.Fatalf("confirmed %s = %+v, want no selection (the file named only two intervals)", ids[2], iv)
	}
	if persisted, err := timesheet.ReadClassificationChoices(statePathFor(cfg), testDate); err != nil || len(persisted) != 0 {
		t.Fatalf("sidecar after finalized classify = %v, %v; want none written", persisted, err)
	}
}

// TestClassifyFinalizedDayEmptyChoicesIsNoOp: `{}` on a finalized day applies
// nothing, runs no confirm and no save, and leaves the saved revision and the
// confirmed intervals untouched.
func TestClassifyFinalizedDayEmptyChoicesIsNoOp(t *testing.T) {
	cfg, _, before := finalizedUnclassifiedDay(t)
	empty := writeChoicesFile(t, cfg, `{}`)
	receipt := runClassifyCapture(t, []string{"--config", cfg, "--date", testDate, "--file", empty})
	if receipt.Selections != 0 || len(receipt.Applied) != 0 {
		t.Fatalf("no-op receipt = %+v, want 0 selections", receipt)
	}
	if receipt.Refinalized {
		t.Fatalf("no-op receipt = %+v, want refinalized false", receipt)
	}
	if receipt.Revision != before {
		t.Fatalf("no-op receipt revision = %d, want the unchanged %d", receipt.Revision, before)
	}
	if !strings.Contains(receipt.Note, "nothing was classified") || !strings.Contains(receipt.Note, itoa(before)) {
		t.Fatalf("no-op note = %q, want the honest unchanged-at-revision wording", receipt.Note)
	}
	day := readDurableDay(t, cfg, testDate)
	if day.Revision != before {
		t.Fatalf("durable revision = %d, want it unchanged at %d", day.Revision, before)
	}
	for _, iv := range day.Confirmed {
		if iv.RemoteClassificationID != "" {
			t.Fatalf("confirmed %s = %+v, want no selection after the no-op", iv.ID, iv)
		}
	}
}

// TestClassifyFinalizedDayStaleIntervalIDFailsClosed: an unknown interval id —
// even a plausible-shaped one that a later entries generation could have
// minted — fails closed with the op's own verbatim error; when it follows a
// valid selection, that earlier in-memory selection must not leak into the
// durable day either.
func TestClassifyFinalizedDayStaleIntervalIDFailsClosed(t *testing.T) {
	cfg, ids, before := finalizedUnclassifiedDay(t)
	choices := `{"selections": {` +
		`"` + ids[0] + `": {"catalog_id": "cat-1", "classification_id": "cls-dev"},` +
		`"iv-20261026-99": {"catalog_id": "cat-1", "classification_id": "cls-dev"}}}`
	choicesPath := writeChoicesFile(t, cfg, choices)
	var err error
	captureStdout(t, func() {
		err = runClassify([]string{"--config", cfg, "--date", testDate, "--file", choicesPath})
	})
	if err == nil || !strings.Contains(err.Error(), "unknown_interval") ||
		!strings.Contains(err.Error(), "interval is not in the staged day; review again") {
		t.Fatalf("classify with stale id = %v, want the verbatim unknown_interval op error", err)
	}
	day := readDurableDay(t, cfg, testDate)
	if day.Revision != before {
		t.Fatalf("durable revision = %d, want it unchanged at %d", day.Revision, before)
	}
	byID := map[string]timesheet.Interval{}
	for _, iv := range day.Confirmed {
		byID[iv.ID] = iv
	}
	for _, id := range ids {
		if iv := byID[id]; iv.RemoteClassificationID != "" || iv.RemoteCatalogID != "" {
			t.Fatalf("confirmed %s = %+v, want the durable day untouched after the fail-closed refusal", id, iv)
		}
	}
}

// TestClassifyFinalizedDayPlanBecomesReady: the full deadlock and its fix
// against the SUBMIT-3 loopback stub — a saved unclassified day plans blocked
// with classification_unselected; the finalized-day classify re-finalizes at a
// new revision; the re-preview is ready with no blockers and a new plan_id.
func TestClassifyFinalizedDayPlanBecomesReady(t *testing.T) {
	stub := &stubRemote{entries: map[string][]map[string]any{testDate: {}}}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	t.Setenv("SYNTHETIC_GRADION_TOKEN", "synthetic-token")
	cfg := writeTestConfigWithGradion(t, server.URL)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "Build the bridge"),
	})
	seedCatalog(t, cfg, "cat-1",
		timesheet.ClassificationOption{ID: "cls-dev", Name: "Development", Code: "DEV", ActivityType: "engineering"},
	)
	stage := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	if len(stage.Staged) != 1 {
		t.Fatalf("stage = %+v, want 1 staged interval", stage.Staged)
	}
	if err := runConfirm([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := runSave([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("save: %v", err)
	}

	// The deadlock, reproduced: the saved day can never pass plan.
	first := runPlanCapture(t, []string{"--config", cfg, "--date", testDate})
	if first.State != "blocked" || !hasBlocker(first.Blockers, "classification_unselected", "") {
		t.Fatalf("first plan = %+v, want blocked on classification_unselected", first)
	}

	confirmed := readDayConfirmed(t, cfg, testDate)
	if len(confirmed) != 1 {
		t.Fatalf("confirmed = %+v, want 1 interval", confirmed)
	}
	choicesPath := writeChoicesFile(t, cfg,
		`{"selections": {"`+confirmed[0].ID+`": {"catalog_id": "cat-1", "classification_id": "cls-dev"}}}`)
	receipt := runClassifyCapture(t, []string{"--config", cfg, "--date", testDate, "--file", choicesPath})
	if !receipt.Refinalized || receipt.Revision != 2 {
		t.Fatalf("classify receipt = %+v, want a re-finalize at revision 2", receipt)
	}

	second := runPlanCapture(t, []string{"--config", cfg, "--date", testDate})
	if second.State != "ready" || len(second.Blockers) != 0 {
		t.Fatalf("second plan = %+v, want a ready preview with no blockers", second)
	}
	if second.PlanID == first.PlanID {
		t.Fatal("the re-preview reused the blocked plan_id; the identity must differ")
	}
	if len(second.Operations) != 1 || second.Operations[0].Kind != "create" || second.Operations[0].ClassificationID != "cls-dev" {
		t.Fatalf("second plan operations = %+v, want one create carrying cls-dev", second.Operations)
	}
}
