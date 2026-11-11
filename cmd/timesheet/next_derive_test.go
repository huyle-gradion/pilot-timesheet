package main

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// --- N1 derivation core: pure predicate-order and content-proof tests --------
//
// deriveDayNext is a pure function over observations that already exist, so
// every predicate is pinned here without a store, a filesystem or a network.
// Freshness is proven from CONTENT (the ledger projection, the stage map's
// unit/span counts, the sidecar's per-block digests, and the judgment answer's
// binding), never from second-resolution timestamps. The end-to-end progression
// lives in next_command_test.go.

const nextDay = "2026-10-27"

const (
	nextExe    = "/opt/ts/bin/timesheet"
	nextConfig = "/home/u/.timesheet/config.json"
	nextEnv    = "/home/u/.timesheet/.env.local"

	nextDraftDigest = "draft-digest-of-the-current-bytes"
	nextOldDigest   = "digest-of-an-earlier-generation"
)

func nextArtifactPath(name string) string {
	return "/tmp/timesheet/" + nextDay + "/" + name
}

func nextEntry(blockID, start, end, description string) timesheet.EntryRecord {
	return timesheet.EntryRecord{
		BlockID: blockID, Date: nextDay, Start: start, End: end, Description: description, Kind: "work",
	}
}

// proofOf is the same content proof the planner uses: the current ledger's
// units, their stage-map binding and the resulting ledger digest.
func proofOf(entries []timesheet.EntryRecord, stageMap map[string][]string) ([]ledgerUnit, map[string]timesheet.EntryRecord, string) {
	units := ledgerUnits(entries, nextDay, "UTC")
	blocks := stagedBlocksFrom(units, stageMap)
	return units, blocks, timesheet.StagedLedgerDigest(nextDay, blocks)
}

// unclassifiedDay is the observation of a day that has been recorded and
// staged, whose source gate is open, and whose classifications are still
// outstanding: exactly the state that must ask for the classification judgment.
func unclassifiedDay() nextObservation {
	entries := []timesheet.EntryRecord{nextEntry("work-1", "09:00", "10:00", "Synthetic work")}
	stageMap := map[string][]string{"work-1": {"iv-1"}}
	units, _, digest := proofOf(entries, stageMap)
	return nextObservation{
		Candidates: &candidatesFile{
			LedgerDigest: digest,
			Sets: []nextCandidateSet{{Ref: "c1", Query: "Synthetic work", CatalogID: "cat-1", Complete: true,
				Options: []nextCandidateOption{{ClassificationID: "cls-1", Label: "Development"}}}},
			Intervals: map[string]string{"iv-1": "c1"},
		},
		Day:                 nextDay,
		Exe:                 nextExe,
		ConfigPath:          nextConfig,
		EnvFile:             nextEnv,
		DraftPath:           nextArtifactPath("draft.json"),
		DescriptionsPath:    nextArtifactPath("descriptions.json"),
		ChoicesPath:         nextArtifactPath("choices.json"),
		SidecarPath:         "/home/u/.timesheet/entries/" + nextDay + ".classify.json",
		DraftDigest:         nextDraftDigest,
		DescriptionsPresent: true,
		DescriptionsBinding: nextDraftDigest,
		Artifacts: timesheet.DayArtifacts{
			EntriesPresent: true,
			Entries:        &timesheet.EntriesFile{Date: nextDay, Entries: entries},
			StageMap:       stageMap,
		},
		Units: units,
		Workflow: timesheet.WorkflowSnapshot{
			Date: nextDay,
			Sources: []timesheet.SourceSnapshot{
				{Source: timesheet.SourceCalendar, Status: timesheet.StatusComplete, CollectionID: "col-1", Attempted: true, Complete: true},
			},
		},
		Project:        "WORK",
		GradionEnabled: true,
	}
}

// recordedDay is unclassifiedDay with a current classification sidecar: exactly
// the state that must derive `save`.
func recordedDay() nextObservation {
	o := unclassifiedDay()
	_, blocks, _ := proofOf(o.Artifacts.Entries.Entries, o.Artifacts.StageMap)
	o.Artifacts.ChoicesPresent = true
	o.Artifacts.Choices = map[string]timesheet.ClassificationChoiceEntry{
		"iv-1": {
			CatalogID: "cat-1", ClassificationID: "cls-1",
			ContentDigest: timesheet.BlockContentDigest(nextDay, blocks["iv-1"]),
		},
	}
	return o
}

