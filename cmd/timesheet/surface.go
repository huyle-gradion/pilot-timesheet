package main

import (
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

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

var retiredFlags = map[string]string{
	"project": "intervals no longer carry a project (ADR 0012); drop the flag, classification comes from gradion.default_classification or the classification judgment",
}

var retiredFlagCommands = map[string]bool{
	"init": true, "log": true, "next": true, "tui": true, "judge": true,
	"stage": true, "classify": true, "save": true, "confirm": true,
}

var boolFlags = map[string]bool{
	"do": true, "auto": true, "revise": true, "configured-default": true,
	"from-history": true, "h": true, "help": true,
}

func retiredFlagError(command string, args []string) error {
	if !retiredFlagCommands[command] {
		return nil
	}
	value := false
	for _, a := range args {
		if value {
			value = false
			continue
		}
		if a == "--" {
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			continue
		}
		name, _, inline := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if why, ok := retiredFlags[name]; ok {
			return fmt.Errorf("`%s --%s` was removed: %s", command, name, why)
		}
		value = !inline && !boolFlags[name]
	}
	return nil
}

func retiredConfigHook(notice func(string)) func(string) {
	seen := map[string]bool{}
	return func(path string) {
		if seen[path] {
			return
		}
		seen[path] = true
		removed, backup, err := timesheet.RemoveRetiredConfigKeys(path)
		if err != nil {
			notice(fmt.Sprintf("config %s: the retired keys projects / migration.project_map are ignored and can be removed by hand (ADR 0012; not rewritten: %v)", path, err))
			return
		}
		if len(removed) == 0 {
			notice(fmt.Sprintf("config %s: the retired keys projects / migration.project_map are ignored and can be removed by hand (ADR 0012)", path))
			return
		}
		notice(fmt.Sprintf("config %s: removed the retired %s (ADR 0012); the previous file is kept at %s", path, strings.Join(removed, " and "), backup))
	}
}

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
