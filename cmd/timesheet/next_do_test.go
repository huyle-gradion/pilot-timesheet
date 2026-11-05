package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func advanceDoc(t *testing.T, args ...string) (advanceReport, error) {
	t.Helper()
	out, _, err := captureOutput(t, func() error { return runNext(args) })
	var rep advanceReport
	if len(out) > 0 {
		if jsonErr := json.Unmarshal(out, &rep); jsonErr != nil {
			t.Fatalf("next --do did not print one JSON report: %v\n%s", jsonErr, out)
		}
	}
	return rep, err
}

// TestNextDoAdvancesThroughEveryDeterministicStep is the token-reduction
// acceptance shape: one invocation collects the day, writes the draft artifact,
// and stops at the one model-owned judgment, with a bounded receipt per act.
func TestNextDoAdvancesToTheDescriptionsJudgmentInOneCall(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	cfg := writeRunConfig(t)
	srv, _ := startFeed(t)
	envFile := writeRunEnvFile(t, srv.URL)
	day := "2026-10-20"
	args := []string{"--config", cfg, "--env-file", envFile, "--date", day}

	rep, err := advanceDoc(t, append(args, "--do")...)
	if err != nil {
		t.Fatalf("next --do: %v", err)
	}
	if rep.State != stateJudgment || rep.Judgment == nil || rep.Judgment.Kind != judgmentBlockDescriptions {
		t.Fatalf("report = %+v, want the descriptions judgment", rep)
	}
	if len(rep.Ran) != 1 || rep.Ran[0].Step != "run" || rep.Ran[0].Result != "ok" {
		t.Fatalf("ran = %+v, want one successful run act", rep.Ran)
	}
	if n := len([]rune(rep.Ran[0].Notice)); n == 0 || n > 200 {
		t.Fatalf("notice length = %d, want 1..200 runes", n)
	}
	if rep.Judgment.Draft == nil || len(rep.Judgment.Draft.Blocks) == 0 {
		t.Fatal("the descriptions judgment does not inline the parsed draft")
	}
	for _, b := range rep.Judgment.Draft.Blocks {
		if b.BlockID == "" {
			t.Fatalf("inline draft block = %+v, want the exact draft content", b)
		}
	}
	draftPath := filepath.Join(testArtifactDir(t, cfg, day), "draft.json")
	if _, statErr := os.Stat(draftPath); statErr != nil {
		t.Fatalf("the run act did not write %s: %v", draftPath, statErr)
	}

	// The read-only twin is the same derivation without the act receipts.
	twin := nextDoc(t, args...)
	if twin.State != stateJudgment || twin.Judgment == nil || twin.Judgment.Draft == nil {
		t.Fatalf("read-only next = %+v, want the same inline judgment", twin)
	}
}

// TestNextDoRunsConsecutiveActsWithoutHoldingTheStore proves the lock
// discipline: two acts that each need the store run in one process, so no
// handle is held across an act.
func TestNextDoRunsConsecutiveActsWithoutHoldingTheStore(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "Build the bridge"),
	})

	rep, err := advanceDoc(t, "--config", cfg, "--date", testDate, "--do")
	if err != nil {
		t.Fatalf("next --do: %v", err)
	}
	steps := make([]string, 0, len(rep.Ran))
	for _, ran := range rep.Ran {
		steps = append(steps, ran.Step)
	}
	if strings.Join(steps, ",") != "stage,save" {
		t.Fatalf("ran = %v, want stage then save", steps)
	}
	if rep.State != stateStopped || !strings.Contains(rep.Reason, "gradion") {
		t.Fatalf("state/reason = %q/%q, want the plan tail stop naming gradion", rep.State, rep.Reason)
	}
}

// TestNextDoRespectsTheActionCap pins the bound: with a one-action budget the
// run executes exactly one act and stops naming the cap.
func TestNextDoRespectsTheActionCap(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "Build the bridge"),
	})

	rep, err := advanceDoc(t, "--config", cfg, "--date", testDate, "--do", "--max-actions", "1")
	if err != nil {
		t.Fatalf("next --do: %v", err)
	}
	if rep.State != stateStopped || !strings.Contains(rep.Reason, "action_cap") {
		t.Fatalf("state/reason = %q/%q, want the action cap stop", rep.State, rep.Reason)
	}
	if len(rep.Ran) != 1 || rep.Ran[0].Step != "stage" {
		t.Fatalf("ran = %+v, want exactly the stage act", rep.Ran)
	}
}