// finalizedDay is the observation of a saved day whose newest plan record is
// ready: the terminal state before the human gates. Its ledger still derives
// exactly the saved intervals.
func finalizedDay() nextObservation {
	o := recordedDay()
	o.Artifacts.ChoicesPresent, o.Artifacts.Choices = false, nil
	o.ChoicesPresent, o.ChoicesBinding = false, ""
	o.Workflow.Finalized = true
	o.Workflow.PlanCurrent = true
	o.Workflow.Confirmed = []timesheet.Interval{{
		ID: "iv-1", Date: nextDay, Start: "09:00", End: "10:00",
		Project: "WORK", Description: "Synthetic work",
	}}
	o.Workflow.Plan = &timesheet.PlanSnapshot{
		PlanID:    "plan-ready",
		State:     timesheet.PlanReady,
		CreatedAt: "2026-10-27T10:05:00Z",
	}
	return o
}

func baseArgv(verb string) []string {
	return []string{nextExe, verb, "--config", nextConfig, "--env-file", nextEnv, "--date", nextDay}
}

func withArgs(base []string, extra ...string) []string {
	return append(append([]string{}, base...), extra...)
}

func actionCmd(t *testing.T, n nextAction) []string {
	t.Helper()
	if n.State != stateAction || n.Action == nil {
		t.Fatalf("state = %q action = %+v, want one executable action", n.State, n.Action)
	}
	return n.Action.Cmd
}

func assertCmd(t *testing.T, n nextAction, want []string) {
	t.Helper()
	if got := actionCmd(t, n); !reflect.DeepEqual(got, want) {
		t.Fatalf("derived argv = %#v, want %#v", got, want)
	}
}

