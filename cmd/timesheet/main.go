// Command timesheet logs and submits a personal timesheet; every command is
// one stateless process over the local store.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

const commandSynopsis = `Usage:
  timesheet init [--seed-config <path>] [--tz <IANA>]
  timesheet log [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>] [--max-actions <n>]
  timesheet next [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>]
  timesheet status [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>]
  timesheet submit [--config <path>] [--env-file <path>] --date <YYYY-MM-DD> [--approval-id <id>]
  timesheet submit --chat [--config <path>] [--env-file <path>] --date <YYYY-MM-DD> [--plan-id <id> --review-id <id> --confirm-chat]
  timesheet submit status [--config <path>] [--env-file <path>] --date <YYYY-MM-DD>
  timesheet submit resolve [--config <path>] [--env-file <path>] --file <decision.json>
  timesheet submit revoke [--config <path>] [--env-file <path>] --date <YYYY-MM-DD> [--approval-id <id>]
  timesheet migrate preview [--config <path>] [--env-file <path>] --date <YYYY-MM-DD|FROM..TO>...
  timesheet migrate apply [--config <path>] [--env-file <path>] --date <YYYY-MM-DD|FROM..TO>... --migration-id <id>
  timesheet tui [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>]
  timesheet daily [--config <path>] [--env-file <path>] [--print-units systemd|launchd]
  timesheet judge [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>]
  timesheet eval export [--config <path>] [--out <file outside any repository>]
  timesheet eval run --golden <file> [--out <file outside any repository>] [--config <path>] [--env-file <path>]
  timesheet eval score --golden <file> [--candidate <file>]
  timesheet version

next prints the one action it derives; the steps it names (run, record, stage,
classifications, classify, save, plan, acknowledge, decide, reconcile, resolve)
stay callable but are internal.
`

func printSynopsis(w io.Writer) {
	fmt.Fprint(w, commandSynopsis)
}

func printNoCommand(w io.Writer) {
	fmt.Fprintln(w, "timesheet: no command given; start with `timesheet log`")
	fmt.Fprintln(w, "  also: timesheet tui, timesheet version, timesheet help")
}

func main() {
	if len(os.Args) < 2 {
		printNoCommand(os.Stderr)
		os.Exit(2)
	}
	if _, retired := retiredCommands[os.Args[1]]; retired {
		fmt.Fprintf(os.Stderr, "timesheet: %s\n", retiredCommandError(os.Args[1]))
		os.Exit(2)
	}
	if err := retiredFlagError(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
		os.Exit(2)
	}
	stderrLine := func(line string) { fmt.Fprintf(os.Stderr, "timesheet: %s\n", line) }
	timesheet.UpgradeNotice = stderrLine
	// init only reads a --seed-config and its own probe; it never edits a
	// configuration it did not create (ADR 0012).
	if os.Args[1] != "init" {
		timesheet.RetiredConfigHook = retiredConfigHook(stderrLine)
	}
	switch os.Args[1] {
	case "help", "-h", "--help":
		printSynopsis(os.Stdout)
	case "version":
		if len(os.Args) != 2 {
			fmt.Fprintln(os.Stderr, "version takes no arguments")
			os.Exit(2)
		}
		fmt.Printf("timesheet %s\n", timesheet.AppVersion)
	case "init":
		if err := runSetup(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			var usageErr *setupUsageError
			if errors.As(err, &usageErr) {
				os.Exit(2)
			}
			os.Exit(1)
		}
	case "migrate":
		if err := runMigrate(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "run":
		if err := runRun(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "log":
		if err := runLog(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "next":
		if err := runNext(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "tui":
		if err := runTui(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "record":
		if err := runRecord(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "status":
		if err := runStatus(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "stage":
		if err := runStage(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "daily":
		if err := runDaily(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "judge":
		if err := runJudge(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "eval":
		if err := runEval(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "classifications":
		if err := runClassifications(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "classify":
		if err := runClassify(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "acknowledge":
		if err := runAcknowledge(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "decide":
		if err := runDecide(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "save":
		if err := runSave(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "plan":
		if err := runPlan(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "reconcile":
		if err := runReconcile(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "resolve":
		if err := runResolve(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "submit":
		if err := runSubmitCommand(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "timesheet: unknown command %q; run `timesheet help`\n", os.Args[1])
		os.Exit(2)
	}
}
