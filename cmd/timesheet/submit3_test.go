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

type stubRemote struct {
	mu                   sync.Mutex
	entries              map[string][]map[string]any
	calls                []string
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

func TestSubmit3FlagErrors(t *testing.T) {
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