// TestDeriveDayNextPipelineOrder pins the fixed predicate order: the switch
// order IS the pipeline, and each step is provable from the observations the
// binary already owns.
func TestDeriveDayNextPipelineOrder(t *testing.T) {
	fresh := func() nextObservation {
		o := unclassifiedDay()
		o.Artifacts = timesheet.DayArtifacts{}
		o.Units = nil
		o.DraftPresent, o.DraftValid, o.DraftDigest = false, false, ""
		o.DescriptionsPresent, o.DescriptionsBinding = false, ""
		o.Workflow.Sources = nil
		return o
	}

	t.Run("no draft derives run", func(t *testing.T) {
		n := deriveDayNext(fresh())
		if n.State != stateAction || n.Step != "run" {
			t.Fatalf("state/step = %q/%q, want action/run", n.State, n.Step)
		}
		assertCmd(t, n, baseArgv("run"))
		if !strings.Contains(n.Action.Why, "draft") {
			t.Fatalf("why = %q, want the missing-draft predicate named", n.Action.Why)
		}
	})

	t.Run("a blockless draft stops as unrecordable", func(t *testing.T) {
		o := fresh()
		o.DraftPresent, o.DraftValid, o.DraftBlocks, o.DraftDigest = true, true, 0, nextDraftDigest
		n := deriveDayNext(o)
		if n.State != stateStopped || n.Action != nil {
			t.Fatalf("state/action = %q/%+v, want stopped with no action", n.State, n.Action)
		}
		if !strings.Contains(n.Reason, "no blocks") || !strings.Contains(n.Reason, "at least one block") {
			t.Fatalf("reason = %q, want the unrecordable-day explanation", n.Reason)
		}
	})

	t.Run("a corrupt draft is re-collected, never judged or recorded", func(t *testing.T) {
		o := fresh()
		o.DraftPresent, o.DraftValid, o.DraftDigest = true, false, nextDraftDigest
		n := deriveDayNext(o)
		if n.State != stateAction || n.Step != "run" {
			t.Fatalf("state/step = %q/%q, want action/run for an unreadable draft", n.State, n.Step)
		}
		if !strings.Contains(n.Action.Why, "not a valid draft") {
			t.Fatalf("why = %q, want the corrupt-draft predicate named", n.Action.Why)
		}
	})

	t.Run("draft without a bound descriptions answer derives the judgment", func(t *testing.T) {
		o := fresh()
		o.DraftPresent, o.DraftValid, o.DraftBlocks, o.DraftDigest = true, true, 1, nextDraftDigest
		n := deriveDayNext(o)
		if n.State != stateJudgment || n.Step != "review" {
			t.Fatalf("state/step = %q/%q, want judgment/review", n.State, n.Step)
		}
		j := n.Judgment
		if j == nil || j.Kind != judgmentBlockDescriptions || j.AnswerFile != o.DescriptionsPath || j.Day != nextDay {
			t.Fatalf("judgment = %+v, want the descriptions contract at %q", j, o.DescriptionsPath)
		}
		if j.DraftDigest != nextDraftDigest {
			t.Fatalf("draft_digest = %q, want the current draft digest reported", j.DraftDigest)
		}
		if !strings.Contains(j.Protocol, "draft_digest") || !strings.Contains(j.Protocol, o.DescriptionsPath) {
			t.Fatalf("protocol = %q, want the binding and the answer path", j.Protocol)
		}
	})

	t.Run("a stale descriptions answer is asked for again", func(t *testing.T) {
		o := fresh()
		o.DraftPresent, o.DraftValid, o.DraftBlocks, o.DraftDigest = true, true, 1, nextDraftDigest
		o.DescriptionsPresent, o.DescriptionsBinding = true, nextOldDigest
		n := deriveDayNext(o)
		if n.State != stateJudgment || n.Judgment == nil {
			t.Fatalf("state = %q, want the judgment again for a stale answer", n.State)
		}
		if !strings.Contains(n.Judgment.Protocol, "stale") {
			t.Fatalf("protocol = %q, want the stale-answer repair instruction", n.Judgment.Protocol)
		}
		// It must never hand record an answer bound to different blocks.
		if n.Action != nil {
			t.Fatalf("stale descriptions derived an action: %#v", n.Action.Cmd)
		}
	})

	t.Run("a bound descriptions answer derives record", func(t *testing.T) {
		o := fresh()
		o.DraftPresent, o.DraftValid, o.DraftBlocks, o.DraftDigest = true, true, 1, nextDraftDigest
		o.DescriptionsPresent, o.DescriptionsBinding = true, nextDraftDigest
		n := deriveDayNext(o)
		if n.State != stateAction || n.Step != "record" {
			t.Fatalf("state/step = %q/%q, want action/record", n.State, n.Step)
		}
		assertCmd(t, n, withArgs(baseArgv("record"), "--draft", o.DraftPath, "--descriptions", o.DescriptionsPath))
	})

	t.Run("recorded day without a stage map derives stage", func(t *testing.T) {
		o := unclassifiedDay()
		o.Artifacts.StageMap = nil
		n := deriveDayNext(o)
		if n.State != stateAction || n.Step != "stage" {
			t.Fatalf("state/step = %q/%q, want action/stage", n.State, n.Step)
		}
		assertCmd(t, n, withArgs(baseArgv("stage"), "--project", "WORK"))
	})

	t.Run("a stage map with a stale span count re-derives stage", func(t *testing.T) {
		// A 09:00-14:00 block splits into two four-hour chunks; a map carrying
		// one id for it is from an earlier, shorter span. Timestamps cannot see
		// this at all, so the count is compared instead.
		o := unclassifiedDay()
		entries := []timesheet.EntryRecord{nextEntry("work-1", "09:00", "14:00", "Long work")}
		o.Artifacts.Entries = &timesheet.EntriesFile{Date: nextDay, Entries: entries}
		o.Units = ledgerUnits(entries, nextDay, "UTC")
		if len(o.Units) != 1 || len(o.Units[0].Spans) != 2 {
			t.Fatalf("units = %+v, want one unit with two chunk spans", o.Units)
		}
		o.Artifacts.StageMap = map[string][]string{"work-1": {"iv-1"}}
		if n := deriveDayNext(o); n.Step != "stage" {
			t.Fatalf("step = %q, want stage for the stale span count", n.Step)
		}
	})

	t.Run("a current stage map with no classification answer derives the judgment", func(t *testing.T) {
		n := deriveDayNext(unclassifiedDay())
		if n.State != stateJudgment || n.Step != "classify" {
			t.Fatalf("state/step = %q/%q, want judgment/classify", n.State, n.Step)
		}
		j := n.Judgment
		if j == nil || j.Kind != judgmentClassifications || j.AnswerFile != nextArtifactPath("choices.json") {
			t.Fatalf("judgment = %+v, want the classifications contract", j)
		}
		if len(j.Blocks) != 1 || j.Blocks[0].IntervalID != "iv-1" || j.Blocks[0].Description != "Synthetic work" {
			t.Fatalf("blocks = %+v, want the current recorded content in interval-id order", j.Blocks)
		}
		if len(j.LedgerDigest) != 64 {
			t.Fatalf("ledger_digest = %q, want a sha256 hex binding", j.LedgerDigest)
		}
		if !strings.Contains(j.Protocol, "ledger_digest") || !strings.Contains(j.Protocol, "stale") {
			t.Fatalf("protocol = %q, want the digest binding and stale repair", j.Protocol)
		}
	})

	t.Run("a bound classification answer derives classify", func(t *testing.T) {
		o := unclassifiedDay()
		_, _, digest := proofOf(o.Artifacts.Entries.Entries, o.Artifacts.StageMap)
		o.ChoicesPresent, o.ChoicesBinding = true, digest
		n := deriveDayNext(o)
		if n.State != stateAction || n.Step != "classify" {
			t.Fatalf("state/step = %q/%q, want action/classify", n.State, n.Step)
		}
		assertCmd(t, n, withArgs(baseArgv("classify"), "--file", o.ChoicesPath, "--project", "WORK"))
	})

	t.Run("a stale choices file is never handed to classify", func(t *testing.T) {
		// The skill's historical answer file carries no ledger binding. It must
		// be re-judged, never auto-applied to the current intervals.
		o := unclassifiedDay()
		o.ChoicesPresent, o.ChoicesBinding = true, ""
		n := deriveDayNext(o)
		if n.State != stateJudgment || n.Step != "classify" {
			t.Fatalf("state/step = %q/%q, want the judgment again for an unbound answer", n.State, n.Step)
		}
		if n.Action != nil {
			t.Fatalf("an unbound choices file derived the action %#v", n.Action.Cmd)
		}
	})

	t.Run("a current sidecar derives save", func(t *testing.T) {
		n := deriveDayNext(recordedDay())
		if n.State != stateAction || n.Step != "save" {
			t.Fatalf("state/step = %q/%q, want action/save", n.State, n.Step)
		}
		assertCmd(t, n, withArgs(baseArgv("save"), "--project", "WORK"))
	})

	t.Run("an edited description with the same interval id is re-judged, never saved", func(t *testing.T) {
		// The blocker regression: the interval id is stable, so an id-only
		// check would silently re-bind the old selection through classify/save.
		// The content digest is what must refuse it.
		o := recordedDay()
		changed := []timesheet.EntryRecord{nextEntry("work-1", "09:00", "10:00", "Edited after classification")}
		o.Artifacts.Entries = &timesheet.EntriesFile{Date: nextDay, Entries: changed}
		o.Units = ledgerUnits(changed, nextDay, "UTC")
		n := deriveDayNext(o)
		if n.State != stateAction || n.Step != "candidates" {
			t.Fatalf("state/step = %q/%q, want fresh candidates after a content change", n.State, n.Step)
		}
		_, _, digest := proofOf(changed, o.Artifacts.StageMap)
		o.Candidates = &candidatesFile{LedgerDigest: digest, Intervals: map[string]string{}}
		n = deriveDayNext(o)
		if n.State != stateJudgment || n.Step != "classify" {
			t.Fatalf("state/step = %q/%q, want the classification judgment after a content change", n.State, n.Step)
		}
		if n.Judgment.Blocks[0].Description != "Edited after classification" {
			t.Fatalf("blocks = %+v, want the edited description reported", n.Judgment.Blocks)
		}
	})

	t.Run("recorded ledger is never re-judged", func(t *testing.T) {
		o := recordedDay()
		o.DraftDigest = ""
		if n := deriveDayNext(o); n.State != stateAction || n.Step != "save" {
			t.Fatalf("state/step = %q/%q, want action/save without re-running the descriptions judgment", n.State, n.Step)
		}
	})
}

