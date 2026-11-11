package timesheet

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const legacyDayOne = `{
  "schema_version": 1,
  "date": "2026-10-20",
  "revision": 3,
  "updated_at": "2026-10-20T18:00:00Z",
  "evidence": [
    {"id": "ev-1", "source": "github", "timestamp": "2026-10-20T09:00:00Z", "end_time": "2026-10-20T10:00:00Z", "title": "Synthetic commit", "ref": "o/r", "observed_at": "2026-10-20T18:00:00Z"}
  ],
  "confirmed": [
    {"id": "legacy-1", "date": "2026-10-20", "start_time": "09:00", "end_time": "10:30", "project": "OLD-1", "description": "Synthetic legacy work", "attendance": false, "confirmed_at": "2026-10-20T17:00:00Z", "revision": 3, "kind": "work", "manual": true, "evidence_ids": ["ev-1"], "classification_id": "0f8fad5b-d9cb-469f-a165-70867728950e"},
    {"id": "legacy-2", "date": "2026-10-20", "start_time": "22:30", "end_time": "01:15", "project": "OLD-1", "description": "Legacy late session", "confirmed_at": "2026-10-20T17:00:00Z", "revision": 3, "kind": "work", "manual": true}
  ],
  "remote_mappings": [
    {"local_id": "legacy-1", "remote_id": "r-100", "date": "2026-10-20", "start_time": "09:00", "end_time": "10:30", "classification_id": "0f8fad5b-d9cb-469f-a165-70867728950e", "description": "Synthetic legacy work", "last_observed": {"remote_id": "r-100", "date": "2026-10-20", "start_time": "09:00", "end_time": "10:30", "classification_id": "0f8fad5b-d9cb-469f-a165-70867728950e", "description": "Synthetic legacy work", "observed_at": "2026-10-20T18:00:00Z", "content_observed": true}, "created_at": "2026-10-20T10:31:00Z", "updated_at": "2026-10-20T18:00:00Z"}
  ],
  "outcomes": [
    {"operation_key": "k-1", "local_id": "legacy-2", "remote_id": "", "action": "create", "status": "uncertain", "date": "2026-10-20", "start_time": "22:30", "end_time": "01:15", "description": "Legacy late session", "error_code": "WRITE_UNCERTAIN", "resolved": false, "recorded_at": "2026-10-20T18:01:00Z", "intent_at": "2026-10-20T18:00:00Z"}
  ],
  "carries": [
    {"source_date": "2026-10-20", "date": "2026-10-21", "start": "00:00", "end": "01:15", "minutes": 75, "split_group_id": "sg-1", "part_index": 2, "kind": "work", "project": "OLD-1", "description": "Legacy late session", "evidence_ids": []}
  ],
  "review": {"reviewed": true, "empty": false, "reviewed_at": "2026-10-20T17:30:00Z", "excluded_evidence": [{"evidence_id": "ev-2", "reason": "not work"}], "attendance": [], "acknowledged_sources": ["github"]},
  "collected_evidence": [{"raw": "SYNTHETIC-RAW-DOCUMENT-NOT-IMPORTED"}],
  "plans": [{"plan_id": "plan-old", "date": "2026-10-20", "operations": [{"local_id": "legacy-1", "kind": "create"}]}],
  "approvals": [{"approval_id": "plan-old", "plan_id": "plan-old", "date": "2026-10-20", "approved_at": "2026-10-20T18:05:00Z"}]
}`

