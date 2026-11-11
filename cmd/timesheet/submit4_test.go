package main

import (
	"bufio"
	"encoding/json"
	"io"
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

func injectTTY(t *testing.T, lines ...string) {
	t.Helper()
	old := interactiveFactory
	interactiveFactory = func(name, verb string) (*interactive, error) {
		return &interactive{
			in:  bufio.NewReader(strings.NewReader(strings.Join(lines, "\n") + "\n")),
			out: io.Discard,
		}, nil
	}
	t.Cleanup(func() { interactiveFactory = old })
}

type stubSubmit struct {
	mu          sync.Mutex
	entries     map[string][]map[string]any
	calls       []string
	writeCalls  []string
	writeParams []string
	writeTools  bool
	failMode    string
	nextCreate  int
}

func (s *stubSubmit) allCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *stubSubmit) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writeCalls)
}

func (s *stubSubmit) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
			"serverInfo":      map[string]any{"name": "stub-submit", "version": "1"},
		}
	case "tools/list":
		names := []any{
			map[string]any{"name": "timesheet__get_my_week"},
			map[string]any{"name": "timesheet__get_classifications"},
		}
		if s.writeTools {
			names = append(names,
				map[string]any{"name": "timesheet__log_time"},
				map[string]any{"name": "timesheet__edit_time"})
		}
		result = map[string]any{"tools": names}
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
			result = map[string]any{
				"structuredContent": map[string]any{
					"resources": []any{map[string]any{"id": "cls-dev", "kind": "classification", "label": "Development"}},
				},
				"content": []any{map[string]any{"type": "text", "text": "- Development (DEV) [engineering] · cls-dev"}},
			}
		case "timesheet__log_time", "timesheet__edit_time":
			if !s.writeTools {
				result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "unexpected tool"}}}
				break
			}
			s.writeCalls = append(s.writeCalls, p.Name)
			s.writeParams = append(s.writeParams, string(req.Params))
			switch s.failMode {
			case "second-tool":
				if len(s.writeCalls) == 2 {
					result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "synthetic second write failure"}}}
				} else {
					result = s.applyWrite(p.Name, p.Arguments)
				}
			case "tool":
				result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "synthetic write failure"}}}
			case "reset":
				_ = s.applyWrite(p.Name, p.Arguments)
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
				_, _ = w.Write([]byte("SUFFIX-NOT-JSON"))
				return
			default:
				result = s.applyWrite(p.Name, p.Arguments)
			}
		default:
			result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "unexpected tool"}}}
		}
	default:
		result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "unexpected method"}}}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
}

func (s *stubSubmit) applyWrite(name string, args map[string]any) map[string]any {
	date, _ := args["date"].(string)
	start, _ := args["startTime"].(string)
	end, _ := args["endTime"].(string)
	desc, _ := args["description"].(string)
	class, _ := args["classificationId"].(string)
	if name == "timesheet__edit_time" {
		id, _ := args["entryId"].(string)
		for _, list := range s.entries {
			for _, e := range list {
				if e["id"] == id {
					if start != "" {
						e["startTime"] = start
					}
					if end != "" {
						e["endTime"] = end
					}
					if desc != "" {
						e["description"] = desc
					}
					if class != "" {
						e["classificationId"] = class
					}
					return map[string]any{
						"structuredContent": map[string]any{"entry": e},
						"content":           []any{map[string]any{"type": "text", "text": "updated"}},
					}
				}
			}
		}
		return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "no such entry"}}}
	}
	s.nextCreate++
	id := "r-new-" + strconv.Itoa(s.nextCreate)
	entry := map[string]any{
		"id": id, "startTime": start, "endTime": end,
		"classificationId": class, "description": desc, "status": "logged",
	}
	s.entries[date] = append(s.entries[date], entry)
	return map[string]any{
		"structuredContent": map[string]any{"entry": entry},
		"content":           []any{map[string]any{"type": "text", "text": "created"}},
	}
}

func savedDayForSubmit(t *testing.T, desc string, remote []map[string]any, failMode string) (string, *stubSubmit) {
	t.Helper()
	stub := &stubSubmit{entries: map[string][]map[string]any{testDate: remote}, failMode: failMode, writeTools: true}
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
	return cfg, stub
}