// TestDeriveDayNextSourceGate pins the gate ordering: collection is never
// preempted, an unattempted source derives run, and an incomplete attempt
// stops with the verbatim acknowledge argv from the observed ids.
func TestDeriveDayNextSourceGate(t *testing.T) {
	t.Run("unattempted source derives run", func(t *testing.T) {
		o := recordedDay()
		o.Workflow.Sources = []timesheet.SourceSnapshot{{Source: timesheet.SourceGithub, Status: "unattempted"}}
		n := deriveDayNext(o)
		if n.State != stateAction || n.Step != "run" {
			t.Fatalf("state/step = %q/%q, want action/run", n.State, n.Step)
		}
		if !strings.Contains(n.Action.Why, "github") || !strings.Contains(n.Action.Why, "collection attempt") {
			t.Fatalf("why = %q, want the unattempted source named", n.Action.Why)
		}
	})

	t.Run("unacknowledged attempt asks the human, and only their answer licenses acknowledge", func(t *testing.T) {
		o := recordedDay()
		o.Workflow.Sources = []timesheet.SourceSnapshot{{
			Source: timesheet.SourceGithub, Status: timesheet.StatusIncomplete,
			CollectionID: "col-gate-1", Attempted: true,
		}}
		n := deriveDayNext(o)
		q := n.Ask
		if n.State != stateAskHuman || q == nil || q.Kind != askSourceAck || len(q.Options) != 1 || q.Options[0] != "acknowledge" {
			t.Fatalf("derivation = %+v, want the source acknowledgement question", n)
		}
		if !strings.Contains(q.Question, "github") || !strings.Contains(q.Protocol, "never answer on the human's behalf") || len(q.Digest) != 64 {
			t.Fatalf("ask = %+v, want the source, the relay rule and a digest", q)
		}
		name := filepath.Base(q.AnswerFile)

		o.Asks = map[string]askAnswer{name: {Present: true, Digest: "stale", Answer: "acknowledge", Surface: "chat"}}
		if got := deriveDayNext(o); got.State != stateAskHuman || !strings.Contains(got.Ask.LastError, "digest") {
			t.Fatalf("stale answer = %+v, want the same question with last_error", got)
		}
		o.Asks[name] = askAnswer{Present: true, Digest: q.Digest, Answer: "yes", Surface: "chat"}
		if got := deriveDayNext(o); got.State != stateAskHuman || !strings.Contains(got.Ask.LastError, "not one of") {
			t.Fatalf("off-menu answer = %+v, want the options named", got)
		}
		o.Asks[name] = askAnswer{Present: true, Digest: q.Digest, Answer: "acknowledge"}
		if got := deriveDayNext(o); got.State != stateAskHuman || !strings.Contains(got.Ask.LastError, "surface") {
			t.Fatalf("answer without a surface = %+v, want it rejected", got)
		}
		o.Asks[name] = askAnswer{Present: true, Digest: q.Digest, Answer: "acknowledge", Surface: "chat"}
		got := deriveDayNext(o)
		if got.State != stateAction || got.Step != "acknowledge" || !strings.Contains(got.Action.Why, "via chat") {
			t.Fatalf("valid answer = %+v, want the acknowledge act", got)
		}
		assertCmd(t, got, withArgs(baseArgv("acknowledge"), "--source", "github", "--collection-id", "col-gate-1"))
	})

	t.Run("acknowledged attempt derives save", func(t *testing.T) {
		o := recordedDay()
		o.Workflow.Sources = []timesheet.SourceSnapshot{{
			Source: timesheet.SourceGithub, Status: timesheet.StatusIncomplete,
			CollectionID: "col-gate-1", Attempted: true, Acknowledged: true,
		}}
		if n := deriveDayNext(o); n.Step != "save" {
			t.Fatalf("step = %q, want save after the acknowledgment", n.Step)
		}
	})
}

