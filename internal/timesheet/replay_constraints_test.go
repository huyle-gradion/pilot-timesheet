package timesheet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInFlightWriteBlocksPlanApproveAndSubmitAfterRestart(t *testing.T) {
	s, fake, store, date := createReadyFixture(t)
	rec := approveReadyPlan(t, s, date)
	fake.failMode = "tool"
	if _, err := s.submitPlan(Request{ID: "s1", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID}); err != nil {
		t.Fatal(err)
	}
	next := store.State().cloneState()
	if len(next.Intents) != 1 {
		t.Fatalf("precondition: one recorded write intent: %+v", next.Intents)
	}
	next.Intents[0].Status = IntentInFlight
	next.Intents[0].ErrorCode = ""
	if err := store.CommitState(next); err != nil {
		t.Fatal(err)
	}
	fake.failMode = ""
	writes := len(fake.writeCalls)

	s2, err := restartSession(s)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.reconcileDate(date); err != nil {
		t.Fatal(err)
	}
	preview, err := s2.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}
	if preview.State == PlanReady || !hasBlocker(preview.Blockers, "unresolved_write") {
		t.Fatalf("an in_flight write must block the plan: state %s blockers %+v", preview.State, preview.Blockers)
	}
	if again, err := s2.approvePlan(Request{ID: "a2", Op: "approve_plan", Day: date, PlanID: preview.PlanID}); err == nil {
		res, _ := s2.submitPlan(Request{ID: "s2", Op: "submit_plan", Day: date, ApprovalID: again.ApprovalID})
		t.Fatalf("an in_flight write must block approval; submit then reported %+v", res)
	}
	res, err := s2.submitPlan(Request{ID: "s3", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitBlockedBeforeDispatch || len(fake.writeCalls) != writes {
		t.Fatalf("an in_flight write must never be replayed: %+v, writes %d -> %d", res, writes, len(fake.writeCalls))
	}
}

func TestPreviewSurfacesOnlyUnboundRemoteEntries(t *testing.T) {
	s, fake, _, date := editReadyFixture(t)
	fake.setWeek([]map[string]any{
		remoteEntry("r-1", "08:00", "10:00", "Synthetic work"),
		remoteEntry("r-other", "14:00", "15:00", "Entered elsewhere"),
	})
	if _, err := s.reconcileDate(date); err != nil {
		t.Fatal(err)
	}
	preview, err := s.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range preview.SurfacedRemotes {
		ids = append(ids, e.RemoteID)
	}
	if len(ids) != 1 || ids[0] != "r-other" {
		t.Fatalf("surfaced remotes = %v, want only the unbound r-other", ids)
	}
}

func TestPreviewApprovedOnlyWhileTheApprovalIsActive(t *testing.T) {
	s, _, _, date := createReadyFixture(t)
	rec := approveReadyPlan(t, s, date)
	preview, err := s.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Approved {
		t.Fatal("precondition: an active approval marks the plan approved")
	}
	if _, err := s.revokeApproval(Request{ID: "r1", Op: "revoke_approval", Day: date, ApprovalID: rec.ApprovalID}); err != nil {
		t.Fatal(err)
	}
	preview, err = s.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Approved {
		t.Fatal("a revoked approval must not mark the plan approved")
	}
}

type stubClassificationReader map[string]*CatalogRecord

func (r stubClassificationReader) Classifications(_ context.Context, query string) (*CatalogRecord, error) {
	if cat, ok := r[query]; ok {
		return cat, nil
	}
	return &CatalogRecord{CatalogID: "cat-empty-" + query, Complete: true}, nil
}

func TestClassificationProofPrefersTheHintThenFallsBackToUnfiltered(t *testing.T) {
	opt := ClassificationOption{ID: "cat-1", Name: "Synthetic Category", Code: "SC", ActivityType: "Development"}
	choice := choiceFromOption(opt)
	remote := &RemoteState{Catalogs: map[string]*CatalogRecord{
		"cat-stored": {CatalogID: "cat-stored", Options: []ClassificationOption{opt}},
	}}
	ops := []PlanOperation{{LocalID: "iv-1", CatalogID: "cat-stored", ClassificationID: "cat-1", SelectedChoice: &choice}}
	withOpt := func(id string) *CatalogRecord {
		return &CatalogRecord{CatalogID: id, Options: []ClassificationOption{opt}, Complete: true}
	}

	proof, err := proofClassifications(context.Background(), stubClassificationReader{
		"SC": {CatalogID: "cat-hint", Complete: true},
		"":   withOpt("cat-all"),
	}, remote, ops)
	if err != nil {
		t.Fatalf("a hint miss must fall back to the unfiltered catalog: %v", err)
	}
	if proof.Catalogs["cat-1"] != "cat-all" {
		t.Fatalf("proof catalog = %q, want the unfiltered cat-all", proof.Catalogs["cat-1"])
	}

	proof, err = proofClassifications(context.Background(), stubClassificationReader{
		"SC": withOpt("cat-hint"),
		"":   withOpt("cat-all"),
	}, remote, ops)
	if err != nil {
		t.Fatal(err)
	}
	if proof.Catalogs["cat-1"] != "cat-hint" {
		t.Fatalf("proof catalog = %q, want the hint query's cat-hint", proof.Catalogs["cat-1"])
	}
}

func TestGithubSendErrorNeverEchoesTheEndpointOrToken(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	base, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	server.Close()
	run := &githubRun{
		scope:  &githubScope{apiBase: base, token: "synthetic-github-token", user: "alice"},
		client: &http.Client{},
	}
	_, err = run.getJSON(url.Values{"q": {"author:alice"}}, "search/issues")
	if err == nil {
		t.Fatal("precondition: an unreachable endpoint must fail to send")
	}
	for _, leak := range []string{base.Host, base.Hostname(), "synthetic-github-token", "search/issues", "author"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("send error %q echoes %q", err, leak)
		}
	}
}

func TestStateFileIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Commit(0, &Day{Date: "2026-10-20", Timezone: "UTC", Revision: 1}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state file mode = %o, want 600", perm)
	}
}

func TestStoreCommitRefusesAStaleSavedRevision(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Commit(0, &Day{Date: "2026-10-20", Timezone: "UTC", Revision: 1}); err != nil {
		t.Fatal(err)
	}
	err = store.Commit(0, &Day{Date: "2026-10-20", Timezone: "UTC", Revision: 2})
	if errorCode(err) != CodeStaleRevision {
		t.Fatalf("a writer that read revision 0 must not overwrite revision 1: %v", err)
	}
	if got := store.State().Days["2026-10-20"].Revision; got != 1 {
		t.Fatalf("the refused save changed the stored day: revision %d", got)
	}
}

func TestResolveAdoptRefusesIdentityConflicts(t *testing.T) {
	s, fake, store, date := editReadyFixture(t)
	if _, err := s.addInterval(Request{ID: "1", Op: "add_interval", Day: date, Revision: intPtr(2),
		Text: date + " 11:00-12:00 Second piece"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.confirm(Request{ID: "2", Op: "confirm", Day: date, Revision: intPtr(2)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.save(Request{ID: "3", Op: "save", Day: date, Revision: intPtr(3)}); err != nil {
		t.Fatal(err)
	}
	fake.setWeek([]map[string]any{
		remoteEntry("r-1", "08:00", "10:00", "Synthetic work"),
		remoteEntry("r-2", "11:00", "12:00", "Second piece"),
	})
	if _, err := s.reconcileDate(date); err != nil {
		t.Fatal(err)
	}
	revision := store.State().Days[date].Revision
	if n := len(store.State().Remote.Bindings); n != 1 {
		t.Fatalf("precondition: one binding (r-1), got %d", n)
	}
	refuse := func(name, remote, local string) {
		t.Helper()
		obsID := store.State().Remote.Observations[date].ObservationID
		_, err := s.resolveRemoteRequest(Request{ID: name, Op: "resolve_remote", Day: date, Revision: intPtr(revision),
			ObservationID: obsID, RemoteID: remote, LocalID: local, Decision: DecisionAdopt})
		if errorCode(err) != CodeIdentityConflict {
			t.Errorf("%s: err = %v, want identity_conflict", name, err)
		}
		if b := store.State().Remote.Bindings; len(b) != 1 || b[0].RemoteID != "r-1" || b[0].LocalID != "iv-20261020-1" {
			t.Fatalf("%s: bindings changed: %+v", name, b)
		}
	}
	refuse("rebind an already bound allocation", "r-2", "iv-20261020-1")
	refuse("steal a remote bound elsewhere", "r-1", "iv-20261020-2")

	next := store.State().cloneState()
	obs := next.Remote.Observations[date]
	for i := range obs.Entries {
		if obs.Entries[i].RemoteID == "r-2" {
			obs.Entries[i].Date = "2026-10-21"
		}
	}
	if err := store.CommitState(next); err != nil {
		t.Fatal(err)
	}
	refuse("adopt an entry dated on another day", "r-2", "iv-20261020-2")
}
