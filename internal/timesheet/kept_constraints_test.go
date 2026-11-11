package timesheet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRequestWithAnUnknownFieldIsRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	s := NewSession(testConfig(path), map[string]string{}, store)

	resp, _ := s.HandleLine([]byte(`{"id":"1","op":"open_day","day":"2026-10-30","dya":"2026-10-31"}`))
	if resp.OK || resp.Error == nil || resp.Error.Code != CodeBadRequest {
		t.Fatalf("a request with an unknown field must be refused as bad_request: %+v", resp)
	}
	if _, err := s.loadedDay("2026-10-30"); err == nil {
		t.Fatal("a refused request must not open the day")
	}
}

func TestUnresolvedWriteOnALaterSpillDayBlocksItsOriginDay(t *testing.T) {
	s, fake, store, date := createReadyFixture(t)
	rec := approveReadyPlan(t, s, date)
	fake.failMode = "tool"
	if _, err := s.submitPlan(Request{ID: "s1", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID}); err != nil {
		t.Fatal(err)
	}
	fake.failMode = ""
	next := store.State().cloneState()
	if len(next.Intents) != 1 || !next.Intents[0].unresolved() {
		t.Fatalf("precondition: one unresolved write intent: %+v", next.Intents)
	}
	spill := nextDate(date)
	next.Intents[0].Date = spill
	next.Intents[0].Operation.Date = spill
	next.Intents[0].Operation.OriginDates = []string{date}
	if err := store.CommitState(next); err != nil {
		t.Fatal(err)
	}

	preview, err := s.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}
	if preview.State == PlanReady || !hasBlocker(preview.Blockers, "unresolved_write") {
		t.Fatalf("an unresolved write allocated on %s from origin %s must block the origin day's plan: state %s blockers %+v",
			spill, date, preview.State, preview.Blockers)
	}
}

func ackFixture(t *testing.T) (*Session, *Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	date := "2026-10-22"
	ev := &DayEvidence{
		Attempts: []SourceAttempt{
			{Source: SourceGithub, CollectionID: "col-gh", AttemptedAt: "2026-10-22T18:00:00Z", Status: StatusIncomplete},
			{Source: SourceCalendar, CollectionID: "col-cal", AttemptedAt: "2026-10-22T18:00:00Z", Status: StatusComplete},
		},
		Meetings: []ProposedMeeting{{
			Ref: "cal:m1@20261022T090000Z", UID: "m1", Title: "Synthetic sync",
			Start: "2026-10-22T09:00:00Z", End: "2026-10-22T09:30:00Z", Status: "scheduled",
		}},
	}
	if err := store.CommitEvidence(date, ev); err != nil {
		t.Fatal(err)
	}
	s := NewSession(testConfig(path), map[string]string{}, store)
	requireOK(t, call(t, s, Request{ID: "o", Op: "open_day", Day: date}))
	return s, store, date
}

func TestFailedReviewDecisionWriteLeavesReviewStateUnchanged(t *testing.T) {
	for name, req := range map[string]Request{
		"acknowledge_source": {Op: "acknowledge_source", Source: SourceGithub, CollectionID: "col-gh"},
		"decide_meeting":     {Op: "decide_meeting", MeetingID: "cal:m1@20261022T090000Z", Decision: "attended", CollectionID: "col-cal"},
	} {
		t.Run(name, func(t *testing.T) {
			s, store, date := ackFixture(t)
			req.ID, req.Day = "r", date

			restore := storeFaultHook
			storeFaultHook = func(stage string) error {
				if stage == "before_rename" {
					return errSyntheticFault
				}
				return nil
			}
			resp := call(t, s, req)
			storeFaultHook = restore
			if resp.OK {
				t.Fatalf("the write fault must fail the decision: %+v", resp)
			}

			ev := store.Evidence(date)
			if ev.Attempts[0].Acknowledged || ev.Attempts[0].AcknowledgedAt != "" {
				t.Fatalf("a failed acknowledgment changed review state: %+v", ev.Attempts[0])
			}
			if ev.Meetings[0].Decision != "" {
				t.Fatalf("a failed decision changed review state: %+v", ev.Meetings[0])
			}

			requireOK(t, call(t, s, req))
			ev = store.Evidence(date)
			if !ev.Attempts[0].Acknowledged && ev.Meetings[0].Decision != "attended" {
				t.Fatalf("the retried decision must land: %+v", ev)
			}
		})
	}
}