// TestDeriveDayNextFinalizedTail pins the plan tail: one plan, then the human
// gates or a precise resumable blocker; a resolve/new observation is the only
// way back to a fresh plan, and changed content is never auto-revised.
func TestDeriveDayNextFinalizedTail(t *testing.T) {
	blocked := func() nextObservation {
		o := finalizedDay()
		o.Workflow.Plan = &timesheet.PlanSnapshot{
			PlanID: "plan-blocked", State: timesheet.PlanBlocked,
			CreatedAt: "2026-10-27T10:05:00Z",
			Blockers:  []timesheet.Blocker{{Code: "unresolved_identity", LocalID: "work-1"}},
		}
		return o
	}

	t.Run("blocked plan without a newer observation asks the human for the resolve decision", func(t *testing.T) {
		o := blocked()
		n := deriveDayNext(o)
		q := n.Ask
		if n.State != stateAskHuman || q == nil || q.Kind != askResolve || len(q.Options) != 3 {
			t.Fatalf("derivation = %+v, want the resolve question", n)
		}
		reconcile := strings.Join(baseArgv("reconcile"), " ")
		for _, want := range []string{reconcile, "unresolved_identity", "plan-blocked"} {
			if !strings.Contains(q.Question, want) {
				t.Fatalf("question = %q, want %q", q.Question, want)
			}
		}
		o.Asks = map[string]askAnswer{filepath.Base(q.AnswerFile): {Present: true, Digest: q.Digest, Answer: "foreign", Surface: "terminal"}}
		got := deriveDayNext(o)
		if got.State != stateAction || got.Step != "resolve" {
			t.Fatalf("valid decision = %+v, want the resolve act", got)
		}
		assertCmd(t, got, withArgs(baseArgv("resolve"), "--file", q.AnswerFile))
	})

	t.Run("newer observation derives a fresh plan", func(t *testing.T) {
		o := blocked()
		o.Workflow.Observation = &timesheet.ObservationSnapshot{ObservationID: "obs-2", ObservedAt: "2026-10-27T10:06:00Z"}
		n := deriveDayNext(o)
		if n.State != stateAction || n.Step != "plan" {
			t.Fatalf("state/step = %q/%q, want action/plan after a fresh observation", n.State, n.Step)
		}
	})

	t.Run("newer resolution derives a fresh plan", func(t *testing.T) {
		o := blocked()
		o.Workflow.Resolution = &timesheet.ResolutionSnapshot{ObservationID: "obs-1", Decision: "foreign", At: "2026-10-27T10:07:00Z"}
		n := deriveDayNext(o)
		if n.State != stateAction || n.Step != "plan" {
			t.Fatalf("state/step = %q/%q, want action/plan after a resolve", n.State, n.Step)
		}
	})

	t.Run("equal observation timestamp stops with reconcile-again", func(t *testing.T) {
		o := blocked()
		o.Workflow.Observation = &timesheet.ObservationSnapshot{ObservationID: "obs-1", ObservedAt: "2026-10-27T10:05:00Z"}
		n := deriveDayNext(o)
		if n.State != stateAskHuman || !strings.Contains(n.Ask.Question, "reconcile again") {
			t.Fatalf("derivation = %+v, want the resolve question with the equal-timestamp remedy", n)
		}
	})

	t.Run("changed ledger content stops naming stage --revise, never the human gates", func(t *testing.T) {
		o := finalizedDay()
		changed := []timesheet.EntryRecord{nextEntry("work-1", "09:00", "10:00", "Changed after save")}
		o.Artifacts.Entries = &timesheet.EntriesFile{Date: nextDay, Entries: changed}
		o.Units = ledgerUnits(changed, nextDay, "UTC")
		n := deriveDayNext(o)
		if n.State != stateStopped || n.Human != nil {
			t.Fatalf("state/human = %q/%+v, want stopped and no human gates for changed content", n.State, n.Human)
		}
		if !strings.Contains(n.Reason, "stage --revise --date "+nextDay) || !strings.Contains(n.Reason, "next will not") {
			t.Fatalf("reason = %q, want stage --revise named and the never-executed boundary stated", n.Reason)
		}
	})

	t.Run("a changed start time is also caught by content, not seconds", func(t *testing.T) {
		o := finalizedDay()
		changed := []timesheet.EntryRecord{nextEntry("work-1", "09:30", "10:30", "Synthetic work")}
		o.Artifacts.Entries = &timesheet.EntriesFile{Date: nextDay, Entries: changed}
		o.Units = ledgerUnits(changed, nextDay, "UTC")
		if n := deriveDayNext(o); n.State != stateStopped {
			t.Fatalf("state = %q, want stopped for a different saved span", n.State)
		}
	})

	t.Run("an unchanged ledger after save reaches the human gates", func(t *testing.T) {
		o := finalizedDay()
		if n := deriveDayNext(o); n.State != stateHumanGates {
			t.Fatalf("state = %q, want human_gates", n.State)
		}
	})

	t.Run("two plan records at one timestamp are underivable", func(t *testing.T) {
		o := finalizedDay()
		o.Workflow.PlanAmbiguous = true
		if n := deriveDayNext(o); n.State != stateStopped || !strings.Contains(n.Reason, "plan") {
			t.Fatalf("state/reason = %q/%q, want a stopped ambiguous-plan reason", n.State, n.Reason)
		}
	})
}

