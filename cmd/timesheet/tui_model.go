package main

// The TUI's pure reducer: model, events, update.

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// loopSnapshot is one tick's derived view of the day; nothing survives a tick.
type loopSnapshot struct {
	day    string
	action nextAction
	status *timesheet.SubmissionStatusResult
}

// tuiEnv is the immutable process context the effects close over: one store,
// one session, one held lock, set once before the alt screen starts.
type tuiEnv struct {
	cfg        *timesheet.Config
	configPath string
	envFile    string
	day        string
	h          *headlessSession
	styles     uiStyles
}

// tuiModel is the Elm-architecture state; update is the only writer. busy is
// the in-flight gate: while one op runs to completion every key is ignored (no
// cancellation; HandleLine takes no context). err is the last failed op or
// tick; the loop keeps the last good snapshot. closeErr is the close op's
// failure on quit; notice is the last act or gate receipt summary; stop ends
// the program.
type tuiModel struct {
	env      tuiEnv
	snap     loopSnapshot
	view     viewState
	busy     bool
	err      error
	closeErr error
	bench    benchState // the authoring or gate form; empty unless one is open
	notice   string
	stop     bool
}

// viewState is the driver's geometry, written only by update's sizeEvent case;
// height 0 (every test-constructed model, and the first frame) means unbounded.
type viewState struct{ height int }

// benchState is the TUI's only mutable domain state: operator text that has
// not gone through a session op. On the bench screen it carries the authored
// document with the judgment's own digest and answer path; on the gate screen,
// the typed echo and fresh artifacts.
type benchState struct {
	screen     tuiScreen
	kind       string                  // judgment kind, or the gate kind approve|submit
	digest     string                  // draft_digest or ledger_digest, copied verbatim
	answerFile string                  // the bench form's target path, or the gate echo prompt
	text       string                  // authored answer document, or the typed echo
	caret      int                     // caret within text, in runes
	preview    *timesheet.PlanPreview  // the approve dialog's fresh preview
	approvals  []timesheet.ApprovalRow // the submit dialog's active approvals
}

// tuiScreen names the screen the operator is on.
type tuiScreen int

const (
	screenDay   tuiScreen = iota
	screenBench           // authoring a judgment answer file
	screenGate            // a display-before-ask gate dialog
)

const (
	gateApprove = "approve"
	gateSubmit  = "submit"
)

type keyEvent struct{ key string }

// sizeEvent carries the driver's terminal height, adapted from
// tea.WindowSizeMsg. It is not a key: it lands while busy (a resize starts no
// op) and produces no effect. Width is dropped until something lays out by it;
// bubbletea already truncates over-wide lines.
type sizeEvent struct{ height int }

// textEvent carries the runes of one text input (typed or pasted). The driver
// produces it from KeyRunes and KeySpace messages only, so a structural key's
// name can never reach an open document as text.
type textEvent struct{ runes []rune }

// tickEvent carries the rebuilt snapshot; reopen reopens a judgment form
// fresh after a stale-revision recovery.
type tickEvent struct {
	snap   loopSnapshot
	reopen string
}

// closeSentEvent reports the close op was sent (the op's error, if any).
type closeSentEvent struct{ err error }

// actDoneEvent reports one act or gate op ran to completion; notice is the
// receipt summary the day screen keeps until the next act or gate reports.
type actDoneEvent struct {
	err    error
	notice string
}

// editorOpenedEvent carries what a form opens with: the bench skeleton, or
// the gate's fresh preview or approvals.
type editorOpenedEvent struct {
	kind       string
	digest     string
	answerFile string
	text       string
	preview    *timesheet.PlanPreview
	approvals  []timesheet.ApprovalRow
	err        error
}

// gateCheckEvent carries the freshness artifact a gate re-ran immediately
// before any op could be emitted.
type gateCheckEvent struct {
	fresh  *timesheet.PlanPreview
	status *timesheet.SubmissionStatusResult
	echo   string
}

// effect is one pending IO step the driver runs to completion; update never
// calls one.
type effect func() any

