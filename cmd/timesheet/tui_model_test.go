package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// --- the pure reducer: table-driven, headless, no terminal state --------------

func dayModel() tuiModel {
	return tuiModel{
		env:  tuiEnv{day: "2026-10-20"},
		snap: loopSnapshot{day: "2026-10-20", action: nextAction{Day: "2026-10-20"}},
	}
}

func busyModel() tuiModel {
	m := dayModel()
	m.busy = true
	return m
}

// TestUpdateKeyTable pins the key map: r re-derives, q and ctrl+c close under
// the busy lock, every other key is a no-op, and while an op is in flight
// every key is ignored so two effects can never run on the session at once.
func TestUpdateKeyTable(t *testing.T) {
	tests := []struct {
		name       string
		model      tuiModel
		key        string
		wantBusy   bool
		wantEffect bool
	}{
		{"r re-derives", dayModel(), "r", true, true},
		{"q closes under the busy lock", dayModel(), "q", true, true},
		{"ctrl+c closes under the busy lock", dayModel(), "ctrl+c", true, true},
		{"other keys are a no-op", dayModel(), "x", false, false},
		{"busy locks re-derive", busyModel(), "r", true, false},
		{"busy locks quit", busyModel(), "q", true, false},
		{"busy locks every other key", busyModel(), "x", true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, eff := update(tc.model, keyEvent{key: tc.key})
			if m.busy != tc.wantBusy {
				t.Fatalf("busy = %v, want %v", m.busy, tc.wantBusy)
			}
			if (eff != nil) != tc.wantEffect {
				t.Fatalf("effect = %v, want presence %v", eff, tc.wantEffect)
			}
			if m.stop {
				t.Fatal("no key may stop the program directly; the close effect does")
			}
		})
	}
}

// TestUpdateEventsTable pins every event's state transition.
func TestUpdateEventsTable(t *testing.T) {
	t.Run("tick replaces the snapshot and clears busy and error", func(t *testing.T) {
		m := busyModel()
		m.err = errors.New("older failure")
		fresh := loopSnapshot{day: "d", action: nextAction{Day: "d", State: stateStopped, Reason: "r"}}
		m, eff := update(m, tickEvent{snap: fresh})
		if eff != nil || m.busy || m.err != nil {
			t.Fatalf("tick left busy/err/effect: %+v %v", m, eff)
		}
		if m.snap.action.State != stateStopped || m.snap.action.Reason != "r" {
			t.Fatalf("snapshot = %+v, want the fresh stop document", m.snap)
		}
	})
	t.Run("an act failure clears the busy lock and keeps the last good snapshot", func(t *testing.T) {
		m := busyModel()
		before := m.snap
		m, _ = update(m, actDoneEvent{err: errors.New("boom")})
		if m.busy || m.err == nil || m.snap != before {
			t.Fatalf("act failure = %+v, want busy cleared, error set, snapshot kept", m)
		}
	})
	t.Run("close sent stops the program", func(t *testing.T) {
		m, _ := update(dayModel(), closeSentEvent{err: errors.New("close failed")})
		if !m.stop || m.closeErr == nil {
			t.Fatalf("close sent = %+v, want stop with the close error kept", m)
		}
	})
	t.Run("a size event stores geometry even while busy", func(t *testing.T) {
		m := busyModel()
		got, eff := update(m, sizeEvent{height: 42})
		if eff != nil || got.view.height != 42 || !got.busy {
			t.Fatalf("size event = height %d busy %v effect %v, want the geometry stored under the busy lock", got.view.height, got.busy, eff)
		}
		if got.snap != m.snap || got.err != m.err {
			t.Fatalf("size event mutated the loop state: %+v", got)
		}
	})
	t.Run("unknown events are ignored", func(t *testing.T) {
		m := dayModel()
		got, _ := update(m, unknownTestEvent{})
		if got.snap != m.snap || got.busy || got.stop {
			t.Fatalf("unknown event mutated the model: %+v", got)
		}
	})
}