// TestDeriveDayNextEnvironmentAndProject pins the two derivable environment
// facts: classification needs the live catalog, planning needs the remote
// boundary, and a multi-project configuration is never guessed.
func TestDeriveDayNextEnvironmentAndProject(t *testing.T) {
	t.Run("classification is skipped without the catalog boundary", func(t *testing.T) {
		o := unclassifiedDay()
		o.GradionEnabled = false
		if n := deriveDayNext(o); n.State != stateAction || n.Step != "save" {
			t.Fatalf("state/step = %q/%q, want action/save (classification is not required to save)", n.State, n.Step)
		}
	})

	t.Run("plan without the remote boundary stops instead of looping", func(t *testing.T) {
		o := finalizedDay()
		o.Workflow.Plan = nil
		o.GradionEnabled = false
		n := deriveDayNext(o)
		if n.State != stateStopped || !strings.Contains(n.Reason, "gradion") {
			t.Fatalf("state/reason = %q/%q, want stopped naming the remote boundary", n.State, n.Reason)
		}
	})

	t.Run("multiple projects without a choice asks the human instead of guessing", func(t *testing.T) {
		o := unclassifiedDay()
		o.Artifacts.StageMap = nil
		o.Project, o.ProjectBlocked = "", true
		o.ProjectIDs = []string{"WORK", "OPS"}
		n := deriveDayNext(o)
		q := n.Ask
		if n.State != stateAskHuman || q.Kind != askProject || strings.Join(q.Options, ",") != "OPS,WORK" {
			t.Fatalf("derivation = %+v, want the project question over the configured ids", n)
		}
		if !strings.Contains(q.Protocol, "--project") {
			t.Fatalf("protocol = %q, want the --project override named", q.Protocol)
		}
		o.ProjectAskError = "answer \"X\" is not one of: OPS, WORK"
		if got := deriveDayNext(o); got.Ask == nil || got.Ask.LastError != o.ProjectAskError {
			t.Fatalf("rejected answer = %+v, want last_error", got)
		}
	})
}