func migrationFixture(t *testing.T) (*Config, string, string) {
	t.Helper()
	dir := t.TempDir()
	legacyRoot := filepath.Join(dir, "legacy")
	if err := os.MkdirAll(filepath.Join(legacyRoot, "workflow"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeLegacy(t, legacyRoot, "2026-10-20", legacyDayOne)
	cfg := &Config{
		Timezone:  "UTC",
		StatePath: filepath.Join(dir, "state.json"),
		Migration: &MigrationConfig{
			Enabled:           true,
			LegacyRoot:        legacyRoot,
			LegacyTimezone:    "Europe/Amsterdam",
			ClassificationMap: map[string]string{},
		},
	}
	return cfg, legacyRoot, dir
}

func writeLegacy(t *testing.T, root, date, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, "workflow", date+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationPreservesWorkAndIsRepeatSafe(t *testing.T) {
	cfg, legacyRoot, dir := migrationFixture(t)
	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	cand, err := buildMigrationCandidate(cfg, store.State(), []string{"2026-10-20"})
	if err != nil {
		t.Fatal(err)
	}
	if !cand.Ready {
		t.Fatalf("candidate not ready: %+v", cand.Conflicts)
	}
	if cand.Counts.Intervals != 2 || cand.Counts.Mappings != 1 || cand.Counts.Unresolved != 1 ||
		cand.Counts.Carries != 1 || cand.Counts.LegacyApprovals != 1 || cand.Counts.Classified != 1 {
		t.Fatalf("unexpected counts: %+v", cand.Counts)
	}
	if err := store.CommitState(mergeMigrationCandidate(store.State(), cand)); err != nil {
		t.Fatal(err)
	}
	st := store.State()
	if st.Schema != stateSchema {
		t.Fatalf("schema = %d", st.Schema)
	}
	day := st.Days["2026-10-20"]
	if day == nil || day.Revision != 3 || len(day.Confirmed) != 2 {
		t.Fatalf("day not preserved: %+v", day)
	}
	if day.Confirmed[0].ID != "legacy-1" || day.Confirmed[1].ID != "legacy-2" {
		t.Fatalf("ids not preserved: %+v", day.Confirmed)
	}
	if day.Confirmed[0].Project != "" || day.Confirmed[0].MigratedClassificationID == "" ||
		day.Confirmed[0].RemoteClassificationID != "" {
		t.Fatalf("classification hint not preserved as unverified: %+v", day.Confirmed[0])
	}
	if len(st.Remote.Bindings) != 1 || st.Remote.Bindings[0].LocalID != "legacy-1" || st.Remote.Bindings[0].RemoteID != "r-100" {
		t.Fatalf("binding not preserved: %+v", st.Remote.Bindings)
	}
	if st.Remote.Bindings[0].LastObserved == nil || !st.Remote.Bindings[0].LastObserved.ContentObserved {
		t.Fatal("observed content distinction lost")
	}
	if len(st.Remote.Outcomes) != 1 || st.Remote.Outcomes[0].Resolved || !outcomeBlocks(st.Remote.Outcomes[0]) {
		t.Fatalf("unresolved outcome not preserved: %+v", st.Remote.Outcomes)
	}
	if len(st.Migration.Carries) != 1 || st.Migration.Carries[0].Confirmed {
		t.Fatalf("carry must stay unconfirmed: %+v", st.Migration.Carries)
	}
	rec := st.Migration.Receipts[0]
	if len(rec.LegacyApprovalsAudit) != 1 || rec.LegacyApprovalsAudit[0].Usable || !rec.LegacyApprovalsAudit[0].Inert ||
		rec.LegacyApprovalsAudit[0].ApprovedAt != "2026-10-20T18:05:00Z" {
		t.Fatalf("approval must be inert history with its timestamp: %+v", rec.LegacyApprovalsAudit)
	}
	if len(rec.LegacyPlanAudits) != 1 || len(rec.LegacyPlanAudits[0].OperationRows) != 1 ||
		rec.LegacyPlanAudits[0].OperationRows[0].Kind != "create" ||
		rec.LegacyPlanAudits[0].OperationRows[0].LocalID != "legacy-1" ||
		!rec.LegacyPlanAudits[0].Inert {
		t.Fatalf("legacy plan operation content must be preserved inertly: %+v", rec.LegacyPlanAudits)
	}
	if len(rec.IDCrosswalk) != 0 || len(rec.IDMapping) != 0 {
		t.Fatalf("unchanged identities must not fabricate a crosswalk: %v %v", rec.IDCrosswalk, rec.IDMapping)
	}
	if len(rec.LegacyEvidence) != 1 || rec.LegacyEvidence[0].Ref != "o/r" {
		t.Fatalf("evidence provenance lost: %+v", rec.LegacyEvidence)
	}
	if len(rec.LegacyReviews) != 1 || rec.LegacyReviews[0].Date != "2026-10-20" ||
		!rec.LegacyReviews[0].Reviewed || rec.LegacyReviews[0].Excluded != 1 ||
		len(rec.LegacyReviews[0].ExcludedRows) != 1 ||
		rec.LegacyReviews[0].ExcludedRows[0].Reason != "not work" {
		t.Fatalf("review audit lost: %+v", rec.LegacyReviews)
	}
	if rec.LegacyReview != nil {
		t.Fatalf("new receipts must not use the legacy single-pointer review: %+v", rec.LegacyReview)
	}
	if rec.LegacyCollectedEvidence != 1 {
		t.Fatal("raw collected-evidence count not recorded")
	}

	// Source bytes are never written by migration.
	original, err := os.ReadFile(filepath.Join(legacyRoot, "workflow", "2026-10-20.json"))
	if err != nil || string(original) != legacyDayOne {
		t.Fatal("legacy source bytes changed")
	}

	// A repeat preview of identical bytes is a no-change result, including after
	// a restart.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	again, err := buildMigrationCandidate(cfg, reopened.State(), []string{"2026-10-20"})
	if err != nil {
		t.Fatal(err)
	}
	if !again.NoChange || again.Ready || again.ExistingReceipt != cand.MigrationID {
		t.Fatalf("repeat preview is not a no-change result: %+v", again)
	}
	if len(reopened.State().Migration.Receipts) != 1 {
		t.Fatal("repeat preview must not add a receipt")
	}
	_ = dir
}

// F8: legacy terminal outcomes are HISTORY, not pending writes. Only
// unresolved in_flight/uncertain/unidentified rows block; an unrecognised
// status is never imported as safe history.
func TestMigrationTerminalAndUnknownOutcomes(t *testing.T) {
	cfg, legacyRoot, _ := migrationFixture(t) // dirs/config reuse; our own files below
	cfg.Migration.LegacyRoot = legacyRoot
	writeLegacy(t, legacyRoot, "2026-10-25", `{
		"schema_version": 1,
		"date": "2026-10-25",
		"revision": 1,
		"updated_at": "2026-10-25T12:00:00Z",
		"evidence": [],
		"confirmed": [{"id": "out-1", "date": "2026-10-25", "start_time": "09:00", "end_time": "10:00", "project": "OLD-1", "description": "Terminal outcomes day", "confirmed_at": "2026-10-25T11:00:00Z", "revision": 1, "kind": "work", "manual": true}],
		"remote_mappings": [],
		"outcomes": [
			{"operation_key": "k1", "local_id": "out-1", "remote_id": "rr-1", "action": "create", "status": "confirmed", "date": "2026-10-25", "description": "x", "resolved": false, "recorded_at": "2026-10-25T12:00:00Z"},
			{"operation_key": "k2", "local_id": "out-1", "remote_id": "rr-2", "action": "create", "status": "failed", "date": "2026-10-25", "description": "x", "resolved": false, "recorded_at": "2026-10-25T12:00:00Z"},
			{"operation_key": "k4", "local_id": "out-1", "remote_id": "rr-4", "action": "create", "status": "in_flight", "date": "2026-10-25", "description": "x", "resolved": true, "recorded_at": "2026-10-25T12:00:00Z"}
		],
		"carries": [],
		"review": {"reviewed": false},
		"plans": [],
		"approvals": []
	}`)
	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	cand, err := buildMigrationCandidate(cfg, store.State(), []string{"2026-10-25"})
	if err != nil {
		t.Fatal(err)
	}
	if !cand.Ready {
		t.Fatalf("terminal resolved:false outcomes must not block: %+v", cand.Conflicts)
	}
	if cand.Counts.Unresolved != 0 {
		t.Fatalf("terminal outcomes are history, not pending writes: %d", cand.Counts.Unresolved)
	}
	if len(cand.Outcomes) != 3 {
		t.Fatalf("outcomes must still be preserved as audit: %+v", cand.Outcomes)
	}

	// A genuinely unresolved in_flight row keeps blocking, and an unrecognised
	// status is rejected rather than imported as safe.
	writeLegacy(t, legacyRoot, "2026-10-26", `{
		"schema_version": 1,
		"date": "2026-10-26",
		"revision": 1,
		"updated_at": "2026-10-26T12:00:00Z",
		"evidence": [],
		"confirmed": [{"id": "out-2", "date": "2026-10-26", "start_time": "09:00", "end_time": "10:00", "project": "OLD-1", "description": "Pending outcomes day", "confirmed_at": "2026-10-26T11:00:00Z", "revision": 1, "kind": "work", "manual": true}],
		"remote_mappings": [],
		"outcomes": [
			{"operation_key": "k5", "local_id": "out-2", "remote_id": "rr-5", "action": "create", "status": "in_flight", "date": "2026-10-26", "description": "x", "resolved": false, "recorded_at": "2026-10-26T12:00:00Z"},
			{"operation_key": "k6", "local_id": "out-2", "remote_id": "rr-6", "action": "create", "status": "weird_status", "date": "2026-10-26", "description": "x", "resolved": false, "recorded_at": "2026-10-26T12:00:00Z"}
		],
		"carries": [],
		"review": {"reviewed": false},
		"plans": [],
		"approvals": []
	}`)
	cand2, err := buildMigrationCandidate(cfg, store.State(), []string{"2026-10-26"})
	if err != nil {
		t.Fatal(err)
	}
	if cand2.Ready {
		t.Fatal("an unknown outcome status must reject the candidate")
	}
	bad := 0
	for _, c := range cand2.Conflicts {
		if c.Code == "malformed_outcome" {
			bad++
		}
	}
	if bad != 1 {
		t.Fatalf("expected one malformed_outcome for the unknown status: %+v", cand2.Conflicts)
	}

	// The same row with only unresolved in_flight keeps its blocking status.
	writeLegacy(t, legacyRoot, "2026-10-27", `{
		"schema_version": 1,
		"date": "2026-10-27",
		"revision": 1,
		"updated_at": "2026-10-27T12:00:00Z",
		"evidence": [],
		"confirmed": [{"id": "out-3", "date": "2026-10-27", "start_time": "09:00", "end_time": "10:00", "project": "OLD-1", "description": "Pending outcomes day two", "confirmed_at": "2026-10-27T11:00:00Z", "revision": 1, "kind": "work", "manual": true}],
		"remote_mappings": [],
		"outcomes": [
			{"operation_key": "k7", "local_id": "out-3", "remote_id": "rr-7", "action": "create", "status": "in_flight", "date": "2026-10-27", "description": "x", "resolved": false, "recorded_at": "2026-10-27T12:00:00Z"}
		],
		"carries": [],
		"review": {"reviewed": false},
		"plans": [],
		"approvals": []
	}`)
	cand3, err := buildMigrationCandidate(cfg, store.State(), []string{"2026-10-27"})
	if err != nil {
		t.Fatal(err)
	}
	if !cand3.Ready || cand3.Counts.Unresolved != 1 {
		t.Fatalf("an unresolved in_flight row must still block readiness: %+v", cand3)
	}
}

// F2: a duplicated local ID inside one source record is rejected before any
// alias or binding is guessed, and nothing is activated.
func TestMigrationRejectsDuplicateLocalID(t *testing.T) {
	cfg, legacyRoot, dir := migrationFixture(t)
	writeLegacy(t, legacyRoot, "2026-10-26", `{
		"schema_version": 1,
		"date": "2026-10-26",
		"revision": 1,
		"updated_at": "2026-10-26T12:00:00Z",
		"evidence": [],
		"confirmed": [
			{"id": "dup-1", "date": "2026-10-26", "start_time": "09:00", "end_time": "10:00", "project": "OLD-1", "description": "First duplicate row", "confirmed_at": "2026-10-26T11:00:00Z", "revision": 1, "kind": "work", "manual": true},
			{"id": "dup-1", "date": "2026-10-26", "start_time": "11:00", "end_time": "12:00", "project": "OLD-1", "description": "Second duplicate row", "confirmed_at": "2026-10-26T11:00:00Z", "revision": 1, "kind": "work", "manual": true}
		],
		"remote_mappings": [{"local_id": "dup-1", "remote_id": "rm-1", "date": "2026-10-26", "start_time": "09:00", "end_time": "10:00", "description": "First duplicate row"}],
		"outcomes": [{"operation_key": "kd", "local_id": "dup-1", "remote_id": "rm-1", "action": "create", "status": "in_flight", "date": "2026-10-26", "description": "x", "resolved": false, "recorded_at": "2026-10-26T12:00:00Z"}],
		"carries": [],
		"review": {"reviewed": false},
		"plans": [],
		"approvals": []
	}`)
	env := map[string]string{}
	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	before, err := os.ReadFile(cfg.StatePath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	beforeBytes := string(before)
	cand, err := buildMigrationCandidate(cfg, store.State(), []string{"2026-10-26"})
	if err != nil {
		t.Fatal(err)
	}
	if cand.Ready {
		t.Fatal("duplicate local ids must keep the candidate blocked")
	}
	ambiguous := 0
	for _, c := range cand.Conflicts {
		if c.Code == "ambiguous_local_id" {
			ambiguous++
		}
	}
	if ambiguous != 1 {
		t.Fatalf("expected one ambiguous_local_id conflict: %+v", cand.Conflicts)
	}
	if day := cand.Days["2026-10-26"]; day == nil || len(day.Confirmed) != 0 {
		t.Fatalf("neither duplicate row may be constructed or aliased: %+v", day)
	}
	if len(cand.Bindings) != 0 {
		t.Fatalf("no mapping may be guessed from the ambiguous rows: %+v", cand.Bindings)
	}
	_ = dir

	s := NewSession(cfg, env, store)
	preview, err := s.migrationPreview(Request{ID: "1", Op: "migration_preview", Dates: []string{"2026-10-26"}})
	if err != nil {
		t.Fatalf("preview must return a bounded preview, not an error: %v", err)
	}
	if preview.Ready {
		t.Fatal("an ambiguous source must never preview as ready")
	}
	_, applyErr := s.migrationApply(Request{ID: "2", Op: "migration_apply", Dates: []string{"2026-10-26"}, MigrationID: preview.MigrationID})
	if applyErr == nil {
		t.Fatalf("an ambiguous source must never activate: %+v", preview)
	}
	appErr, ok := applyErr.(*AppError)
	if !ok || appErr.Code != CodeMigrationConflict {
		t.Fatalf("apply must fail with migration_conflict: %v", applyErr)
	}
	after, err := os.ReadFile(cfg.StatePath)
	if err == nil && string(after) != beforeBytes {
		t.Fatal("durable target bytes changed for a conflicted candidate")
	}
	if !os.IsNotExist(err) && err != nil {
		t.Fatal(err)
	}
}

// F2: legitimate cross-date ID collisions stay distinct: the first date keeps
// the legacy ID, the second gets a deterministic date-scoped alias, and both
// mapping and outcome references translate per date. The audit keeps the
// original ID and date for every row.
func TestMigrationAliasesCollidingIDsPerDate(t *testing.T) {
	cfg, legacyRoot, _ := migrationFixture(t)
	mappingA := `{"local_id": "legacy-shared", "remote_id": "rs-a", "date": "2026-10-27", "start_time": "09:00", "end_time": "10:00", "description": "Alias work", "last_observed": {"remote_id": "rs-a", "date": "2026-10-27", "start_time": "09:00", "end_time": "10:00", "description": "Alias work", "observed_at": "2026-10-27T12:00:00Z", "content_observed": true}}`
	mappingB := `{"local_id": "legacy-shared", "remote_id": "rs-b", "date": "2026-10-28", "start_time": "09:00", "end_time": "10:00", "description": "Alias work", "last_observed": {"remote_id": "rs-b", "date": "2026-10-28", "start_time": "09:00", "end_time": "10:00", "description": "Alias work", "observed_at": "2026-10-28T12:00:00Z", "content_observed": true}}`
	intervalA := `{"id": "legacy-shared", "date": "2026-10-27", "start_time": "09:00", "end_time": "10:00", "project": "OLD-1", "description": "Alias work", "confirmed_at": "2026-10-27T11:00:00Z", "revision": 1, "kind": "work", "manual": true}`
	intervalB := `{"id": "legacy-shared", "date": "2026-10-28", "start_time": "09:00", "end_time": "10:00", "project": "OLD-1", "description": "Alias work", "confirmed_at": "2026-10-28T11:00:00Z", "revision": 1, "kind": "work", "manual": true}`
	writeLegacy(t, legacyRoot, "2026-10-27", `{"schema_version": 1, "date": "2026-10-27", "revision": 1,
		"updated_at": "2026-10-27T12:00:00Z", "evidence": [], "remote_mappings": [`+mappingA+`],
		"confirmed": [`+intervalA+`],
		"outcomes": [{"operation_key": "ka", "local_id": "legacy-shared", "remote_id": "rs-a", "action": "create", "status": "uncertain", "date": "2026-10-27", "description": "x", "resolved": false, "recorded_at": "2026-10-27T12:30:00Z"}],
		"carries": [], "review": {"reviewed": false}, "plans": [], "approvals": []}`)
	writeLegacy(t, legacyRoot, "2026-10-28", `{"schema_version": 1, "date": "2026-10-28", "revision": 1,
		"updated_at": "2026-10-28T12:00:00Z", "evidence": [], "remote_mappings": [`+mappingB+`],
		"confirmed": [`+intervalB+`],
		"outcomes": [{"operation_key": "kb", "local_id": "legacy-shared", "remote_id": "rs-b", "action": "create", "status": "uncertain", "date": "2026-10-28", "description": "x", "resolved": false, "recorded_at": "2026-10-28T12:30:00Z"}],
		"carries": [], "review": {"reviewed": false}, "plans": [], "approvals": []}`)
	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	st := store.State()
	cand, err := buildMigrationCandidate(cfg, st, []string{"2026-10-27", "2026-10-28"})
	if err != nil {
		t.Fatal(err)
	}
	if !cand.Ready {
		t.Fatalf("cross-date id collisions must alias, not conflict: %+v", cand.Conflicts)
	}
	if err := store.CommitState(mergeMigrationCandidate(store.State(), cand)); err != nil {
		t.Fatal(err)
	}
	st = store.State()
	idA := st.Days["2026-10-27"].Confirmed[0].ID
	idB := st.Days["2026-10-28"].Confirmed[0].ID
	if idA != "legacy-shared" {
		t.Fatalf("the first date keeps the legacy id, got %s", idA)
	}
	if idB != "iv-20261028-1" {
		t.Fatalf("the second date gets a deterministic date-scoped alias, got %s", idB)
	}
	if len(st.Remote.Bindings) != 2 {
		t.Fatalf("both remote identities must be preserved: %+v", st.Remote.Bindings)
	}
	byRemote := map[string]string{}
	for _, b := range st.Remote.Bindings {
		byRemote[b.RemoteID] = b.LocalID
	}
	if byRemote["rs-a"] != idA || byRemote["rs-b"] != idB {
		t.Fatalf("mappings must translate per date, not collapse: %+v", byRemote)
	}
	if len(st.Remote.Outcomes) != 2 || st.Remote.Outcomes[0].LocalID != idA || st.Remote.Outcomes[1].LocalID != idB {
		t.Fatalf("outcome references must translate consistently: %+v", st.Remote.Outcomes)
	}
	rec := st.Migration.Receipts[len(st.Migration.Receipts)-1]
	if len(rec.IDCrosswalk) != 1 || rec.IDCrosswalk[0] != (LegacyIDCrosswalk{Date: "2026-10-28", LegacyID: "legacy-shared", NewID: idB}) {
		t.Fatalf("the crosswalk must be date-scoped: %+v", rec.IDCrosswalk)
	}
	if len(rec.LegacyIntervals) != 2 || rec.LegacyIntervals[0].Date != "2026-10-27" ||
		rec.LegacyIntervals[1].Date != "2026-10-28" ||
		rec.LegacyIntervals[0].LegacyID != "legacy-shared" || rec.LegacyIntervals[1].LegacyID != "legacy-shared" {
		t.Fatalf("audit must keep original ids and dates per row: %+v", rec.LegacyIntervals)
	}
}

// F3: audit capacity bounds are explicit. A selection exceeding a preserved
// audit bound is rejected with a capacity conflict before activation; rows are
// never silently truncated, and target bytes do not change.
func TestMigrationCapacityRejectsInsteadOfTruncating(t *testing.T) {
	cfg, legacyRoot, _ := migrationFixture(t)
	var evidence strings.Builder
	for i := 0; i < 2050; i++ {
		evidence.WriteString(fmt.Sprintf(`{"id": "e-%d", "source": "github", "timestamp": "2026-10-29T08:00:00Z", "title": "Historical provenance pointer %d", "ref": "o/r%d", "observed_at": "2026-10-29T08:01:00Z"},`, i, i, i))
	}
	rows := strings.TrimSuffix(evidence.String(), ",")
	writeLegacy(t, legacyRoot, "2026-10-29", `{
		"schema_version": 1,
		"date": "2026-10-29",
		"revision": 1,
		"updated_at": "2026-10-29T12:00:00Z",
		"evidence": [`+rows+`],
		"confirmed": [{"id": "big-1", "date": "2026-10-29", "start_time": "09:00", "end_time": "10:00", "project": "OLD-1", "description": "Heavy provenance day", "confirmed_at": "2026-10-29T11:00:00Z", "revision": 1, "kind": "work", "manual": true}],
		"remote_mappings": [],
		"outcomes": [],
		"carries": [],
		"review": {"reviewed": false},
		"plans": [],
		"approvals": []
	}`)
	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	before, err := os.ReadFile(cfg.StatePath)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	beforeBytes := string(before)
	cand, err := buildMigrationCandidate(cfg, store.State(), []string{"2026-10-29"})
	if err != nil {
		t.Fatal(err)
	}
	if cand.Ready {
		t.Fatal("an over-bound selection must stay blocked")
	}

	// Within the bound, nothing is truncated: 2000 preserved pointers is one
	// coherent audit, not a silent slice.
	evidence.Reset()
	for i := 0; i < maxMigrationEvidenceRows; i++ {
		evidence.WriteString(fmt.Sprintf(`{"id": "e-%d", "source": "github", "title": "Pointer %d", "ref": "o/r%d", "timestamp": "2026-10-29T08:00:00Z", "observed_at": "2026-10-29T08:01:00Z"},`, i, i, i))
	}
	rows2 := strings.TrimSuffix(evidence.String(), ",")
	writeLegacy(t, legacyRoot, "2026-10-29", `{
		"schema_version": 1, "date": "2026-10-29", "revision": 1,
		"updated_at": "2026-10-29T12:00:00Z", "evidence": [`+rows2+`],
		"confirmed": [{"id": "big-1", "date": "2026-10-29", "start_time": "09:00", "end_time": "10:00", "project": "OLD-1", "description": "Heavy provenance day", "confirmed_at": "2026-10-29T11:00:00Z", "revision": 1, "kind": "work", "manual": true}],
		"remote_mappings": [], "outcomes": [], "carries": [],
		"review": {"reviewed": false}, "plans": [], "approvals": []
	}`)
	cand, err = buildMigrationCandidate(cfg, store.State(), []string{"2026-10-29"})
	if err != nil {
		t.Fatal(err)
	}
	if !cand.Ready {
		t.Fatalf("a bounded selection must stay ready: %+v", cand.Conflicts)
	}
	if len(cand.Receipt.LegacyEvidence) != maxMigrationEvidenceRows || cand.Receipt.LegacyEvidence[0].ID != "e-0" ||
		cand.Receipt.LegacyEvidence[maxMigrationEvidenceRows-1].ID != fmt.Sprintf("e-%d", maxMigrationEvidenceRows-1) {
		t.Fatalf("the bounded audit must be complete: %d rows", len(cand.Receipt.LegacyEvidence))
	}

	// An over-bound selection refuses activation and changes no target bytes.
	evidence.Reset()
	for i := 0; i < maxMigrationEvidenceRows+1; i++ {
		evidence.WriteString(fmt.Sprintf(`{"id": "e-%d", "source": "github", "title": "Pointer %d", "ref": "o/r%d", "timestamp": "2026-10-29T08:00:00Z", "observed_at": "2026-10-29T08:01:00Z"},`, i, i, i))
	}
	rows3 := strings.TrimSuffix(evidence.String(), ",")
	writeLegacy(t, legacyRoot, "2026-10-29", `{
		"schema_version": 1, "date": "2026-10-29", "revision": 1,
		"updated_at": "2026-10-29T12:00:00Z", "evidence": [`+rows3+`],
		"confirmed": [{"id": "big-1", "date": "2026-10-29", "start_time": "09:00", "end_time": "10:00", "project": "OLD-1", "description": "Heavy provenance day", "confirmed_at": "2026-10-29T11:00:00Z", "revision": 1, "kind": "work", "manual": true}],
		"remote_mappings": [], "outcomes": [], "carries": [],
		"review": {"reviewed": false}, "plans": [], "approvals": []
	}`)
	cand, err = buildMigrationCandidate(cfg, store.State(), []string{"2026-10-29"})
	if err != nil {
		t.Fatal(err)
	}
	if cand.Ready {
		t.Fatal("an over-bound selection must stay blocked")
	}
	capped := false
	for _, c := range cand.Conflicts {
		if c.Code == "migration_capacity" {
			capped = true
		}
	}
	if !capped {
		t.Fatalf("the capacity bound must be an explicit conflict: %+v", cand.Conflicts)
	}
	s := NewSession(cfg, map[string]string{}, store)
	preview, err := s.migrationPreview(Request{ID: "1", Op: "migration_preview", Dates: []string{"2026-10-29"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.migrationApply(Request{ID: "2", Op: "migration_apply", Dates: []string{"2026-10-29"}, MigrationID: preview.MigrationID}); !isErrorCode(err, CodeMigrationConflict) {
		t.Fatalf("an over-bound candidate must refuse activation: %v", err)
	}
	after, err := os.ReadFile(cfg.StatePath)
	if err == nil && string(after) != beforeBytes {
		t.Fatal("durable target bytes changed for a capacity-rejected candidate")
	}
	if !os.IsNotExist(err) && err != nil {
		t.Fatal(err)
	}
}

// F3: review history is preserved per date with authored reasons, exclusions and
// attendance decisions, restart-safe and inert. Raw collected documents are
// never imported; only their count is recorded.
func TestMigrationPreservesPerDateReviewAudit(t *testing.T) {
	cfg, legacyRoot, _ := migrationFixture(t)
	writeLegacy(t, legacyRoot, "2026-10-21", `{
		"schema_version": 1,
		"date": "2026-10-21",
		"revision": 1,
		"updated_at": "2026-10-21T18:00:00Z",
		"evidence": [{"id": "ev-9", "source": "github", "timestamp": "2026-10-21T09:00:00Z", "title": "Second day commit", "ref": "o/r2", "observed_at": "2026-10-21T18:00:00Z"}],
		"confirmed": [{"id": "legacy-9", "date": "2026-10-21", "start_time": "11:00", "end_time": "12:00", "project": "OLD-1", "description": "Second day session", "confirmed_at": "2026-10-21T17:00:00Z", "revision": 1, "kind": "work", "manual": true}],
		"remote_mappings": [],
		"outcomes": [],
		"carries": [],
		"review": {"reviewed": true, "empty": false, "reviewed_at": "2026-10-21T17:30:00Z",
			"excluded_evidence": [{"evidence_id": "ev-9", "reason": "not billable work", "excluded_at": "2026-10-21T16:00:00Z"}],
			"attendance": [{"evidence_id": "ev-9", "attended": true, "decided_at": "2026-10-21T16:30:00Z"}],
			"acknowledged_sources": ["calendar"]},
		"collected_evidence": [{"raw": "PROVIDER-DOCUMENT-NOT-IMPORTED"}, {"raw": "SECOND-PROVIDER-DOCUMENT-NOT-IMPORTED"}],
		"plans": [],
		"approvals": []
	}`)
	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	st := store.State()
	cand, err := buildMigrationCandidate(cfg, st, []string{"2026-10-20", "2026-10-21"})
	if err != nil {
		t.Fatal(err)
	}
	if !cand.Ready {
		t.Fatalf("both reviewable days must migrate: %+v", cand.Conflicts)
	}
	if len(cand.Receipt.LegacyReviews) != 2 {
		t.Fatalf("a single last-day pointer is not multi-day audit: %+v", cand.Receipt.LegacyReviews)
	}
	if err := store.CommitState(mergeMigrationCandidate(st, cand)); err != nil {
		t.Fatal(err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	rec := reopened.State().Migration.Receipts[len(reopened.State().Migration.Receipts)-1]
	if rec.LegacyReview != nil {
		t.Fatal("new receipts must not use the single-pointer review shape")
	}
	if len(rec.LegacyReviews) != 2 {
		t.Fatalf("per-date reviews must survive restart: %+v", rec.LegacyReviews)
	}
	first, second := rec.LegacyReviews[0], rec.LegacyReviews[1]
	if first.Date != "2026-10-20" || second.Date != "2026-10-21" {
		t.Fatalf("review audits must be date-associated: %+v", rec.LegacyReviews)
	}
	if len(first.ExcludedRows) != 1 || first.ExcludedRows[0].Reason != "not work" ||
		first.ExcludedRows[0].EvidenceID != "ev-2" {
		t.Fatalf("authored first-day exclusion must be preserved: %+v", first)
	}
	if len(second.ExcludedRows) != 1 || second.ExcludedRows[0].Reason != "not billable work" ||
		second.ExcludedRows[0].ExcludedAt != "2026-10-21T16:00:00Z" {
		t.Fatalf("authored second-day exclusion must be preserved: %+v", second)
	}
	if len(second.AttendanceRows) != 1 || second.AttendanceRows[0].EvidenceID != "ev-9" ||
		!second.AttendanceRows[0].Attended || second.AttendanceRows[0].DecidedAt != "2026-10-21T16:30:00Z" {
		t.Fatalf("attendance decisions must be preserved: %+v", second)
	}
	if second.ReviewedAt != "2026-10-21T17:30:00Z" || second.Acknowledged[0] != "calendar" {
		t.Fatalf("second-day review outcome must be preserved: %+v", second)
	}
	if rec.LegacyCollectedEvidence != 3 {
		t.Fatalf("legacy collected evidence is counted, never imported: %+v", rec.LegacyCollectedEvidence)
	}
	// The unimported raw documents left no evidence collector state behind.
	if len(reopened.State().Evidence) != 0 {
		t.Fatal("legacy raw provider documents must not enter evidence state")
	}
}

func isErrorCode(err error, code string) bool {
	app, ok := err.(*AppError)
	return ok && app.Code == code
}

func TestMigrationRejectsChangedSourceAndOccupiedDestination(t *testing.T) {
	cfg, legacyRoot, _ := migrationFixture(t)
	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	cand, err := buildMigrationCandidate(cfg, store.State(), []string{"2026-10-20"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitState(mergeMigrationCandidate(store.State(), cand)); err != nil {
		t.Fatal(err)
	}

	// Changed source bytes on an already-imported date are a conflict, never an
	// overwrite.
	writeLegacy(t, legacyRoot, "2026-10-20", strings.Replace(legacyDayOne, "Synthetic legacy work", "Changed legacy work", 1))
	changed, err := buildMigrationCandidate(cfg, store.State(), []string{"2026-10-20"})
	if err != nil {
		t.Fatal(err)
	}
	if changed.Ready || changed.NoChange {
		t.Fatal("changed source must be rejected")
	}
	if !hasConflict(changed.Conflicts, "source_changed") {
		t.Fatalf("expected source_changed: %+v", changed.Conflicts)
	}

	// A different store that already has work on the destination date is a
	// conflict too.
	fresh, err := OpenStore(filepath.Join(t.TempDir(), "other-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fresh.Close() }()
	occupied := newState()
	occupied.Days["2026-10-20"] = &Day{Date: "2026-10-20", Timezone: "UTC", Revision: 1, NextInterval: 2,
		Confirmed: []Interval{{ID: "iv-20261020-1", Date: "2026-10-20", Start: "09:00", End: "10:00", Project: "P1", Description: "existing"}}}
	if err := fresh.CommitState(occupied); err != nil {
		t.Fatal(err)
	}
	writeLegacy(t, legacyRoot, "2026-10-20", legacyDayOne)
	blocked, err := buildMigrationCandidate(cfg, fresh.State(), []string{"2026-10-20"})
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Ready || !hasConflict(blocked.Conflicts, "destination_occupied") {
		t.Fatalf("expected destination_occupied: %+v", blocked.Conflicts)
	}
}

// TestMigrationRejectsOverlapButNotAnUnmappedProject: overlap stays a
// conflict; since ADR 0012 a legacy project needs no configured target and
// survives only in the receipt's audit.
func TestMigrationRejectsOverlapButNotAnUnmappedProject(t *testing.T) {
	cfg, legacyRoot, _ := migrationFixture(t)
	body := `{"schema_version":1,"date":"2026-10-20","revision":1,"updated_at":"2026-10-20T18:00:00Z",
	  "confirmed":[
	    {"id":"a","date":"2026-10-20","start_time":"09:00","end_time":"10:00","project":"OLD-1","description":"one"},
	    {"id":"b","date":"2026-10-20","start_time":"09:30","end_time":"10:30","project":"OLD-1","description":"two"}
	  ]}`
	writeLegacy(t, legacyRoot, "2026-10-20", body)
	st := newState()
	cand, err := buildMigrationCandidate(cfg, st, []string{"2026-10-20"})
	if err != nil {
		t.Fatal(err)
	}
	if cand.Ready || !hasConflict(cand.Conflicts, "overlap") {
		t.Fatalf("expected overlap conflict: %+v", cand.Conflicts)
	}

	unmapped := strings.Replace(body, `"OLD-1"`, `"NOPE"`, 1)
	unmapped = strings.Replace(unmapped, `"start_time":"09:30","end_time":"10:30"`, `"start_time":"11:00","end_time":"12:00"`, 1)
	writeLegacy(t, legacyRoot, "2026-10-20", unmapped)
	cand, err = buildMigrationCandidate(cfg, newState(), []string{"2026-10-20"})
	if err != nil {
		t.Fatal(err)
	}
	if !cand.Ready {
		t.Fatalf("an unmapped legacy project must not conflict: %+v", cand.Conflicts)
	}
	for _, iv := range cand.Days["2026-10-20"].Confirmed {
		if iv.Project != "" {
			t.Fatalf("migrated interval carries a project: %+v", iv)
		}
	}
	if len(cand.Receipt.LegacyIntervals) == 0 || cand.Receipt.LegacyIntervals[0].LegacyProject != "NOPE" {
		t.Fatalf("legacy project audit = %+v, want NOPE kept", cand.Receipt.LegacyIntervals)
	}
}

func TestSchemaOneStateUpgradesInMemoryOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	schemaOne := `{"schema":1,"days":{"2026-10-20":{"date":"2026-10-20","timezone":"UTC","revision":2,"next_interval":2,"confirmed":[{"id":"iv-20261020-1","date":"2026-10-20","start":"09:00","end":"10:00","project":"P1","description":"kept"}]}},"evidence":{"2026-10-20":{"attempts":[{"source":"github","collection_id":"c1","attempted_at":"2026-10-20T18:00:00Z","status":"complete","acknowledged":false}]}}}`
	if err := os.WriteFile(path, []byte(schemaOne), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.DurableSchema(); got != legacyStateSchema {
		t.Fatalf("durable schema = %d, want 1 before any write", got)
	}
	if store.State().Schema != stateSchema {
		t.Fatalf("in-memory schema = %d", store.State().Schema)
	}
	if got := len(store.State().Days["2026-10-20"].Confirmed); got != 1 {
		t.Fatalf("work lost: %d", got)
	}
	// The file is untouched by loading.
	raw, _ := os.ReadFile(path)
	var onDisk map[string]any
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk["schema"] != float64(1) {
		t.Fatal("startup rewrote the schema-1 file")
	}
	// A durable write activates schema 2.
	day := store.State().Days["2026-10-20"].clone()
	if err := store.Commit(day.Revision, day); err != nil {
		t.Fatal(err)
	}
	if store.DurableSchema() != stateSchema {
		t.Fatal("schema 2 was not activated by a successful save")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if reopened.DurableSchema() != stateSchema || reopened.State().Schema != stateSchema {
		t.Fatal("schema 2 did not survive restart")
	}
	if len(reopened.State().Evidence["2026-10-20"].Attempts) != 1 {
		t.Fatal("R2 evidence lost through the upgrade")
	}
}

func hasConflict(conflicts []MigrationConflict, code string) bool {
	for _, c := range conflicts {
		if c.Code == code {
			return true
		}
	}
	return false
}

func TestMigrationPreviewApplyThroughSession(t *testing.T) {
	cfg, _, _ := migrationFixture(t)
	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	s := NewSession(cfg, map[string]string{}, store)

	preview, err := s.migrationPreview(Request{ID: "1", Op: "migration_preview", Dates: []string{"2026-10-20"}})
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Ready || preview.MigrationID == "" {
		t.Fatalf("preview not ready: %+v", preview)
	}
	if _, err := s.migrationApply(Request{ID: "2", Op: "migration_apply", Dates: []string{"2026-10-20"}, MigrationID: "bogus"}); err == nil {
		t.Fatal("an unpreviewed migration id must be rejected")
	}
	applied, err := s.migrationApply(Request{ID: "3", Op: "migration_apply", Dates: []string{"2026-10-20"}, MigrationID: preview.MigrationID})
	if err != nil {
		t.Fatal(err)
	}
	if applied.Status != MigrationApplied || applied.ReceiptID != preview.MigrationID {
		t.Fatalf("apply receipt: %+v", applied)
	}
	if applied.Unresolved != 1 {
		t.Fatalf("unresolved outcome must stay visible: %+v", applied)
	}

	// A repeated preview + apply after the same process is a no-change result.
	again, err := s.migrationPreview(Request{ID: "4", Op: "migration_preview", Dates: []string{"2026-10-20"}})
	if err != nil {
		t.Fatal(err)
	}
	if !again.NoChange {
		t.Fatalf("repeat preview must be no_change: %+v", again)
	}
	noop, err := s.migrationApply(Request{ID: "5", Op: "migration_apply", Dates: []string{"2026-10-20"}, MigrationID: again.MigrationID})
	if err != nil {
		t.Fatal(err)
	}
	if noop.Status != MigrationNoChange || noop.ReceiptID != preview.MigrationID {
		t.Fatalf("repeat apply must not duplicate: %+v", noop)
	}
	if len(store.State().Migration.Receipts) != 1 {
		t.Fatal("repeat apply duplicated a receipt")
	}

	// No opened day is allowed during activation.
	if _, err := s.openDay(Request{ID: "6", Op: "open_day", Day: "2026-10-20"}); err != nil {
		t.Fatal(err)
	}
	writeLegacy(t, cfg.Migration.LegacyRoot, "2026-10-21", `{"schema_version":1,"date":"2026-10-21","revision":1,"updated_at":"2026-10-21T18:00:00Z","confirmed":[{"id":"x1","date":"2026-10-21","start_time":"09:00","end_time":"10:00","project":"OLD-1","description":"next day"}]}`)
	third, err := s.migrationPreview(Request{ID: "7", Op: "migration_preview", Dates: []string{"2026-10-21"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.migrationApply(Request{ID: "8", Op: "migration_apply", Dates: []string{"2026-10-21"}, MigrationID: third.MigrationID}); err == nil {
		t.Fatal("activation with an opened day must be rejected")
	}
}

// F3 continuation: a legacy approval's own binding history (record_revision,
// remote_observed_at) survives migration and restart as inert audit with its
// exact values, distinct from the linked plan's values and never inferred from
// them. The audit stays unusable history.
const legacyDayApprovalBinding = `{
  "schema_version": 1,
  "date": "2026-11-10",
  "revision": 3,
  "updated_at": "2026-11-10T18:00:00Z",
  "confirmed": [
    {"id": "legacy-b1", "date": "2026-11-10", "start_time": "09:00", "end_time": "10:00", "project": "OLD-1", "description": "Binding audit day", "attendance": false, "confirmed_at": "2026-11-10T17:00:00Z", "revision": 3, "kind": "work", "manual": true, "classification_id": "0f8fad5b-d9cb-469f-a165-70867728950e"}
  ],
  "plans": [{"plan_id": "plan-binding", "date": "2026-11-10", "local_fingerprint": "fp-binding", "record_revision": 16, "remote_observed_at": "2026-11-10T09:00:00Z", "remote_complete": true, "operations": [{"local_id": "legacy-b1", "kind": "create"}], "created_at": "2026-11-10T08:00:00Z"}],
  "approvals": [{"approval_id": "plan-binding", "plan_id": "plan-binding", "date": "2026-11-10", "record_revision": 17, "remote_observed_at": "2026-11-10T19:10:00Z", "approved_at": "2026-11-10T19:00:00Z"}]
}`

func TestMigrationApprovalBindingAuditFields(t *testing.T) {
	cfg, legacyRoot, _ := migrationFixture(t)
	writeLegacy(t, legacyRoot, "2026-11-10", legacyDayApprovalBinding)
	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	cand, err := buildMigrationCandidate(cfg, store.State(), []string{"2026-11-10"})
	if err != nil {
		t.Fatal(err)
	}
	if !cand.Ready || len(cand.Conflicts) > 0 {
		t.Fatalf("candidate must be ready: %+v", cand.Conflicts)
	}
	if err := store.CommitState(mergeMigrationCandidate(store.State(), cand)); err != nil {
		t.Fatal(err)
	}
	assertAudit := func(st *State) {
		t.Helper()
		for _, rec := range st.Migration.Receipts {
			for _, d := range rec.Dates {
				if d != "2026-11-10" {
					continue
				}
				if len(rec.LegacyApprovalsAudit) != 1 {
					t.Fatalf("approval audit missing: %+v", rec.LegacyApprovalsAudit)
				}
				a := rec.LegacyApprovalsAudit[0]
				if a.RecordRevision != 17 || a.RemoteObservedAt != "2026-11-10T19:10:00Z" {
					t.Fatalf("the approval's own historical binding fields must survive exactly: %+v", a)
				}
				if a.ApprovedAt != "2026-11-10T19:00:00Z" || !a.Inert || a.Usable {
					t.Fatalf("approval must stay inert unusable history: %+v", a)
				}
				if len(rec.LegacyPlanAudits) != 1 || rec.LegacyPlanAudits[0].RecordRevision != 16 {
					t.Fatalf("the plan keeps its own distinct revision: %+v", rec.LegacyPlanAudits)
				}
				return
			}
		}
		t.Fatal("no receipt preserved the migrated date")
	}
	assertAudit(store.State())

	// The fields live in the durable state, not only the in-memory candidate:
	// they survive a restart verbatim.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	assertAudit(reopened.State())
}

// F9: every successful no-change apply revalidates the fresh candidate first.
// A stale source or a durable target changed by a valid transaction since the
// preview is a structured failure, never a false exact-preview success.
func TestMigrationApplyRevalidatesBeforeNoChange(t *testing.T) {
	cfg, legacyRoot, _ := migrationFixture(t)
	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	s := NewSession(cfg, map[string]string{}, store)

	first, err := s.migrationPreview(Request{ID: "1", Op: "migration_preview", Dates: []string{"2026-10-20"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.migrationApply(Request{ID: "2", Op: "migration_apply", Dates: []string{"2026-10-20"}, MigrationID: first.MigrationID}); err != nil {
		t.Fatal(err)
	}

	// A repeated no-change preview is ordinary idempotence.
	repeat, err := s.migrationPreview(Request{ID: "3", Op: "migration_preview", Dates: []string{"2026-10-20"}})
	if err != nil {
		t.Fatal(err)
	}
	if !repeat.NoChange {
		t.Fatalf("identical repeat must be no_change: %+v", repeat)
	}
	noop, err := s.migrationApply(Request{ID: "4", Op: "migration_apply", Dates: []string{"2026-10-20"}, MigrationID: repeat.MigrationID})
	if err != nil || noop.Status != MigrationNoChange || noop.ReceiptID != first.MigrationID {
		t.Fatalf("identical repeat apply must stay no_change: %+v (%v)", noop, err)
	}

	// Change the durable target through one valid protocol transaction while a
	// newer no-change preview is still pending.
	revalidation, err := s.migrationPreview(Request{ID: "5", Op: "migration_preview", Dates: []string{"2026-10-20"}})
	if err != nil {
		t.Fatal(err)
	}
	if !revalidation.NoChange {
		t.Fatalf("still a no-change selection: %+v", revalidation)
	}
	writeLegacy(t, legacyRoot, "2026-11-10", legacyDayApprovalBinding)
	other := NewSession(cfg, map[string]string{}, store)
	otherPreview, err := other.migrationPreview(Request{ID: "o1", Op: "migration_preview", Dates: []string{"2026-11-10"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.migrationApply(Request{ID: "o2", Op: "migration_apply", Dates: []string{"2026-11-10"}, MigrationID: otherPreview.MigrationID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.migrationApply(Request{ID: "6", Op: "migration_apply", Dates: []string{"2026-10-20"}, MigrationID: revalidation.MigrationID}); !isErrorCode(err, CodeMigrationStale) {
		t.Fatalf("the old pending preview must not succeed against changed target state: %v", err)
	}

	// Change the source bytes of the imported date: the pending preview must
	// not restate a success over input it did not revalidate.
	staleSource, err := s.migrationPreview(Request{ID: "7", Op: "migration_preview", Dates: []string{"2026-10-20"}})
	if err != nil {
		t.Fatal(err)
	}
	if !staleSource.NoChange {
		t.Fatalf("pre-change repeat must be no_change: %+v", staleSource)
	}
	writeLegacy(t, legacyRoot, "2026-10-20", strings.Replace(legacyDayOne, "Synthetic legacy work", "Changed legacy work", 1))
	if _, err := s.migrationApply(Request{ID: "8", Op: "migration_apply", Dates: []string{"2026-10-20"}, MigrationID: staleSource.MigrationID}); !isErrorCode(err, CodeMigrationStale) {
		t.Fatalf("stale source under a pending no-change preview must fail: %v", err)
	}
	disclosed, err := s.migrationPreview(Request{ID: "9", Op: "migration_preview", Dates: []string{"2026-10-20"}})
	if err != nil {
		t.Fatal(err)
	}
	if disclosed.NoChange || !hasConflict(disclosed.Conflicts, "source_changed") {
		t.Fatalf("a fresh preview must disclose the source change: %+v", disclosed)
	}

	// Work and receipts remain intact: two applies only, nothing overwritten.
	if len(store.State().Migration.Receipts) != 2 {
		t.Fatalf("failed applies must not alter receipts: %+v", store.State().Migration.Receipts)
	}
	day := store.State().Days["2026-10-20"]
	if day == nil || len(day.Confirmed) != 2 || day.Confirmed[0].Description != "Synthetic legacy work" {
		t.Fatalf("target bytes must remain intact: %+v", day)
	}
	writeLegacy(t, legacyRoot, "2026-10-20", legacyDayOne)
}

// F9: the declared legacy timezone is part of the per-date repeat identity —
// the same bytes under a different declaration are an interpretation conflict,
// never an already-imported no-change result — while a repeated subset of a
// larger imported batch with matching bytes and timezone stays a no-change
// result.
func TestMigrationRepeatIdentityCoversTimezoneAndSubsets(t *testing.T) {
	cfg, legacyRoot, _ := migrationFixture(t)
	writeLegacy(t, legacyRoot, "2026-11-03", `{"schema_version":1,"date":"2026-11-03","revision":1,"updated_at":"2026-11-03T18:00:00Z","confirmed":[{"id":"legacy-m1","date":"2026-11-03","start_time":"09:00","end_time":"10:00","project":"OLD-1","description":"batch one","attendance":false,"confirmed_at":"2026-11-03T17:00:00Z","revision":1,"kind":"work","manual":true}]}`)
	writeLegacy(t, legacyRoot, "2026-11-04", `{"schema_version":1,"date":"2026-11-04","revision":1,"updated_at":"2026-11-04T18:00:00Z","confirmed":[{"id":"legacy-m2","date":"2026-11-04","start_time":"09:00","end_time":"10:00","project":"OLD-1","description":"batch two","attendance":false,"confirmed_at":"2026-11-04T17:00:00Z","revision":1,"kind":"work","manual":true}]}`)
	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	s := NewSession(cfg, map[string]string{}, store)

	batch, err := s.migrationPreview(Request{ID: "1", Op: "migration_preview", Dates: []string{"2026-11-03", "2026-11-04"}})
	if err != nil {
		t.Fatal(err)
	}
	if !batch.Ready || batch.NoChange {
		t.Fatalf("two-date batch must be ready: %+v", batch)
	}
	if _, err := s.migrationApply(Request{ID: "2", Op: "migration_apply", Dates: []string{"2026-11-03", "2026-11-04"}, MigrationID: batch.MigrationID}); err != nil {
		t.Fatal(err)
	}

	// A repeated single-date subset of the imported batch is a legitimate
	// no-change result, including after a restart.
	subsetPreview := func() MigrationPreview {
		t.Helper()
		p, err := s.migrationPreview(Request{ID: "3", Op: "migration_preview", Dates: []string{"2026-11-04"}})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	subset := subsetPreview()
	if !subset.NoChange || subset.ExistingReceipt != batch.MigrationID {
		t.Fatalf("repeated subset must be no_change evidence of the covering receipt: %+v", subset)
	}
	if _, err := s.migrationApply(Request{ID: "4", Op: "migration_apply", Dates: []string{"2026-11-04"}, MigrationID: subset.MigrationID}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	s = NewSession(cfg, map[string]string{}, reopened)
	if again := subsetPreview(); !again.NoChange {
		t.Fatalf("repeated subset must stay no_change after restart: %+v", again)
	}

	// The same source bytes under a different declared legacy timezone are an
	// interpretation conflict: no no-change success, and the recorded
	// day/receipt timezone is preserved.
	ampsterdamState := reopened.State()
	cfgUTC := *cfg
	cfgUTC.Migration = &MigrationConfig{
		Enabled: true, LegacyRoot: cfg.Migration.LegacyRoot, LegacyTimezone: "UTC",
		ClassificationMap: cfg.Migration.ClassificationMap,
	}
	sUTC := NewSession(&cfgUTC, map[string]string{}, reopened)
	tzPreview, err := sUTC.migrationPreview(Request{ID: "5", Op: "migration_preview", Dates: []string{"2026-11-04"}})
	if err != nil {
		t.Fatal(err)
	}
	if tzPreview.NoChange || !hasConflict(tzPreview.Conflicts, "interpretation_conflict") {
		t.Fatalf("a different declared timezone must be an interpretation conflict: %+v", tzPreview)
	}
	if _, err := sUTC.migrationApply(Request{ID: "6", Op: "migration_apply", Dates: []string{"2026-11-04"}, MigrationID: tzPreview.MigrationID}); !isErrorCode(err, CodeMigrationConflict) {
		t.Fatalf("interpretation-conflicting apply must fail: %v", err)
	}
	day := ampsterdamState.Days["2026-11-04"]
	if day == nil || day.Timezone != "Europe/Amsterdam" ||
		len(day.Confirmed) != 1 || day.Confirmed[0].Start != "09:00" {
		t.Fatalf("recorded interpretation must stay intact: %+v", day)
	}
	for _, rec := range ampsterdamState.Migration.Receipts {
		if rec.Timezone != "Europe/Amsterdam" {
			t.Fatalf("receipt timezone must stay the originally declared one: %+v", rec)
		}
	}
	if len(ampsterdamState.Migration.Receipts) != 1 {
		t.Fatalf("a conflicted apply must not add receipts: %+v", ampsterdamState.Migration.Receipts)
	}

	// The original declaration is still the governing one.
	if back := subsetPreview(); !back.NoChange {
		t.Fatalf("the original timezone repeat stays no_change: %+v", back)
	}
}

func TestSchemaTwoApprovedPlanIsRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	body := `{"schema":2,"days":{},"plans":{"p1":{"plan_id":"p1","date":"2026-10-20","state":"ready","approved":true,"approval_available":false,"operations":[]}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStore(path); err == nil {
		t.Fatal("a state file claiming approval authority must fail closed")
	}
}

func TestCommitStateKeepsDurableFailureSemantics(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	seed := newState()
	seed.Days["2026-10-20"] = &Day{Date: "2026-10-20", Timezone: "UTC", Revision: 1, NextInterval: 2,
		Confirmed: []Interval{{ID: "iv-20261020-1", Date: "2026-10-20", Start: "09:00", End: "10:00", Project: "P1", Description: "seed"}}}
	if err := store.CommitState(seed); err != nil {
		t.Fatal(err)
	}
	before := store.State().cloneState()

	// A pre-rename failure rolls back exactly and a retry is safe.
	storeFaultHook = func(stage string) error {
		if stage == "before_rename" {
			return os.ErrPermission
		}
		return nil
	}
	candidate := store.State().cloneState()
	candidate.Days["2026-10-20"].Confirmed[0].Description = "should roll back"
	if err := store.CommitState(candidate); err == nil {
		t.Fatal("expected a pre-rename failure")
	}
	if store.State().Days["2026-10-20"].Confirmed[0].Description != "seed" ||
		!sameState(before, store.State()) {
		t.Fatal("pre-rename failure did not restore the previous snapshot")
	}
	storeFaultHook = nil
	if err := store.CommitState(candidate); err != nil {
		t.Fatalf("retry after a pre-rename failure must be safe: %v", err)
	}

	// A post-rename failure keeps the activated snapshot and blocks further
	// claims until restart.
	storeFaultHook = func(stage string) error {
		if stage == "directory_sync" {
			return os.ErrPermission
		}
		return nil
	}
	candidate = store.State().cloneState()
	candidate.Days["2026-10-20"].Confirmed[0].Description = "may be active"
	err = store.CommitState(candidate)
	if err == nil || errorCode(err) != CodeWriteUncertain {
		t.Fatalf("expected write_uncertain, got %v", err)
	}
	if !store.Uncertain() {
		t.Fatal("post-rename failure must block further claims")
	}
	if err := store.CommitState(store.State().cloneState()); err == nil {
		t.Fatal("further commits must stay blocked")
	}
	storeFaultHook = nil
}

func sameState(a, b *State) bool {
	ax, _ := json.Marshal(a)
	bx, _ := json.Marshal(b)
	return string(ax) == string(bx)
}
