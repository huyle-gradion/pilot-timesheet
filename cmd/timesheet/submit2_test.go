package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func seedCatalog(t *testing.T, cfg, catalogID string, options ...timesheet.ClassificationOption) {
	t.Helper()
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
	defer func() { _ = store.Close() }()
	st := store.State()
	if st.Remote == nil {
		st.Remote = &timesheet.RemoteState{}
	}
	if st.Remote.Catalogs == nil {
		st.Remote.Catalogs = map[string]*timesheet.CatalogRecord{}
	}
	st.Remote.Catalogs[catalogID] = &timesheet.CatalogRecord{
		CatalogID:  catalogID,
		Query:      "test",
		Complete:   true,
		ObservedAt: time.Now().UTC().Format(time.RFC3339),
		Options:    options,
	}
	if err := store.CommitState(st); err != nil {
		t.Fatalf("seed catalog: %v", err)
	}
}

func writeChoicesFile(t *testing.T, cfg, body string) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(cfg), "choices.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runClassifyCapture(t *testing.T, args []string) ClassifyReceipt {
	t.Helper()
	var receipt ClassifyReceipt
	out := captureStdout(t, func() {
		if err := runClassify(args); err != nil {
			t.Fatalf("classify: %v", err)
		}
	})
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		t.Fatalf("classify receipt is not valid JSON: %v\n%s", err, out)
	}
	return receipt
}

func TestClassifyCarriesSelectionThroughSave(t *testing.T) {
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "Build the bridge"),
		timedBlock("work-2", "11:00", "12:00", "Review the deck"),
	})
	seedCatalog(t, cfg, "cat-1",
		timesheet.ClassificationOption{ID: "cls-dev", Name: "Development", Code: "DEV", ActivityType: "engineering"},
		timesheet.ClassificationOption{ID: "cls-ops", Name: "Operations", Code: "OPS"},
	)
	stage := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	if len(stage.Staged) != 2 {
		t.Fatalf("stage = %+v, want 2 staged intervals", stage.Staged)
	}
	ids := map[string]string{}
	for _, ref := range stage.Staged {
		ids[ref.BlockID] = ref.IntervalID
	}
	choices := `{"selections": {` +
		`"` + ids["work-1"] + `": {"catalog_id": "cat-1", "classification_id": "cls-dev"},` +
		`"` + ids["work-2"] + `": {"catalog_id": "cat-1", "classification_id": "cls-ops"}}}`
	choicesPath := writeChoicesFile(t, cfg, choices)
	receipt := runClassifyCapture(t, []string{"--config", cfg, "--date", testDate, "--file", choicesPath})
	if receipt.Selections != 2 || len(receipt.Applied) != 2 || receipt.Note != "" {
		t.Fatalf("classify receipt = %+v, want 2 applied and no note", receipt)
	}
	if receipt.StagedIntervals != 2 || receipt.Revision != stage.Revision {
		t.Fatalf("classify receipt = %+v, want 2 staged at revision %d", receipt, stage.Revision)
	}
	byInterval := map[string]ClassifyApplied{}
	for _, a := range receipt.Applied {
		byInterval[a.IntervalID] = a
	}
	if a := byInterval[ids["work-1"]]; a.ClassificationID != "cls-dev" || a.Classification != "Development" || a.Code != "DEV" || !a.CatalogComplete {
		t.Fatalf("applied work-1 = %+v, want cls-dev Development/DEV complete", a)
	}
	if receipt.SidecarFile != timesheet.ChoicesPath(statePathFor(cfg), testDate) {
		t.Fatalf("sidecar path = %q, want %q", receipt.SidecarFile, timesheet.ChoicesPath(statePathFor(cfg), testDate))
	}
	persisted, err := timesheet.ReadClassificationChoices(statePathFor(cfg), testDate)
	if err != nil || len(persisted) != 2 || persisted[ids["work-1"]].ClassificationID != "cls-dev" {
		t.Fatalf("sidecar = %v, %v; want both selections persisted", persisted, err)
	}
	entries, err := timesheet.ReadEntries(statePathFor(cfg), testDate)
	if err != nil || entries == nil {
		t.Fatalf("entries = %v, %v", entries, err)
	}
	for _, e := range entries.Entries {
		got := persisted[ids[e.BlockID]].ContentDigest
		if got == "" || got != timesheet.BlockContentDigest(testDate, e) {
			t.Fatalf("sidecar digest for %s = %q, want the block content digest", e.BlockID, got)
		}
	}

	if err := runConfirm([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := runSave([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("save: %v", err)
	}
	confirmed := readDayConfirmed(t, cfg, testDate)
	if len(confirmed) != 2 {
		t.Fatalf("confirmed = %+v, want 2 intervals", confirmed)
	}
	byConfirmed := map[string]timesheet.Interval{}
	for _, iv := range confirmed {
		byConfirmed[iv.Description] = iv
	}
	if iv := byConfirmed["Build the bridge"]; iv.RemoteClassificationID != "cls-dev" || iv.RemoteCatalogID != "cat-1" {
		t.Fatalf("saved work-1 = %+v, want durable cls-dev from cat-1", iv)
	}
	if iv := byConfirmed["Review the deck"]; iv.RemoteClassificationID != "cls-ops" || iv.RemoteCatalogID != "cat-1" {
		t.Fatalf("saved work-2 = %+v, want durable cls-ops from cat-1", iv)
	}
}

func TestClassifyStaleIntervalIDFailsClosed(t *testing.T) {
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "One block"),
	})
	seedCatalog(t, cfg, "cat-1", timesheet.ClassificationOption{ID: "cls-dev", Name: "Development"})
	runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	choicesPath := writeChoicesFile(t, cfg,
		`{"selections": {"iv-9999": {"catalog_id": "cat-1", "classification_id": "cls-dev"}}}`)
	err := runClassify([]string{"--config", cfg, "--date", testDate, "--file", choicesPath})
	if err == nil || !strings.Contains(err.Error(), "unknown_interval") ||
		!strings.Contains(err.Error(), "interval is not in the staged day") {
		t.Fatalf("stale interval id = %v, want the verbatim unknown_interval refusal", err)
	}
	persisted, err := timesheet.ReadClassificationChoices(statePathFor(cfg), testDate)
	if err != nil || persisted != nil {
		t.Fatalf("sidecar after refusal = %v, %v; want nothing persisted", persisted, err)
	}
}

