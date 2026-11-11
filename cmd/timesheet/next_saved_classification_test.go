package main

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func savedClassificationObservation() nextObservation {
	o := finalizedDay()
	o.Workflow.Revision = 4
	o.Workflow.Plan.State = timesheet.PlanBlocked
	o.Workflow.Plan.Blockers = []timesheet.Blocker{{Code: "classification_unselected", LocalID: "iv-1"}}
	o.Workflow.Observation = &timesheet.ObservationSnapshot{ObservedAt: "2026-10-27T10:06:00Z"}
	return o
}

func TestSavedClassificationDisabledRemoteStopsBeforeTimestampReplanning(t *testing.T) {
	o := savedClassificationObservation()
	o.GradionEnabled = false
	if got := deriveDayNext(o); got.State != stateStopped {
		t.Fatalf("disabled remote must not offer judgment, classify or plan: %+v", got)
	}
}

func TestSavedClassificationAnswerFreshness(t *testing.T) {
	for _, binding := range []string{"", "stale"} {
		t.Run(binding, func(t *testing.T) {
			o := savedClassificationObservation()
			o.ChoicesPresent, o.ChoicesBinding = true, binding
			got := deriveDayNext(o)
			if got.State != stateJudgment || got.Judgment == nil || got.Judgment.LedgerDigest == "" {
				t.Fatalf("unbound answer must produce a fresh judgment: %+v", got)
			}
		})
	}
}

func TestSavedClassificationBoundEmptyStops(t *testing.T) {
	o := savedClassificationObservation()
	o.ChoicesBinding = timesheet.SavedClassificationDigest(o.Day, o.Workflow.Revision, o.Workflow.Confirmed)
	if got := deriveDayNext(o); got.State != stateStopped || got.Action != nil {
		t.Fatalf("empty bound answer must not loop through classify: %+v", got)
	}
}

func TestSavedClassificationForeignIntervalStops(t *testing.T) {
	o := savedClassificationObservation()
	o.Workflow.Plan.Blockers[0].LocalID = "iv-prior-day"
	if got := deriveDayNext(o); got.State != stateStopped || got.Judgment != nil {
		t.Fatalf("foreign interval must not be classified on this day: %+v", got)
	}
}

func TestNextFinalizedClassificationPartialProgressRejudgesRemaining(t *testing.T) {
	cfg, args, _ := savedNextClassificationFixture(t)
	plan := runPlanCapture(t, args)
	ageSavedClassificationPlan(t, cfg, plan.PlanID)
	step := nextDoc(t, args...)
	if step.Judgment == nil || len(step.Judgment.Blocks) != 2 {
		t.Fatalf("initial saved judgment = %+v", step)
	}
	initial := step.Judgment
	selectedID := initial.Blocks[0].IntervalID
	body, err := json.Marshal(struct {
		Digest     string                                         `json:"ledger_digest"`
		Selections map[string]timesheet.ClassificationChoiceEntry `json:"selections"`
	}{initial.LedgerDigest, map[string]timesheet.ClassificationChoiceEntry{
		selectedID: {CatalogID: "cat-1", ClassificationID: "cls-dev"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	writeAnswerFile(t, initial.AnswerFile, string(body))
	step = nextDoc(t, args...)
	if step.Action == nil || step.Step != "classify" {
		t.Fatalf("partial answer = %+v", step)
	}
	runClassifyCapture(t, step.Action.Cmd[2:])
	beforeReplay, err := os.ReadFile(statePathFor(cfg))
	if err != nil {
		t.Fatal(err)
	}
	_, _, replayErr := captureOutput(t, func() error {
		return runClassify(append(args, "--file", initial.AnswerFile))
	})
	afterReplay, err := os.ReadFile(statePathFor(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if replayErr == nil || !bytes.Equal(beforeReplay, afterReplay) {
		t.Fatal("old partial answer must fail without changing the newly saved revision")
	}
	step = nextDoc(t, args...)
	if step.Action == nil || step.Step != "plan" {
		t.Fatalf("partial apply requires fresh preview: %+v", step)
	}
	plan = runPlanCapture(t, step.Action.Cmd[2:])
	if plan.State != timesheet.PlanBlocked {
		t.Fatalf("partial selection must remain blocked: %+v", plan)
	}
	step = nextDoc(t, args...)
	if step.Judgment == nil || step.Judgment.LedgerDigest == initial.LedgerDigest || len(step.Judgment.Blocks) != 1 || step.Judgment.Blocks[0].IntervalID == selectedID {
		t.Fatalf("partial progress must rejudge only remaining intervals with a new binding: %+v", step)
	}
}

func TestClassifyFinalizedRejectsEmptyStaleBindingWithoutMutation(t *testing.T) {
	cfg, args, _ := savedNextClassificationFixture(t)
	file := writeChoicesFile(t, cfg, `{"ledger_digest":"stale","selections":{}}`)
	before, err := os.ReadFile(statePathFor(cfg))
	if err != nil {
		t.Fatal(err)
	}
	_, _, classifyErr := captureOutput(t, func() error {
		return runClassify(append(args, "--file", file))
	})
	after, err := os.ReadFile(statePathFor(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if classifyErr == nil || !bytes.Equal(before, after) {
		t.Fatal("empty choices must still enforce the supplied saved binding without mutation")
	}
}

func TestSavedClassificationPreservesDistinctBlockers(t *testing.T) {
	o := savedClassificationObservation()
	o.Workflow.Plan.Blockers = append(o.Workflow.Plan.Blockers, timesheet.Blocker{Code: "remote_missing", LocalID: "iv-1"})
	if got := deriveDayNext(o); got.State != stateJudgment {
		t.Fatalf("classification blocker should remain repairable: %+v", got)
	}
	o.Workflow.Confirmed[0].RemoteClassificationID = "cls-dev"
	o.Workflow.Plan.Blockers = o.Workflow.Plan.Blockers[1:]
	o.Workflow.Observation = nil
	if got := deriveDayNext(o); got.State != stateAskHuman || got.Ask.Kind != askResolve || got.Human != nil || got.Judgment != nil {
		t.Fatalf("distinct blocker must not become a ready plan or another classification judgment: %+v", got)
	}
}
