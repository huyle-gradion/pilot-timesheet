package timesheet

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// createReadyFixture is a classed, saved day with an EMPTY remote day, so the
// fresh preview is ready with exactly one create operation.
func createReadyFixture(t *testing.T) (*Session, *fakeMCP, *Store, string) {
	t.Helper()
	fake := &fakeMCP{
		day:        "2026-10-20",
		entries:    []map[string]any{},
		catOptions: []map[string]any{{"id": "cat-1", "kind": "classification", "label": "Synthetic Category"}},
		catText:    "- Synthetic Category (SC) [Development] · cat-1",
	}
	s, store, date := openFixtureStore(t, fake)
	fake.writeTools = true
	if _, err := s.openDay(Request{ID: "1", Op: "open_day", Day: date}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.addInterval(Request{ID: "2", Op: "add_interval", Day: date, Revision: intPtr(0),
		Text: date + " 09:00-10:00 Synthetic work"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.confirm(Request{ID: "3", Op: "confirm", Day: date, Revision: intPtr(0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.save(Request{ID: "4", Op: "save", Day: date, Revision: intPtr(1)}); err != nil {
		t.Fatal(err)
	}
	cat, err := s.classifications(Request{ID: "5", Op: "classifications"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.selectClassification(Request{ID: "6", Op: "select_classification", Day: date,
		Revision: intPtr(1), IntervalID: "iv-20261020-1", CatalogID: cat.CatalogID,
		ClassificationID: "cat-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.confirm(Request{ID: "7", Op: "confirm", Day: date, Revision: intPtr(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.save(Request{ID: "8", Op: "save", Day: date, Revision: intPtr(2)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.reconcileDate(date); err != nil {
		t.Fatal(err)
	}
	preview, err := s.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}
	if preview.State != PlanReady || len(preview.Operations) != 1 || preview.Operations[0].Kind != "create" {
		t.Fatalf("expected a ready one-create preview: %+v", preview)
	}
	return s, fake, store, date
}

// openFixtureStore builds a session over a private state file and a loopback
// provider, without any work content.
func openFixtureStore(t *testing.T, fake *fakeMCP) (*Session, *Store, string) {
	t.Helper()
	return newSessionFixture(t, fake, "2026-10-20")
}

func openFixtureStoreAt(t *testing.T, fake *fakeMCP, date string) (*Session, *Store, string) {
	t.Helper()
	return newSessionFixture(t, fake, date)
}

func newSessionFixture(t *testing.T, fake *fakeMCP, date string) (*Session, *Store, string) {
	t.Helper()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	cfg := &Config{
		Timezone:  "UTC",
		StatePath: filepath.Join(t.TempDir(), "state.json"),
		Gradion:   &GradionConfig{Enabled: true, Endpoint: server.URL, TokenEnv: DefaultGradionTokenEnv},
	}
	store, err := OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	env := map[string]string{DefaultGradionTokenEnv: "synthetic-token"}
	return NewSession(cfg, env, store), store, date
}

// restartSession reopens the durable store to emulate a clean process restart.
func restartSession(s *Session) (*Session, error) {
	if err := s.store.Close(); err != nil {
		return nil, err
	}
	store, err := OpenStore(s.cfg.StatePath)
	if err != nil {
		return nil, err
	}
	return NewSession(s.cfg, s.env, store), nil
}

// errSyntheticFault is a deterministic durability fault used by tests.
var errSyntheticFault = &AppError{Code: CodeWriteFailed, Message: "synthetic durability fault"}

func TestSubmitCreateVerifiesReadBackAndPersists(t *testing.T) {
	s, fake, store, date := createReadyFixture(t)
	preview, err := s.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}
	op := preview.Operations[0]
	rec := approveReadyPlan(t, s, date)

	res, err := s.submitPlan(Request{ID: "s1", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitAllConfirmed || res.Attempts != 1 || res.Confirmed != 1 || res.Unresolved != 0 {
		t.Fatalf("unexpected submission receipt: %+v", res)
	}
	if res.Calls.WriteCalls != 1 || len(res.Writes) != 1 {
		t.Fatalf("call inventory mismatch: %+v", res.Calls)
	}
	if res.Writes[0].Status != IntentConfirmed || res.Writes[0].RemoteID != "r-new-1" {
		t.Fatalf("write row mismatch: %+v", res.Writes[0])
	}

	// The dispatched body is exactly the approved operation content: five
	// approved fields and nothing else (no task/billable/derived activity type).
	if len(fake.writeBodies) != 1 {
		t.Fatalf("writes performed: %d", len(fake.writeBodies))
	}
	body := fake.writeBodies[0]
	if len(body) != 5 ||
		body["date"] != op.Date || body["startTime"] != op.StartTime || body["endTime"] != op.EndTime ||
		body["description"] != op.Description || body["classificationId"] != op.ClassificationID {
		t.Fatalf("dispatched body is not the exact approved body: %v (op %+v)", body, op)
	}

	// The verified observed content, not planned state, is durable.
	st := store.State()
	if len(st.Remote.Bindings) != 1 {
		t.Fatalf("bindings: %+v", st.Remote.Bindings)
	}
	b := st.Remote.Bindings[0]
	if b.LocalID != op.LocalID || b.RemoteID != "r-new-1" || b.StartTime != op.StartTime ||
		b.Description == "" || b.LastObserved == nil || b.LastObserved.RemoteID != "r-new-1" {
		t.Fatalf("submission binding mismatch: %+v", b)
	}
	if b.Origin != "submission" {
		t.Fatalf("binding origin: %+v", b.Origin)
	}
	if len(st.Intents) != 1 || st.Intents[0].Status != IntentConfirmed || st.Intents[0].AcknowledgedID != "r-new-1" {
		t.Fatalf("intent audit: %+v", st.Intents)
	}
	if len(st.Executions) != 1 || st.Executions[0].State != ExecutionCompleted || st.Executions[0].Confirmed != 1 {
		t.Fatalf("execution audit: %+v", st.Executions)
	}
	if st.Approvals[0].State != ApprovalExecuting || st.Approvals[0].ExecutionID == "" {
		t.Fatalf("approval consumption: %+v", st.Approvals[0])
	}
}

func TestSubmitConsumedOnceIncludingAcrossRestart(t *testing.T) {
	s, fake, _, date := createReadyFixture(t)
	rec := approveReadyPlan(t, s, date)

	if _, err := s.submitPlan(Request{ID: "s1", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID}); err != nil {
		t.Fatal(err)
	}
	if len(fake.writeCalls) != 1 {
		t.Fatalf("expected exactly one write: %+v", fake.writeCalls)
	}

	// A restart never duplicates: reopening the store, the spent approval stays
	// spent and replay is blocked with zero new mutations.
	s2, err := restartSession(s)
	if err != nil {
		t.Fatal(err)
	}
	fake2 := fake // same server
	res, err := s2.submitPlan(Request{ID: "s2", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitBlockedBeforeDispatch || len(fake2.writeCalls) != 1 {
		t.Fatalf("a spent approval must never re-dispatch: %+v", res)
	}

	// Identical content can be reapproved only with a DISTINCT fresh approval.
	rec2 := approveReadyPlan(t, s2, date)
	if rec2.ApprovalID == rec.ApprovalID {
		t.Fatal("a new approval must be a new occurrence")
	}
	// The first approval is still immutable audit in the ledger.
	if len(s2.store.State().Approvals) < 2 {
		t.Fatalf("ledger: %+v", s2.store.State().Approvals)
	}
}

func TestSubmitToolFailurePersistsUncertainAndBlocksReplay(t *testing.T) {
	s, fake, store, date := createReadyFixture(t)
	rec := approveReadyPlan(t, s, date)

	fake.failMode = "tool"
	res, err := s.submitPlan(Request{ID: "s1", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitPartialStopped || res.Attempts != 1 || res.Confirmed != 0 || res.Unresolved != 1 {
		t.Fatalf("uncertain attempt receipt: %+v", res)
	}
	if len(fake.writeCalls) != 1 {
		t.Fatalf("a failed write is attempted exactly once: %+v", fake.writeCalls)
	}
	if store.State().Intents[0].Status != IntentUncertain || store.State().Intents[0].ErrorCode == "" {
		t.Fatalf("uncertain intent audit: %+v", store.State().Intents[0])
	}
	if len(store.State().Remote.Bindings) != 0 {
		t.Fatal("no binding may exist for an unverified write")
	}

	// Restart: the uncertainty is durable and blocks replay; the spent approval
	// cannot be reused and no automatic retry happens.
	s2, err := restartSession(s)
	if err != nil {
		t.Fatal(err)
	}
	status, err := s2.submissionStatus(Request{ID: "t1", Op: "submission_status", Day: date})
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Unresolved) != 1 || status.Unresolved[0].Origin != "native" || status.Unresolved[0].WriteRef == "" {
		t.Fatalf("restart blockers: %+v", status.Unresolved)
	}
	if len(fake.writeCalls) != 1 {
		t.Fatal("restart must never retry or resume a batch")
	}
	res, err = s2.submitPlan(Request{ID: "t2", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitBlockedBeforeDispatch || len(fake.writeCalls) != 1 {
		t.Fatalf("a spent approval with unresolved writes must stay blocked: %+v", res)
	}

	// Explicit recovery: not_applied with the current complete observation
	// clears exactly this blocker with zero mutation calls, preserving audit.
	ref := status.Unresolved[0].WriteRef
	obs := store.State().Remote.Observations[date]
	if obs == nil || !obs.Complete {
		t.Fatalf("expected a durable complete observation: %+v", obs)
	}
	before := len(fake.writeCalls)
	rr, err := s2.resolveWrite(Request{ID: "t3", Op: "resolve_write", WriteRef: ref,
		ObservationID: obs.ObservationID, Decision: DecisionWriteNotApplied})
	if err != nil {
		t.Fatal(err)
	}
	if !rr.Resolved || rr.Bound {
		t.Fatalf("resolution receipt: %+v", rr)
	}
	if len(fake.writeCalls) != before {
		t.Fatal("recovery must never dispatch anything")
	}
	intent := s2.store.State().Intents[0]
	if !intent.Resolved || intent.Resolution != DecisionWriteNotApplied || intent.Status != IntentUncertain {
		t.Fatalf("original audit must be preserved: %+v", intent)
	}
	status, err = s2.submissionStatus(Request{ID: "t4", Op: "submission_status", Day: date})
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Unresolved) != 0 {
		t.Fatalf("the addressed blocker must clear: %+v", status.Unresolved)
	}
}

func TestSubmitAppliedThenResetIsRecoverableNotReplayed(t *testing.T) {
	s, fake, _, date := createReadyFixture(t)
	rec := approveReadyPlan(t, s, date)

	// The effect applies remotely and then the connection corrupts the
	// response: uncertain, never confirmed, never retried.
	fake.failMode = "reset"
	res, err := s.submitPlan(Request{ID: "s1", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitPartialStopped || res.Unresolved != 1 || res.Confirmed != 0 {
		t.Fatalf("applied-then-reset receipt: %+v", res)
	}
	if len(fake.writeCalls) != 1 || len(fake.days[date]) != 1 {
		t.Fatalf("the effect was applied once: calls %v days %+v", fake.writeCalls, fake.days)
	}
	if _, ok := fake.days[date][0]["id"]; !ok {
		t.Fatal("synthetic effect missing")
	}

	// Restart: still blocked, still no replay.
	s2, err := restartSession(s)
	if err != nil {
		t.Fatal(err)
	}
	status, err := s2.submissionStatus(Request{ID: "t1", Op: "submission_status", Day: date})
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Unresolved) != 1 {
		t.Fatalf("restart blockers: %+v", status.Unresolved)
	}
	if len(fake.writeCalls) != 1 {
		t.Fatal("restart must never replay")
	}
}

func TestSubmitMalformedAckNeverConfirms(t *testing.T) {
	s, fake, store, date := createReadyFixture(t)
	rec := approveReadyPlan(t, s, date)

	// No mutation happens and no id can be decoded: unidentified, never
	// confirmed.
	fake.failMode = "malformed"
	res, err := s.submitPlan(Request{ID: "s1", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitPartialStopped || res.Confirmed != 0 || res.Unresolved != 1 {
		t.Fatalf("malformed ack receipt: %+v", res)
	}
	if len(fake.writeBodies) != 1 || len(fake.days[date]) != 0 {
		t.Fatalf("malformed mode must not mutate: %+v", fake.days)
	}
	intent := store.State().Intents[0]
	if intent.Status != IntentUnidentified || intent.ErrorCode != ErrCodeWriteUnidentified {
		t.Fatalf("unidentified intent audit: %+v", intent)
	}

	// A not_applied attestation against the current complete observation (the
	// effect provably absent) clears exactly this reference.
	obs := store.State().Remote.Observations[date]
	rr, err := s.resolveWrite(Request{ID: "r1", Op: "resolve_write", WriteRef: intent.WriteRef,
		ObservationID: obs.ObservationID, Decision: DecisionWriteNotApplied})
	if err != nil {
		t.Fatal(err)
	}
	if !rr.Resolved || len(fake.writeCalls) != 1 {
		t.Fatalf("resolution: %+v", rr)
	}

	// A contradictory not_applied is refused: an effect matching the intended
	// write is visible in the current observation.
	s2, fake2, _, date2 := createReadyFixture(t)
	rec2 := approveReadyPlan(t, s2, date2)
	fake2.failMode = "reset"
	if _, err := s2.submitPlan(Request{ID: "s2", Op: "submit_plan", Day: date2, ApprovalID: rec2.ApprovalID}); err != nil {
		t.Fatal(err)
	}
	st2 := s2.store.State()
	status, err := s2.submissionStatus(Request{ID: "t3", Op: "submission_status", Day: date2})
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Unresolved) != 1 {
		t.Fatalf("expected one unresolved write: %+v", status.Unresolved)
	}
	// The store still holds the pre-write observation; reconcile first, then
	// the matching effect makes not_applied impossible and adopt exact.
	rec3, err := s2.reconcileDate(date2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.resolveWrite(Request{ID: "t4", Op: "resolve_write", WriteRef: status.Unresolved[0].WriteRef,
		ObservationID: rec3.ObservationID, Decision: DecisionWriteNotApplied}); err == nil {
		t.Fatal("not_applied must be refused when a matching effect exists")
	}
	if _, err := s2.resolveWrite(Request{ID: "t5", Op: "resolve_write", WriteRef: status.Unresolved[0].WriteRef,
		ObservationID: rec3.ObservationID, Decision: DecisionWriteAdopt, RemoteID: "wrong-id"}); err == nil {
		t.Fatal("adopt must require an explicitly selected observed id present in the observation")
	}
	// A stale observation id cannot carry a resolution.
	if _, err := s2.resolveWrite(Request{ID: "t6", Op: "resolve_write", WriteRef: status.Unresolved[0].WriteRef,
		ObservationID: st2.Remote.Observations[date2].ObservationID, Decision: DecisionWriteNotApplied}); err == nil {
		t.Fatal("a stale observation must never carry a resolution")
	}
	_ = fake2
}

func TestSubmitIntentPersistenceFailureIsZeroMutation(t *testing.T) {
	s, fake, store, date := createReadyFixture(t)
	rec := approveReadyPlan(t, s, date)

	storeFaultHook = func(stage string) error {
		if stage == "before_rename" {
			return errSyntheticFault
		}
		return nil
	}
	defer func() { storeFaultHook = nil }()

	res, err := s.submitPlan(Request{ID: "s1", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.writeCalls) != 0 {
		t.Fatalf("pre-rename intent failure must cause zero mutations: %+v", fake.writeCalls)
	}
	if res.Attempts != 0 || len(res.Blockers) == 0 {
		t.Fatalf("honest blocked receipt: %+v", res)
	}
	if len(store.State().Intents) != 0 {
		t.Fatalf("no intent may be durably recorded pre-rename: %+v", store.State().Intents)
	}
	if got := store.State().Approvals[0].State; got != ApprovalActive {
		t.Fatalf("the approval was never consumed: %+v", got)
	}

	// With durability restored, the STILL-ACTIVE approval can be consumed by a
	// real submit; authorization was never silently lost or duplicated.
	storeFaultHook = nil
	fake.failMode = ""
	res, err = s.submitPlan(Request{ID: "s2", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitAllConfirmed || len(fake.writeCalls) != 1 {
		t.Fatalf("recovered submission: %+v", res)
	}
}

func TestSubmitExternalDriftStopsLaterOperations(t *testing.T) {
	s, fake, _, date := createReadyFixture(t)

	// A second, independent allocation on the same date: two create operations.
	if _, err := s.addInterval(Request{ID: "9", Op: "add_interval", Day: date, Revision: intPtr(2),
		Text: date + " 11:00-12:00 Second task"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.confirm(Request{ID: "10", Op: "confirm", Day: date, Revision: intPtr(2)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.save(Request{ID: "11", Op: "save", Day: date, Revision: intPtr(3)}); err != nil {
		t.Fatal(err)
	}
	cat, err := s.classifications(Request{ID: "12", Op: "classifications"})
	if err != nil {
		t.Fatal(err)
	}
	secondID := ""
	day := s.store.State().Days[date]
	for _, iv := range day.Staged {
		if iv.Start == "11:00" {
			secondID = iv.ID
		}
	}
	if secondID == "" {
		t.Fatalf("second interval not staged: %+v", day.Staged)
	}
	if _, err := s.selectClassification(Request{ID: "13", Op: "select_classification", Day: date,
		Revision: intPtr(3), IntervalID: secondID, CatalogID: cat.CatalogID,
		ClassificationID: "cat-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.confirm(Request{ID: "14", Op: "confirm", Day: date, Revision: intPtr(3)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.save(Request{ID: "15", Op: "save", Day: date, Revision: intPtr(4)}); err != nil {
		t.Fatal(err)
	}
	preview, err := s.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}
	if preview.State != PlanReady || len(preview.Operations) != 2 {
		t.Fatalf("expected a two-create preview: %+v", preview)
	}
	rec := approveReadyPlan(t, s, date)

	// Between the first dispatch's read-back and the second operation's drift
	// check, an outside actor adds an unexpected remote entry.
	fake.injectWeek = func() {
		if len(fake.writeCalls) > 0 && len(fake.days[date]) == 1 {
			fake.days[date] = append(fake.days[date], map[string]any{
				"id": "r-foreign", "startTime": "13:00", "endTime": "14:00",
				"classificationId": "cat-1", "description": "Outside change", "status": "logged",
			})
		}
	}

	res, err := s.submitPlan(Request{ID: "s1", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitPartialStopped || res.Confirmed != 1 || res.Attempts != 1 || res.Unattempted != 1 {
		t.Fatalf("drift-stopped receipt: %+v", res)
	}
	if len(fake.writeCalls) != 1 {
		t.Fatalf("later operations must not dispatch after drift: %+v", fake.writeCalls)
	}
	hasDrift := false
	for _, b := range res.Blockers {
		if b.Code == "external_remote_drift" {
			hasDrift = true
		}
	}
	if !hasDrift {
		t.Fatalf("expected an explicit drift blocker: %+v", res.Blockers)
	}
}

func TestResolveMigratedOutcomeClearsOnlyItsOwnBlocker(t *testing.T) {
	s, fake, store, date := createReadyFixture(t)

	// Inject one migrated unresolved outcome row (uncertain legacy write that
	// matches the local intended content) plus its durable observation.
	obs := &RemoteObservation{
		ObservationID: "obs-mig", Date: date, ObservedAt: "2026-10-21T00:00:00Z", Complete: true,
		Entries: []RemoteEntry{{RemoteID: "r-mig", Date: date, StartTime: "09:00", EndTime: "10:00",
			ClassificationID: "cat-1", Description: "Synthetic work", ContentObserved: true}},
	}
	st := store.State()
	if st.Remote == nil {
		st.Remote = &RemoteState{}
	}
	st.Remote.Observations[date] = obs
	st.Remote.Outcomes = append(st.Remote.Outcomes, WriteOutcome{
		OperationKey: "legacy-op-1", LocalID: "iv-20261020-1", Action: "create", Status: IntentUncertain,
		Date: date, StartTime: "09:00", EndTime: "10:00", ClassificationID: "cat-1",
		Description: "Synthetic work", ErrorCode: CodeWriteUncertain, RecordedAt: "2026-10-21T00:00:00Z",
	})
	if err := store.CommitState(store.State().cloneState()); err != nil {
		t.Fatal(err)
	}

	// The migrated row is addressable by a stable reference and blocks.
	status, err := s.submissionStatus(Request{ID: "1", Op: "submission_status", Day: date})
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Unresolved) != 1 || status.Unresolved[0].Origin != "migrated" ||
		status.Unresolved[0].WriteRef != "mw-legacy-op-1" {
		t.Fatalf("migrated unresolved rows: %+v", status.Unresolved)
	}

	// adopt requires content that matches the intended write AND the actual
	// timezone-resolved allocation (R4F6): the observed legacy entry matches
	// the outcome's intended content and the local interval's allocation, so
	// it can be bound.
	rr, err := s.resolveWrite(Request{ID: "2", Op: "resolve_write", WriteRef: "mw-legacy-op-1",
		ObservationID: obs.ObservationID, Decision: DecisionWriteAdopt, RemoteID: "r-mig"})
	if err != nil {
		t.Fatal(err)
	}
	if !rr.Resolved || !rr.Bound || rr.Origin != "migrated" {
		t.Fatalf("migrated adoption: %+v", rr)
	}
	if len(fake.writeCalls) != 0 {
		t.Fatal("recovery must never dispatch anything")
	}
	bindings := store.State().Remote.Bindings
	if len(bindings) != 1 || bindings[0].RemoteID != "r-mig" || bindings[0].LocalID != "iv-20261020-1" {
		t.Fatalf("adopted binding: %+v", bindings)
	}
	outcome := store.State().Remote.Outcomes[0]
	if outcome.Status != IntentUncertain || outcome.Resolution != DecisionWriteAdopt {
		t.Fatalf("original audit must be preserved with the new decision beside it: %+v", outcome)
	}
	status, err = s.submissionStatus(Request{ID: "3", Op: "submission_status", Day: date})
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Unresolved) != 0 {
		t.Fatalf("the addressed blocker must clear: %+v", status.Unresolved)
	}

	// Ordinary migrated confirmed/failed history is never treated as pending.
	st = store.State()
	st.Remote.Outcomes = append(st.Remote.Outcomes, WriteOutcome{
		OperationKey: "legacy-op-2", LocalID: "iv-20261020-2", Status: "confirmed",
		Date: date, RecordedAt: "2026-10-21T01:00:00Z", Resolved: false,
	})
	if err := store.CommitState(store.State().cloneState()); err != nil {
		t.Fatal(err)
	}
	status, err = s.submissionStatus(Request{ID: "4", Op: "submission_status", Day: date})
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Unresolved) != 0 {
		t.Fatalf("confirmed history must not be pending: %+v", status.Unresolved)
	}
}

// TestDirtyAffectedSpillDraftBlocksApprovalAndSubmit (R4F2): an open staged
// draft on the origin day must not silently affect the plan's affected day.
// The guard uses each open session's saved AND staged allocation footprint, so
// a prior-day spill draft blocks both approval and consumption; discarding
// the draft restores eligibility. Zero writes are made while blocked.
func TestDirtyAffectedSpillDraftBlocksApprovalAndSubmit(t *testing.T) {
	s, fake, _, date := createReadyFixture(t)

	draft := func() {
		if _, err := s.openDay(Request{ID: "d0", Op: "open_day", Day: "2026-10-19"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.addInterval(Request{ID: "d1", Op: "add_interval", Day: "2026-10-19", Revision: intPtr(0),
			Text: "2026-10-19 23:30-00:30 unsaved spill draft"}); err != nil {
			t.Fatal(err)
		}
	}
	// Staged edits live only in the process that made them (there is no
	// cancel op since serve retired, #272): dropping the open day is the
	// draft's process ending.
	cancelDraft := func() { delete(s.days, "2026-10-19") }

	// Approval is refused while the affected draft exists.
	draft()
	if _, err := s.previewPlan(date); err == nil {
		t.Fatal("an open session whose staged allocations touch the plan's affected dates must block the preview")
	}
	if _, err := s.approvePlan(Request{ID: "1", Op: "approve_plan", Day: date, PlanID: "plan-any"}); err == nil {
		t.Fatal("approval must be rejected while such a draft exists")
	}
	if len(fake.writeCalls) != 0 {
		t.Fatalf("no writes may happen on the blocked path: %+v", fake.writeCalls)
	}

	// Cancellation restores eligibility.
	cancelDraft()
	rec := approveReadyPlan(t, s, date)

	// Consumption re-renders under the same guard: blocked before dispatch.
	draft()
	res, err := s.submitPlan(Request{ID: "2", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitBlockedBeforeDispatch || res.Attempts != 0 || res.Confirmed != 0 {
		t.Fatalf("the affected draft must block consumption before any dispatch: %+v", res)
	}
	if len(fake.writeCalls) != 0 {
		t.Fatalf("blocked consumption must make zero writes: %+v", fake.writeCalls)
	}

	// Cancelling the draft again restores a confirmed submission.
	cancelDraft()
	res, err = s.submitPlan(Request{ID: "3", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitAllConfirmed || len(fake.writeCalls) != 1 {
		t.Fatalf("cancellation must restore eligibility: %+v / %v", res, fake.writeCalls)
	}
}

// TestUnrelatedDirtySessionKeepsSubmissionEligible (R4F2 control): a dirty
// session whose saved and staged allocations do NOT touch the plan's affected
// dates never blocks approval or consumption.
func TestUnrelatedDirtySessionKeepsSubmissionEligible(t *testing.T) {
	s, fake, _, date := createReadyFixture(t)
	if _, err := s.openDay(Request{ID: "d0", Op: "open_day", Day: "2026-10-21"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.addInterval(Request{ID: "d1", Op: "add_interval", Day: "2026-10-21", Revision: intPtr(0),
		Text: "2026-10-21 09:00-10:00 unrelated draft"}); err != nil {
		t.Fatal(err)
	}
	rec := approveReadyPlan(t, s, date)
	res, err := s.submitPlan(Request{ID: "1", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitAllConfirmed || len(fake.writeCalls) != 1 {
		t.Fatalf("an unrelated dirty date must not block: %+v / %v", res, fake.writeCalls)
	}
}

// TestChangedExistingEntryStopsLaterOperations (R4F3): the batch precondition
// is exact content, not just id membership. An existing baseline entry that an
// outside actor changes before a later dispatch stops the batch after exactly
// one write; the fresh snapshot is never absorbed over the baseline.
func TestChangedExistingEntryStopsLaterOperations(t *testing.T) {
	date := "2026-10-20"
	fake := &fakeMCP{
		day: date,
		entries: []map[string]any{
			{"id": "r-ext", "startTime": "16:00", "endTime": "17:00",
				"classificationId": "cat-1", "description": "Outside", "status": "logged"},
		},
		catOptions: []map[string]any{{"id": "cat-1", "kind": "classification", "label": "Synthetic Category"}},
		catText:    "- Synthetic Category (SC) [Development] · cat-1",
	}
	s, _, _ := openFixtureStoreAt(t, fake, date)
	fake.writeTools = true
	if _, err := s.openDay(Request{ID: "1", Op: "open_day", Day: date}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.addInterval(Request{ID: "2", Op: "add_interval", Day: date, Revision: intPtr(0),
		Text: date + " 09:00-10:00 Synthetic work"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.addInterval(Request{ID: "3", Op: "add_interval", Day: date, Revision: intPtr(0),
		Text: date + " 11:00-12:00 Later work"}); err != nil {
		t.Fatal(err)
	}
	cat, err := s.classifications(Request{ID: "4", Op: "classifications"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"iv-20261020-1", "iv-20261020-2"} {
		if _, err := s.selectClassification(Request{ID: "5", Op: "select_classification", Day: date,
			Revision: intPtr(0), IntervalID: id, CatalogID: cat.CatalogID,
			ClassificationID: "cat-1"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.confirm(Request{ID: "6", Op: "confirm", Day: date, Revision: intPtr(0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.save(Request{ID: "7", Op: "save", Day: date, Revision: intPtr(1)}); err != nil {
		t.Fatal(err)
	}
	preview, err := s.previewPlan(date)
	if err != nil {
		t.Fatalf("a non-overlapping unbound remote must not block a ready plan: %v", err)
	}
	if preview.State != PlanReady || len(preview.Operations) != 2 {
		t.Fatalf("expected a ready two-create preview: %+v", preview)
	}
	rec := approveReadyPlan(t, s, date)

	// Between the first read-back and the second operation, an outside actor
	// moves the pre-existing baseline entry.
	fake.injectWeek = func() {
		if len(fake.writeCalls) > 0 {
			for _, e := range fake.days[date] {
				if e["id"] == "r-ext" {
					e["startTime"], e["endTime"] = "11:00", "12:00"
				}
			}
		}
	}
	res, err := s.submitPlan(Request{ID: "9", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitPartialStopped || res.Confirmed != 1 || res.Unattempted != 1 {
		t.Fatalf("changed baseline content must stop later operations: %+v", res)
	}
	if len(fake.writeCalls) != 1 {
		t.Fatalf("only the first write may happen: %+v", fake.writeCalls)
	}
	hasDrift := false
	for _, b := range res.Blockers {
		if b.Code == "external_remote_drift" {
			hasDrift = true
		}
	}
	if !hasDrift {
		t.Fatalf("expected the drift blocker: %+v", res.Blockers)
	}
}

// TestEditAcknowledgingDifferentIdentityStaysUnresolved (R4F4): when the
// provider acknowledges a different entry id than the edit's approved bound
// identity, the write stays unresolved and never reports confirmed, even when
// other entries carry matching content. The attempted and acknowledged
// identities stay distinct audit, the durable binding and LastObserved are not
// touched, and the unresolved state survives a restart so no replay happens.
func TestEditAcknowledgingDifferentIdentityStaysUnresolved(t *testing.T) {
	s, fake, _, date := editReadyFixture(t)
	fake.setWriteMultiDays(map[string][]map[string]any{
		date: {map[string]any{"id": "r-1", "startTime": "08:00", "endTime": "10:00",
			"classificationId": "cat-1", "description": "Synthetic work", "status": "logged"}},
	})
	rec := approveReadyPlan(t, s, date)

	before, err := s.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}
	fake.wrongAckID = "r-other"

	res, err := s.submitPlan(Request{ID: "1", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitPartialStopped || res.Confirmed != 0 || res.Unresolved != 1 {
		t.Fatalf("a wrong edit acknowledgment must stay unresolved: %+v", res)
	}
	if len(fake.writeCalls) != 1 {
		t.Fatalf("exactly one dispatch happened and is never retried: %+v", fake.writeCalls)
	}
	st := s.store.State()
	if len(st.Intents) != 1 {
		t.Fatalf("intent inventory: %+v", st.Intents)
	}
	it := st.Intents[0]
	if it.Status != IntentUnidentified || it.ErrorCode != editIdentityMismatch ||
		it.RemoteID != before.Operations[0].RemoteID || it.AcknowledgedID != "r-other" {
		t.Fatalf("identity audit must keep the attempted and acknowledged ids distinct: %+v", it)
	}
	// The durable binding is untouched: neither confirmed content nor
	// LastObserved may claim a confirmed effect from an unresolved write.
	bs := st.Remote.Bindings
	if len(bs) != 1 || bs[0].RemoteID != "r-1" || bs[0].LastObserved == nil ||
		bs[0].LastObserved.StartTime != "08:00" {
		t.Fatalf("binding must keep its adoption-time state: %+v", bs)
	}
	// The provider's own entry WAS mutated but nothing may claim it confirmed.
	still, err := s.submissionStatus(Request{ID: "2", Op: "submission_status", Day: date})
	if err != nil {
		t.Fatal(err)
	}
	if len(still.Unresolved) != 1 || still.Unresolved[0].Origin != "native" {
		t.Fatalf("native unresolved rows: %+v", still.Unresolved)
	}

	// Restart: the unresolved intent is durable, blocks, and cannot replay.
	s2, err := restartSession(s)
	if err != nil {
		t.Fatal(err)
	}
	still, err = s2.submissionStatus(Request{ID: "3", Op: "submission_status", Day: date})
	if err != nil {
		t.Fatal(err)
	}
	if len(still.Unresolved) != 1 || still.Unresolved[0].Origin != "native" {
		t.Fatalf("restart must keep the unresolved write visible: %+v", still.Unresolved)
	}
	res2, err := s2.submitPlan(Request{ID: "4", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Status == SubmitAllConfirmed || len(fake.writeCalls) != 1 {
		t.Fatalf("neither replay nor new dispatch may happen: %+v / %v", res2, fake.writeCalls)
	}
}

// TestEditSubmissionConfirmsWithExactBody (R4F4 control): the normal edit path
// still confirms against exactly the approved bound identity, with the exact
// approved body (including the rendered reason) and a durable one-to-one
// binding.
func TestEditSubmissionConfirmsWithExactBody(t *testing.T) {
	s, fake, _, date := editReadyFixture(t)
	fake.setWriteMultiDays(map[string][]map[string]any{
		date: {map[string]any{"id": "r-1", "startTime": "08:00", "endTime": "10:00",
			"classificationId": "cat-1", "description": "Synthetic work", "status": "logged"}},
	})
	rec := approveReadyPlan(t, s, date)
	preview, err := s.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}
	want := preview.Operations[0]

	res, err := s.submitPlan(Request{ID: "1", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitAllConfirmed || res.Confirmed != 1 || res.Unresolved != 0 {
		t.Fatalf("the normal one-edit submission must confirm: %+v", res)
	}
	if len(fake.writeCalls) != 1 || fake.writeCalls[0] != toolEditTime {
		t.Fatalf("exactly one edit dispatch: %+v", fake.writeCalls)
	}
	body := fake.writeBodies[0]
	if body["entryId"] != "r-1" || body["startTime"] != want.StartTime ||
		body["endTime"] != want.EndTime || body["description"] != want.Description ||
		body["reason"] != want.Reason || want.Reason == "" {
		t.Fatalf("the edit body must be the approved one: %+v want %+v", body, want)
	}
	bs := s.store.State().Remote.Bindings
	if len(bs) != 1 || bs[0].RemoteID != "r-1" || bs[0].LastObserved == nil ||
		bs[0].LastObserved.StartTime != want.StartTime {
		t.Fatalf("the binding must record the verified read-back: %+v", bs)
	}
}

// TestResolveMigratedOutcomeRejectsWrongAllocationDate (R4F6): a row whose
// recorded write date is not a timezone-resolved allocation of its local
// interval cannot be bound; the blocker remains visible.
func TestResolveMigratedOutcomeRejectsWrongAllocationDate(t *testing.T) {
	s, fake, store, _ := createReadyFixture(t)
	wrongDate := "2026-10-21"
	obs := &RemoteObservation{
		ObservationID: "obs-wrong", Date: wrongDate, ObservedAt: "2026-10-22T00:00:00Z", Complete: true,
		Entries: []RemoteEntry{{RemoteID: "r-wrong", Date: wrongDate, StartTime: "09:00", EndTime: "10:00",
			ClassificationID: "cat-1", Description: "Synthetic work", ContentObserved: true}},
	}
	st := store.State()
	if st.Remote == nil {
		st.Remote = &RemoteState{}
	}
	st.Remote.Observations[wrongDate] = obs
	st.Remote.Outcomes = append(st.Remote.Outcomes, WriteOutcome{
		OperationKey: "legacy-wrong", LocalID: "iv-20261020-1", Action: "create", Status: IntentUncertain,
		Date: wrongDate, StartTime: "09:00", EndTime: "10:00", ClassificationID: "cat-1",
		Description: "Synthetic work", ErrorCode: CodeWriteUncertain, RecordedAt: "2026-10-22T00:00:00Z",
	})
	if err := store.CommitState(store.State().cloneState()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.resolveWrite(Request{ID: "1", Op: "resolve_write", WriteRef: "mw-legacy-wrong",
		ObservationID: obs.ObservationID, Decision: DecisionWriteAdopt, RemoteID: "r-wrong"}); err == nil {
		t.Fatal("a local interval that does not allocate on the recorded write date must never be bound")
	}
	if len(fake.writeCalls) != 0 {
		t.Fatal("recovery must never dispatch anything")
	}
	outcome := store.State().Remote.Outcomes[0]
	if outcome.Resolved || outcome.Resolution != "" {
		t.Fatalf("the row must stay unresolved and unbound: %+v", outcome)
	}
	if len(store.State().Remote.Bindings) != 0 {
		t.Fatalf("no binding may be created: %+v", store.State().Remote.Bindings)
	}
}

// TestResolveMigratedOutcomeRejectsUnknownCrosswalk (R4F6): a row claiming a
// migration that no durable receipt backs is insufficient legacy provenance
// and stays a visible blocker.
func TestResolveMigratedOutcomeRejectsUnknownCrosswalk(t *testing.T) {
	s, fake, store, date := createReadyFixture(t)
	obs := &RemoteObservation{
		ObservationID: "obs-x", Date: date, ObservedAt: "2026-10-21T00:00:00Z", Complete: true,
		Entries: []RemoteEntry{{RemoteID: "r-mig", Date: date, StartTime: "09:00", EndTime: "10:00",
			ClassificationID: "cat-1", Description: "Synthetic work", ContentObserved: true}},
	}
	st := store.State()
	if st.Remote == nil {
		st.Remote = &RemoteState{}
	}
	st.Remote.Observations[date] = obs
	st.Remote.Outcomes = append(st.Remote.Outcomes, WriteOutcome{
		OperationKey: "legacy-x", LocalID: "iv-20261020-1", Action: "create", Status: IntentUncertain,
		MigrationID: "mig-unknown", Date: date, StartTime: "09:00", EndTime: "10:00",
		ClassificationID: "cat-1", Description: "Synthetic work",
		ErrorCode: CodeWriteUncertain, RecordedAt: "2026-10-21T00:00:00Z",
	})
	if err := store.CommitState(store.State().cloneState()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.resolveWrite(Request{ID: "1", Op: "resolve_write", WriteRef: "mw-legacy-x",
		ObservationID: obs.ObservationID, Decision: DecisionWriteAdopt, RemoteID: "r-mig"}); err == nil {
		t.Fatal("an unknown migration crosswalk must fail closed")
	}
	if len(fake.writeCalls) != 0 {
		t.Fatal("recovery must never dispatch anything")
	}
	if got := store.State().Remote.Outcomes[0]; got.Resolved {
		t.Fatalf("the blocker must remain: %+v", got)
	}
}

// TestUnsavedFinalRemovalCannotAuthorizeSavedPlan (R4F2 continuation): an
// unsaved confirmation that removes the final interval empties BOTH session
// lists while the preview still renders the durable saved work. The saved
// footprint of the dirty origin is part of the guard, so neither approval nor
// consumption may authorize the stale saved view, and the durable state is
// untouched.
func TestUnsavedFinalRemovalCannotAuthorizeSavedPlan(t *testing.T) {
	s, fake, store, date := createReadyFixture(t)
	rec := approveReadyPlan(t, s, date)

	if _, err := s.removeInterval(Request{ID: "r1", Op: "remove_interval", Day: date,
		Revision: intPtr(2), IntervalID: "iv-20261020-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.confirm(Request{ID: "r2", Op: "confirm", Day: date, Revision: intPtr(2)}); err != nil {
		t.Fatal(err)
	}
	if open := s.days[date]; open == nil || len(open.Confirmed) != 0 || len(open.Staged) != 0 {
		t.Fatalf("precondition: the unsaved confirmation must empty the session lists: %+v", open)
	}
	if got := len(store.State().Days[date].Confirmed); got != 1 {
		t.Fatalf("precondition: the durable saved interval must still exist: %d", got)
	}

	res, err := s.submitPlan(Request{ID: "s1", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitBlockedBeforeDispatch || !hasBlocker(res.Blockers, CodeDirtySession) {
		t.Fatalf("the saved footprint of an unsaved final removal must block consumption: %+v", res)
	}
	if res.Attempts != 0 || res.Confirmed != 0 || len(fake.writeCalls) != 0 {
		t.Fatalf("the blocked consumption must make zero writes: %+v / %v", res, fake.writeCalls)
	}
	if got := len(store.State().Days[date].Confirmed); got != 1 {
		t.Fatalf("a blocked consumption must not alter durable work: %d", got)
	}
	approval := approvalByDateAndID(store.State(), date, rec.ApprovalID)
	if approval == nil || approval.State != ApprovalActive {
		t.Fatalf("the unconsumed approval must remain active: %+v", approval)
	}
}

// TestUnsavedPriorDaySpillRemovalCannotAuthorizeSavedPlan (R4F2 continuation,
// prior-day spill): removing the final interval of a prior-day interval whose
// allocation rendered on the planned date leaves empty current origin lists,
// but the saved spill footprint still guards the planned date.
func TestUnsavedPriorDaySpillRemovalCannotAuthorizeSavedPlan(t *testing.T) {
	s, fake, store, date := createReadyFixture(t)
	const origin = "2026-10-19"

	if _, err := s.openDay(Request{ID: "o1", Op: "open_day", Day: origin}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.addInterval(Request{ID: "o2", Op: "add_interval", Day: origin, Revision: intPtr(0),
		Text: origin + " 23:00-01:00 late session"}); err != nil {
		t.Fatal(err)
	}
	cat, err := s.classifications(Request{ID: "o3", Op: "classifications"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.selectClassification(Request{ID: "o4", Op: "select_classification", Day: origin,
		Revision: intPtr(0), IntervalID: "iv-20261019-1", CatalogID: cat.CatalogID,
		ClassificationID: "cat-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.confirm(Request{ID: "o5", Op: "confirm", Day: origin, Revision: intPtr(0)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.save(Request{ID: "o6", Op: "save", Day: origin, Revision: intPtr(1)}); err != nil {
		t.Fatal(err)
	}
	rec := approveReadyPlan(t, s, date)

	// Remove the final interval of the prior day and confirm revision 2 without
	// saving: the spill allocation on the planned date is gone from the session
	// but still present in the durable saved day.
	if _, err := s.removeInterval(Request{ID: "o7", Op: "remove_interval", Day: origin,
		Revision: intPtr(1), IntervalID: "iv-20261019-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.confirm(Request{ID: "o8", Op: "confirm", Day: origin, Revision: intPtr(1)}); err != nil {
		t.Fatal(err)
	}
	if open := s.days[origin]; open == nil || len(open.Confirmed) != 0 || len(open.Staged) != 0 {
		t.Fatalf("precondition: the unsaved confirmation must empty the prior-day lists: %+v", open)
	}

	res, err := s.submitPlan(Request{ID: "s1", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitBlockedBeforeDispatch || !hasBlocker(res.Blockers, CodeDirtySession) {
		t.Fatalf("the saved prior-day spill footprint must block consumption: %+v", res)
	}
	if len(fake.writeCalls) != 0 {
		t.Fatalf("the blocked consumption must make zero writes: %v", fake.writeCalls)
	}
	if got := len(store.State().Days[origin].Confirmed); got != 1 {
		t.Fatalf("the durable prior-day work must stay intact: %d", got)
	}
	if got := len(store.State().Days[date].Confirmed); got != 1 {
		t.Fatalf("the durable planned-day work must stay intact: %d", got)
	}
}

// TestZeroOperationConsumptionPreActivationFailureIsHonest (R4F7): the local
// commit that consumes a zero-operation approval fails BEFORE activation. The
// receipt must not claim consumption or a durable execution, the approval stays
// active, and an explicit later submit consumes it exactly once.
func TestZeroOperationConsumptionPreActivationFailureIsHonest(t *testing.T) {
	s, fake, store, date := createReadyFixture(t)
	first := approveReadyPlan(t, s, date)
	if _, err := s.submitPlan(Request{ID: "s0", Op: "submit_plan", Day: date, ApprovalID: first.ApprovalID}); err != nil {
		t.Fatal(err)
	}
	preview, err := s.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Operations) != 0 {
		t.Fatalf("precondition: mapped unchanged work must preview zero operations: %+v", preview.Operations)
	}
	rec, err := s.approvePlan(Request{ID: "a2", Op: "approve_plan", Day: date, PlanID: preview.PlanID})
	if err != nil {
		t.Fatal(err)
	}
	writes := len(fake.writeCalls)
	executions := len(store.State().Executions)

	restoreHook := storeFaultHook
	defer func() { storeFaultHook = restoreHook }()
	fake.injectCat = func() {
		// Armed only when the submission's fresh classification proof runs,
		// which is AFTER the fresh preview persisted its observation.
		storeFaultHook = func(stage string) error {
			if stage == "before_rename" {
				return errSyntheticFault
			}
			return nil
		}
	}
	defer func() { fake.injectCat = nil }()

	res, err := s.submitPlan(Request{ID: "s1", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	storeFaultHook = restoreHook
	fake.injectCat = nil
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitPersistenceFailed || res.ExecutionID != "" {
		t.Fatalf("a pre-activation failure must be reported honestly, not as success: %+v", res)
	}
	if res.Attempts != 0 || res.Confirmed != 0 || !hasBlocker(res.Blockers, CodeWriteFailed) {
		t.Fatalf("the failure receipt must claim no work and name the write failure: %+v", res)
	}
	approval := approvalByDateAndID(store.State(), date, rec.ApprovalID)
	if approval == nil || approval.State != ApprovalActive || approval.ExecutionID != "" {
		t.Fatalf("nothing activated, so the approval must stay active: %+v", approval)
	}
	if got := len(store.State().Executions); got != executions {
		t.Fatalf("a failed consumption must not append an execution: %d -> %d", executions, got)
	}
	if len(fake.writeCalls) != writes {
		t.Fatalf("a zero-operation consumption may never dispatch: %v", fake.writeCalls)
	}
	if store.Uncertain() {
		t.Fatal("a failure before activation must not become sticky uncertainty")
	}

	// Explicit resubmission consumes the same approval exactly once.
	res, err = s.submitPlan(Request{ID: "s2", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitAllConfirmed || res.Attempts != 0 {
		t.Fatalf("the same active approval must consume cleanly on an explicit retry: %+v", res)
	}
	if got := len(store.State().Executions); got != executions+1 {
		t.Fatalf("exactly one execution must be recorded: %d", got)
	}
	approval = approvalByDateAndID(store.State(), date, rec.ApprovalID)
	if approval == nil || approval.State != ApprovalExecuting {
		t.Fatalf("the approval must be consumed after the clean retry: %+v", approval)
	}
	if len(fake.writeCalls) != writes {
		t.Fatalf("consumption must still dispatch nothing: %v", fake.writeCalls)
	}

	// Restart: consumed once, never again.
	s2, err := restartSession(s)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s2.submitPlan(Request{ID: "s3", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if replay.Status != SubmitBlockedBeforeDispatch || len(fake.writeCalls) != writes {
		t.Fatalf("a consumed approval must never be consumable again: %+v", replay)
	}
}

// TestZeroOperationApprovalProofSurvivesRestart (R4F8): a legitimate EMPTY
// choice proof is encoded explicitly, reloads as a non-nil empty map, and the
// persisted approval consumes durably with zero writes after a clean restart.
func TestZeroOperationApprovalProofSurvivesRestart(t *testing.T) {
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
	if rec.Summary.Choices == nil || len(rec.Summary.Choices) != 0 {
		t.Fatalf("a zero-operation approval must carry an established empty proof: %+v", rec.Summary)
	}
	raw, err := os.ReadFile(s.cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"choices": {}`) {
		t.Fatalf("the established empty proof must be encoded explicitly, not omitted: %s", raw)
	}
	writes := len(fake.writeCalls)

	s2, err := restartSession(s)
	if err != nil {
		t.Fatal(err)
	}
	reloaded := approvalByDateAndID(s2.store.State(), date, rec.ApprovalID)
	if reloaded == nil || reloaded.Summary.Choices == nil || len(reloaded.Summary.Choices) != 0 {
		t.Fatalf("restart must reload an established empty proof, not a missing one: %+v", reloaded)
	}
	res, err := s2.submitPlan(Request{ID: "s1", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitAllConfirmed || res.Attempts != 0 || res.Confirmed != 0 {
		t.Fatalf("the persisted zero-operation approval must consume cleanly: %+v", res)
	}
	if len(fake.writeCalls) != writes {
		t.Fatalf("zero-operation consumption must dispatch nothing: %v", fake.writeCalls)
	}
	if len(s2.store.State().Executions) != 2 {
		t.Fatalf("exactly one new execution must be durable: %+v", s2.store.State().Executions)
	}
	replay, err := s2.submitPlan(Request{ID: "s2", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if replay.Status != SubmitBlockedBeforeDispatch {
		t.Fatalf("a second consumption must be refused: %+v", replay)
	}

	// Restart again: the consumption and the refusal both survive.
	s3, err := restartSession(s2)
	if err != nil {
		t.Fatal(err)
	}
	replay, err = s3.submitPlan(Request{ID: "s3", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if replay.Status != SubmitBlockedBeforeDispatch {
		t.Fatalf("the second restart must still refuse consumption: %+v", replay)
	}
}

// TestMissingLegacyChoiceProofStillCannotAuthorize (R4F8 gate): deleting the
// established proof from a persisted row reproduces a pre-proof dev.10 shape.
// It reloads as nil, remains inert and is never reconstructed as approval.
func TestMissingLegacyChoiceProofStillCannotAuthorize(t *testing.T) {
	s, fake, _, date := createReadyFixture(t)
	rec := approveReadyPlan(t, s, date)
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
	rows, _ := state["approvals"].([]any)
	if len(rows) != 1 {
		t.Fatalf("precondition: one approval expected: %v", rows)
	}
	summary, _ := rows[0].(map[string]any)["summary"].(map[string]any)
	delete(summary, "choices")
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
	loaded := approvalByDateAndID(store.State(), date, rec.ApprovalID)
	if loaded == nil || loaded.Summary.Choices != nil {
		t.Fatalf("a removed proof must reload as missing, never as empty: %+v", loaded)
	}
	res, err := s2.submitPlan(Request{ID: "s1", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitBlockedBeforeDispatch || !hasBlocker(res.Blockers, CodeStaleApproval) {
		t.Fatalf("a missing legacy proof must fail closed: %+v", res)
	}
	if len(fake.writeCalls) != 0 {
		t.Fatalf("an unauthorized legacy approval must never dispatch: %v", fake.writeCalls)
	}
}

// TestZeroOperationConsumptionPostRenameUncertaintyStaysSticky (R4F7 control):
// the pre-existing post-rename uncertainty behavior is unchanged. An activated
// file whose directory sync failed is reported as durability_uncertain, blocks
// further claims in-process, and loads as the activated consumption on restart.
func TestZeroOperationConsumptionPostRenameUncertaintyStaysSticky(t *testing.T) {
	s, fake, store, date := createReadyFixture(t)
	first := approveReadyPlan(t, s, date)
	if _, err := s.submitPlan(Request{ID: "s0", Op: "submit_plan", Day: date, ApprovalID: first.ApprovalID}); err != nil {
		t.Fatal(err)
	}
	preview, err := s.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.approvePlan(Request{ID: "a2", Op: "approve_plan", Day: date, PlanID: preview.PlanID})
	if err != nil {
		t.Fatal(err)
	}
	writes := len(fake.writeCalls)

	restoreHook := storeFaultHook
	fake.injectCat = func() {
		storeFaultHook = func(stage string) error {
			if stage == "directory_sync" {
				return errSyntheticFault
			}
			return nil
		}
	}
	res, err := s.submitPlan(Request{ID: "s1", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	fake.injectCat = nil
	storeFaultHook = restoreHook
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitDurabilityUncertain {
		t.Fatalf("a post-rename failure must stay durability_uncertain: %+v", res)
	}
	if !store.Uncertain() || !s.store.Uncertain() {
		t.Fatal("post-rename failure must leave the store sticky-uncertain")
	}
	if len(fake.writeCalls) != writes {
		t.Fatalf("zero-operation uncertainty must not dispatch: %v", fake.writeCalls)
	}

	// Further claims are refused in-process.
	res, err = s.submitPlan(Request{ID: "s2", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitDurabilityUncertain || len(fake.writeCalls) != writes {
		t.Fatalf("the sticky store must refuse further claims: %+v", res)
	}

	// The activated file is the durable consumption: restart loads it.
	s2, err := restartSession(s)
	if err != nil {
		t.Fatal(err)
	}
	approval := approvalByDateAndID(s2.store.State(), date, rec.ApprovalID)
	if approval == nil || approval.State != ApprovalExecuting {
		t.Fatalf("the activated consumption must load after restart: %+v", approval)
	}
	if got := len(s2.store.State().Executions); got != 2 {
		t.Fatalf("the activated execution row must load after restart: %d", got)
	}
	replay, err := s2.submitPlan(Request{ID: "s3", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if replay.Status != SubmitBlockedBeforeDispatch || len(fake.writeCalls) != writes {
		t.Fatalf("the consumed approval must not be consumable after restart: %+v", replay)
	}
}
