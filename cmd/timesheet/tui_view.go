package main

// The TUI's rendering: pure functions, one per screen, model to string.

import (
	"strconv"
	"strings"
	"unicode"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// sanitizeContent strips every control rune except '\n' from one recorded
// string at its last hop into a frame. Recorded content — evidence,
// descriptions, protocol strings, plan bodies, errors — passes through here
// exactly once; the UI's own styling never does, so every control rune in a
// finished frame came from the style layer, never from content.
func sanitizeContent(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' {
			return -1
		}
		return r
	}, s)
}

// rows counts the terminal rows a composed block renders: every '\n' ends a
// row, and a block that does not end with '\n' still occupies its last one.
func rows(s string) int {
	n := strings.Count(s, "\n")
	if s != "" && !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// renderDay renders the day screen: the loop state and the key legend. The
// state block is composed as a pinned part (the judgment's instruction
// surface, the gate commands, counts and next-line) and a windowable part
// (block lists, status rows; the whole block for the act and stopped states),
// so clipping spends the budget on repetitive rows and never on the text that
// tells the operator what to do.
func renderDay(m tuiModel) string {
	s := m.env.styles
	var head strings.Builder
	head.WriteString(s.title.Render("pilot timesheet "+timesheet.AppVersion) + "   day " + sanitizeContent(m.snap.day))
	if m.snap.project != "" {
		head.WriteString("   project " + sanitizeContent(m.snap.project))
	}
	head.WriteString("\n")

	a := m.snap.action
	line := "state " + sanitizeContent(a.State)
	if a.Step != "" {
		line += "   step " + sanitizeContent(a.Step)
	}
	head.WriteString(line + "\n")

	var headPart, listRows, tailPart strings.Builder
	switch {
	case a.State == stateAction && a.Action != nil:
		listRows.WriteString("\n")
		renderAction(&listRows, a, s)
	case a.State == stateJudgment && a.Judgment != nil:
		headPart.WriteString("\n")
		renderJudgment(&headPart, &listRows, a, s)
	case a.State == stateAskHuman && a.Ask != nil:
		headPart.WriteString("\n")
		renderAsk(&headPart, a.Ask, s)
	case a.State == stateStopped:
		// The blocker and remedy render verbatim; CLI remedies arrive as
		// commands inside the reason text.
		listRows.WriteString("\nreason\n  " + sanitizeContent(a.Reason) + "\n")
	case a.State == stateHumanGates && a.Human != nil:
		renderHumanGates(&headPart, &listRows, &tailPart, a, m.snap.status, s)
	}
	dayHead, dayTail := head.String(), renderFooter(m)
	window := listRows.String()
	if n := m.view.bodyHeight(rows(dayHead) + rows(dayTail) + rows(headPart.String()) + rows(tailPart.String())); n > 0 && window != "" {
		window = fitTail(window, n, s)
	}
	return dayHead + headPart.String() + window + tailPart.String() + dayTail
}

// renderAction renders the derived argv and the observation that selected it.
func renderAction(b *strings.Builder, a nextAction, s uiStyles) {
	b.WriteString(s.label.Render("argv") + "\n  " + sanitizeContent(shellJoin(a.Action.Cmd)) +
		"\n" + s.label.Render("why") + "\n  " + sanitizeContent(a.Action.Why) + "\n")
}

// renderAsk writes the typed human question the CLI reports, verbatim.
func renderAsk(head *strings.Builder, q *askHuman, s uiStyles) {
	head.WriteString(s.label.Render("ask") + "          " + sanitizeContent(q.Kind) + "\n")
	head.WriteString(s.label.Render("question") + "\n  " + sanitizeContent(q.Question) + "\n")
	if len(q.Options) > 0 {
		head.WriteString(s.label.Render("options") + "      " + sanitizeContent(strings.Join(q.Options, " | ")) + "\n")
	}
	head.WriteString(s.label.Render("answer file") + "  " + sanitizeContent(q.AnswerFile) + "\n")
	head.WriteString(s.label.Render("digest") + "       " + sanitizeContent(q.Digest) + "\n")
	if q.LastError != "" {
		head.WriteString(s.label.Render("last error") + "\n  " + sanitizeContent(q.LastError) + "\n")
	}
	head.WriteString(s.label.Render("protocol") + "\n  " + sanitizeContent(q.Protocol) + "\n")
}

// renderJudgment writes the judgment's instruction surface (kind, answer file,
// digests, protocol) to head and its block rows to list.
func renderJudgment(head, list *strings.Builder, a nextAction, s uiStyles) {
	j := a.Judgment
	head.WriteString(s.label.Render("kind") + "         " + sanitizeContent(j.Kind) + "\n")
	head.WriteString(s.label.Render("answer file") + "  " + sanitizeContent(j.AnswerFile) + "\n")
	if j.DraftDigest != "" {
		head.WriteString(s.label.Render("draft digest") + " " + sanitizeContent(j.DraftDigest) + "\n")
	}
	if j.LedgerDigest != "" {
		head.WriteString(s.label.Render("ledger digest") + " " + sanitizeContent(j.LedgerDigest) + "\n")
	}
	head.WriteString(s.label.Render("protocol") + "\n  " + sanitizeContent(j.Protocol) + "\n")
	if len(j.Blocks) > 0 {
		head.WriteString(s.label.Render("blocks") + "\n")
		for _, bl := range j.Blocks {
			list.WriteString("  " + sanitizeContent(bl.IntervalID) + "  " + sanitizeContent(bl.BlockID) + "  " + sanitizeContent(bl.Start) + "-" + sanitizeContent(bl.End) + "  " + sanitizeContent(bl.Description) + "\n")
		}
	}
}

// renderHumanGates writes the terminal gate state as a pinned head (the
// human-only commands and the store-wide unresolved-write counts), a windowable
// list (unresolved rows, blockers, approvals), and a pinned tail (the day's
// next action).
func renderHumanGates(head, list, tail *strings.Builder, a nextAction, status *timesheet.SubmissionStatusResult, s uiStyles) {
	h := a.Human
	head.WriteString("\n")
	head.WriteString(s.label.Render("approve") + "\n  " + sanitizeContent(h.ApproveCommand) + "\n")
	head.WriteString(s.label.Render("submit") + "\n  " + sanitizeContent(h.SubmitCommand) + "\n")
	head.WriteString(s.label.Render("status") + "\n  " + sanitizeContent(h.StatusCommand) + "\n")
	head.WriteString(s.label.Render("unresolved writes") + " " + strconv.Itoa(h.Unresolved) + " legacy, " + strconv.Itoa(h.NativeUnresolved) + " native\n")
	if status == nil {
		return
	}
	for _, r := range status.Unresolved {
		row := "  unresolved " + sanitizeContent(r.WriteRef) + " (" + sanitizeContent(r.Origin) + " " + sanitizeContent(r.Status)
		if r.ErrorCode != "" {
			row += " " + sanitizeContent(r.ErrorCode)
		}
		row += ") " + sanitizeContent(r.Detail) + "\n"
		list.WriteString(row)
	}
	for _, bl := range status.Blockers {
		list.WriteString("  blocker " + sanitizeContent(bl.Code) + ": " + sanitizeContent(bl.Detail) + "\n")
	}
	for _, ap := range status.Approvals {
		list.WriteString("  approval " + sanitizeContent(ap.ApprovalID) + " plan " + sanitizeContent(ap.PlanID) + " " + sanitizeContent(ap.State) + "\n")
	}
	tail.WriteString("next: " + sanitizeContent(status.NextAction) + "\n")
}

// renderFooter renders the one dynamic line: the busy lock, the last error, or
// the key legend.
func renderFooter(m tuiModel) string {
	s := m.env.styles
	var line string
	switch {
	case m.err != nil:
		line = s.errSty.Render("error: ") + sanitizeContent(m.err.Error())
	case m.busy:
		line = s.hint.Render("working: keys are locked until the current step finishes")
	case m.notice != "":
		line = sanitizeContent(m.notice)
	case stepAct(m.snap.action) == stateHumanGates:
		line = s.hint.Render("[enter] approve gate   [s] submit gate   [r] re-derive   [q] quit")
	default:
		line = s.hint.Render("[r] re-derive   [enter] run the derived step   [q] quit, discarding unsaved session state")
	}
	return "\n" + line + "\n"
}

// renderBench renders the bench form: the pre-filled answer document the
// operator edits, with the judgment's digest shown above it.
func renderBench(m tuiModel) string {
	s := m.env.styles
	b := m.bench
	head := s.label.Render("author") + " " + sanitizeContent(b.kind) + "\n" +
		s.label.Render("answer file") + "  " + sanitizeContent(b.answerFile) + "\n" +
		s.label.Render("digest") + "       " + sanitizeContent(b.digest) + " (copied from the judgment; keep this field)\n\n"
	tail := "\n" + s.hint.Render("[ctrl+s] write   [esc] abandon form, unsaved text dies") + "\n"
	if m.err != nil {
		tail = "\n" + s.errSty.Render("error: ") + sanitizeContent(m.err.Error()) + "\n" + tail
	}
	doc := renderCaret(b.text, b.caret, s)
	if n := m.view.bodyHeight(rows(head) + rows(tail) + 1); n > 0 {
		doc = fitAround(doc, n, caretLineOf(b.text, b.caret), s)
	}
	return head + doc + "\n" + tail
}

// renderGate renders a display-before-ask gate dialog: the fresh artifact and
// the exact id to type. The head and the typed echo are pinned; only the plan
// body or approvals list clips.
func renderGate(m tuiModel) string {
	s := m.env.styles
	b := m.bench
	head := s.gate.Render("human gate:") + " " + sanitizeContent(b.kind) + "\n"
	var middle string
	switch {
	case b.kind == gateApprove && b.preview != nil:
		head += s.gate.Render("plan_id:") + " " + sanitizeContent(b.preview.PlanID) +
			"   " + s.gate.Render("state:") + " " + sanitizeContent(b.preview.State) + "\n"
		middle = sanitizeContent(renderPlanBody(*b.preview))
	case b.kind == gateSubmit:
		head += s.label.Render("active approvals") + "\n"
		for _, a := range b.approvals {
			middle += "  " + sanitizeContent(a.ApprovalID) + "  plan " + sanitizeContent(a.PlanID) + "  " + sanitizeContent(a.State) + "\n"
		}
		if len(b.approvals) == 0 {
			middle = "  (none)\n"
		}
	}
	tail := "\n" + sanitizeContent(b.answerFile) + "\n" + renderCaret(b.text, b.caret, s) + "\n"
	if m.err != nil {
		tail += "\n" + s.errSty.Render("error: ") + sanitizeContent(m.err.Error()) + "\n"
	}
	tail += "\n" + s.hint.Render("[enter] confirm (re-checks freshness first)   [esc] dismiss") + "\n"
	if n := m.view.bodyHeight(rows(head) + rows(tail)); n > 0 {
		middle = fitTail(middle, n, s)
	}
	return head + middle + tail
}

func renderCaret(text string, caret int, s uiStyles) string {
	runes := []rune(text)
	if caret < 0 {
		caret = 0
	}
	if caret > len(runes) {
		caret = len(runes)
	}
	return sanitizeContent(string(runes[:caret])) + s.caret.Render("▌") + sanitizeContent(string(runes[caret:]))
}

// splitRows splits a composed block into its rendered rows. A trailing '\n'
// terminates the last row rather than opening an empty one; ended reports
// whether it was there so the window helpers can put it back.
func splitRows(frame string) (lines []string, ended bool) {
	lines = strings.Split(frame, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
		ended = true
	}
	return lines, ended
}

// fitTail returns the last n rows of frame, prepending a faint "+N lines"
// indicator naming the hidden count when it clips; the indicator is inside the
// budget. n <= 1 returns frame unchanged: no room for an indicator plus
// content, which is today's overflow behavior. Splitting on '\n' cannot cut a
// style sequence: Render arguments are single-line (the adjacency rule).
func fitTail(frame string, n int, s uiStyles) string {
	lines, ended := splitRows(frame)
	if n <= 1 || len(lines) <= n {
		return frame
	}
	hidden := len(lines) - (n - 1)
	out := s.hint.Render("+"+strconv.Itoa(hidden)+" lines") + "\n" + strings.Join(lines[len(lines)-(n-1):], "\n")
	if ended {
		out += "\n"
	}
	return out
}

// fitAround returns at most n rows of frame with anchorLine inside: content
// around the anchor, a faint "+N lines" indicator above when rows are hidden
// there and one below when rows are hidden there, both inside the budget.
// n <= 2 returns frame unchanged: no room for content plus indicators.
func fitAround(frame string, n, anchorLine int, s uiStyles) string {
	lines, ended := splitRows(frame)
	if n <= 2 || len(lines) <= n {
		return frame
	}
	if anchorLine < 0 {
		anchorLine = 0
	}
	if anchorLine > len(lines)-1 {
		anchorLine = len(lines) - 1
	}
	content := n - 2
	start := anchorLine - content/2
	if start < 0 {
		start = 0
	}
	end := start + content
	if end >= len(lines) {
		end = len(lines)
		start = end - (n - 1)
		if start < 0 {
			start = 0
		}
	}
	var b strings.Builder
	if start > 0 {
		b.WriteString(s.hint.Render("+"+strconv.Itoa(start)+" lines") + "\n")
	}
	b.WriteString(strings.Join(lines[start:end], "\n"))
	if end < len(lines) {
		b.WriteString("\n" + s.hint.Render("+"+strconv.Itoa(len(lines)-end)+" lines"))
	}
	if ended {
		b.WriteString("\n")
	}
	return b.String()
}

// caretLineOf reports which line of the raw text the caret (a rune index) sits on.
func caretLineOf(text string, caret int) int {
	runes := []rune(text)
	if caret < 0 {
		caret = 0
	}
	if caret > len(runes) {
		caret = len(runes)
	}
	return strings.Count(string(runes[:caret]), "\n")
}

// bodyHeight is the clip budget after reserving chrome rows; <= 0 means
// unbounded (the degenerate window falls back to today's overflow behavior).
func (v viewState) bodyHeight(reserved int) int { return v.height - reserved }