// update is the pure reducer: (model, event) -> (model, effect).
func update(m tuiModel, ev any) (tuiModel, effect) {
	switch e := ev.(type) {
	case sizeEvent:
		m.view.height = e.height
		return m, nil
	case keyEvent:
		return updateKey(m, e.key)
	case textEvent:
		return updateText(m, e)
	case tickEvent:
		m.busy, m.err = false, nil
		m.snap = e.snap
		if e.reopen != "" {
			j := e.snap.action.Judgment
			if e.snap.action.State == stateJudgment && j != nil && j.Kind == e.reopen {
				m.bench = benchState{screen: screenBench}
				return m, openEditorEffect(m, e.reopen)
			}
			m.bench = benchState{} // the loop moved on; never a dead form
		}
		return m, nil
	case closeSentEvent:
		m.stop, m.closeErr = true, e.err
		return m, nil
	case actDoneEvent:
		return updateActDone(m, e)
	case gateCheckEvent:
		return updateGateCheck(m, e)
	case editorOpenedEvent:
		m.busy = false
		if e.err != nil {
			m.err = e.err
			return m, nil
		}
		m.err = nil
		m.bench = benchState{
			screen: screenBench, kind: e.kind, digest: e.digest,
			answerFile: e.answerFile, text: e.text, caret: utf8.RuneCountInString(e.text),
			preview: e.preview, approvals: e.approvals,
		}
		if e.kind == gateApprove || e.kind == gateSubmit {
			m.bench.screen = screenGate
		}
		return m, nil
	default:
		return m, nil
	}
}

// updateActDone finishes one act or gate op: success re-derives and keeps the
// receipt's notice line; a stale_revision re-runs review and reopens the form
// fresh; any other failure is reported under a cleared busy lock and an open
// form keeps the operator's text.
func updateActDone(m tuiModel, e actDoneEvent) (tuiModel, effect) {
	if e.err == nil {
		m.err, m.notice, m.bench = nil, e.notice, benchState{}
		return m, tickEffect(m.env)
	}
	var app *timesheet.AppError
	if errors.As(e.err, &app) && app.Code == timesheet.CodeStaleRevision {
		reopen := ""
		if m.bench.screen == screenBench {
			reopen = m.bench.kind
		}
		return m, func() any { return m.env.refreshAfterStale(reopen) }
	}
	m.err, m.busy = e.err, false
	return m, nil
}

// updateGateCheck is both gates' decision point: approve_plan only for the
// fresh preview's exact plan_id, submit_plan only for an approval_id still
// ACTIVE on the fresh submission_status. Any drift emits nothing.
func updateGateCheck(m tuiModel, e gateCheckEvent) (tuiModel, effect) {
	m.busy = false
	echo := strings.TrimSpace(e.echo)
	switch {
	case e.fresh != nil:
		if e.fresh.PlanID == echo && echo != "" {
			m.busy = true
			return m, func() any { return m.env.approveOp(echo) }
		}
	case e.status != nil:
		if activeApprovalFor(e.status.Approvals, echo) != nil {
			m.busy = true
			return m, func() any { return m.env.submitOp(echo) }
		}
	}
	m.err = errors.New("the typed id does not match the fresh artifact (or nothing was typed); nothing was emitted, the gate re-renders")
	return m, nil
}

// updateKey maps keys to acts; while an op is in flight every key is ignored,
// so two effects can never run on the session at once.
func updateKey(m tuiModel, key string) (tuiModel, effect) {
	if m.busy {
		return m, nil
	}
	if m.bench.screen != screenDay {
		return updateFormKey(m, key)
	}
	switch key {
	case "q", "ctrl+c":
		// The close op runs under the busy lock, so keys pressed while it is
		// in flight cannot start a racing act on the unsynchronized session;
		// closeSentEvent stops the program either way, so busy is never
		// cleared.
		m.busy, m.err = true, nil
		return m, m.env.sendClose
	case "r":
		// A new op supersedes the last failure's report.
		m.busy, m.err = true, nil
		return m, tickEffect(m.env)
	case "enter":
		step := stepAct(m.snap.action)
		if step == "" {
			return m, nil
		}
		if step == stateHumanGates {
			m.busy, m.err = true, nil
			return m, func() any { return m.env.openGate(gateApprove) }
		}
		if step == judgmentBlockDescriptions || step == judgmentClassifications {
			if m.snap.action.Judgment == nil {
				return m, nil
			}
			m.busy, m.err = true, nil
			return m, openEditorEffect(m, step)
		}
		m.busy, m.err = true, nil
		var cmd []string
		if m.snap.action.Action != nil {
			cmd = m.snap.action.Action.Cmd
		}
		return m, func() any { return m.env.act(step, cmd) }
	case "s": // the submit gate; the approve gate is enter, the day screen's act key
		if stepAct(m.snap.action) != stateHumanGates {
			return m, nil
		}
		m.busy, m.err = true, nil
		return m, func() any { return m.env.openGate(gateSubmit) }
	default:
		return m, nil
	}
}