func approveReadyPlan(t *testing.T, failMode string) (string, string, string, *stubSubmit) {
	t.Helper()
	cfg, stub := savedDayForSubmit(t, "Build the bridge", nil, failMode)
	preview := runPlanCapture(t, []string{"--config", cfg, "--date", testDate})
	if preview.State != "ready" || len(preview.Operations) != 1 {
		t.Fatalf("plan = %+v, want a ready one-operation preview", preview)
	}
	injectTTY(t, preview.PlanID)
	receipt := runApproveCapture(t, []string{"--config", cfg, "--date", testDate})
	if receipt.ApprovalID == "" || receipt.PlanID != preview.PlanID || receipt.Operations != 1 {
		t.Fatalf("approve = %+v, want an approval binding %s", receipt, preview.PlanID)
	}
	return cfg, preview.PlanID, receipt.ApprovalID, stub
}

func runApproveCapture(t *testing.T, args []string) timesheet.ApprovalReceipt {
	t.Helper()
	var receipt timesheet.ApprovalReceipt
	out := captureStdout(t, func() {
		if err := runApprove(args); err != nil {
			t.Fatalf("approve: %v", err)
		}
	})
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		t.Fatalf("approve receipt is not valid JSON: %v\n%s", err, out)
	}
	return receipt
}

func runSubmitCapture(t *testing.T, args []string) timesheet.SubmissionReceipt {
	t.Helper()
	var receipt timesheet.SubmissionReceipt
	out := captureStdout(t, func() {
		if err := runSubmit(args); err != nil {
			t.Fatalf("submit: %v", err)
		}
	})
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		t.Fatalf("submit receipt is not valid JSON: %v\n%s", err, out)
	}
	return receipt
}

func runSubmitStatusCapture(t *testing.T, args []string) timesheet.SubmissionStatusResult {
	t.Helper()
	var receipt timesheet.SubmissionStatusResult
	out := captureStdout(t, func() {
		if err := runSubmitStatus(args); err != nil {
			t.Fatalf("submit-status: %v", err)
		}
	})
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		t.Fatalf("submit-status receipt is not valid JSON: %v\n%s", err, out)
	}
	return receipt
}

func runResolveWriteCapture(t *testing.T, args []string) timesheet.WriteResolutionReceipt {
	t.Helper()
	var receipt timesheet.WriteResolutionReceipt
	out := captureStdout(t, func() {
		if err := runResolveWrite(args); err != nil {
			t.Fatalf("resolve-write: %v", err)
		}
	})
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		t.Fatalf("resolve-write receipt is not valid JSON: %v\n%s", err, out)
	}
	return receipt
}