// TestDeriveDayNextHumanGates pins the terminal report: the exact approve and
// submit forms, the read-only status command, and the unresolved counts.
func TestDeriveDayNextHumanGates(t *testing.T) {
	o := finalizedDay()
	o.Workflow.UnresolvedWrites = 2
	o.Workflow.NativeUnresolved = 1
	n := deriveDayNext(o)
	if n.State != stateHumanGates || n.Human == nil {
		t.Fatalf("state = %q human = %+v, want human_gates", n.State, n.Human)
	}
	if want := strings.Join(baseArgv("submit"), " "); n.Human.ApproveCommand != want {
		t.Fatalf("approve = %q, want the terminal submit %q (it approves, then submits)", n.Human.ApproveCommand, want)
	}
	if !strings.HasSuffix(n.Human.SubmitCommand, "--approval-id '<approval_id>'") ||
		!strings.HasPrefix(n.Human.SubmitCommand, nextExe+" submit ") {
		t.Fatalf("submit = %q, want the exact submit argv with the approval-id placeholder", n.Human.SubmitCommand)
	}
	if want := strings.Replace(strings.Join(baseArgv("submit"), " "), " submit ", " submit status ", 1); n.Human.StatusCommand != want {
		t.Fatalf("status = %q, want %q", n.Human.StatusCommand, want)
	}
	if n.Human.Unresolved != 2 || n.Human.NativeUnresolved != 1 {
		t.Fatalf("human = %+v, want the durable unresolved counts", n.Human)
	}
	if n.Action != nil || n.Judgment != nil {
		t.Fatalf("human_gates carries an action or judgment: %+v", n)
	}
}

// TestDeriveDayNextNeverEmitsHumanVerbs is the aging boundary as a test: the
// derived verb set is closed, and no generation emits a human-gated or
// revision/replace verb under any observation shape.
func TestDeriveDayNextNeverEmitsHumanVerbs(t *testing.T) {
	derivable := map[string]bool{"run": true, "record": true, "stage": true, "classify": true, "save": true, "plan": true, "classifications": true}
	observations := []nextObservation{unclassifiedDay(), recordedDay(), finalizedDay()}
	variants := []func(*nextObservation){
		func(o *nextObservation) { o.Artifacts.StageMap = nil },
		func(o *nextObservation) { o.Artifacts.ChoicesPresent, o.Artifacts.Choices = false, nil },
		func(o *nextObservation) { o.ChoicesPresent, o.ChoicesBinding = true, "" },
		func(o *nextObservation) { o.DescriptionsPresent, o.DescriptionsBinding = true, nextOldDigest },
		func(o *nextObservation) { o.GradionEnabled = false },
		func(o *nextObservation) {
			o.GradionEnabled = false
			o.Artifacts.ChoicesPresent = true
			o.Artifacts.Choices = map[string]timesheet.ClassificationChoiceEntry{
				"iv-1": {CatalogID: "cat-1", ClassificationID: "cls-1", ContentDigest: "stale"},
			}
		},
		func(o *nextObservation) { o.Workflow.Plan = nil },
		func(o *nextObservation) { o.Workflow.PlanAmbiguous = true },
		func(o *nextObservation) {
			o.Workflow.Plan = &timesheet.PlanSnapshot{PlanID: "p", State: timesheet.PlanBlocked, CreatedAt: "2026-10-27T10:05:00Z"}
		},
		func(o *nextObservation) {
			o.Artifacts.Entries.Entries[0].Description = "Changed"
			o.Units = ledgerUnits(o.Artifacts.Entries.Entries, nextDay, "UTC")
		},
	}
	for _, base := range observations {
		for _, mutate := range variants {
			o := base
			o.Artifacts.Entries = &timesheet.EntriesFile{Date: nextDay, Entries: append([]timesheet.EntryRecord(nil), base.Artifacts.Entries.Entries...)}
			mutate(&o)
			n := deriveDayNext(o)
			if n.Action == nil {
				continue
			}
			verb := n.Action.Cmd[1]
			if !derivable[verb] {
				t.Fatalf("derived the non-derivable verb %q", verb)
			}
			for _, forbidden := range []string{"--revise", "--replace", "approve", "submit", "resolve-write"} {
				for _, arg := range n.Action.Cmd {
					if strings.Contains(arg, forbidden) {
						t.Fatalf("derived argv %#v carries the human/forbidden token %q", n.Action.Cmd, forbidden)
					}
				}
			}
		}
	}
}

