package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// --- the step table: every deriveDayNext output maps to exactly one act ------

func TestStepForCoversEveryDerivationOutput(t *testing.T) {
	classifyDefault := nextAction{Day: "d"}.withAction("classify",
		[]string{"/exe", "classify", "--configured-default"}, "why")
	tests := []struct {
		name string
		act  nextAction
		want string
	}{
		{"run", nextAction{Day: "d"}.withAction("run", []string{"/exe", "run"}, "why"), "run"},
		{"record", nextAction{Day: "d"}.withAction("record", []string{"/exe", "record"}, "why"), "record"},
		{"stage", nextAction{Day: "d"}.withAction("stage", []string{"/exe", "stage"}, "why"), "stage"},
		{"save", nextAction{Day: "d"}.withAction("save", []string{"/exe", "save"}, "why"), "save"},
		{"plan", nextAction{Day: "d"}.withAction("plan", []string{"/exe", "plan"}, "why"), "plan"},
		{"classify authored", nextAction{Day: "d"}.withAction("classify", []string{"/exe", "classify", "--file", "/c.json"}, "why"), "classify"},
		{"classify configured default", classifyDefault, "classify-default"},
		{"descriptions judgment", nextAction{Day: "d"}.withJudgment("review", nextJudgment{Kind: judgmentBlockDescriptions}), judgmentBlockDescriptions},
		{"classifications judgment", nextAction{Day: "d"}.withJudgment("classify", nextJudgment{Kind: judgmentClassifications}), judgmentClassifications},
		{"stopped renders only", nextAction{Day: "d"}.withStop("reason"), ""},
		{"human gates", nextAction{Day: "d"}.withHuman(nextHumanGates{}), stateHumanGates},
		{"unknown action step", nextAction{Day: "d"}.withAction("reconcile", []string{"/exe"}, "why"), "reconcile"},
		{"action payload missing", nextAction{Day: "d", State: stateAction}, ""},
		{"judgment payload missing", nextAction{Day: "d", State: stateJudgment}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := stepAct(tc.act); got != tc.want {
				t.Fatalf("stepAct = %q, want %q", got, tc.want)
			}
		})
	}
}

// drive applies one key and runs the returned effect chain to completion, so a
// test drives the same act -> actDone -> tick -> tickEvent path the driver does.
func drive(t *testing.T, m tuiModel, key string) tuiModel {
	t.Helper()
	m, eff := update(m, keyEvent{key: key})
	for hops := 0; eff != nil; hops++ {
		if hops > 8 {
			t.Fatal("the effect chain did not settle")
		}
		m, eff = update(m, eff())
	}
	return m
}

// typeInto feeds one rune per text event to the reducer without running
// effects, the way the driver delivers typed text.
func typeInto(m tuiModel, text string) tuiModel {
	for _, r := range text {
		m, _ = update(m, textEvent{runes: []rune{r}})
	}
	return m
}

// clearBench empties the form the way an operator would: backspace to the
// start (the caret opens at the end of the pre-filled skeleton).
func clearBench(m tuiModel) tuiModel {
	n := len([]rune(m.bench.text))
	for i := 0; i < n; i++ {
		m, _ = update(m, keyEvent{key: "backspace"})
	}
	return m
}