// activeApprovalFor returns the approval row the echo names, if still active.
func activeApprovalFor(approvals []timesheet.ApprovalRow, echo string) *timesheet.ApprovalRow {
	echo = strings.TrimSpace(echo)
	if echo == "" {
		return nil
	}
	for i := range approvals {
		if approvals[i].ApprovalID == echo && approvals[i].State == timesheet.ApprovalActive {
			return &approvals[i]
		}
	}
	return nil
}

func openEditorEffect(m tuiModel, kind string) effect {
	digest, answerFile, blocks := "", "", []nextBlock(nil)
	if j := m.snap.action.Judgment; j != nil {
		digest, answerFile, blocks = j.DraftDigest, j.AnswerFile, j.Blocks
		if digest == "" {
			digest = j.LedgerDigest
		}
	}
	return func() any { return m.env.openEditor(kind, digest, answerFile, blocks) }
}

// tickEffect wraps one tick as an effect; a failed derivation lands on the
// act failure path with its real error, a stop document is a tickEvent.
func tickEffect(e tuiEnv) effect {
	return func() any {
		te, err := e.tick()
		if err != nil {
			return actDoneEvent{err: err}
		}
		return te
	}
}

// updateFormKey is both form screens' key map. Authored text arrives as a
// textEvent, never through here, so every named key outside this switch is
// ignored on a form.
func updateFormKey(m tuiModel, key string) (tuiModel, effect) {
	b := m.bench
	switch key {
	case "esc":
		m.bench = benchState{}
		return m, nil
	case "enter":
		if b.screen == screenGate {
			kind := b.kind
			m.busy, m.err = true, nil
			return m, func() any { return m.env.confirmGate(kind, b.text) }
		}
		return m, nil
	case "ctrl+s":
		if b.screen == screenBench {
			m.busy, m.err = true, nil
			return m, func() any { return m.env.writeBench(b) }
		}
	case "left", "right", "backspace":
		b.text, b.caret = editLine(b.text, b.caret, key)
		m.bench = b
	}
	return m, nil
}

// updateText inserts authored text at the caret on the form screens; on the
// day screen the act keys are plain runes, so text routes to the key map.
func updateText(m tuiModel, e textEvent) (tuiModel, effect) {
	if m.bench.screen == screenDay {
		return updateKey(m, string(e.runes))
	}
	if m.busy {
		return m, nil
	}
	b := m.bench
	b.text, b.caret = insertRunes(b.text, b.caret, e.runes)
	m.bench = b
	return m, nil
}

// editLine applies one structural editing key to the form's document and
// caret; it never inserts text.
func editLine(text string, caret int, key string) (string, int) {
	runes := []rune(text)
	switch key {
	case "left":
		return text, max(caret-1, 0)
	case "right":
		return text, min(caret+1, len(runes))
	case "backspace":
		if caret == 0 {
			return text, caret
		}
		out := make([]rune, 0, len(runes))
		out = append(out, runes[:caret-1]...)
		out = append(out, runes[caret:]...)
		return string(out), caret - 1
	}
	return text, caret
}

func insertRunes(text string, caret int, ins []rune) (string, int) {
	if len(ins) == 0 {
		return text, caret
	}
	runes := []rune(text)
	out := make([]rune, 0, len(runes)+len(ins))
	out = append(out, runes[:caret]...)
	out = append(out, ins...)
	out = append(out, runes[caret:]...)
	return string(out), caret + len(ins)
}