// TestDeriveDayNextStaleSidecarWithoutCatalog pins the offline boundary: with
// the remote catalog disabled a stale persisted sidecar cannot be repaired by
// next, so it must stop naming the supported remedies instead of deriving a
// save that the replay would reject.
func TestDeriveDayNextStaleSidecarWithoutCatalog(t *testing.T) {
	stale := func() nextObservation {
		o := unclassifiedDay()
		o.GradionEnabled = false
		o.Artifacts.ChoicesPresent = true
		o.Artifacts.Choices = map[string]timesheet.ClassificationChoiceEntry{
			"iv-1": {CatalogID: "cat-1", ClassificationID: "cls-1", ContentDigest: "a-digest-from-earlier-content"},
		}
		return o
	}

	t.Run("stale sidecar stops instead of deriving a failing save", func(t *testing.T) {
		n := deriveDayNext(stale())
		if n.State != stateStopped || n.Action != nil {
			t.Fatalf("state/action = %q/%+v, want a stopped state with no action", n.State, n.Action)
		}
		for _, want := range []string{
			"gradion.enabled", "clear-classifications.json", "classify", "selections", "will never clear",
		} {
			if !strings.Contains(n.Reason, want) {
				t.Fatalf("reason = %q, want %q named", n.Reason, want)
			}
		}
		if !strings.Contains(n.Reason, "--file") || !strings.Contains(n.Reason, "--project WORK") {
			t.Fatalf("reason = %q, want the exact offline clear argv", n.Reason)
		}
	})

	t.Run("an absent sidecar still saves unclassified", func(t *testing.T) {
		o := unclassifiedDay()
		o.GradionEnabled = false
		if n := deriveDayNext(o); n.State != stateAction || n.Step != "save" {
			t.Fatalf("state/step = %q/%q, want action/save", n.State, n.Step)
		}
	})

	t.Run("a binding sidecar still saves", func(t *testing.T) {
		o := recordedDay()
		o.GradionEnabled = false
		if n := deriveDayNext(o); n.State != stateAction || n.Step != "save" {
			t.Fatalf("state/step = %q/%q, want action/save", n.State, n.Step)
		}
	})

	t.Run("an explicitly empty sidecar still saves", func(t *testing.T) {
		o := unclassifiedDay()
		o.GradionEnabled = false
		o.Artifacts.ChoicesPresent = true
		o.Artifacts.Choices = map[string]timesheet.ClassificationChoiceEntry{}
		if n := deriveDayNext(o); n.State != stateAction || n.Step != "save" {
			t.Fatalf("state/step = %q/%q, want action/save for the documented no-op", n.State, n.Step)
		}
	})
}

// TestDeriveDayNextIsDeterministic pins that two derivations from an identical
// observation are deep-equal — the planner carries no hidden state.
func TestDeriveDayNextIsDeterministic(t *testing.T) {
	for _, o := range []nextObservation{unclassifiedDay(), recordedDay(), finalizedDay()} {
		if first, second := deriveDayNext(o), deriveDayNext(o); !reflect.DeepEqual(first, second) {
			t.Fatalf("derivations differ:\n%+v\n%+v", first, second)
		}
	}
}
