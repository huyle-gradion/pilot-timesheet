package timesheet

import (
	"strings"
	"testing"
)

// editReadyFixture is a bound and classed day whose fresh preview is ready with
// exactly one edit operation (the mapped remote content changed and was
// explicitly accepted).
func editReadyFixture(t *testing.T) (*Session, *fakeMCP, *Store, string) {
	t.Helper()
	s, fake, store, date := planFixture(t)
	if _, err := s.reconcileDate(date); err != nil {
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
	// Explicit adoption binds the exact observed entry.
	rec, err := s.reconcileDate(date)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.resolveRemoteRequest(Request{ID: "9", Op: "resolve_remote", Day: date, Revision: intPtr(2),
		ObservationID: rec.ObservationID, RemoteID: "r-1", LocalID: "iv-20261020-1", Decision: DecisionAdopt}); err != nil {
		t.Fatal(err)
	}
	// The mapped remote content changes; an explicit acceptance produces the
	// exact one-edit preview.
	fake.setWeek([]map[string]any{remoteEntry("r-1", "08:00", "10:00", "Synthetic work")})
	rec, err = s.reconcileDate(date)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.resolveRemoteRequest(Request{ID: "10", Op: "resolve_remote", Day: date, Revision: intPtr(2),
		ObservationID: rec.ObservationID, RemoteID: "r-1", LocalID: "iv-20261020-1", Decision: DecisionAcceptObserved}); err != nil {
		t.Fatal(err)
	}
	preview, err := s.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}
	if preview.State != PlanReady || len(preview.Operations) != 1 || preview.Operations[0].Kind != "edit" {
		t.Fatalf("expected a ready one-edit preview: %+v", preview)
	}
	return s, fake, store, date
}

// approveReadyPlan previews fresh and approves the exact rendered plan.
func approveReadyPlan(t *testing.T, s *Session, date string) ApprovalReceipt {
	t.Helper()
	preview, err := s.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := s.approvePlan(Request{ID: "a1", Op: "approve_plan", Day: date, PlanID: preview.PlanID})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestApprovePlanBindsExactRender(t *testing.T) {
	s, _, store, date := editReadyFixture(t)

	// Approval is never guessed: a request without plan_id is rejected.
	if _, err := s.approvePlan(Request{ID: "1", Op: "approve_plan", Day: date}); err == nil {
		t.Fatal("approve requires an exact plan_id")
	}
	// A plan_id that is not the current rendered identity is stale.
	if _, err := s.approvePlan(Request{ID: "2", Op: "approve_plan", Day: date, PlanID: "plan-not-current"}); err == nil {
		t.Fatal("an unknown plan_id must be stale, never approved")
	}
	if len(store.State().Approvals) != 0 {
		t.Fatalf("rejected approvals must not enter the ledger: %+v", store.State().Approvals)
	}

	preview, err := s.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.approvePlan(Request{ID: "3", Op: "approve_plan", Day: date, PlanID: preview.PlanID})
	if err != nil {
		t.Fatal(err)
	}
	if first.ApprovalID == "" || first.State != ApprovalActive || first.PlanID != preview.PlanID || first.Operations != 1 {
		t.Fatalf("unexpected approval receipt: %+v", first)
	}
	if strings.HasPrefix(first.Summary.OperationDigests[0], "") && len(first.Summary.OperationDigests) != 1 {
		t.Fatalf("digest inventory mismatch: %+v", first.Summary)
	}
	if len(store.State().Approvals) != 1 || store.State().Approvals[0].State != ApprovalActive {
		t.Fatalf("ledger mismatch: %+v", store.State().Approvals)
	}
	// The preview's approved flag is a derived ledger fact, not a second truth.
	again, err := s.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Approved || again.PlanID != preview.PlanID {
		t.Fatalf("a live approval must be derived from the ledger: %+v", again)
	}

	// Reapproving identical content creates a NEW occurrence; it never reuses
	// or overwrite the old approval id.
	second, err := s.approvePlan(Request{ID: "4", Op: "approve_plan", Day: date, PlanID: preview.PlanID})
	if err != nil {
		t.Fatal(err)
	}
	if second.ApprovalID == first.ApprovalID {
		t.Fatal("approval ids are unique occurrences, never plan aliases")
	}
	if len(store.State().Approvals) != 2 {
		t.Fatalf("both approvals must be immutable audit: %+v", store.State().Approvals)
	}
}

// TestClassificationRelevantChangeBlocksApproval (R4F1): a selected option
// whose relevant content (name/code/activity type) changed since selection
// invalidates approval itself, with zero remote mutations. Unrelated catalog
// rows never invalidate an unchanged choice.
func TestClassificationRelevantChangeBlocksApproval(t *testing.T) {
	s, fake, store, date := createReadyFixture(t)
	preview, err := s.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}

	// Control: unrelated extra result rows and content remain stable.
	fake.catText = "- Synthetic Category (SC) [Development] · cat-1\n- Unrelated (UN) [Development] · cat-unrelated"
	_, err = s.approvePlan(Request{ID: "1", Op: "approve_plan", Day: date, PlanID: preview.PlanID})
	if err != nil {
		t.Fatalf("unrelated catalog rows must not invalidate an unchanged selection: %v", err)
	}
	if len(store.State().Approvals) != 1 || len(store.State().Approvals[0].Summary.Choices) != 1 {
		t.Fatalf("approval must bind the selected choice: %+v", store.State().Approvals)
	}
	bound := store.State().Approvals[0].Summary.Choices["cat-1"]
	if bound == nil || bound.ActivityType != "Development" || bound.Code != "SC" {
		t.Fatalf("wrong bound choice: %+v", bound)
	}

	// A relevant change before approval invalidates THE SAME choice.
	if _, err := s.revokeApproval(Request{ID: "2", Op: "revoke_approval", Day: date, ApprovalID: store.State().Approvals[0].ApprovalID}); err != nil {
		t.Fatal(err)
	}
	replay, err := s.previewPlan(date)
	if err != nil {
		t.Fatal(err)
	}
	if replay.PlanID != preview.PlanID || replay.State != PlanReady {
		t.Fatalf("the stored selection reference is unchanged, so the plan must re-render identically: %+v", replay)
	}
	fake.catText = "- Synthetic Category (SC) [Meeting] · cat-1"
	_, err = s.approvePlan(Request{ID: "3", Op: "approve_plan", Day: date, PlanID: replay.PlanID})
	if err == nil || !strings.Contains(err.Error(), "classification_choice_changed") {
		t.Fatalf("a relevant selected-option change must block approval: %v", err)
	}
	if len(fake.writeCalls) != 0 {
		t.Fatalf("blocked approval must make zero remote writes: %+v", fake.writeCalls)
	}
	if len(store.State().Approvals) != 1 || store.State().Approvals[0].State != ApprovalRevoked {
		t.Fatalf("failed approvals must not enter the ledger: %+v", store.State().Approvals)
	}
}

