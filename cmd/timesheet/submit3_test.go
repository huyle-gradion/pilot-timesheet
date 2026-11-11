package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// --- SUBMIT-3 synthetic acceptance tests ---------------------------------------
//
// plan and reconcile's live observation is exercised at full depth against a
// loopback MCP provider (the same fixture approach as the internal plan and
// submission tests): a synthetic server that declares ONLY the two read tools
// and serves synthetic week documents, with the Gradion configuration pointed
// at it and the credential resolved from the process environment. No network
// beyond the loopback. Every command runs as its own one-shot process would
// against a fresh temporary store; resolve's decision files are authored the
// way the runbook prescribes, from the printed reconcile receipts.

// stubRemote is a loopback MCP provider serving the two read tools. It is a
// test fixture, not a product path.
type stubRemote struct {
	mu      sync.Mutex
	entries map[string][]map[string]any
	calls   []string
	// classifications is the get_classifications text content; with
	// classificationsError the tool returns a tool-level error instead.
	classifications      string
	classificationsError bool
}

func (s *stubRemote) setEntries(date string, entries []map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[date] = entries
}

func (s *stubRemote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      *int64          `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if req.ID == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var result any
	switch req.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]any{"name": "stub", "version": "1"},
		}
	case "tools/list":
		result = map[string]any{"tools": []any{
			map[string]any{"name": "timesheet__get_my_week"},
			map[string]any{"name": "timesheet__get_classifications"},
		}}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(req.Params, &p)
		s.calls = append(s.calls, p.Name)
		switch p.Name {
		case "timesheet__get_my_week":
			ws, _ := p.Arguments["weekStart"].(string)
			entries := s.entries[ws]
			if entries == nil {
				entries = []map[string]any{}
			}
			days := []any{map[string]any{"date": ws, "entries": entries}}
			result = map[string]any{
				"structuredContent": map[string]any{
					"days": days, "weekStart": ws,
					"minDate": "2026-01-01", "maxDate": "2026-12-31",
				},
				"content": []any{map[string]any{"type": "text", "text": "week"}},
			}
		case "timesheet__get_classifications":
			if s.classificationsError || s.classifications == "" {
				result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "catalog unavailable"}}}
				break
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": s.classifications}}}
		default:
			result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "unexpected tool"}}}
		}
	default:
		result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "unexpected method"}}}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
}

func remoteWeekEntry(id, start, end, class, desc string) map[string]any {
	return map[string]any{
		"id": id, "startTime": start, "endTime": end,
		"classificationId": class, "description": desc, "status": "logged",
	}
}

func writeTestConfigWithGradion(t *testing.T, endpoint string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	body := `{
  "timezone": "Asia/Ho_Chi_Minh",
  "state_path": "` + filepath.Join(dir, "state.json") + `",
  "gradion": {"enabled": true, "endpoint": "` + endpoint + `", "token_env": "SYNTHETIC_GRADION_TOKEN"}
}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// classifiedSavedDay builds the full SUBMIT-1+2 flow up to save against a
// synthetic remote: one recorded timed block, one seeded catalog, one classify
// selection, confirm, save. It returns the config path, the saved local
// interval id and the stub.
func classifiedSavedDay(t *testing.T, desc string, remote []map[string]any) (string, string, *stubRemote) {
	t.Helper()
	stub := &stubRemote{entries: map[string][]map[string]any{testDate: remote}}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	t.Setenv("SYNTHETIC_GRADION_TOKEN", "synthetic-token")
	cfg := writeTestConfigWithGradion(t, server.URL)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", desc),
	})
	seedCatalog(t, cfg, "cat-1",
		timesheet.ClassificationOption{ID: "cls-dev", Name: "Development", Code: "DEV", ActivityType: "engineering"},
	)
	stage := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	ivID := stage.Staged[0].IntervalID
	choicesPath := writeChoicesFile(t, cfg,
		`{"selections": {"`+ivID+`": {"catalog_id": "cat-1", "classification_id": "cls-dev"}}}`)
	if receipt := runClassifyCapture(t, []string{"--config", cfg, "--date", testDate, "--file", choicesPath}); receipt.Selections != 1 {
		t.Fatalf("classify = %+v, want 1 selection", receipt)
	}
	if err := runConfirm([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := runSave([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("save: %v", err)
	}
	confirmed := readDayConfirmed(t, cfg, testDate)
	if len(confirmed) != 1 {
		t.Fatalf("confirmed = %+v, want 1 interval", confirmed)
	}
	return cfg, confirmed[0].ID, stub
}

func runPlanCapture(t *testing.T, args []string) timesheet.PlanPreview {
	t.Helper()
	var receipt timesheet.PlanPreview
	out := captureStdout(t, func() {
		if err := runPlan(args); err != nil {
			t.Fatalf("plan: %v", err)
		}
	})
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		t.Fatalf("plan receipt is not valid JSON: %v\n%s", err, out)
	}
	return receipt
}

func runReconcileCapture(t *testing.T, args []string) timesheet.ReconcileReceipt {
	t.Helper()
	var receipt timesheet.ReconcileReceipt
	out := captureStdout(t, func() {
		if err := runReconcile(args); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	})
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		t.Fatalf("reconcile receipt is not valid JSON: %v\n%s", err, out)
	}
	return receipt
}