func TestClassifyUnknownSelectionRefusals(t *testing.T) {
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "One block"),
	})
	seedCatalog(t, cfg, "cat-1", timesheet.ClassificationOption{ID: "cls-dev", Name: "Development"})
	stage := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	ivID := stage.Staged[0].IntervalID

	unknownOption := writeChoicesFile(t, cfg,
		`{"selections": {"`+ivID+`": {"catalog_id": "cat-1", "classification_id": "cls-nope"}}}`)
	err := runClassify([]string{"--config", cfg, "--date", testDate, "--file", unknownOption})
	if err == nil || !strings.Contains(err.Error(), "unknown_classification") ||
		!strings.Contains(err.Error(), "not returned by this catalog") {
		t.Fatalf("unknown classification_id = %v, want the verbatim unknown_classification refusal", err)
	}

	unknownCatalog := writeChoicesFile(t, cfg,
		`{"selections": {"`+ivID+`": {"catalog_id": "cat-ghost", "classification_id": "cls-dev"}}}`)
	err = runClassify([]string{"--config", cfg, "--date", testDate, "--file", unknownCatalog})
	if err == nil || !strings.Contains(err.Error(), "stale_catalog") ||
		!strings.Contains(err.Error(), "not a stored catalog") {
		t.Fatalf("unknown catalog_id = %v, want the verbatim stale_catalog refusal", err)
	}
}

