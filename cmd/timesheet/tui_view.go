package main

// The TUI's rendering: pure functions, one per screen, model to string.

import (
	"fmt"
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
//
// Renderers do not call it by hand: recorded content enters a frame only as a
// value of recorded (or labelRow, built on it), so the writer, not each call
// site, owns the sanitization. The adjacency rule stays the fallback for what
// the writer cannot express: recorded content never enters a Style.Render
// call; a styled UI fragment sits next to a recorded stretch, never around it.
// renderCaret is the one such site: it splits one recorded string around a
// styled glyph and sanitizes each half directly.
func sanitizeContent(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' {
			return -1
		}
		return r
	}, s)
}

// recorded composes one stretch of frame text: format is the UI's own literal
// text with one %s per value, and every value is recorded content, sanitized
// here. A format never carries recorded content. A literal '%' in the format
// must be written "%%", and the number of %s verbs must equal len(values):
// go vet cannot check either, since values is ...string and labelRow and
// approvalRow build their formats at runtime.
func recorded(format string, values ...string) string {
	args := make([]any, len(values))
	for i, v := range values {
		args[i] = sanitizeContent(v)
	}
	return fmt.Sprintf(format, args...)
}

// labelRow writes one label/value row: the faint label, the UI's separator
// (the hand-aligned padding to the value column, or "\n  " for an indented
// value), the recorded value and the row's end. The padding stays at each call
// site: the label columns are aligned by hand (13 and 14 columns), not by a
// padding policy.
func labelRow(b *strings.Builder, s uiStyles, label, sep, value string) {
	b.WriteString(s.label.Render(label) + recorded(sep+"%s\n", value))
}