func runResolveCapture(t *testing.T, args []string) timesheet.ResolutionReceipt {
	t.Helper()
	var receipt timesheet.ResolutionReceipt
	out := captureStdout(t, func() {
		if err := runResolve(args); err != nil {
			t.Fatalf("resolve: %v", err)
		}
	})
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		t.Fatalf("resolve receipt is not valid JSON: %v\n%s", err, out)
	}
	return receipt
}

func writeDecisionFile(t *testing.T, cfg, body string) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(cfg), "decision.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readBindings(t *testing.T, cfg string) []timesheet.RemoteBinding {
	t.Helper()
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	st := store.State()
	if st.Remote == nil {
		return nil
	}
	return st.Remote.Bindings
}

func bindingsJSON(t *testing.T, cfg string) string {
	t.Helper()
	data, err := json.Marshal(readBindings(t, cfg))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func hasBlocker(list []timesheet.Blocker, code, remoteID string) bool {
	for _, b := range list {
		if b.Code == code && (remoteID == "" || b.RemoteID == remoteID) {
			return true
		}
	}
	return false
}

// TestPlanBlockedThenAdoptYieldsNewReadyPlan: plan on a saved classified day
// with a synthetic overlapping unbound remote is blocked (unresolved_identity)
// with no operations; reconcile surfaces the unbound remote and the overlap
// candidate read-only; the human-authored adopt decision binds the identity;
// the re-preview is ready, carries the edit operation and a NEW plan_id.
func TestPlanBlockedThenAdoptYieldsNewReadyPlan(t *testing.T) {
	cfg, localID, _ := classifiedSavedDay(t, "Build the bridge",
		[]map[string]any{remoteWeekEntry("r-1", "09:00", "10:00", "cat-1", "Someone else's work")})

	first := runPlanCapture(t, []string{"--config", cfg, "--date", testDate})
	if first.PlanID == "" || first.State != "blocked" {
		t.Fatalf("first plan = %+v, want a blocked preview with a plan_id", first)
	}
	if !hasBlocker(first.Blockers, "unresolved_identity", "r-1") {
		t.Fatalf("first plan blockers = %+v, want unresolved_identity for r-1", first.Blockers)
	}
	if len(first.Operations) != 0 {
		t.Fatalf("first plan operations = %+v, want none while the identity is unresolved", first.Operations)
	}

	// Reconcile is read-only: the fresh observation is persisted but the
	// mapping state is untouched.
	before := bindingsJSON(t, cfg)
	rec := runReconcileCapture(t, []string{"--config", cfg, "--date", testDate})
	if after := bindingsJSON(t, cfg); after != before {
		t.Fatalf("reconcile changed bindings: %s -> %s", before, after)
	}
	if rec.ObservationID == "" || rec.Revision == 0 {
		t.Fatalf("reconcile receipt = %+v, want an observation_id and the saved revision", rec)
	}
	if rec.Complete != true || rec.Ready {
		t.Fatalf("reconcile receipt = %+v, want a complete observation with blockers", rec)
	}
	if len(rec.Unbound) != 1 || rec.Unbound[0].RemoteID != "r-1" {
		t.Fatalf("reconcile unbound = %+v, want exactly r-1", rec.Unbound)
	}
	if len(rec.Candidates) != 1 || rec.Candidates[0].Reason != "overlap" || rec.Candidates[0].RemoteID != "r-1" {
		t.Fatalf("reconcile candidates = %+v, want the overlap candidate for r-1", rec.Candidates)
	}
	if !hasBlocker(rec.Blockers, "unresolved_identity", "") {
		t.Fatalf("reconcile blockers = %+v, want unresolved_identity", rec.Blockers)
	}

	decision := writeDecisionFile(t, cfg,
		`{"revision": `+itoa(rec.Revision)+`, "observation_id": "`+rec.ObservationID+`", "decision": "adopt", "remote_id": "r-1", "local_id": "`+localID+`"}`)
	res := runResolveCapture(t, []string{"--config", cfg, "--date", testDate, "--file", decision})
	if res.Decision != "adopt" || !res.Bound || res.LocalID != localID || res.RemoteID != "r-1" {
		t.Fatalf("resolve receipt = %+v, want adopted r-1 bound to %s", res, localID)
	}
	bindings := readBindings(t, cfg)
	if len(bindings) != 1 || bindings[0].LocalID != localID || bindings[0].RemoteID != "r-1" || bindings[0].Origin != "adoption" {
		t.Fatalf("bindings = %+v, want one adoption binding %s -> r-1", bindings, localID)
	}

	second := runPlanCapture(t, []string{"--config", cfg, "--date", testDate})
	if second.State != "ready" || len(second.Blockers) != 0 {
		t.Fatalf("second plan = %+v, want a ready preview with no blockers", second)
	}
	if second.PlanID == first.PlanID {
		t.Fatal("the re-preview after resolve reused the blocked plan_id; the identity must differ")
	}
	if len(second.Operations) != 1 || second.Operations[0].Kind != "edit" || second.Operations[0].RemoteID != "r-1" {
		t.Fatalf("second plan operations = %+v, want one edit for r-1", second.Operations)
	}
}

// TestForeignDecisionYieldsReadyCreatePlan: an unbound remote entry that
// repeats a local description but does not overlap blocks the plan
// (unresolved_identity); the human's foreign decision records the judgment
// without binding anything; the re-preview is ready with the create operation
// and a new plan_id.
func TestForeignDecisionYieldsReadyCreatePlan(t *testing.T) {
	cfg, localID, _ := classifiedSavedDay(t, "Build the bridge",
		[]map[string]any{remoteWeekEntry("r-2", "13:00", "14:00", "cat-1", "Build the bridge")})

	first := runPlanCapture(t, []string{"--config", cfg, "--date", testDate})
	if first.State != "blocked" || !hasBlocker(first.Blockers, "unresolved_identity", "r-2") {
		t.Fatalf("first plan = %+v, want blocked with unresolved_identity for r-2", first)
	}
	rec := runReconcileCapture(t, []string{"--config", cfg, "--date", testDate})
	if len(rec.Candidates) != 1 || rec.Candidates[0].Reason != "equal_text" || rec.Candidates[0].RemoteID != "r-2" {
		t.Fatalf("reconcile candidates = %+v, want the equal_text candidate for r-2", rec.Candidates)
	}

	decision := writeDecisionFile(t, cfg,
		`{"revision": `+itoa(rec.Revision)+`, "observation_id": "`+rec.ObservationID+`", "decision": "foreign", "remote_id": "r-2"}`)
	res := runResolveCapture(t, []string{"--config", cfg, "--date", testDate, "--file", decision})
	if res.Decision != "foreign" || res.Bound || res.LocalID != "" {
		t.Fatalf("resolve receipt = %+v, want an unbound foreign decision", res)
	}
	if bindings := readBindings(t, cfg); len(bindings) != 0 {
		t.Fatalf("bindings after foreign = %+v, want none", bindings)
	}

	second := runPlanCapture(t, []string{"--config", cfg, "--date", testDate})
	if second.State != "ready" || len(second.Blockers) != 0 || second.PlanID == first.PlanID {
		t.Fatalf("second plan = %+v, want a ready NEW plan_id", second)
	}
	if len(second.Operations) != 1 || second.Operations[0].Kind != "create" || second.Operations[0].LocalID != localID {
		t.Fatalf("second plan operations = %+v, want one create for %s", second.Operations, localID)
	}
}

// TestAcceptObservedRebaselinesChangedRemote: after adoption, remote content
// drift marks the mapping changed (remote_changed blocker); the human's
// accept_observed decision re-baselines it and the next reconcile reports it
// mapped and unchanged.
func TestAcceptObservedRebaselinesChangedRemote(t *testing.T) {
	cfg, localID, stub := classifiedSavedDay(t, "Build the bridge",
		[]map[string]any{remoteWeekEntry("r-1", "09:00", "10:00", "cat-1", "Someone else's work")})
	rec := runReconcileCapture(t, []string{"--config", cfg, "--date", testDate})
	decision := writeDecisionFile(t, cfg,
		`{"revision": `+itoa(rec.Revision)+`, "observation_id": "`+rec.ObservationID+`", "decision": "adopt", "remote_id": "r-1", "local_id": "`+localID+`"}`)
	runResolveCapture(t, []string{"--config", cfg, "--date", testDate, "--file", decision})

	stub.setEntries(testDate, []map[string]any{
		remoteWeekEntry("r-1", "09:00", "10:00", "cat-1", "Rewritten elsewhere"),
	})
	rec2 := runReconcileCapture(t, []string{"--config", cfg, "--date", testDate})
	if len(rec2.Changed) != 1 || rec2.Changed[0].RemoteID != "r-1" || rec2.Changed[0].Accepted {
		t.Fatalf("reconcile changed = %+v, want r-1 changed and not accepted", rec2.Changed)
	}
	if !hasBlocker(rec2.Blockers, "remote_changed", "r-1") {
		t.Fatalf("reconcile blockers = %+v, want remote_changed for r-1", rec2.Blockers)
	}
	accept := writeDecisionFile(t, cfg,
		`{"revision": `+itoa(rec2.Revision)+`, "observation_id": "`+rec2.ObservationID+`", "decision": "accept_observed", "remote_id": "r-1", "local_id": "`+localID+`"}`)
	res := runResolveCapture(t, []string{"--config", cfg, "--date", testDate, "--file", accept})
	if res.Decision != "accept_observed" || !res.Bound {
		t.Fatalf("resolve receipt = %+v, want an accepted observation", res)
	}
	rec3 := runReconcileCapture(t, []string{"--config", cfg, "--date", testDate})
	if len(rec3.Mapped) != 1 || rec3.Mapped[0].RemoteID != "r-1" || rec3.Mapped[0].Changed {
		t.Fatalf("reconcile mapped = %+v, want r-1 mapped unchanged", rec3.Mapped)
	}
	if !rec3.Ready {
		t.Fatalf("reconcile blockers = %+v, want a ready receipt after accept_observed", rec3.Blockers)
	}
}

// TestResolveFailClosedRefusals: a wrong observation_id, an unknown remote_id,
// an adopt naming work that is not confirmed durable local work, a foreign
// decision carrying a local_id and a stale revision are each the op's own
// verbatim refusal, and none of them changes the mapping state.
func TestResolveFailClosedRefusals(t *testing.T) {
	cfg, localID, _ := classifiedSavedDay(t, "Build the bridge",
		[]map[string]any{remoteWeekEntry("r-1", "09:00", "10:00", "cat-1", "Someone else's work")})
	rec := runReconcileCapture(t, []string{"--config", cfg, "--date", testDate})

	cases := []struct {
		name string
		body string
		want string
	}{
		{"wrong observation_id",
			`{"revision": ` + itoa(rec.Revision) + `, "observation_id": "obs-wrong", "decision": "adopt", "remote_id": "r-1", "local_id": "` + localID + `"}`,
			"stale_observation"},
		{"unknown remote_id",
			`{"revision": ` + itoa(rec.Revision) + `, "observation_id": "` + rec.ObservationID + `", "decision": "adopt", "remote_id": "r-nope", "local_id": "` + localID + `"}`,
			"unknown_remote"},
		{"adopt without confirmed local work",
			`{"revision": ` + itoa(rec.Revision) + `, "observation_id": "` + rec.ObservationID + `", "decision": "adopt", "remote_id": "r-1", "local_id": "iv-nope"}`,
			"unknown_local"},
		{"foreign with local_id",
			`{"revision": ` + itoa(rec.Revision) + `, "observation_id": "` + rec.ObservationID + `", "decision": "foreign", "remote_id": "r-1", "local_id": "` + localID + `"}`,
			"local_id must be omitted for a foreign decision"},
		{"stale revision",
			`{"revision": ` + itoa(rec.Revision+1) + `, "observation_id": "` + rec.ObservationID + `", "decision": "adopt", "remote_id": "r-1", "local_id": "` + localID + `"}`,
			"stale_revision"},
		{"invalid decision value",
			`{"revision": ` + itoa(rec.Revision) + `, "observation_id": "` + rec.ObservationID + `", "decision": "maybe", "remote_id": "r-1", "local_id": "` + localID + `"}`,
			"decision must be adopt, accept_observed, or foreign"},
	}
	for _, tc := range cases {
		path := writeDecisionFile(t, cfg, tc.body)
		err := runResolve([]string{"--config", cfg, "--date", testDate, "--file", path})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want the verbatim %q refusal", tc.name, err, tc.want)
		}
	}
	if bindings := readBindings(t, cfg); len(bindings) != 0 {
		t.Fatalf("bindings after refusals = %+v, want none", bindings)
	}

	// Decision-file shape errors.
	if err := runResolve([]string{"--config", cfg, "--date", testDate, "--file", filepath.Join(filepath.Dir(cfg), "missing.json")}); err == nil ||
		!strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("missing decision file = %v, want a read error", err)
	}
	badJSON := writeDecisionFile(t, cfg, `{"decision": }`)
	if err := runResolve([]string{"--config", cfg, "--date", testDate, "--file", badJSON}); err == nil ||
		!strings.Contains(err.Error(), "expected schema") {
		t.Fatalf("invalid decision file = %v, want the schema-naming error", err)
	}
	noRevision := writeDecisionFile(t, cfg,
		`{"observation_id": "x", "decision": "adopt", "remote_id": "r-1", "local_id": "y"}`)
	if err := runResolve([]string{"--config", cfg, "--date", testDate, "--file", noRevision}); err == nil ||
		!strings.Contains(err.Error(), "missing revision") {
		t.Fatalf("decision file without revision = %v, want the missing-revision error", err)
	}
}