func stateSnapshot(t *testing.T, cfg string) string {
	t.Helper()
	data, err := os.ReadFile(statePathFor(cfg))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readApprovals(t *testing.T, cfg string) []timesheet.ApprovalRecord {
	t.Helper()
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	return append([]timesheet.ApprovalRecord(nil), store.State().Approvals...)
}

func TestGateRefusesNonTTYWithZeroRemoteCallsAndNoStateChange(t *testing.T) {
	cfg, stub := savedDayForSubmit(t, "Build the bridge", nil, "")
	decision := filepath.Join(filepath.Dir(cfg), "write-decision.json")
	if err := os.WriteFile(decision, []byte(
		`{"write_ref": "wr-x", "observation_id": "obs-x", "decision": "not_applied"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before := stateSnapshot(t, cfg)

	if err := runApprove([]string{"--config", cfg, "--date", testDate}); err == nil ||
		!strings.Contains(err.Error(), "interactive-only") {
		t.Fatalf("approve non-TTY = %v, want the interactive-only gate refusal", err)
	}
	if err := runSubmit([]string{"--config", cfg, "--date", testDate, "--approval-id", "apr-nope"}); err == nil ||
		!strings.Contains(err.Error(), "interactive-only") {
		t.Fatalf("submit non-TTY = %v, want the interactive-only gate refusal", err)
	}
	if err := runResolveWrite([]string{"--config", cfg, "--file", decision}); err == nil ||
		!strings.Contains(err.Error(), "interactive-only") {
		t.Fatalf("resolve-write non-TTY = %v, want the interactive-only gate refusal", err)
	}

	if calls := stub.allCalls(); len(calls) != 0 {
		t.Fatalf("gated commands made remote calls: %v", calls)
	}
	if stub.writeCount() != 0 {
		t.Fatalf("gated commands dispatched %d writes", stub.writeCount())
	}
	if after := stateSnapshot(t, cfg); after != before {
		t.Fatal("the gated refusals changed the store")
	}
}

func TestRenderPlanBodyShowsTaskAndReason(t *testing.T) {
	body := renderPlanBody(timesheet.PlanPreview{Operations: []timesheet.PlanOperation{{Task: "task-synthetic", Reason: "reason-synthetic"}}})
	if !strings.Contains(body, "task=task-synthetic") || !strings.Contains(body, "reason=reason-synthetic") {
		t.Fatalf("approve prompt body = %q, want the task and the audit reason", body)
	}
}

func TestApproveTTYGateRefusesBeforeTheStoreOpens(t *testing.T) {
	cfg := writeTestConfig(t)
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := runApprove([]string{"--config", cfg, "--date", testDate}); err == nil || !strings.Contains(err.Error(), "interactive-only") {
		t.Fatalf("approve with the store held = %v, want the TTY refusal first", err)
	}
}

func TestSubmitStatusIsNotTTYGated(t *testing.T) {
	cfg, _ := savedDayForSubmit(t, "Build the bridge", nil, "")
	res := runSubmitStatusCapture(t, []string{"--config", cfg, "--date", testDate})
	if res.Date != testDate || len(res.Approvals) != 0 {
		t.Fatalf("submit-status = %+v, want an empty read-only report for %s", res, testDate)
	}
}

func TestApproveInteractiveHappyPathAndWrongIDAborts(t *testing.T) {
	cfg, stub := savedDayForSubmit(t, "Build the bridge", nil, "")
	preview := runPlanCapture(t, []string{"--config", cfg, "--date", testDate})

	injectTTY(t, "not-the-plan-id")
	if err := runApprove([]string{"--config", cfg, "--date", testDate}); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("approve with wrong id = %v, want the abort", err)
	}
	if approvals := readApprovals(t, cfg); len(approvals) != 0 {
		t.Fatalf("approvals after wrong id = %+v, want none", approvals)
	}

	injectTTY(t)
	if err := runApprove([]string{"--config", cfg, "--date", testDate}); err == nil ||
		!strings.Contains(err.Error(), "aborted:") {
		t.Fatalf("approve with EOF = %v, want the abort", err)
	}

	injectTTY(t, preview.PlanID)
	receipt := runApproveCapture(t, []string{"--config", cfg, "--date", testDate})
	if !strings.HasPrefix(receipt.ApprovalID, "apr-") || receipt.PlanID != preview.PlanID ||
		receipt.State != timesheet.ApprovalActive || receipt.Operations != 1 {
		t.Fatalf("approve receipt = %+v, want an active one-operation approval for %s", receipt, preview.PlanID)
	}
	approvals := readApprovals(t, cfg)
	if len(approvals) != 1 || approvals[0].ApprovalID != receipt.ApprovalID || approvals[0].PlanID != preview.PlanID {
		t.Fatalf("approval ledger = %+v, want exactly %s", approvals, receipt.ApprovalID)
	}
	if calls := stub.allCalls(); len(calls) == 0 {
		t.Fatal("the gated-approve happy path made no remote calls at all; the fixture is broken")
	}

	cfgBlocked, _ := savedDayForSubmit(t, "Build the bridge",
		[]map[string]any{remoteWeekEntry("r-1", "09:00", "10:00", "cat-1", "Someone else's work")}, "")
	blocked := runPlanCapture(t, []string{"--config", cfgBlocked, "--date", testDate})
	if blocked.State != "blocked" {
		t.Fatalf("blocked plan = %+v, want state blocked", blocked)
	}
	injectTTY(t, blocked.PlanID)
	if err := runApprove([]string{"--config", cfgBlocked, "--date", testDate}); err == nil ||
		!strings.Contains(err.Error(), "plan_not_ready") {
		t.Fatalf("approve blocked plan = %v, want the verbatim plan_not_ready refusal", err)
	}
}

func TestSubmitConsumesApprovalThenReplayIsBlocked(t *testing.T) {
	cfg, planID, approvalID, stub := approveReadyPlan(t, "")

	injectTTY(t, approvalID)
	receipt := runSubmitCapture(t, []string{"--config", cfg, "--date", testDate, "--approval-id", approvalID})
	if receipt.Status != timesheet.SubmitAllConfirmed || receipt.Confirmed != 1 ||
		receipt.Attempts != 1 || receipt.Unattempted != 0 || receipt.Unresolved != 0 {
		t.Fatalf("submit receipt = %+v, want all_confirmed with one verified write", receipt)
	}
	if receipt.PlanID != planID || len(receipt.Writes) != 1 || receipt.Writes[0].RemoteID != "r-new-1" ||
		receipt.Writes[0].Status != timesheet.IntentConfirmed {
		t.Fatalf("submit receipt writes = %+v, want the created entry r-new-1 confirmed", receipt.Writes)
	}
	if receipt.Calls.WriteCalls != 1 || stub.writeCount() != 1 {
		t.Fatalf("call inventory = %+v / stub %d, want exactly one write dispatch", receipt.Calls, stub.writeCount())
	}

	injectTTY(t, approvalID)
	replay := runSubmitCapture(t, []string{"--config", cfg, "--date", testDate, "--approval-id", approvalID})
	if replay.Status != timesheet.SubmitBlockedBeforeDispatch || len(replay.Blockers) == 0 ||
		replay.Blockers[0].Code != timesheet.CodeStaleApproval {
		t.Fatalf("replay receipt = %+v, want blocked_before_dispatch with %s", replay, timesheet.CodeStaleApproval)
	}
	if stub.writeCount() != 1 {
		t.Fatalf("replay dispatched writes: stub now at %d", stub.writeCount())
	}

	injectTTY(t, "apr-does-not-exist")
	unknown := runSubmitCapture(t, []string{"--config", cfg, "--date", testDate, "--approval-id", "apr-does-not-exist"})
	if unknown.Status != timesheet.SubmitBlockedBeforeDispatch || len(unknown.Blockers) == 0 ||
		unknown.Blockers[0].Code != timesheet.CodeUnknownApproval {
		t.Fatalf("unknown approval receipt = %+v, want blocked_before_dispatch with %s", unknown, timesheet.CodeUnknownApproval)
	}
	if stub.writeCount() != 1 {
		t.Fatalf("unknown-approval submit dispatched writes: stub now at %d", stub.writeCount())
	}
}

func TestSubmitStatusReportsUnresolvedWrites(t *testing.T) {
	cfg, _, approvalID, _ := approveReadyPlan(t, "reset")
	injectTTY(t, approvalID)
	receipt := runSubmitCapture(t, []string{"--config", cfg, "--date", testDate, "--approval-id", approvalID})
	if receipt.Status != timesheet.SubmitPartialStopped || receipt.Unresolved != 1 || receipt.Confirmed != 0 {
		t.Fatalf("submit receipt = %+v, want partial_stopped with one unresolved write", receipt)
	}
	ref := receipt.Writes[0].WriteRef

	status := runSubmitStatusCapture(t, []string{"--config", cfg, "--date", testDate})
	if len(status.Unresolved) != 1 || status.Unresolved[0].WriteRef != ref || status.Unresolved[0].Origin != "native" {
		t.Fatalf("submit-status unresolved = %+v, want the native write reference %s", status.Unresolved, ref)
	}
	if len(status.Blockers) == 0 || status.Blockers[0].Code != "unresolved_write" {
		t.Fatalf("submit-status blockers = %+v, want unresolved_write", status.Blockers)
	}
	if !strings.Contains(status.NextAction, "resolve_write") {
		t.Fatalf("next_safe_action = %q, want the resolve_write recovery instruction", status.NextAction)
	}
	again := runSubmitStatusCapture(t, []string{"--config", cfg, "--date", testDate})
	if len(again.Unresolved) != 1 || again.Unresolved[0].WriteRef != ref {
		t.Fatalf("second submit-status = %+v, want the same single unresolved write", again.Unresolved)
	}
}

func writeDecisionFor(t *testing.T, cfg, body string) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(cfg), "write-decision.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveWriteAdoptAndNotApplied(t *testing.T) {
	cfg, _, approvalID, stub := approveReadyPlan(t, "reset")
	injectTTY(t, approvalID)
	receipt := runSubmitCapture(t, []string{"--config", cfg, "--date", testDate, "--approval-id", approvalID})
	if receipt.Unresolved != 1 {
		t.Fatalf("submit receipt = %+v, want one unresolved write", receipt)
	}
	ref := receipt.Writes[0].WriteRef
	if stub.writeCount() != 1 {
		t.Fatalf("stub write count = %d, want 1", stub.writeCount())
	}

	decision := writeDecisionFor(t, cfg,
		`{"write_ref": "`+ref+`", "observation_id": "obs-x", "decision": "adopt", "remote_id": "r-new-1"}`)
	savedFactory := interactiveFactory
	interactiveFactory = newInteractive
	err := runResolveWrite([]string{"--config", cfg, "--file", decision})
	interactiveFactory = savedFactory
	if err == nil || !strings.Contains(err.Error(), "interactive-only") {
		t.Fatalf("resolve-write non-TTY = %v, want the gate refusal", err)
	}

	rec := runReconcileCapture(t, []string{"--config", cfg, "--date", testDate})
	if rec.ObservationID == "" {
		t.Fatalf("reconcile = %+v, want a persisted observation", rec)
	}

	wrongObs := writeDecisionFor(t, cfg,
		`{"write_ref": "`+ref+`", "observation_id": "obs-wrong", "decision": "adopt", "remote_id": "r-new-1"}`)
	injectTTY(t, ref)
	if err := runResolveWrite([]string{"--config", cfg, "--file", wrongObs}); err == nil ||
		!strings.Contains(err.Error(), "stale_observation") {
		t.Fatalf("resolve-write wrong observation = %v, want the verbatim stale_observation refusal", err)
	}
	unknownRemote := writeDecisionFor(t, cfg,
		`{"write_ref": "`+ref+`", "observation_id": "`+rec.ObservationID+`", "decision": "adopt", "remote_id": "r-nope"}`)
	injectTTY(t, ref)
	if err := runResolveWrite([]string{"--config", cfg, "--file", unknownRemote}); err == nil ||
		!strings.Contains(err.Error(), "unknown_remote") {
		t.Fatalf("resolve-write unknown remote = %v, want the verbatim unknown_remote refusal", err)
	}

	adopt := writeDecisionFor(t, cfg,
		`{"write_ref": "`+ref+`", "observation_id": "`+rec.ObservationID+`", "decision": "adopt", "remote_id": "r-new-1"}`)
	injectTTY(t, ref)
	res := runResolveWriteCapture(t, []string{"--config", cfg, "--file", adopt})
	if !res.Resolved || !res.Bound || res.RemoteID != "r-new-1" || res.Origin != "native" || res.Decision != "adopt" {
		t.Fatalf("resolve-write receipt = %+v, want adopt bound to r-new-1", res)
	}
	status := runSubmitStatusCapture(t, []string{"--config", cfg, "--date", testDate})
	if len(status.Unresolved) != 0 || len(status.Blockers) != 0 {
		t.Fatalf("submit-status after adopt = %+v, want the blocker cleared", status)
	}

	cfg2, _, approvalID2, _ := approveReadyPlan(t, "tool")
	injectTTY(t, approvalID2)
	receipt2 := runSubmitCapture(t, []string{"--config", cfg2, "--date", testDate, "--approval-id", approvalID2})
	if receipt2.Unresolved != 1 || receipt2.Confirmed != 0 {
		t.Fatalf("submit receipt = %+v, want one unresolved write and no confirmations", receipt2)
	}
	ref2 := receipt2.Writes[0].WriteRef
	rec2 := runReconcileCapture(t, []string{"--config", cfg2, "--date", testDate})
	notApplied := writeDecisionFor(t, cfg2,
		`{"write_ref": "`+ref2+`", "observation_id": "`+rec2.ObservationID+`", "decision": "not_applied"}`)
	injectTTY(t, ref2)
	res2 := runResolveWriteCapture(t, []string{"--config", cfg2, "--file", notApplied})
	if !res2.Resolved || res2.Bound || res2.Decision != "not_applied" {
		t.Fatalf("resolve-write receipt = %+v, want an unbound not_applied resolution", res2)
	}
	status2 := runSubmitStatusCapture(t, []string{"--config", cfg2, "--date", testDate})
	if len(status2.Unresolved) != 0 || len(status2.Blockers) != 0 {
		t.Fatalf("submit-status after not_applied = %+v, want the blocker cleared", status2)
	}

	injectTTY(t, ref2)
	if err := runResolveWrite([]string{"--config", cfg2, "--file", notApplied}); err == nil ||
		!strings.Contains(err.Error(), "never re-resolved") {
		t.Fatalf("re-resolve = %v, want the verbatim never-re-resolved refusal", err)
	}
}

func TestSubmit4FlagErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := writeTestConfig(t)
	decision := writeDecisionFor(t, cfg, `{"write_ref": "wr", "observation_id": "obs", "decision": "not_applied"}`)
	cases := []struct {
		name string
		run  func([]string) error
		args []string
		want string
	}{
		{"approve missing date", runApprove, []string{"--config", cfg}, "approve requires --date"},
		{"approve bad date", runApprove, []string{"--config", cfg, "--date", "2026-13-01"}, "must be YYYY-MM-DD"},
		{"approve positional", runApprove, []string{"--config", cfg, "--date", testDate, "extra"}, "positional"},
		{"submit missing approval", runSubmit, []string{"--config", cfg, "--date", testDate}, "submit requires --approval-id"},
		{"submit missing config", runSubmit, []string{"--date", testDate, "--approval-id", "apr-1"}, "no configuration at"},
		{"submit bad date", runSubmit, []string{"--config", cfg, "--date", "nope", "--approval-id", "apr-1"}, "must be YYYY-MM-DD"},
		{"status missing date", runSubmitStatus, []string{"--config", cfg}, "submit-status requires --date"},
		{"resolve-write missing file", runResolveWrite, []string{"--config", cfg}, "resolve-write requires --file"},
		{"resolve-write missing config", runResolveWrite, []string{"--file", decision}, "no configuration at"},
		{"resolve-write positional", runResolveWrite, []string{"--config", cfg, "--file", decision, "x"}, "positional"},
	}
	for _, tc := range cases {
		err := tc.run(tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	bad := writeDecisionFor(t, cfg, `{"decision": }`)
	err := runResolveWrite([]string{"--config", cfg, "--file", bad})
	if err == nil || !strings.Contains(err.Error(), "expected schema") {
		t.Fatalf("invalid decision file = %v, want the schema-naming error", err)
	}
	adoptNoRemote := writeDecisionFor(t, cfg, `{"write_ref": "wr", "observation_id": "obs", "decision": "adopt"}`)
	err = runResolveWrite([]string{"--config", cfg, "--file", adoptNoRemote})
	if err == nil || !strings.Contains(err.Error(), "adopt requires remote_id") {
		t.Fatalf("adopt without remote_id = %v, want the remote_id requirement", err)
	}
	unknownField := writeDecisionFor(t, cfg, `{"write_ref": "wr", "observation_id": "obs", "decision": "adopt", "remote_id": "r", "extra": 1}`)
	err = runResolveWrite([]string{"--config", cfg, "--file", unknownField})
	if err == nil || !strings.Contains(err.Error(), "expected schema") {
		t.Fatalf("unknown field = %v, want the schema-naming error", err)
	}
}