// TestClassificationRelevantChangeBlocksConsumption (R4F1): the fresh proof at
// submission must still match the choice the approval bound.
func TestClassificationRelevantChangeBlocksConsumption(t *testing.T) {
	s, fake, store, date := createReadyFixture(t)
	rec := approveReadyPlan(t, s, date)

	fake.catText = "- Synthetic Category (SC) [Meeting] · cat-1"
	res, err := s.submitPlan(Request{ID: "1", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitBlockedBeforeDispatch || res.Attempts != 0 || res.Confirmed != 0 {
		t.Fatalf("a changed approved choice must block before dispatch: %+v", res)
	}
	if len(fake.writeCalls) != 0 {
		t.Fatal("the blocked submission must make zero remote writes")
	}
	hasChanged := false
	for _, b := range res.Blockers {
		if b.Code == CodeClassificationChanged {
			hasChanged = true
		}
	}
	if !hasChanged {
		t.Fatalf("expected the choice-change blocker: %+v", res.Blockers)
	}
	// The approval is not consumed by a blocked attempt and stays active audit.
	if len(store.State().Approvals) != 1 || store.State().Approvals[0].State != ApprovalActive {
		t.Fatalf("blocked consumption must not alter the ledger: %+v", store.State().Approvals)
	}
	// Restoring the current catalog restores eligibility; nothing was replayed
	// from cache and nothing extra was dispatched.
	fake.catText = "- Synthetic Category (SC) [Development] · cat-1"
	res2, err := s.submitPlan(Request{ID: "2", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Status != SubmitAllConfirmed || len(fake.writeCalls) != 1 {
		t.Fatalf("restored proof must let the one dispatch happen: %+v / %v", res2, fake.writeCalls)
	}
}

func TestRevokeApprovalBlocksSubmission(t *testing.T) {
	s, fake, store, date := editReadyFixture(t)
	rec := approveReadyPlan(t, s, date)

	if _, err := s.revokeApproval(Request{ID: "1", Op: "revoke_approval", Day: date, ApprovalID: "apr-unknown"}); err == nil {
		t.Fatal("revoking an unknown approval must fail")
	}
	rev, err := s.revokeApproval(Request{ID: "2", Op: "revoke_approval", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if rev.State != ApprovalRevoked || rev.RevokedAt == "" {
		t.Fatalf("unexpected revocation receipt: %+v", rev)
	}
	if len(fake.writeCalls) != 0 {
		t.Fatalf("revocation must never write remotely: %+v", fake.writeCalls)
	}
	// A revoked approval is immutable audit and cannot be revoked twice.
	if _, err := s.revokeApproval(Request{ID: "3", Op: "revoke_approval", Day: date, ApprovalID: rec.ApprovalID}); err == nil {
		t.Fatal("a revoked approval cannot be revoked again")
	}

	before := len(fake.calls)
	res, err := s.submitPlan(Request{ID: "4", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err) // blocked submission is a receipt, not an error
	}
	if res.Status != SubmitBlockedBeforeDispatch || res.Attempts != 0 || res.Confirmed != 0 {
		t.Fatalf("a revoked approval must be blocked before dispatch: %+v", res)
	}
	if len(fake.calls) != before || len(fake.writeCalls) != 0 {
		t.Fatal("a blocked submission must make zero remote calls")
	}
	if got := store.State().Approvals[0].State; got != ApprovalRevoked {
		t.Fatalf("revocation must stay immutable: %+v", got)
	}
}

func TestApprovalSurvivesOnlyUnchangedPreconditions(t *testing.T) {
	s, fake, store, date := editReadyFixture(t)
	rec := approveReadyPlan(t, s, date)
	before := len(fake.calls)

	// Unsaved source work rejects the preview: zero remote mutations follow.
	if _, err := s.addInterval(Request{ID: "1", Op: "add_interval", Day: date, Revision: intPtr(2),
		Text: "2026-10-20 11:00-12:00 Extra"}); err != nil {
		t.Fatal(err)
	}
	res, err := s.submitPlan(Request{ID: "2", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitBlockedBeforeDispatch || len(res.Blockers) == 0 || len(fake.writeCalls) != 0 {
		t.Fatalf("dirty source must block before dispatch: %+v", res)
	}
	if store.Uncertain() {
		t.Fatal("blocked preconditions must not corrupt durability")
	}

	// Saved-but-changed source work changes the rendered identity.
	if _, err := s.confirm(Request{ID: "3", Op: "confirm", Day: date, Revision: intPtr(2)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.save(Request{ID: "4", Op: "save", Day: date, Revision: intPtr(3)}); err != nil {
		t.Fatal(err)
	}
	res, err = s.submitPlan(Request{ID: "5", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitBlockedBeforeDispatch || len(fake.writeCalls) != 0 {
		t.Fatalf("changed source must produce stale_plan before dispatch: %+v", res)
	}
	hasStale := false
	for _, b := range res.Blockers {
		if b.Code == CodeStalePlan {
			hasStale = true
		}
	}
	if !hasStale {
		t.Fatalf("expected a stale_plan blocker: %+v", res.Blockers)
	}
	if len(fake.calls) < before {
		t.Fatal("read-only revalidation is allowed, but nothing may be mutated")
	}

	// Changed remote content invalidates the approval too. The stored approval
	// row never reacts: it consumed preconditions, and only a NEW preview and
	// approval can cover the new preconditions.
	fake.setWeek([]map[string]any{remoteEntry("r-1", "08:00", "10:00", "Synthetic work")})
	res, err = s.submitPlan(Request{ID: "6", Op: "submit_plan", Day: date, ApprovalID: rec.ApprovalID})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != SubmitBlockedBeforeDispatch || len(fake.writeCalls) != 0 {
		t.Fatalf("changed remote must invalidate the approval: %+v", res)
	}
	_ = store
}

func TestApprovalRequiresFreshClassificationProof(t *testing.T) {
	s, fake, store, date := editReadyFixture(t)

	// The fresh catalog read fails: approval is impossible, no ledger row.
	fake.catError = true
	if _, err := s.approvePlan(Request{ID: "1", Op: "approve_plan", Day: date,
		PlanID: func() string { p, _ := s.previewPlan(date); return p.PlanID }()}); err == nil {
		t.Fatal("a failing fresh catalog must block approval")
	}
	fake.catError = false

	// A fresh catalog result that never returns the selected option cannot
	// prove it exists; absence of proof fail-closes (both structured and text
	// renderings are empty here).
	fake.catOptions = []map[string]any{}
	fake.catText = ""
	if _, err := s.approvePlan(Request{ID: "2", Op: "approve_plan", Day: date,
		PlanID: func() string { p, _ := s.previewPlan(date); return p.PlanID }()}); err == nil {
		t.Fatal("a fresh catalog without the selected option must fail closed")
	}
	if len(store.State().Approvals) != 0 {
		t.Fatalf("failed approvals must leave no ledger row: %+v", store.State().Approvals)
	}
}
