package main

import (
	"fmt"
	"slices"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// The public command surface (issue #245): init, log, status, submit (with
// its status and resolve subcommands), tui, daily, judge, eval and version.
// The deterministic verbs next derives (run, record, stage, classify, save,
// plan, ...) stay callable as its printed actions but are not documented as
// public. A retired command prints one line naming its replacement.

// retiredCommands maps a removed command to the one that replaces it.
var retiredCommands = map[string]string{
	"setup":         "timesheet init",
	"approve":       "timesheet submit --date <day> (it approves, then submits, in the terminal)",
	"agent-submit":  "timesheet submit --chat --date <day>",
	"submit-status": "timesheet submit status --date <day>",
	"resolve-write": "timesheet submit resolve --file <decision.json>",
	"collect":       "timesheet log",
	"draft":         "timesheet log",
	"confirm":       "timesheet log (save confirms and saves in one step)",
}

func retiredCommandError(name string) error {
	return fmt.Errorf("`%s` was removed; use `%s`", name, retiredCommands[name])
}

// runLog logs a day end to end: next --do, answering judgments with the
// configured judge when there is one. Without a judge it stops at the first
// judgment for the agent path, exactly as next --do does.
func runLog(args []string) error {
	f, err := parseNextFlags("log", args)
	if err != nil {
		return err
	}
	if f.do || f.auto {
		return fmt.Errorf("log always runs the day; drop --do and --auto (the judge section decides whether judgments are answered in-process)")
	}
	cfg, err := timesheet.LoadConfig(f.configPath)
	if err != nil {
		return err
	}
	f.do = true
	f.auto = cfg.Judge != nil
	if f.auto && !f.maxActionsSet {
		f.maxActions = defaultAutoMaxActions
	}
	return runNextFlags(f)
}

// runSubmitCommand is the one submission gate. Terminal mode (default)
// approves the plan and submits it, each step behind its own typed
// confirmation; --approval-id resumes an approval made earlier; --chat is the
// chat-confirmed mode the /submit skill drives (ADR 0009). status and resolve
// are its read-only report and its human write resolution.
func runSubmitCommand(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "status":
			return runSubmitStatus(args[1:])
		case "resolve":
			return runResolveWrite(args[1:])
		}
	}
	if i := slices.Index(args, "--chat"); i >= 0 {
		return runAgentSubmit(slices.Delete(slices.Clone(args), i, i+1))
	}
	for _, a := range args {
		if a == "--approval-id" || len(a) > len("--approval-id=") && a[:len("--approval-id=")] == "--approval-id=" {
			return runSubmit(args)
		}
	}
	return runSubmitTerminal(args)
}

// runSubmitTerminal approves the day's current plan (type its plan_id) and
// submits that approval (type its approval_id) in one terminal session.
func runSubmitTerminal(args []string) error {
	f, err := parseHeadlessFlags("submit", args)
	if err != nil {
		return err
	}
	if f.date == "" {
		return fmt.Errorf("submit requires --date")
	}
	term, err := interactiveFactory("submit", "submit")
	if err != nil {
		return err
	}
	h, err := openHeadless(f.configPath, f.envFile)
	if err != nil {
		return err
	}
	defer h.done()
	approval, err := approveDay(h, term, f.date)
	if err != nil {
		return err
	}
	if err := confirmSubmission(term, f.date, approval.ApprovalID); err != nil {
		return fmt.Errorf("%w; the approval %s is kept: resume with `timesheet submit --date %s --approval-id %s`",
			err, approval.ApprovalID, f.date, approval.ApprovalID)
	}
	receipt, err := submitApproved(h, term, f.date, approval.ApprovalID)
	if err != nil {
		return err
	}
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(struct {
		Approval   timesheet.ApprovalReceipt   `json:"approval"`
		Submission timesheet.SubmissionReceipt `json:"submission"`
	}{approval, receipt})
}