// TestTuiDayLoopRoundTrip is the Unit-3 acceptance proof: the TUI drives a
// scratch day from an empty store through collect, the descriptions judgment,
// record, stage and save using only the derived steps' acts, and the writer
// copies the judgment's digest verbatim.
func TestTuiDayLoopRoundTrip(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	pretendTTY(t)
	cfg := writeRunConfig(t)
	srv, _ := startFeed(t)
	envFile := writeRunEnvFile(t, srv.URL)
	day := "2026-10-20"
	m, err := preflightTui(&nextFlags{configPath: cfg, envFile: envFile, date: day})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	defer func() { _ = m.env.h.store.Close() }()
	if m.snap.action.State != stateAction || m.snap.action.Step != "run" {
		t.Fatalf("first snapshot = %q/%q, want action/run", m.snap.action.State, m.snap.action.Step)
	}

	// The derived run act collects and writes the draft; the loop re-derives
	// into the descriptions judgment.
	m = drive(t, m, "enter")
	if m.snap.action.State != stateJudgment || m.snap.action.Judgment == nil ||
		m.snap.action.Judgment.Kind != judgmentBlockDescriptions {
		t.Fatalf("after the run act = %+v, want the descriptions judgment", m.snap.action)
	}

	// The bench form opens pre-filled with the protocol's own document and the
	// judgment's digest; the operator authors prose over it and writes.
	m = drive(t, m, "enter")
	if m.bench.screen != screenBench || m.bench.text == "" {
		t.Fatalf("bench = %+v, want an open form with the skeleton document", m.bench)
	}
	digest, answerFile := m.bench.digest, m.bench.answerFile
	if digest == "" || digest != m.snap.action.Judgment.DraftDigest {
		t.Fatalf("bench digest %q does not copy the judgment's %q", digest, m.snap.action.Judgment.DraftDigest)
	}
	var skeleton struct {
		Descriptions map[string]string `json:"descriptions"`
	}
	if err := json.Unmarshal([]byte(m.bench.text), &skeleton); err != nil {
		t.Fatalf("the pre-filled document is not the protocol's JSON: %v\n%s", err, m.bench.text)
	}
	if len(skeleton.Descriptions) == 0 {
		t.Fatalf("the pre-filled document lists no blocks: %s", m.bench.text)
	}
	authored := map[string]string{}
	for id := range skeleton.Descriptions {
		authored[id] = "Synthetic description for " + id
	}
	body, err := json.MarshalIndent(map[string]any{
		"draft_digest": digest, "notes": "synthetic day notes", "descriptions": authored,
	}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	m = clearBench(m)
	m = typeInto(m, string(body))
	if m.bench.text != string(body) {
		t.Fatalf("authored text did not survive the editing keys:\n%s", m.bench.text)
	}
	m = drive(t, m, "ctrl+s")
	if m.bench.screen == screenBench {
		t.Fatalf("bench still open after a successful write: %+v", m.bench)
	}
	if m.snap.action.State != stateAction || m.snap.action.Step != "record" {
		t.Fatalf("after the write = %q/%q (error %v), want action/record", m.snap.action.State, m.snap.action.Step, m.err)
	}
	var answer struct {
		DraftDigest  string            `json:"draft_digest"`
		Notes        string            `json:"notes"`
		Descriptions map[string]string `json:"descriptions"`
	}
	data, err := os.ReadFile(answerFile)
	if err != nil {
		t.Fatalf("the answer file was not written: %v", err)
	}
	if err := json.Unmarshal(data, &answer); err != nil {
		t.Fatalf("the answer file is not valid JSON: %v\n%s", err, data)
	}
	if answer.DraftDigest != digest || answer.Notes != "synthetic day notes" {
		t.Fatalf("answer = %+v, want the digest copied verbatim and the typed notes", answer)
	}
	for id, text := range authored {
		if answer.Descriptions[id] != text {
			t.Fatalf("description for %s = %q, want %q", id, answer.Descriptions[id], text)
		}
	}

	// Record, stage and save each run from the derived step alone, and the
	// stage and save receipts fold their summaries into the day screen's
	// notice line; the offline configuration then stops at the plan remedy,
	// rendered verbatim.
	for _, want := range []string{"record", "stage", "save"} {
		if m.snap.action.Step != want {
			t.Fatalf("step = %q, want %q (last error: %v)", m.snap.action.Step, want, m.err)
		}
		m = drive(t, m, "enter")
		switch want {
		case "stage":
			if !strings.Contains(m.notice, "staged 1 interval(s) at revision ") {
				t.Fatalf("stage notice = %q, want the receipt summary", m.notice)
			}
		case "save":
			if !strings.Contains(m.notice, "saved revision ") {
				t.Fatalf("save notice = %q, want the receipt summary", m.notice)
			}
		}
	}
	if m.snap.action.State != stateStopped {
		t.Fatalf("final state = %q, want stopped at the offline plan remedy", m.snap.action.State)
	}
	if !strings.Contains(m.snap.action.Reason, "gradion.enabled") {
		t.Fatalf("stopped reason = %q, want the verbatim remote-boundary remedy", m.snap.action.Reason)
	}
}

// TestBenchWriteAsAuthored pins the writer's contract: the document is
// written exactly as authored, a lost digest refuses the write, an empty
// document refuses, and esc abandons the form.
func TestBenchWriteAsAuthored(t *testing.T) {
	t.Run("the document is written exactly as authored", func(t *testing.T) {
		bench := benchState{
			screen: screenBench, kind: judgmentClassifications, digest: "ledger-1",
			answerFile: filepath.Join(t.TempDir(), "choices.json"),
			text:       "{\"ledger_digest\":\"ledger-1\",\"selections\":{\"iv-1\":{\"catalog_id\":\"cat-1\",\"classification_id\":\"opt-a\"}}}",
			caret:      10,
		}
		done := (tuiEnv{}).writeBench(bench).(actDoneEvent)
		if done.err != nil {
			t.Fatalf("write: %v", done.err)
		}
		data, err := os.ReadFile(bench.answerFile)
		if err != nil {
			t.Fatal(err)
		}
		var answer struct {
			LedgerDigest string `json:"ledger_digest"`
			Selections   map[string]struct {
				CatalogID        string `json:"catalog_id"`
				ClassificationID string `json:"classification_id"`
			} `json:"selections"`
		}
		if err := json.Unmarshal(data, &answer); err != nil {
			t.Fatalf("the answer file is not valid JSON: %v\n%s", err, data)
		}
		if answer.LedgerDigest != "ledger-1" {
			t.Fatalf("ledger_digest = %q, want the judgment's digest verbatim", answer.LedgerDigest)
		}
		if answer.Selections["iv-1"].ClassificationID != "opt-a" || answer.Selections["iv-1"].CatalogID != "cat-1" {
			t.Fatalf("selections = %+v, want the authored assignment untouched", answer.Selections)
		}
	})
	t.Run("a lost digest refuses the write", func(t *testing.T) {
		bench := benchState{
			screen: screenBench, kind: judgmentClassifications, digest: "ledger-1",
			answerFile: filepath.Join(t.TempDir(), "choices.json"),
			text:       "{\"ledger_digest\":\"\",\"selections\":{}}",
		}
		done := (tuiEnv{}).writeBench(bench).(actDoneEvent)
		if done.err == nil || !strings.Contains(done.err.Error(), "digest") {
			t.Fatalf("lost-digest write = %v, want the refusal", done.err)
		}
	})
	t.Run("an empty document refuses", func(t *testing.T) {
		done := (tuiEnv{}).writeBench(benchState{screen: screenBench, kind: judgmentBlockDescriptions}).(actDoneEvent)
		if done.err == nil || !strings.Contains(done.err.Error(), "empty") {
			t.Fatalf("empty write = %v, want the refusal", done.err)
		}
	})
	t.Run("esc abandons the form", func(t *testing.T) {
		bench := benchState{screen: screenBench, kind: judgmentBlockDescriptions, text: "x", caret: 1}
		m, _ := update(tuiModel{env: tuiEnv{}, bench: bench}, keyEvent{key: "esc"})
		if m.bench.screen == screenBench || m.bench.kind != "" {
			t.Fatalf("esc left bench state behind: %+v", m.bench)
		}
	})
}

// TestStaleRevisionRecovery pins the recovery contract: an act failing
// stale_revision re-runs review, re-derives, and reopens a bench form of the
// same judgment kind fresh; a snapshot that no longer asks for that judgment
// leaves the operator on the day screen instead of a dead form.
func TestStaleRevisionRecovery(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	pretendTTY(t)
	cfgPath := writeRunConfig(t)
	m, err := preflightTui(&nextFlags{configPath: cfgPath, date: "2026-10-20"})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	defer func() { _ = m.env.h.store.Close() }()
	if _, err := m.env.h.call("open_day", timesheet.Request{Day: "2026-10-20"}); err != nil {
		t.Fatalf("open_day: %v", err)
	}

	// A stale bench write runs the review-and-retick recovery chain.
	m.bench = benchState{screen: screenBench, kind: judgmentClassifications}
	m, eff := update(m, actDoneEvent{err: &timesheet.AppError{Code: timesheet.CodeStaleRevision, Message: "expected revision 1 but the day is at revision 2"}})
	if eff == nil || m.busy {
		t.Fatalf("stale act = effect %v busy %v, want the recovery effect", eff, m.busy)
	}
	m, eff = update(m, eff())
	if eff != nil {
		t.Fatalf("recovery left an effect pending")
	}
	if m.bench.screen == screenBench {
		t.Fatalf("the form reopened over a day that no longer asks for a judgment")
	}
	if m.snap.action.State != stateAction || m.snap.action.Step != "run" {
		t.Fatalf("recovered snapshot = %+v, want the fresh derived state", m.snap.action)
	}

	// A fresh snapshot still asking for the same judgment reopens the form.
	m.bench = benchState{}
	fresh := loopSnapshot{day: "d", action: nextAction{Day: "d"}.withJudgment("classify",
		nextJudgment{Kind: judgmentClassifications, AnswerFile: "/c.json", LedgerDigest: "ld"})}
	m, eff = update(m, tickEvent{snap: fresh, reopen: judgmentClassifications})
	if m.bench.screen != screenBench || eff == nil {
		t.Fatalf("reopen = bench %+v effect %v, want the form reopened with a fresh pre-fill effect", m.bench, eff)
	}
}

// TestActPlanFailsClosedWithoutRemote pins the plan act's op dispatch: on a
// scratch store with no saved work the preview_plan op refuses with its own
// typed error, passed through.
func TestActPlanFailsClosedWithoutRemote(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	pretendTTY(t)
	cfgPath := writeRunConfig(t)
	m, err := preflightTui(&nextFlags{configPath: cfgPath, date: "2026-10-20"})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	defer func() { _ = m.env.h.store.Close() }()
	ev := m.env.act("plan", nil)
	done, ok := ev.(actDoneEvent)
	if !ok {
		t.Fatalf("act returned %T, want actDoneEvent", ev)
	}
	var app *timesheet.AppError
	if !errors.As(done.err, &app) || app.Code != timesheet.CodeNoConfirmedWork {
		t.Fatalf("plan act error = %v, want the op's own typed refusal", done.err)
	}
}
