// Command timesheet runs the local timesheet server.
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
  timesheet version
  timesheet setup [--project <id>=<name>[=<classification>]]... [--seed-config <path>] [--tz <IANA>]
  timesheet serve --config <path> --env-file <path>
  timesheet collect [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>]
  timesheet draft [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>]
  timesheet run [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>]
  timesheet next [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>] [--project <id>] [--do] [--max-actions <n>]
  timesheet tui [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>] [--project <id>]
  timesheet record [--config <path>] [--date <YYYY-MM-DD>] (--draft <draft.json> --descriptions <desc.json> | --file <reviewed.json>) [--replace]
  timesheet status [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>]
  timesheet stage [--config <path>] [--env-file <path>] --date <YYYY-MM-DD> [--project <id>] [--revise]
  timesheet classifications [--config <path>] [--env-file <path>] --query <text>
  timesheet classify [--config <path>] [--env-file <path>] --date <YYYY-MM-DD> (--file <choices.json> | --configured-default) [--project <id>]
  timesheet acknowledge [--config <path>] [--env-file <path>] --date <YYYY-MM-DD> --source <github|calendar> --collection-id <id>
  timesheet decide [--config <path>] [--env-file <path>] --date <YYYY-MM-DD> --meeting-id <id> --decision <attended|skipped> --collection-id <id>
  timesheet confirm [--config <path>] [--env-file <path>] --date <YYYY-MM-DD> [--project <id>]
  timesheet save [--config <path>] [--env-file <path>] --date <YYYY-MM-DD> [--project <id>]
  timesheet plan [--config <path>] [--env-file <path>] --date <YYYY-MM-DD>
  timesheet reconcile [--config <path>] [--env-file <path>] --date <YYYY-MM-DD>
  timesheet resolve [--config <path>] [--env-file <path>] --date <YYYY-MM-DD> --file <decision.json>
  timesheet approve [--config <path>] [--env-file <path>] --date <YYYY-MM-DD>
  timesheet submit [--config <path>] [--env-file <path>] --date <YYYY-MM-DD> --approval-id <id>
  timesheet submit-status [--config <path>] [--env-file <path>] --date <YYYY-MM-DD>
  timesheet agent-submit [--config <path>] [--env-file <path>] --date <YYYY-MM-DD> [--plan-id <id> --review-id <id> --confirm-chat]
  timesheet resolve-write [--config <path>] [--env-file <path>] --file <decision.json>
`

// printSynopsis is the clean help surface: the command list, no prose.
func printSynopsis(w io.Writer) {
	fmt.Fprint(w, commandSynopsis)
}

// printNoCommand keeps a bare invocation two lines long, so a caller that runs
// the binary with no arguments cannot pull the command list into context.
func printNoCommand(w io.Writer) {
	fmt.Fprintln(w, "timesheet: no command given; start with `timesheet next --do`")
	fmt.Fprintln(w, "  also: timesheet tui, timesheet version, timesheet help")
}

func main() {
	if len(os.Args) < 2 {
		printNoCommand(os.Stderr)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "help", "-h", "--help":
		printSynopsis(os.Stdout)
	case "version":
		if len(os.Args) != 2 {
			fmt.Fprintln(os.Stderr, "version takes no arguments")
			os.Exit(2)
		}
		fmt.Printf("timesheet %s (protocol schema %d)\n", timesheet.AppVersion, timesheet.ProtocolSchema)
	case "setup":
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
	case "serve":
		if err := runServe(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			// AppError carries a stable code and a safe message. Startup errors
			// never echo configuration or credential values.
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "collect":
		if err := runCollect(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "draft":
		if err := runDraft(os.Args[2:]); err != nil {
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
	case "confirm":
		if err := runConfirm(os.Args[2:]); err != nil {
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
	case "agent-submit":
		if err := runAgentSubmit(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "approve":
		if err := runApprove(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "submit":
		if err := runSubmit(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "submit-status":
		if err := runSubmitStatus(os.Args[2:]); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "timesheet: %s\n", err)
			os.Exit(1)
		}
	case "resolve-write":
		if err := runResolveWrite(os.Args[2:]); err != nil {
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

func runServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configPath := flags.String("config", "", "fixed configuration path (required)")
	envFile := flags.String("env-file", "", "fixed environment-file path (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("serve does not accept positional arguments")
	}
	if *configPath == "" || *envFile == "" {
		return fmt.Errorf("serve requires --config and --env-file")
	}

	cfg, err := timesheet.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	fileEnv, err := timesheet.LoadEnvFile(*envFile)
	if err != nil {
		return err
	}
	env := timesheet.MergeEnv(fileEnv)
	store, err := timesheet.OpenStore(cfg.StatePath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	session := timesheet.NewSession(cfg, env, store)
	return session.Serve(os.Stdin, os.Stdout)
}
