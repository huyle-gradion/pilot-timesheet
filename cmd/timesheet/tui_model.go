package main

import (
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

type loopSnapshot struct {
	day    string
	action nextAction
	status *timesheet.SubmissionStatusResult
}

type tuiEnv struct {
	cfg        *timesheet.Config
	configPath string
	envFile    string
	day        string
	h          *headlessSession
	styles     uiStyles
}

type tuiModel struct {
	env      tuiEnv
	snap     loopSnapshot
	view     viewState
	busy     bool
	err      error
	closeErr error
	bench    benchState
	notice   string
	stop     bool
}

// viewState is the driver's geometry, written only by update's sizeEvent case;
// height 0 (every test-constructed model, and the first frame) means unbounded.
type viewState struct{ height int }

type benchState struct {
	screen     tuiScreen
	kind       string
	digest     string
	answerFile string
	text       string
	caret      int // caret within text, in runes
	preview    *timesheet.PlanPreview
	approvals  []timesheet.ApprovalRow
	question   string
	options    []string
	choice     int
}

type tuiScreen int

const (
	screenDay tuiScreen = iota
	screenBench
	screenGate
	screenAsk
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

type textEvent struct{ runes []rune }

type tickEvent struct {
	snap   loopSnapshot
	reopen string
}

type closeSentEvent struct{ err error }

type actDoneEvent struct {
	err    error
	notice string
}

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
			m.bench = benchState{}
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

func updateKey(m tuiModel, key string) (tuiModel, effect) {
	if m.busy {
		return m, nil
	}
	if m.bench.screen != screenDay {
		return updateFormKey(m, key)
	}
	switch key {
	case "q", "ctrl+c":
		m.busy, m.err = true, nil
		return m, m.env.sendClose
	case "r":
		m.busy, m.err = true, nil
		return m, tickEffect(m.env)
	case "enter":
		if m.snap.action.State == stateAskHuman && m.snap.action.Ask != nil {
			return openAskForm(m, m.snap.action.Ask), nil
		}
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
	case "s":
		if stepAct(m.snap.action) != stateHumanGates {
			return m, nil
		}
		m.busy, m.err = true, nil
		return m, func() any { return m.env.openGate(gateSubmit) }
	default:
		return m, nil
	}
}

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

func tickEffect(e tuiEnv) effect {
	return func() any {
		te, err := e.tick()
		if err != nil {
			return actDoneEvent{err: err}
		}
		return te
	}
}

func openAskForm(m tuiModel, q *askHuman) tuiModel {
	if err := answerable(q); err != nil {
		m.err = err
		return m
	}
	m.err = nil
	m.bench = benchState{
		screen: screenAsk, kind: q.Kind, digest: q.Digest, answerFile: q.AnswerFile,
		question: q.Question, options: slices.Clone(q.Options),
	}
	return m
}

func updateAskKey(m tuiModel, key string) (tuiModel, effect) {
	b := m.bench
	switch key {
	case "esc":
		m.bench, m.err = benchState{}, nil
	case "up", "k":
		b.choice = max(b.choice-1, 0)
		m.bench = b
	case "down", "j":
		b.choice = min(b.choice+1, len(b.options)-1)
		m.bench = b
	case "enter":
		if b.choice < 0 || b.choice >= len(b.options) {
			return m, nil
		}
		m.busy, m.err = true, nil
		answer := pendingAnswer{Kind: b.kind, Digest: b.digest, Answer: b.options[b.choice], Surface: "tui"}
		return m, func() any { return m.env.answerAsk(answer) }
	}
	return m, nil
}

func updateFormKey(m tuiModel, key string) (tuiModel, effect) {
	if m.bench.screen == screenAsk {
		return updateAskKey(m, key)
	}
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

func updateText(m tuiModel, e textEvent) (tuiModel, effect) {
	if m.bench.screen == screenDay {
		return updateKey(m, string(e.runes))
	}
	if m.busy {
		return m, nil
	}
	if m.bench.screen == screenAsk {
		return updateAskKey(m, string(e.runes))
	}
	b := m.bench
	b.text, b.caret = insertRunes(b.text, b.caret, e.runes)
	m.bench = b
	return m, nil
}

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