// approvalRow formats one active-approval row. The day screen and the submit
// gate share the row and differ only in the lead word and column separator.
func approvalRow(lead, sep string, ap timesheet.ApprovalRow) string {
	return recorded("  "+lead+"%s"+sep+"plan %s"+sep+"%s\n", ap.ApprovalID, ap.PlanID, ap.State)
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
	head.WriteString(s.title.Render("pilot timesheet "+timesheet.AppVersion) + recorded("   day %s", m.snap.day))
	head.WriteString("\n")

	a := m.snap.action
	line := recorded("state %s", a.State)
	if a.Step != "" {
		line += recorded("   step %s", a.Step)
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
		listRows.WriteString(recorded("\nreason\n  %s\n", a.Reason))
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
	labelRow(b, s, "argv", "\n  ", shellJoin(a.Action.Cmd))
	labelRow(b, s, "why", "\n  ", a.Action.Why)
}

// renderAsk writes the typed human question the CLI reports, verbatim.
func renderAsk(head *strings.Builder, q *askHuman, s uiStyles) {
	labelRow(head, s, "ask", "          ", q.Kind)
	labelRow(head, s, "question", "\n  ", q.Question)
	if len(q.Options) > 0 {
		labelRow(head, s, "options", "      ", strings.Join(q.Options, " | "))
	}
	labelRow(head, s, "answer file", "  ", q.AnswerFile)
	labelRow(head, s, "digest", "       ", q.Digest)
	if q.LastError != "" {
		labelRow(head, s, "last error", "\n  ", q.LastError)
	}
	labelRow(head, s, "protocol", "\n  ", q.Protocol)
}

// renderJudgment writes the judgment's instruction surface (kind, answer file,
// digests, protocol) to head and its block rows to list.
func renderJudgment(head, list *strings.Builder, a nextAction, s uiStyles) {
	j := a.Judgment
	labelRow(head, s, "kind", "         ", j.Kind)
	labelRow(head, s, "answer file", "  ", j.AnswerFile)
	if j.DraftDigest != "" {
		labelRow(head, s, "draft digest", " ", j.DraftDigest)
	}
	if j.LedgerDigest != "" {
		labelRow(head, s, "ledger digest", " ", j.LedgerDigest)
	}
	labelRow(head, s, "protocol", "\n  ", j.Protocol)
	if len(j.Blocks) > 0 {
		head.WriteString(s.label.Render("blocks") + "\n")
		for _, bl := range j.Blocks {
			list.WriteString(recorded("  %s  %s  %s-%s  %s\n", bl.IntervalID, bl.BlockID, bl.Start, bl.End, bl.Description))
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
	labelRow(head, s, "approve", "\n  ", h.ApproveCommand)
	labelRow(head, s, "submit", "\n  ", h.SubmitCommand)
	labelRow(head, s, "status", "\n  ", h.StatusCommand)
	head.WriteString(s.label.Render("unresolved writes") + " " + strconv.Itoa(h.Unresolved) + " legacy, " + strconv.Itoa(h.NativeUnresolved) + " native\n")
	// The overnight carry is shown at review (ADR 0011): what this day took in
	// from the previous date and what it carries into the next.
	for _, c := range h.Carried {
		list.WriteString(recorded("  carried %s -> %s  %s-%s (%s min)  %s\n",
			c.From, c.To, c.Start, c.End, strconv.Itoa(c.Minutes), c.Description))
	}
	if status == nil {
		return
	}
	for _, r := range status.Unresolved {
		row := recorded("  unresolved %s (%s %s", r.WriteRef, r.Origin, r.Status)
		if r.ErrorCode != "" {
			row += recorded(" %s", r.ErrorCode)
		}
		row += recorded(") %s\n", r.Detail)
		list.WriteString(row)
	}
	for _, bl := range status.Blockers {
		list.WriteString(recorded("  blocker %s: %s\n", bl.Code, bl.Detail))
	}
	for _, ap := range status.Approvals {
		list.WriteString(approvalRow("approval ", " ", ap))
	}
	tail.WriteString(recorded("next: %s\n", status.NextAction))
}

// renderFooter renders the one dynamic line: the busy lock, the last error, or
// the key legend.
func renderFooter(m tuiModel) string {
	s := m.env.styles
	var line string
	switch {
	case m.err != nil:
		line = s.errSty.Render("error: ") + recorded("%s", m.err.Error())
	case m.busy:
		line = s.hint.Render("working: keys are locked until the current step finishes")
	case m.notice != "":
		line = recorded("%s", m.notice)
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
	var hb strings.Builder
	labelRow(&hb, s, "author", " ", b.kind)
	labelRow(&hb, s, "answer file", "  ", b.answerFile)
	hb.WriteString(s.label.Render("digest") + recorded("       %s (copied from the judgment; keep this field)\n\n", b.digest))
	head := hb.String()
	tail := "\n" + s.hint.Render("[ctrl+s] write   [esc] abandon form, unsaved text dies") + "\n"
	if m.err != nil {
		tail = "\n" + s.errSty.Render("error: ") + recorded("%s\n", m.err.Error()) + tail
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
	head := s.gate.Render("human gate:") + recorded(" %s\n", b.kind)
	var middle string
	switch {
	case b.kind == gateApprove && b.preview != nil:
		head += s.gate.Render("plan_id:") + recorded(" %s   ", b.preview.PlanID) +
			s.gate.Render("state:") + recorded(" %s\n", b.preview.State)
		middle = recorded("%s", renderPlanBody(*b.preview))
	case b.kind == gateSubmit:
		head += s.label.Render("active approvals") + "\n"
		for _, a := range b.approvals {
			middle += approvalRow("", "  ", a)
		}
		if len(b.approvals) == 0 {
			middle = "  (none)\n"
		}
	}
	tail := recorded("\n%s\n", b.answerFile) + renderCaret(b.text, b.caret, s) + "\n"
	if m.err != nil {
		tail += "\n" + s.errSty.Render("error: ") + recorded("%s\n", m.err.Error())
	}
	tail += "\n" + s.hint.Render("[enter] confirm (re-checks freshness first)   [esc] dismiss") + "\n"
	if n := m.view.bodyHeight(rows(head) + rows(tail)); n > 0 {
		middle = fitTail(middle, n, s)
	}
	return head + middle + tail
}

// clampCaret returns the text's runes and the caret (a rune index) clamped
// into [0, len(runes)].
func clampCaret(text string, caret int) ([]rune, int) {
	runes := []rune(text)
	return runes, max(0, min(caret, len(runes)))
}

// renderCaret renders the recorded text with the styled caret glyph at the
// caret: the one site that splits recorded content around a style, so it
// sanitizes each half directly (see sanitizeContent).
func renderCaret(text string, caret int, s uiStyles) string {
	runes, caret := clampCaret(text, caret)
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
	runes, caret := clampCaret(text, caret)
	return strings.Count(string(runes[:caret]), "\n")
}

// bodyHeight is the clip budget after reserving chrome rows; <= 0 means
// unbounded (the degenerate window falls back to today's overflow behavior).
func (v viewState) bodyHeight(reserved int) int { return v.height - reserved }