func TestFailedIssueLookupIsRetriedForALaterComment(t *testing.T) {
	start, end := statusTestWindow, powWindowEnd()
	lookups := 0
	run, _ := commentWindowRun(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/issues/comments") {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				commentWindowIssueDoc(1, 7, statusTestUser, "2026-10-22T09:00:00Z"),
				commentWindowIssueDoc(2, 7, statusTestUser, "2026-10-22T09:30:00Z"),
			})
			return
		}
		lookups++
		if lookups == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "pull_request": map[string]any{}})
	})

	items, report := fetchIssueComments(run, statusTestRepo, "o", "r", start, end)

	if lookups != 2 {
		t.Fatalf("issue document lookups = %d, want 2 (a failed lookup is not cached)", lookups)
	}
	if len(items) != 2 || items[0].Kind != "issue_comment" || items[1].Kind != "pr_conversation_comment" {
		t.Fatalf("items = %+v, want the unverified comment kept as an issue comment and the verified one as a pull request comment", items)
	}
	if report.Status != StatusIncomplete {
		t.Fatalf("report = %+v, want incomplete for the unverified comment", report)
	}
}

func TestSubmittedReviewSearchBoundsMakeFamilyIncomplete(t *testing.T) {
	for name, doc := range map[string]map[string]any{
		"incomplete_results": {"total_count": 1, "incomplete_results": true, "items": []any{}},
		"over 1000 results":  {"total_count": 1001, "incomplete_results": false, "items": []any{}},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(doc) }))
		base, _ := url.Parse(srv.URL)
		run := &githubRun{scope: &githubScope{user: "me", apiBase: base, token: "synthetic"}, client: srv.Client()}
		start := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
		_, rep := fetchSubmittedReviews(run, "o/r", "o", "r", start, start.Add(24*time.Hour))
		srv.Close()
		if rep.Status != StatusIncomplete {
			t.Fatalf("%s: submitted reviews report = %+v, want incomplete", name, rep)
		}
	}
}

type staticCatalog struct{ options []ClassificationOption }

func (c staticCatalog) Classifications(context.Context, string) (*CatalogRecord, error) {
	return &CatalogRecord{CatalogID: "cat", Options: c.options, Complete: true}, nil
}

func TestClassificationProofComparesOnlyRelevantContent(t *testing.T) {
	bound := ClassificationChoice{ID: "c1", Name: "Synthetic", Code: "SY", ActivityType: "Development"}
	current := ClassificationOption{ID: "c1", Name: "Synthetic", Code: "SY", ActivityType: "Development"}
	ops := []PlanOperation{{LocalID: "l1", ClassificationID: "c1", SelectedChoice: &bound}}

	unrelated := staticCatalog{options: []ClassificationOption{current, {ID: "c2", Name: "Renamed neighbour"}}}
	if _, err := proofClassifications(context.Background(), unrelated, nil, ops); err != nil {
		t.Fatalf("an unrelated option must not invalidate the choice: %v", err)
	}
	for field, edit := range map[string]func(*ClassificationOption){
		"name":          func(o *ClassificationOption) { o.Name = "Renamed" },
		"code":          func(o *ClassificationOption) { o.Code = "RN" },
		"activity type": func(o *ClassificationOption) { o.ActivityType = "Meeting" },
	} {
		changed := current
		edit(&changed)
		_, err := proofClassifications(context.Background(), staticCatalog{options: []ClassificationOption{changed}}, nil, ops)
		if app, ok := err.(*AppError); !ok || app.Code != CodeClassificationChanged {
			t.Fatalf("a changed %s must fail with %s, got %v", field, CodeClassificationChanged, err)
		}
	}
}

func TestClassificationProofRefusesAPlanBindingOneIDTwoWays(t *testing.T) {
	first := ClassificationChoice{ID: "c1", Name: "Synthetic", Code: "SY"}
	second := ClassificationChoice{ID: "c1", Name: "Synthetic (old)", Code: "SY"}
	ops := []PlanOperation{
		{LocalID: "l1", ClassificationID: "c1", SelectedChoice: &first},
		{LocalID: "l2", ClassificationID: "c1", SelectedChoice: &second},
	}
	catalog := staticCatalog{options: []ClassificationOption{{ID: "c1", Name: "Synthetic", Code: "SY"}}}
	if _, err := proofClassifications(context.Background(), catalog, nil, ops); err == nil {
		t.Fatal("two operations binding one classification id to different content must be refused")
	}
}