// TestPlanReconcileWithoutRemoteFailsClosed: without a Gradion configuration
// plan and reconcile fail with the op's own remote_disabled error — the live
// observation path is owner-verified; this proves the wiring fails closed.
// The op checks for saved confirmed work first, so the day is saved through
// the ordinary local flow before the remote boundary is reached.
func TestPlanReconcileWithoutRemoteFailsClosed(t *testing.T) {
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "One block"),
	})
	runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	if err := runConfirm([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := runSave([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := runPlan([]string{"--config", cfg, "--date", testDate}); err == nil ||
		!strings.Contains(err.Error(), "remote_disabled") {
		t.Fatalf("plan without remote = %v, want the remote_disabled refusal", err)
	}
	if err := runReconcile([]string{"--config", cfg, "--date", testDate}); err == nil ||
		!strings.Contains(err.Error(), "remote_disabled") {
		t.Fatalf("reconcile without remote = %v, want the remote_disabled refusal", err)
	}
}

// TestPlanRefusedWithoutSavedWork: plan previews only durable confirmed work.
func TestPlanRefusedWithoutSavedWork(t *testing.T) {
	stub := &stubRemote{entries: map[string][]map[string]any{}}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	t.Setenv("SYNTHETIC_GRADION_TOKEN", "synthetic-token")
	cfg := writeTestConfigWithGradion(t, server.URL)
	if err := runPlan([]string{"--config", cfg, "--date", testDate}); err == nil ||
		!strings.Contains(err.Error(), "no_confirmed_work") {
		t.Fatalf("plan without saved work = %v, want the no_confirmed_work refusal", err)
	}
}

// TestSubmit3FlagErrors: the new commands' flag surface.
func TestSubmit3FlagErrors(t *testing.T) {
	// The missing-config case resolves the $HOME default; pin it to an empty
	// directory so the default config never exists and the test is hermetic.
	t.Setenv("HOME", t.TempDir())
	cfg := writeTestConfig(t)
	cases := []struct {
		name string
		run  func([]string) error
		args []string
		want string
	}{
		{"plan missing date", runPlan, []string{"--config", cfg}, "plan requires --date"},
		{"plan bad date", runPlan, []string{"--config", cfg, "--date", "2026-13-01"}, "must be YYYY-MM-DD"},
		{"plan positional", runPlan, []string{"--config", cfg, "--date", testDate, "extra"}, "positional"},
		{"reconcile missing config", runReconcile, []string{"--date", testDate}, "no configuration at"},
		{"reconcile missing date", runReconcile, []string{"--config", cfg}, "reconcile requires --date"},
		{"resolve missing file", runResolve, []string{"--config", cfg, "--date", testDate}, "resolve requires --file"},
		{"resolve missing date", runResolve, []string{"--config", cfg, "--file", "d.json"}, "resolve requires --date"},
		{"resolve positional", runResolve, []string{"--config", cfg, "--date", testDate, "--file", "d.json", "x"}, "positional"},
	}
	for _, tc := range cases {
		err := tc.run(tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func itoa(v int) string {
	return strconv.Itoa(v)
}
