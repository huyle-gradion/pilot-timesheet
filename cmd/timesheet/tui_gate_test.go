package main

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func gateModel(preview *timesheet.PlanPreview, status *timesheet.SubmissionStatusResult, echo string) tuiModel {
	m := dayModel()
	m.bench = benchState{
		screen: screenGate, kind: gateApprove, text: echo, caret: len(echo),
		preview: preview, approvals: approvalRowsOf(status),
	}
	return m
}

func approvalRowsOf(status *timesheet.SubmissionStatusResult) []timesheet.ApprovalRow {
	if status == nil {
		return nil
	}
	return status.Approvals
}

func TestApproveGateEchoDecision(t *testing.T) {
	fresh := &timesheet.PlanPreview{PlanID: "plan-1", State: timesheet.PlanReady}
	tests := []struct {
		name string
		echo string
		want bool
	}{
		{"exact echo emits the op", "plan-1", true},
		{"wrong echo emits nothing", "plan-2", false},
		{"missing echo emits nothing", "", false},
		{"drifted fresh id emits nothing", "", false},
	}
	for _, tc := range tests[:3] {
		t.Run(tc.name, func(t *testing.T) {
			m, eff := update(gateModel(fresh, nil, tc.echo), gateCheckEvent{fresh: fresh, echo: tc.echo})
			if (eff != nil) != tc.want {
				t.Fatalf("echo %q: effect %v, want emitted %v", tc.echo, eff, tc.want)
			}
			if !tc.want && m.err == nil {
				t.Fatalf("echo %q: refusal carried no message", tc.echo)
			}
		})
	}
	t.Run("the fresh id drifting from the echo emits nothing", func(t *testing.T) {
		drifted := &timesheet.PlanPreview{PlanID: "plan-9", State: timesheet.PlanReady}
		_, eff := update(gateModel(fresh, nil, "plan-1"), gateCheckEvent{fresh: drifted, echo: "plan-1"})
		if eff != nil {
			t.Fatalf("drifted re-preview emitted an op")
		}
	})
}

func TestSubmitGateStatusDecision(t *testing.T) {
	status := &timesheet.SubmissionStatusResult{Approvals: []timesheet.ApprovalRow{
		{ApprovalID: "app-1", PlanID: "plan-1", State: timesheet.ApprovalActive},
		{ApprovalID: "app-2", PlanID: "plan-1", State: timesheet.ApprovalRevoked},
		{ApprovalID: "app-3", PlanID: "plan-1", State: timesheet.ApprovalExecuting},
	}}
	tests := []struct {
		name string
		echo string
		want bool
	}{
		{"active approval submits", "app-1", true},
		{"revoked approval emits nothing", "app-2", false},
		{"consumed approval emits nothing", "app-3", false},
		{"missing echo emits nothing", "", false},
		{"unknown id emits nothing", "app-9", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, eff := update(gateModel(nil, status, tc.echo), gateCheckEvent{status: status, echo: tc.echo})
			if (eff != nil) != tc.want {
				t.Fatalf("echo %q: effect %v, want emitted %v", tc.echo, eff, tc.want)
			}
			if tc.want && m.busy != true {
				t.Fatalf("emitting op did not set the busy lock")
			}
		})
	}
	t.Run("activeApprovalFor matches only exact active rows", func(t *testing.T) {
		if activeApprovalFor(status.Approvals, " app-1 ") == nil {
			t.Fatal("a trimmed exact echo must match")
		}
		if activeApprovalFor(nil, "app-1") != nil {
			t.Fatal("no approvals must match nothing")
		}
	})
}

func TestApproveOpDispatchesOnceOnScratchStore(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	pretendTTY(t)
	cfgPath := writeRunConfig(t)
	m, err := preflightTui(&nextFlags{configPath: cfgPath, date: "2026-10-20"})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	defer func() { _ = m.env.h.store.Close() }()
	ev := m.env.approveOp("plan-that-does-not-exist")
	done, ok := ev.(actDoneEvent)
	if !ok {
		t.Fatalf("approve op returned %T, want actDoneEvent", ev)
	}
	var app *timesheet.AppError
	if !errors.As(done.err, &app) {
		t.Fatalf("approve error = %v, want the op's own typed refusal", done.err)
	}
	if done.notice != "" {
		t.Fatalf("a refused approve carried a success notice %q", done.notice)
	}
}

func TestGateDialogKeys(t *testing.T) {
	m := gateModel(nil, nil, "")
	m.bench.kind = gateSubmit
	m.bench.answerFile = "type the exact approval_id shown above to submit"
	m, eff := update(m, textEvent{runes: []rune{'a'}})
	if eff != nil || m.bench.text != "a" {
		t.Fatalf("typing built %q with effect %v", m.bench.text, eff)
	}
	m, eff = update(m, keyEvent{key: "esc"})
	if eff != nil || m.bench.screen == screenGate {
		t.Fatalf("esc dismissed to %+v with effect %v", m.bench, eff)
	}
	m = gateModel(nil, nil, "")
	m, eff = update(m, keyEvent{key: "enter"})
	if eff == nil || !m.busy {
		t.Fatalf("confirm = effect %v busy %v, want the freshness check under the busy lock", eff, m.busy)
	}
	_ = eff
}

func TestInteractiveConfirmAbortsOnEOF(t *testing.T) {
	term := &interactive{in: bufio.NewReader(strings.NewReader("")), out: io.Discard}
	if err := term.confirm("approve?", "plan-1"); err == nil {
		t.Fatal("EOF at the confirmation prompt was accepted")
	}
}