// TestFormTextEditing pins the form input boundary: a named structural key
// never changes the document, text inserts at the caret, and a multi-rune
// paste inserts exactly.
func TestFormTextEditing(t *testing.T) {
	form := func() tuiModel {
		m := dayModel()
		m.bench = benchState{screen: screenBench, kind: judgmentBlockDescriptions, text: "ab", caret: 1}
		return m
	}
	t.Run("named keys never change the document", func(t *testing.T) {
		for _, key := range []string{"home", "end", "delete", "insert", "shift+tab", "f3", "up", "tab", "pgup", "ctrl+left"} {
			m, _ := update(form(), keyEvent{key: key})
			if m.bench.text != "ab" || m.bench.caret != 1 {
				t.Fatalf("key %q changed the document to %q at caret %d", key, m.bench.text, m.bench.caret)
			}
		}
	})
	t.Run("text inserts at the caret", func(t *testing.T) {
		m, _ := update(form(), textEvent{runes: []rune{'X'}})
		if m.bench.text != "aXb" || m.bench.caret != 2 {
			t.Fatalf("text insert = %q at caret %d, want aXb at 2", m.bench.text, m.bench.caret)
		}
	})
	t.Run("a space inserts a space", func(t *testing.T) {
		m, _ := update(form(), textEvent{runes: []rune{' '}})
		if m.bench.text != "a b" {
			t.Fatalf("space insert = %q, want a b", m.bench.text)
		}
	})
	t.Run("a multi-rune paste inserts exactly", func(t *testing.T) {
		m, _ := update(form(), textEvent{runes: []rune("hello")})
		if m.bench.text != "ahellob" || m.bench.caret != 6 {
			t.Fatalf("paste = %q at caret %d, want ahellob at 6", m.bench.text, m.bench.caret)
		}
	})
	t.Run("structural keys still move the caret and delete", func(t *testing.T) {
		m, _ := update(form(), keyEvent{key: "backspace"})
		if m.bench.text != "b" || m.bench.caret != 0 {
			t.Fatalf("backspace = %q at caret %d, want b at 0", m.bench.text, m.bench.caret)
		}
		m, _ = update(form(), keyEvent{key: "left"})
		if m.bench.caret != 0 {
			t.Fatalf("left moved the caret to %d, want 0", m.bench.caret)
		}
	})
	t.Run("the day screen keeps its rune act keys", func(t *testing.T) {
		m, eff := update(dayModel(), textEvent{runes: []rune{'r'}})
		if eff == nil || !m.busy {
			t.Fatalf("day-screen r = effect %v busy %v, want the re-derive", eff, m.busy)
		}
	})
}

// TestReDeriveAfterErrorSupersedesReport pins the honest busy line: a new
// re-derive clears the stale error report while it runs, so the footer shows
// the busy lock, not an older failure.
func TestReDeriveAfterErrorSupersedesReport(t *testing.T) {
	m := dayModel()
	m, _ = update(m, actDoneEvent{err: errors.New("boom")})
	if m.err == nil {
		t.Fatal("the failure was not reported")
	}
	m, eff := update(m, keyEvent{key: "r"})
	if eff == nil || !m.busy || m.err != nil {
		t.Fatalf("re-derive after an error = busy %v err %v effect %v, want busy with the error superseded", m.busy, m.err, eff)
	}
}

type unknownTestEvent struct{}

// TestPreflightBuildsFirstSnapshot drives the real preflight over a scratch
// store: an empty day derives the run action before any screen starts.
func TestPreflightBuildsFirstSnapshot(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	pretendTTY(t)
	cfgPath := writeRunConfig(t)
	m, err := preflightTui(&nextFlags{configPath: cfgPath, date: "2026-10-20"})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	defer func() { _ = m.env.h.store.Close() }()
	if m.snap.action.State != stateAction || m.snap.action.Step != "run" {
		t.Fatalf("first snapshot = %+v, want action/run", m.snap.action)
	}
	if m.snap.day != "2026-10-20" {
		t.Fatalf("snapshot day = %q, want the resolved day", m.snap.day)
	}
	if m.env.h == nil || m.env.h.store == nil || m.env.h.session == nil {
		t.Fatal("preflight built no held session over its own store")
	}
}

// TestEnvTickRebuildsLoopSnapshot pins the tick recipe: one call, one rebuilt
// snapshot, the caller's store still open afterwards.
func TestEnvTickRebuildsLoopSnapshot(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	pretendTTY(t)
	cfgPath := writeRunConfig(t)
	m, err := preflightTui(&nextFlags{configPath: cfgPath, date: "2026-10-20"})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	defer func() { _ = m.env.h.store.Close() }()
	te, err := m.env.tick()
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if te.snap.day != "2026-10-20" || te.snap.action.State != stateAction {
		t.Fatalf("tick snapshot = %+v, want the derived action for the day", te.snap)
	}
	// The store is still held: a second read through the same lock succeeds.
	if _, err := timesheet.ReadWorkflowSnapshot(m.env.cfg, m.env.h.store, "2026-10-20"); err != nil {
		t.Fatalf("the tick closed the caller's store: %v", err)
	}
}

// TestPreflightReleasesTheStoreOnError pins the preflight's error path: a
// failure after the store is opened (here an unreadable env file; the
// unknown --project that once triggered it is retired, ADR 0012) surfaces its
// own refusal, and the held store is released.
func TestPreflightReleasesTheStoreOnError(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	pretendTTY(t)
	cfgPath := writeRunConfig(t)
	missing := filepath.Join(t.TempDir(), "missing.env")
	if _, err := preflightTui(&nextFlags{configPath: cfgPath, envFile: missing, date: "2026-10-20"}); err == nil {
		t.Fatal("preflight with an unreadable env file succeeded")
	}
	cfg, err := timesheet.LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := timesheet.OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatalf("the failed preflight did not release the store: %v", err)
	}
	_ = reopened.Close()
}