func TestClassifyNoopEmptySelections(t *testing.T) {
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "One block"),
	})
	seedCatalog(t, cfg, "cat-1", timesheet.ClassificationOption{ID: "cls-dev", Name: "Development"})
	stage := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	ivID := stage.Staged[0].IntervalID

	first := writeChoicesFile(t, cfg,
		`{"selections": {"`+ivID+`": {"catalog_id": "cat-1", "classification_id": "cls-dev"}}}`)
	if receipt := runClassifyCapture(t, []string{"--config", cfg, "--date", testDate, "--file", first}); receipt.Selections != 1 {
		t.Fatalf("first classify = %+v, want 1 selection", receipt)
	}
	empty := writeChoicesFile(t, cfg, `{}`)
	receipt := runClassifyCapture(t, []string{"--config", cfg, "--date", testDate, "--file", empty})
	if receipt.Selections != 0 || len(receipt.Applied) != 0 {
		t.Fatalf("no-op receipt = %+v, want 0 selections", receipt)
	}
	if !strings.Contains(receipt.Note, "nothing was classified") {
		t.Fatalf("no-op note = %q, want the honest no-op wording", receipt.Note)
	}
	persisted, err := timesheet.ReadClassificationChoices(statePathFor(cfg), testDate)
	if err != nil || len(persisted) != 0 {
		t.Fatalf("sidecar after no-op = %v, %v; want the earlier selection cleared", persisted, err)
	}
	if err := runConfirm([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := runSave([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("save: %v", err)
	}
	for _, iv := range readDayConfirmed(t, cfg, testDate) {
		if iv.RemoteClassificationID != "" {
			t.Fatalf("saved interval = %+v, want no remote classification after the no-op", iv)
		}
	}
}

func TestClassifyChoicesSchemaErrors(t *testing.T) {
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "One block"),
	})
	runStageCapture(t, []string{"--config", cfg, "--date", testDate})

	missingClassification := writeChoicesFile(t, cfg,
		`{"selections": {"iv-1": {"catalog_id": "cat-1"}}}`)
	err := runClassify([]string{"--config", cfg, "--date", testDate, "--file", missingClassification})
	if err == nil || !strings.Contains(err.Error(), "iv-1") || !strings.Contains(err.Error(), "missing classification_id") {
		t.Fatalf("missing classification_id = %v, want an error naming iv-1", err)
	}

	missingCatalog := writeChoicesFile(t, cfg,
		`{"selections": {"iv-2": {"classification_id": "cls-dev"}}}`)
	err = runClassify([]string{"--config", cfg, "--date", testDate, "--file", missingCatalog})
	if err == nil || !strings.Contains(err.Error(), "iv-2") || !strings.Contains(err.Error(), "missing catalog_id") {
		t.Fatalf("missing catalog_id = %v, want an error naming iv-2", err)
	}

	notJSON := writeChoicesFile(t, cfg, `{"selections": "all of them"}`)
	err = runClassify([]string{"--config", cfg, "--date", testDate, "--file", notJSON})
	if err == nil || !strings.Contains(err.Error(), "expected schema") {
		t.Fatalf("invalid choices file = %v, want the schema-naming error", err)
	}
}

func TestConfirmSaveFailsClosedOnVanishedSidecarID(t *testing.T) {
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "First block"),
		timedBlock("work-2", "11:00", "12:00", "Second block"),
	})
	seedCatalog(t, cfg, "cat-1", timesheet.ClassificationOption{ID: "cls-dev", Name: "Development"})
	stage := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	if len(stage.Staged) != 2 {
		t.Fatalf("stage = %+v, want 2 staged intervals", stage.Staged)
	}
	choicesPath := writeChoicesFile(t, cfg,
		`{"selections": {"`+stage.Staged[1].IntervalID+`": {"catalog_id": "cat-1", "classification_id": "cls-dev"}}}`)
	runClassifyCapture(t, []string{"--config", cfg, "--date", testDate, "--file", choicesPath})

	writeEntriesFile(t, cfg, testDate, []timesheet.EntryRecord{
		{BlockID: "work-1", Date: testDate, Start: "09:00", End: "10:00", Description: "First block", Kind: "work"},
	})
	err := runConfirm([]string{"--config", cfg, "--date", testDate})
	if err == nil || !strings.Contains(err.Error(), "cannot be bound to a recorded block") ||
		!strings.Contains(err.Error(), "re-run stage + classify") {
		t.Fatalf("confirm after removal = %v, want the fail-closed unbindable-selection refusal", err)
	}
	err = runSave([]string{"--config", cfg, "--date", testDate})
	if err == nil || !strings.Contains(err.Error(), "cannot be bound to a recorded block") {
		t.Fatalf("save after removal = %v, want the fail-closed unbindable-selection refusal", err)
	}
	assertNoConfirmedDay(t, cfg)
}

