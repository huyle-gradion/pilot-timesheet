package main

import (
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// The public command surface (issue #245): init, log, status, submit (with
// its status, resolve and revoke subcommands), migrate, tui, daily, judge,
// eval and version.
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
	"serve":         "timesheet migrate preview|apply, timesheet submit revoke or timesheet status",
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
// are its read-only report and its human write resolution; revoke withdraws a
// kept approval in the terminal and is never reachable through --chat.
func runSubmitCommand(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "status":
			return runSubmitStatus(args[1:])
		case "resolve":
			return runResolveWrite(args[1:])
		case "revoke":
			return runSubmitRevoke(args[1:])
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
		return fmt.Errorf("%w; the approval %s is kept: resume with `timesheet submit --date %s --approval-id %s`, or revoke it with `timesheet submit revoke --date %s --approval-id %s`",
			err, approval.ApprovalID, f.date, approval.ApprovalID, f.date, approval.ApprovalID)
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

// runSubmitRevoke revokes one kept (active, undispatched) approval for a day
// behind a typed confirmation of its approval_id (issue #272). Without
// --approval-id it takes the day's single active approval, and refuses,
// listing them, when there are several. The TTY gate fires before the store
// opens; revoke_approval's own refusals (unknown, already consumed or revoked)
// pass through verbatim. Revocation never undoes a remote effect.
func runSubmitRevoke(args []string) error {
	flags := flag.NewFlagSet("submit revoke", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var configPath, envFile, date, approvalID string
	flags.StringVar(&configPath, "config", "", "configuration path (optional; defaults to $HOME/.timesheet/config.json)")
	flags.StringVar(&envFile, "env-file", "", "environment-file path (optional; defaults to $HOME/.timesheet/.env.local when it exists)")
	flags.StringVar(&date, "date", "", "day to operate on, YYYY-MM-DD (required)")
	flags.StringVar(&approvalID, "approval-id", "", "approval_id to revoke (optional when the day has exactly one active approval)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("submit revoke does not accept positional arguments")
	}
	if err := validateDate("submit revoke", date, dateSupplied(flags)); err != nil {
		return err
	}
	configPath, envFile, err := resolveStoreFlags("submit revoke", configPath, envFile)
	if err != nil {
		return err
	}
	term, err := interactiveFactory("submit revoke", "revoke approvals")
	if err != nil {
		return err
	}
	h, err := openHeadless(configPath, envFile)
	if err != nil {
		return err
	}
	defer h.done()
	res, err := h.call("submission_status", timesheet.Request{Day: date})
	if err != nil {
		return err
	}
	status, ok := res.(timesheet.SubmissionStatusResult)
	if !ok {
		return fmt.Errorf("submit revoke: unexpected submission_status payload")
	}
	planID := ""
	if approvalID == "" {
		var active []timesheet.ApprovalRow
		for _, a := range status.Approvals {
			if a.State == timesheet.ApprovalActive {
				active = append(active, a)
			}
		}
		switch len(active) {
		case 0:
			return fmt.Errorf("submit revoke: %s has no active approval; nothing was changed", date)
		case 1:
			approvalID, planID = active[0].ApprovalID, active[0].PlanID
		default:
			ids := make([]string, len(active))
			for i, a := range active {
				ids[i] = a.ApprovalID + " (plan " + a.PlanID + ")"
			}
			return fmt.Errorf("submit revoke: %s has %d active approvals: %s; pass --approval-id to choose one (nothing was changed)",
				date, len(active), strings.Join(ids, ", "))
		}
	} else {
		// Refuse an unknown or no-longer-active id before asking the operator
		// to type it, with the same codes and messages revoke_approval uses.
		state := ""
		for _, a := range status.Approvals {
			if a.ApprovalID == approvalID {
				planID, state = a.PlanID, a.State
			}
		}
		switch state {
		case "":
			return &timesheet.AppError{Code: timesheet.CodeUnknownApproval,
				Message: "approval_id does not name a native approval for this date"}
		case timesheet.ApprovalActive:
		default:
			return &timesheet.AppError{Code: timesheet.CodeStaleApproval,
				Message: "approval is already " + state + " and cannot be revoked"}
		}
	}
	prompt := fmt.Sprintf("About to revoke approval %s for %s\n  plan_id: %s\nRevocation is permanent audit; the plan needs a new approval before it can be submitted.",
		approvalID, date, planID)
	if err := term.confirm(prompt, approvalID); err != nil {
		return err
	}
	res, err = h.call("revoke_approval", timesheet.Request{Day: date, ApprovalID: approvalID})
	if err != nil {
		return err
	}
	receipt, ok := res.(timesheet.ApprovalRevocationReceipt)
	if !ok {
		return fmt.Errorf("submit revoke: unexpected revoke_approval payload")
	}
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(receipt)
}