func TestSubmitRefusesAnApprovalWhoseBoundChoiceDiffersFromTheCurrentProof(t *testing.T) {
	s, fake, store, date := createReadyFixture(t)
	rec := approveReadyPlan(t, s, date)
	next := store.State().cloneState()
	approval := approvalByDateAndID(next, date, rec.ApprovalID)
	if approval == nil || len(approval.Summary.Choices) != 1 {
		t.Fatalf("precondition: the approval binds one choice: %+v", approval)
	}
	for _, c := range approval.Summary.Choices {
		c.Name = "Synthetic Category (before rename)"
	}
	if err := store.CommitState(next); err != nil {
		t.Fatal(err)
	}

	res, err := s.submitPlan(Request{ID: "s1", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitBlockedBeforeDispatch || !hasBlocker(res.Blockers, CodeClassificationChanged) {
		t.Fatalf("a bound choice that no longer matches the current proof must block: %+v", res)
	}
	if len(fake.writeCalls) != 0 {
		t.Fatalf("nothing may be dispatched: %v", fake.writeCalls)
	}
}

func TestZeroOperationApprovalWithMissingLegacyProofFailsClosed(t *testing.T) {
	s, fake, _, date := createReadyFixture(t)
	first := approveReadyPlan(t, s, date)
	if _, err := s.submitPlan(Request{ID: "s0", Op: "submit_plan", Day: date, ApprovalID: first.ApprovalID}); err != nil {
		t.Fatal(err)
	}
	preview, err := s.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Operations) != 0 {
		t.Fatalf("precondition: zero operations expected: %+v", preview.Operations)
	}
	rec, err := s.approvePlan(Request{ID: "a2", Op: "approve_plan", Day: date, PlanID: preview.PlanID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(s.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	removed := false
	rows, _ := state["approvals"].([]any)
	for _, row := range rows {
		r, _ := row.(map[string]any)
		if r["approval_id"] == rec.ApprovalID {
			summary, _ := r["summary"].(map[string]any)
			delete(summary, "choices")
			removed = true
		}
	}
	if !removed {
		t.Fatalf("precondition: the zero-operation approval is stored: %v", rows)
	}
	legacy, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.cfg.StatePath, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(s.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	s2 := NewSession(s.cfg, s.env, store)
	writes := len(fake.writeCalls)

	res, err := s2.submitPlan(Request{ID: "s1", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitBlockedBeforeDispatch || !hasBlocker(res.Blockers, CodeStaleApproval) {
		t.Fatalf("a zero-operation approval with a missing legacy proof must fail closed: %+v", res)
	}
	if len(fake.writeCalls) != writes || len(store.State().Executions) != 1 {
		t.Fatalf("a refused consumption must record and dispatch nothing: writes %d -> %d, executions %+v",
			writes, len(fake.writeCalls), store.State().Executions)
	}
}

func TestCarriedBlockNeverCollidesWithTheTargetDaysOwnBlock(t *testing.T) {
	source, target := "2026-10-30", "2026-10-31"
	carried := CarrySegment{
		SourceDate: source, SourceBlockID: "work-23:00-02:00", Date: target,
		Start: "00:00", End: "02:00", Minutes: 120, Description: "Carried work", Kind: "work", Timezone: "UTC",
	}
	own := EntryRecord{BlockID: "work-23:00-02:00", Date: target, Start: "23:00", End: "23:30", Description: "Own work", Kind: "work"}

	clusters := EffectiveEntrySpansOn(target, []EntryRecord{own, carried.Entry()})

	got := map[string]string{}
	for _, c := range clusters {
		got[c.Description] = c.BlockID
	}
	if got["Own work"] != own.BlockID {
		t.Fatalf("the target day's own block lost its id: %+v", clusters)
	}
	if id := got["Carried work"]; id == own.BlockID || CarriedSourceDate(id) != source {
		t.Fatalf("the carried block id %q must name its source %s and differ from the own block id: %+v", id, source, clusters)
	}
}
