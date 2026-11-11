package main

import (
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func TestSubmitWithAWrongTypedApprovalIDWritesNothing(t *testing.T) {
	cfg, _, approvalID, stub := approveReadyPlan(t, "")
	injectTTY(t, "apr-not-this-one")
	if err := runSubmit([]string{"--config", cfg, "--date", testDate, "--approval-id", approvalID}); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("submit with a wrong typed id = %v, want the abort", err)
	}
	if stub.writeCount() != 0 {
		t.Fatalf("an aborted submit dispatched %d writes", stub.writeCount())
	}
	approvals := readApprovals(t, cfg)
	if len(approvals) != 1 || approvals[0].State != timesheet.ApprovalActive {
		t.Fatalf("approvals after an aborted submit = %+v, want the one approval still active", approvals)
	}
}

func TestResolveWriteWithAWrongTypedRefRecordsNothing(t *testing.T) {
	cfg, _, approvalID, _ := approveReadyPlan(t, "reset")
	injectTTY(t, approvalID)
	receipt := runSubmitCapture(t, []string{"--config", cfg, "--date", testDate, "--approval-id", approvalID})
	if receipt.Unresolved != 1 {
		t.Fatalf("submit receipt = %+v, want one unresolved write", receipt)
	}
	ref := receipt.Writes[0].WriteRef
	rec := runReconcileCapture(t, []string{"--config", cfg, "--date", testDate})
	adopt := writeDecisionFor(t, cfg,
		`{"write_ref": "`+ref+`", "observation_id": "`+rec.ObservationID+`", "decision": "adopt", "remote_id": "r-new-1"}`)

	injectTTY(t, "not-the-write-ref")
	if err := runResolveWrite([]string{"--config", cfg, "--file", adopt}); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("resolve-write with a wrong typed ref = %v, want the abort", err)
	}
	status := runSubmitStatusCapture(t, []string{"--config", cfg, "--date", testDate})
	if len(status.Unresolved) != 1 {
		t.Fatalf("submit status after an aborted resolve = %+v, want the write still unresolved", status.Unresolved)
	}
}

func TestDeriveDayNextStopsWhenAFinalizedDaysCarryRecordDiffers(t *testing.T) {
	segment := timesheet.CarrySegment{
		SourceDate: testDate, SourceBlockID: "work-1", Date: "2026-10-28",
		Start: "00:00", End: "01:00", Minutes: 60, Description: "Overnight", Timezone: "UTC",
	}
	cases := map[string]func(*nextObservation){
		"recorded carry the derivation does not make": func(o *nextObservation) {
			o.Artifacts.CarryOut = []timesheet.CarrySegment{segment}
		},
		"derived carry with no record": func(o *nextObservation) {
			o.CarryDerived = []timesheet.CarrySegment{segment}
		},
	}
	for name, mutate := range cases {
		o := finalizedDay()
		mutate(&o)
		n := deriveDayNext(o)
		if n.State != stateStopped || !strings.Contains(n.Reason, "carry record") {
			t.Fatalf("%s: derivation = %s %q, want the carry-record stop", name, n.State, n.Reason)
		}
	}
}