func TestConfirmSaveFailsClosedOnInsertionShift(t *testing.T) {
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "First block"),
		timedBlock("work-2", "11:00", "12:00", "Second block"),
	})
	seedCatalog(t, cfg, "cat-1", timesheet.ClassificationOption{ID: "cls-dev", Name: "Development"})
	stage := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	classifiedID := stage.Staged[1].IntervalID
	choicesPath := writeChoicesFile(t, cfg,
		`{"selections": {"`+classifiedID+`": {"catalog_id": "cat-1", "classification_id": "cls-dev"}}}`)
	runClassifyCapture(t, []string{"--config", cfg, "--date", testDate, "--file", choicesPath})

	writeEntriesFile(t, cfg, testDate, []timesheet.EntryRecord{
		{BlockID: "work-1", Date: testDate, Start: "09:00", End: "10:00", Description: "First block", Kind: "work"},
		{BlockID: "work-1b", Date: testDate, Start: "10:00", End: "11:00", Description: "Inserted block", Kind: "work"},
		{BlockID: "work-2", Date: testDate, Start: "11:00", End: "12:00", Description: "Second block", Kind: "work"},
	})
	err := runConfirm([]string{"--config", cfg, "--date", testDate})
	if err == nil || !strings.Contains(err.Error(), "changed since classify") ||
		!strings.Contains(err.Error(), "work-1b") || !strings.Contains(err.Error(), "re-run stage + classify") {
		t.Fatalf("confirm after insertion = %v, want the fail-closed content mismatch naming work-1b", err)
	}
	err = runSave([]string{"--config", cfg, "--date", testDate})
	if err == nil || !strings.Contains(err.Error(), "changed since classify") {
		t.Fatalf("save after insertion = %v, want the fail-closed content mismatch", err)
	}
	assertNoConfirmedDay(t, cfg)
}

func TestConfirmSaveFailsClosedOnSameCountReplacement(t *testing.T) {
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "First block"),
		timedBlock("work-2", "11:00", "12:00", "Second block"),
	})
	seedCatalog(t, cfg, "cat-1", timesheet.ClassificationOption{ID: "cls-dev", Name: "Development"})
	stage := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	choicesPath := writeChoicesFile(t, cfg,
		`{"selections": {"`+stage.Staged[1].IntervalID+`": {"catalog_id": "cat-1", "classification_id": "cls-dev"}}}`)
	runClassifyCapture(t, []string{"--config", cfg, "--date", testDate, "--file", choicesPath})

	writeEntriesFile(t, cfg, testDate, []timesheet.EntryRecord{
		{BlockID: "work-1", Date: testDate, Start: "09:00", End: "10:00", Description: "First block", Kind: "work"},
		{BlockID: "work-2", Date: testDate, Start: "11:00", End: "12:30", Description: "Second block rewritten", Kind: "work"},
	})
	err := runConfirm([]string{"--config", cfg, "--date", testDate})
	if err == nil || !strings.Contains(err.Error(), "changed since classify") ||
		!strings.Contains(err.Error(), "work-2") || !strings.Contains(err.Error(), "re-run stage + classify") {
		t.Fatalf("confirm after replacement = %v, want the fail-closed content mismatch naming work-2", err)
	}
	err = runSave([]string{"--config", cfg, "--date", testDate})
	if err == nil || !strings.Contains(err.Error(), "changed since classify") {
		t.Fatalf("save after replacement = %v, want the fail-closed content mismatch", err)
	}
	assertNoConfirmedDay(t, cfg)
}

func assertNoConfirmedDay(t *testing.T, cfg string) {
	t.Helper()
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if day, ok := store.State().Days[testDate]; ok && len(day.Confirmed) > 0 {
		t.Fatalf("day %s is durable with %d confirmed intervals; want nothing saved", testDate, len(day.Confirmed))
	}
}

func TestReadChoicesMissingSelectionsKeyFailsClosed(t *testing.T) {
	cfg := writeTestConfig(t)
	path := timesheet.ChoicesPath(statePathFor(cfg), testDate)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"date": "` + testDate + `", "app_version": "test", "updated_at": "2026-10-26T00:00:00Z"}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	sel, err := timesheet.ReadClassificationChoices(statePathFor(cfg), testDate)
	if err == nil || !strings.Contains(err.Error(), "state_invalid") ||
		!strings.Contains(err.Error(), "no selections key") || !strings.Contains(err.Error(), path) {
		t.Fatalf("missing selections key = (%v, %v), want state_invalid naming the file and key", sel, err)
	}
}

func TestClassificationsWithoutRemoteFailsClosed(t *testing.T) {
	cfg := writeTestConfig(t)
	err := runClassifications([]string{"--config", cfg, "--query", "development"})
	if err == nil || !strings.Contains(err.Error(), "remote_disabled") {
		t.Fatalf("classifications without remote = %v, want the remote_disabled refusal", err)
	}
	if err := runClassifications([]string{"--config", cfg}); err == nil ||
		!strings.Contains(err.Error(), "requires --query") {
		t.Fatalf("classifications without query = %v, want the flag error", err)
	}
}