// TestNextDoActionFailureReportsAndReturnsAnError pins the failure contract: the
// report reaches stdout naming the step, and the process still fails so a shell
// loop cannot mistake it for progress.
func TestNextDoActionFailureReportsAndReturnsAnError(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	cfg := writeTestConfigWithGradion(t, "http://127.0.0.1:1/mcp")
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "Build the bridge"),
	})
	runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	if err := runConfirm([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := runSave([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("save: %v", err)
	}

	rep, err := advanceDoc(t, "--config", cfg, "--date", testDate, "--do")
	if err == nil {
		t.Fatal("a failed action returned no process error")
	}
	if rep.State != stateStopped || !strings.HasPrefix(rep.Reason, "action_failed: plan: ") {
		t.Fatalf("report = %+v, want stopped naming the failed plan step", rep)
	}
	if len(rep.Ran) != 1 || rep.Ran[0].Step != "plan" || rep.Ran[0].Result != "failed" {
		t.Fatalf("ran = %+v, want the failed plan act recorded", rep.Ran)
	}
}

// TestNextDoAttachesSubmissionStatusAtHumanGates removes the extra
// submit-status turn: reaching the gates under --do carries the same read-only
// status the tui shows.
func TestNextDoAttachesSubmissionStatusAtHumanGates(t *testing.T) {
	cfg := writeTestConfig(t)
	day := testDate
	seedEntries(t, cfg, day, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "Build the bridge"),
	})
	runStageCapture(t, []string{"--config", cfg, "--date", day})
	if err := runConfirm([]string{"--config", cfg, "--date", day}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := runSave([]string{"--config", cfg, "--date", day}); err != nil {
		t.Fatalf("save: %v", err)
	}
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	state := store.State()
	state.Plans = map[string]*timesheet.PlanRecord{
		"plan-ready": {
			PlanID: "plan-ready", Date: day, State: timesheet.PlanReady,
			CreatedAt:       "2026-10-26T10:05:00Z",
			SourceRevisions: map[string]int{day: state.Days[day].Revision},
		},
	}
	if err := store.CommitState(state); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	_ = store.Close()

	rep, err := advanceDoc(t, "--config", cfg, "--date", day, "--do")
	if err != nil {
		t.Fatalf("next --do: %v", err)
	}
	if rep.State != stateHumanGates || rep.Human == nil {
		t.Fatalf("report = %+v, want human_gates", rep)
	}
	if rep.Human.Status == nil || rep.Human.Status.Date != day || rep.Human.Status.NextAction == "" {
		t.Fatalf("human.status = %+v, want the day's submission status", rep.Human.Status)
	}
	if len(rep.Ran) != 0 {
		t.Fatalf("ran = %+v, want no act between save and the gates", rep.Ran)
	}

	// The read-only twin stays exactly as before: no status read, no extra field.
	if twin := nextDoc(t, "--config", cfg, "--date", day); twin.Human == nil || twin.Human.Status != nil {
		t.Fatalf("read-only human gates = %+v, want no inlined status", twin.Human)
	}
}

func TestNextDoRejectsAnOutOfRangeActionCap(t *testing.T) {
	cfg := writeTestConfig(t)
	err := runNext([]string{"--config", cfg, "--date", nextDay, "--do", "--max-actions", "0"})
	if err == nil || !strings.Contains(err.Error(), "max-actions") {
		t.Fatalf("next --do --max-actions 0 = %v, want the range error", err)
	}
	if _, statErr := os.Stat(statePathFor(cfg)); statErr == nil {
		t.Fatal("an invalid cap opened the store")
	}
}

func TestTuiRefusesDoFlagAndActionCap(t *testing.T) {
	cfg := writeTestConfig(t)
	for _, args := range [][]string{
		{"--config", cfg, "--date", nextDay, "--do"},
		{"--config", cfg, "--date", nextDay, "--max-actions", "3"},
	} {
		_, _, err := captureOutput(t, func() error { return runTui(args) })
		if err == nil || !strings.Contains(err.Error(), "tui:") {
			t.Fatalf("runTui(%v) = %v, want an explicit refusal", args, err)
		}
	}
}

func TestJudgmentDraftIsInlinedOnlyForDescriptions(t *testing.T) {
	draft := &timesheet.Draft{Date: nextDay, Blocks: []timesheet.DraftBlock{{BlockID: "work-1"}}}
	descriptions := unclassifiedDay()
	descriptions.Artifacts, descriptions.Units = timesheet.DayArtifacts{}, nil
	descriptions.DraftPresent, descriptions.DraftValid, descriptions.DraftBlocks = true, true, 1
	descriptions.DraftDigest, descriptions.Draft = nextDraftDigest, draft
	descriptions.DescriptionsPresent, descriptions.DescriptionsBinding = false, ""
	descriptions.Workflow.Sources = nil
	n := deriveDayNext(descriptions)
	if n.Judgment == nil || n.Judgment.Draft != draft {
		t.Fatalf("descriptions judgment = %+v, want the parsed draft inlined", n.Judgment)
	}

	classifications := unclassifiedDay()
	classifications.Draft = draft
	classifications.DraftPresent, classifications.DraftValid = true, true
	n = deriveDayNext(classifications)
	if n.Judgment == nil || n.Judgment.Kind != judgmentClassifications {
		t.Fatalf("classification derivation = %+v, want the classifications judgment", n)
	}
	if n.Judgment.Draft != nil {
		t.Fatalf("classifications judgment carries a draft: %+v", n.Judgment.Draft)
	}
}

// TestBareInvocationAndHelpStayConcise pins the anti-dump surface: no arguments
// names the four entry points in two lines, and help lists the next --do flags.
func TestBareInvocationAndHelpStayConcise(t *testing.T) {
	var noCommand strings.Builder
	printNoCommand(&noCommand)
	text := noCommand.String()
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("bare invocation printed %d lines:\n%s", len(lines), text)
	}
	for _, want := range []string{"timesheet next --do", "timesheet tui", "timesheet version", "timesheet help"} {
		if !strings.Contains(text, want) {
			t.Fatalf("bare invocation does not name %q:\n%s", want, text)
		}
	}

	var synopsis strings.Builder
	printSynopsis(&synopsis)
	for _, want := range []string{"Usage:", "timesheet next", "--do", "--max-actions", "timesheet version"} {
		if !strings.Contains(synopsis.String(), want) {
			t.Fatalf("synopsis does not carry %q", want)
		}
	}
}
