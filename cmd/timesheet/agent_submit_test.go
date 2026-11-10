package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func agentReview(t *testing.T, cfg string) agentSubmitReview {
	t.Helper()
	var review agentSubmitReview
	out := captureStdout(t, func() {
		if err := runAgentSubmit([]string{"--config", cfg, "--date", testDate}); err != nil {
			t.Fatal(err)
		}
	})
	if err := json.Unmarshal([]byte(out), &review); err != nil {
		t.Fatal(err)
	}
	return review
}

func agentConfirmArgs(cfg string, review agentSubmitReview) []string {
	return []string{
		"--config", cfg, "--date", review.Plan.Date, "--plan-id", review.Plan.PlanID,
		"--review-id", review.ReviewID, "--confirm-chat",
	}
}

func TestAgentSubmitReviewAndConfirmation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, remote := savedDayForSubmit(t, "Chat reviewed work", nil, "")
	review := agentReview(t, cfg)
	if review.Plan.State != "ready" || len(review.Plan.Operations) != 1 || review.ReviewID == "" || remote.writeCount() != 0 {
		t.Fatalf("review = %+v, writes = %d", review, remote.writeCount())
	}
	args := agentConfirmArgs(cfg, review)
	if err := runAgentSubmit(args[:len(args)-1]); err == nil || remote.writeCount() != 0 {
		t.Fatalf("missing confirmation err=%v writes=%d", err, remote.writeCount())
	}
	if status := runSubmitStatusCapture(t, []string{"--config", cfg, "--date", testDate}); len(status.Approvals) != 0 {
		t.Fatalf("missing consent granted approval: %+v", status)
	}
	var receipt timesheet.SubmissionReceipt
	out := captureStdout(t, func() {
		if err := runAgentSubmit(args); err != nil {
			t.Fatal(err)
		}
	})
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Confirmed != 1 || remote.writeCount() != 1 {
		t.Fatalf("receipt = %+v, writes = %d", receipt, remote.writeCount())
	}
	if err := runAgentSubmit(args); err == nil || remote.writeCount() != 1 {
		t.Fatalf("replay err=%v writes=%d", err, remote.writeCount())
	}
}

func TestAgentSubmitRefusesStaleBindings(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, stale := range []string{"plan", "review", "credential", "day", "ledger", "remote", "endpoint"} {
		t.Run(stale, func(t *testing.T) {
			cfg, remote := savedDayForSubmit(t, "Original work", nil, "")
			review := agentReview(t, cfg)
			switch stale {
			case "plan":
				review.Plan.PlanID = "stale-plan"
			case "review":
				review.ReviewID = "stale-review"
			case "credential":
				t.Setenv("SYNTHETIC_GRADION_TOKEN", "different-credential")
			case "day":
				review.Plan.Date = "2026-10-21"
			case "ledger":
				seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{timedBlock("work-1", "09:00", "10:00", "Changed after review")})
			case "remote":
				remote.mu.Lock()
				remote.entries[testDate] = []map[string]any{{
					"id": "new-remote", "startTime": "11:00", "endTime": "12:00",
					"classificationId": "cls-dev", "description": "Changed remote", "status": "logged",
				}}
				remote.mu.Unlock()
			case "endpoint":
				server := httptest.NewServer(remote)
				t.Cleanup(server.Close)
				loaded, err := timesheet.LoadConfig(cfg)
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(cfg, []byte(strings.ReplaceAll(string(data), loaded.Gradion.Endpoint, server.URL)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := runAgentSubmit(agentConfirmArgs(cfg, review)); err == nil || remote.writeCount() != 0 {
				t.Fatalf("stale %s err=%v writes=%d", stale, err, remote.writeCount())
			}
			status := runSubmitStatusCapture(t, []string{"--config", cfg, "--date", testDate})
			if len(status.Approvals) != 0 {
				t.Fatalf("stale review granted approval: %+v", status)
			}
		})
	}
}

func TestAgentSubmitUncertainWriteIsNotRetried(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, mode := range []string{"tool", "reset"} {
		t.Run(mode, func(t *testing.T) {
			cfg, remote := savedDayForSubmit(t, "Uncertain work", nil, mode)
			review := agentReview(t, cfg)
			out := captureStdout(t, func() {
				if err := runAgentSubmit(agentConfirmArgs(cfg, review)); err != nil {
					t.Fatal(err)
				}
			})
			var receipt timesheet.SubmissionReceipt
			if err := json.Unmarshal([]byte(out), &receipt); err != nil {
				t.Fatal(err)
			}
			if receipt.Unresolved != 1 || remote.writeCount() != 1 {
				t.Fatalf("receipt=%+v writes=%d", receipt, remote.writeCount())
			}
			if err := runAgentSubmit(agentConfirmArgs(cfg, review)); err == nil || remote.writeCount() != 1 {
				t.Fatalf("retry err=%v writes=%d", err, remote.writeCount())
			}
		})
	}
}

func TestAgentSubmitRequiresExactIdentifiers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, args := range [][]string{
		{},
		{"--date", "bad"},
		{"--date", testDate, "--confirm-chat"},
		{"--date", testDate, "--plan-id", "id"},
		{"--date", testDate, "--review-id", "id"},
		{"--date", testDate, "extra"},
	} {
		if err := runAgentSubmit(args); err == nil {
			t.Fatalf("accepted %s", strings.Join(args, " "))
		}
	}
}

func TestAgentSubmitCLIFromForeignDirectory(t *testing.T) {
	buildHome := os.Getenv("HOME")
	t.Setenv("HOME", t.TempDir())
	cfg, remote := savedDayForSubmit(t, "CLI reviewed work", nil, "")
	dir := filepath.Join(t.TempDir(), "paths with spaces")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "timesheet")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "HOME="+buildHome, "GOPROXY=off", "GOTOOLCHAIN=local")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg = filepath.Join(dir, "config with spaces.json")
	if err := os.WriteFile(cfg, data, 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command(binary, append([]string{"submit", "--chat"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("submit --chat %v: %v", args, err)
		}
		return out
	}
	var review agentSubmitReview
	if err := json.Unmarshal(run("--config", cfg, "--date", testDate), &review); err != nil {
		t.Fatal(err)
	}
	if remote.writeCount() != 0 {
		t.Fatal("review wrote remotely")
	}
	var receipt timesheet.SubmissionReceipt
	if err := json.Unmarshal(run(agentConfirmArgs(cfg, review)...), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Confirmed != 1 || remote.writeCount() != 1 {
		t.Fatalf("receipt=%+v writes=%d", receipt, remote.writeCount())
	}
}

func TestAgentSubmitZeroOperationsIsNoOp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, remote := savedDayForSubmit(t, "Already logged", nil, "")
	initial := agentReview(t, cfg)
	captureStdout(t, func() {
		if err := runAgentSubmit(agentConfirmArgs(cfg, initial)); err != nil {
			t.Fatal(err)
		}
	})
	review := agentReview(t, cfg)
	if len(review.Plan.Operations) != 0 || review.Plan.State != "ready" {
		t.Fatalf("review=%+v", review)
	}
	var receipt timesheet.SubmissionReceipt
	out := captureStdout(t, func() {
		if err := runAgentSubmit(agentConfirmArgs(cfg, review)); err != nil {
			t.Fatal(err)
		}
	})
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Confirmed != 0 || receipt.Unresolved != 0 || remote.writeCount() != 1 {
		t.Fatalf("receipt=%+v writes=%d", receipt, remote.writeCount())
	}
}
