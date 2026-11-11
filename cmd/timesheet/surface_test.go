package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// echoTerminal types the plan_id at the approve prompt, then the approval_id
// the approve step printed at the submit prompt, like an operator reading the
// screen.
type echoTerminal struct {
	out    *bytes.Buffer
	planID string
	step   int
}

var approvalPrinted = regexp.MustCompile(`approval_id (\S+)`)

func (e *echoTerminal) Read(p []byte) (int, error) {
	e.step++
	line := e.planID
	if e.step > 1 {
		m := approvalPrinted.FindStringSubmatch(e.out.String())
		if m == nil {
			line = "nothing-printed"
		} else {
			line = m[1]
		}
	}
	return copy(p, line+"\n"), nil
}

func injectEchoTerminal(t *testing.T, planID string) *bytes.Buffer {
	t.Helper()
	out := &bytes.Buffer{}
	old := interactiveFactory
	interactiveFactory = func(name, verb string) (*interactive, error) {
		return &interactive{in: bufio.NewReader(&echoTerminal{out: out, planID: planID}), out: out}, nil
	}
	t.Cleanup(func() { interactiveFactory = old })
	return out
}

func TestSubmitTerminalApprovesThenSubmitsBehindTwoConfirmations(t *testing.T) {
	cfg, stub := savedDayForSubmit(t, "Build the bridge", nil, "")
	preview := runPlanCapture(t, []string{"--config", cfg, "--date", testDate})
	screen := injectEchoTerminal(t, preview.PlanID)
	out, _, err := captureOutput(t, func() error { return runSubmitCommand([]string{"--config", cfg, "--date", testDate}) })
	if err != nil {
		t.Fatalf("submit: %v\n%s", err, screen)
	}
	var r struct {
		Approval   timesheet.ApprovalReceipt   `json:"approval"`
		Submission timesheet.SubmissionReceipt `json:"submission"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatal(err)
	}
	if r.Approval.PlanID != preview.PlanID || r.Submission.Status != timesheet.SubmitAllConfirmed || stub.writeCount() != 1 {
		t.Fatalf("receipt = %+v, writes %d", r, stub.writeCount())
	}
	if strings.Count(screen.String(), "About to") != 2 {
		t.Fatalf("want two typed confirmations, screen:\n%s", screen)
	}
}

func TestSubmitTerminalWrongPlanIDWritesNothing(t *testing.T) {
	cfg, stub := savedDayForSubmit(t, "Build the bridge", nil, "")
	injectTTY(t, "not-the-plan")
	if err := runSubmitCommand([]string{"--config", cfg, "--date", testDate}); err == nil {
		t.Fatal("a mistyped plan_id was accepted")
	}
	if stub.writeCount() != 0 {
		t.Fatalf("a refused submit wrote %d time(s)", stub.writeCount())
	}
}

func TestSubmitSubcommandsRouteToTheirCores(t *testing.T) {
	cfg, _ := savedDayForSubmit(t, "Build the bridge", nil, "")
	out, _, err := captureOutput(t, func() error {
		return runSubmitCommand([]string{"status", "--config", cfg, "--date", testDate})
	})
	if err != nil || !strings.Contains(string(out), `"approvals"`) {
		t.Fatalf("submit status = %s, %v", out, err)
	}
	out, _, err = captureOutput(t, func() error {
		return runSubmitCommand([]string{"--chat", "--config", cfg, "--date", testDate})
	})
	var review agentSubmitReview
	if err != nil || json.Unmarshal(out, &review) != nil || review.ReviewID == "" {
		t.Fatalf("submit --chat = %s, %v", out, err)
	}
	err = runSubmitCommand([]string{"--chat", "--config", cfg, "--date", testDate, "--plan-id", "p"})
	if err == nil || !strings.Contains(err.Error(), "submit --chat requires --plan-id, --review-id and --confirm-chat") {
		t.Fatalf("partial chat confirmation = %v", err)
	}
	if err := runSubmitCommand([]string{"resolve", "--config", cfg}); err == nil {
		t.Fatal("submit resolve without --file was accepted")
	}
}

func TestLogRunsTheDayAndRejectsTheOldFlags(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{timedBlock("work-1", "09:00", "10:00", "Build the bridge")})
	out, _, err := captureOutput(t, func() error { return runLog([]string{"--config", cfg, "--date", testDate}) })
	var rep advanceReport
	if jsonErr := json.Unmarshal(out, &rep); jsonErr != nil || len(rep.Ran) == 0 || rep.Ran[0].Step != "stage" {
		t.Fatalf("log = %s, %v", out, err)
	}
	if err := runLog([]string{"--config", cfg, "--do"}); err == nil || !strings.Contains(err.Error(), "drop --do and --auto") {
		t.Fatalf("log --do = %v", err)
	}
}

func TestRetiredCommandsNameTheirReplacement(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "timesheet")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	for old, want := range map[string]string{
		"setup": "timesheet init", "approve": "timesheet submit", "agent-submit": "timesheet submit --chat",
		"submit-status": "timesheet submit status", "resolve-write": "timesheet submit resolve",
		"collect": "timesheet log", "draft": "timesheet log", "confirm": "timesheet log",
		"serve": "timesheet migrate preview",
	} {
		cmd := exec.Command(binary, old, "--date", testDate)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		exit, ok := err.(*exec.ExitError)
		if !ok || exit.ExitCode() != 2 || strings.Count(strings.TrimSpace(stderr.String()), "\n") != 0 || !strings.Contains(stderr.String(), want) {
			t.Errorf("%s: exit %v, stderr %q; want one line naming %q", old, err, stderr.String(), want)
		}
	}
}
