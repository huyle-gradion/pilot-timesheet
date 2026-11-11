package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func runRevokeCapture(t *testing.T, args ...string) (timesheet.ApprovalRevocationReceipt, error) {
	t.Helper()
	var receipt timesheet.ApprovalRevocationReceipt
	out, _, err := captureOutput(t, func() error {
		return runSubmitCommand(append([]string{"revoke"}, args...))
	})
	if err == nil {
		if jerr := json.Unmarshal(out, &receipt); jerr != nil {
			t.Fatalf("revoke receipt is not JSON: %v\n%s", jerr, out)
		}
	}
	return receipt, err
}

func approvalState(t *testing.T, cfg, approvalID string) string {
	t.Helper()
	for _, a := range runSubmitStatusCapture(t, []string{"--config", cfg, "--date", testDate}).Approvals {
		if a.ApprovalID == approvalID {
			return a.State
		}
	}
	return ""
}

// TestSubmitRevokeRevokesTheKeptApproval (issue #272): the approval a stopped
// terminal submit keeps is revoked from the terminal behind a typed
// confirmation of its approval_id. Without --approval-id the day's single
// active approval is chosen. Revocation is immutable: a second revoke is
// refused with the existing stale_approval refusal, and the revoked approval
// can no longer be submitted.
func TestSubmitRevokeRevokesTheKeptApproval(t *testing.T) {
	cfg, planID, approvalID, stub := approveReadyPlan(t, "")

	injectTTY(t, approvalID)
	receipt, err := runRevokeCapture(t, "--config", cfg, "--date", testDate)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if receipt.ApprovalID != approvalID || receipt.PlanID != planID || receipt.State != timesheet.ApprovalRevoked {
		t.Fatalf("revoke receipt = %+v", receipt)
	}
	if got := approvalState(t, cfg, approvalID); got != timesheet.ApprovalRevoked {
		t.Fatalf("approval state after revoke = %q", got)
	}

	injectTTY(t, approvalID)
	_, err = runRevokeCapture(t, "--config", cfg, "--date", testDate, "--approval-id", approvalID)
	var appErr *timesheet.AppError
	if !errors.As(err, &appErr) || appErr.Code != timesheet.CodeStaleApproval {
		t.Fatalf("second revoke = %v, want stale_approval", err)
	}
	injectTTY(t, approvalID)
	if got := runSubmitCapture(t, []string{"--config", cfg, "--date", testDate, "--approval-id", approvalID}); got.Status != timesheet.SubmitBlockedBeforeDispatch {
		t.Fatalf("submitting a revoked approval = %+v, want blocked before dispatch", got)
	}
	if stub.writeCount() != 0 {
		t.Fatalf("revoke path wrote %d time(s)", stub.writeCount())
	}
}

// TestSubmitRevokeRefusesWithoutAConfirmedSingleTarget: a mistyped
// confirmation changes nothing, several active approvals are listed and
// refused without --approval-id, and a day with none says so.
func TestSubmitRevokeRefusesWithoutAConfirmedSingleTarget(t *testing.T) {
	cfg, planID, first, _ := approveReadyPlan(t, "")

	injectTTY(t, "not-the-approval")
	if _, err := runRevokeCapture(t, "--config", cfg, "--date", testDate); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("mistyped confirmation = %v", err)
	}
	if got := approvalState(t, cfg, first); got != timesheet.ApprovalActive {
		t.Fatalf("a refused revoke changed the approval to %q", got)
	}

	injectTTY(t, planID)
	second := runApproveCapture(t, []string{"--config", cfg, "--date", testDate}).ApprovalID
	injectTTY(t, first)
	_, err := runRevokeCapture(t, "--config", cfg, "--date", testDate)
	if err == nil || !strings.Contains(err.Error(), first) || !strings.Contains(err.Error(), second) || !strings.Contains(err.Error(), "--approval-id") {
		t.Fatalf("several active approvals = %v, want both listed and --approval-id named", err)
	}

	for _, id := range []string{first, second} {
		injectTTY(t, id)
		if _, err := runRevokeCapture(t, "--config", cfg, "--date", testDate, "--approval-id", id); err != nil {
			t.Fatalf("revoke %s: %v", id, err)
		}
	}
	injectTTY(t, first)
	if _, err := runRevokeCapture(t, "--config", cfg, "--date", testDate); err == nil || !strings.Contains(err.Error(), "no active approval") {
		t.Fatalf("revoke with none active = %v", err)
	}
}

// TestSubmitRevokeIsTerminalOnly: under go test stdio is not a terminal, so
// the production gate refuses before the store opens; --chat never reaches
// revoke.
func TestSubmitRevokeIsTerminalOnly(t *testing.T) {
	cfg, _, approvalID, _ := approveReadyPlan(t, "")
	interactiveFactory = newInteractive
	if err := runSubmitCommand([]string{"revoke", "--config", cfg, "--date", testDate}); err == nil || !strings.Contains(err.Error(), "interactive-only") {
		t.Fatalf("non-terminal revoke = %v", err)
	}
	if err := runSubmitCommand([]string{"--chat", "revoke", "--config", cfg, "--date", testDate}); err == nil {
		t.Fatal("--chat reached revoke")
	}
	if got := approvalState(t, cfg, approvalID); got != timesheet.ApprovalActive {
		t.Fatalf("a refused revoke changed the approval to %q", got)
	}
}

// TestStoppedSubmitNamesTheRevokeCommand: the hint after a stopped terminal
// submit names both ways out of a kept approval.
func TestStoppedSubmitNamesTheRevokeCommand(t *testing.T) {
	cfg, _ := savedDayForSubmit(t, "Build the bridge", nil, "")
	preview := runPlanCapture(t, []string{"--config", cfg, "--date", testDate})
	injectTTY(t, preview.PlanID, "")
	err := runSubmitCommand([]string{"--config", cfg, "--date", testDate})
	if err == nil || !strings.Contains(err.Error(), "is kept") ||
		!strings.Contains(err.Error(), "timesheet submit revoke --date "+testDate+" --approval-id ") {
		t.Fatalf("stopped submit = %v, want the kept-approval hint naming submit revoke", err)
	}
}
