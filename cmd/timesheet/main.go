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
  timesheet init [--project <id>=<name>[=<classification>]]... [--seed-config <path>] [--tz <IANA>]
  timesheet log [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>] [--project <id>] [--max-actions <n>]
  timesheet next [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>] [--project <id>]
  timesheet status [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>]
  timesheet submit [--config <path>] [--env-file <path>] --date <YYYY-MM-DD> [--approval-id <id>]
  timesheet submit --chat [--config <path>] [--env-file <path>] --date <YYYY-MM-DD> [--plan-id <id> --review-id <id> --confirm-chat]
  timesheet submit status [--config <path>] [--env-file <path>] --date <YYYY-MM-DD>
  timesheet submit resolve [--config <path>] [--env-file <path>] --file <decision.json>
  timesheet tui [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>] [--project <id>]
  timesheet daily [--config <path>] [--env-file <path>] [--print-units systemd|launchd]
  timesheet judge [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>] [--project <id>]
  timesheet eval export [--config <path>] [--out <file outside any repository>]
  timesheet eval score --golden <file> [--candidate <file>]
  timesheet version

next prints the one action it derives; the steps it names (run, record, stage,
classifications, classify, save, plan, acknowledge, decide, reconcile, resolve)
and serve stay callable but are internal.
`

// printSynopsis is the clean help surface: the command list, no prose.
func printSynopsis(w io.Writer) {
	fmt.Fprint(w, commandSynopsis)
}

// printNoCommand keeps a bare invocation two lines long, so a caller that runs
// the binary with no arguments cannot pull the command list into context.
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
	switch os.Args[1] {
	case "help", "-h", "--help":
		printSynopsis(os.Stdout)
	case "version":
		if len(os.Args) != 2 {
			fmt.Fprintln(os.Stderr, "version takes no arguments")
			os.Exit(2)
		}
		fmt.Printf("timesheet %s (protocol schema %d)\n", timesheet.AppVersion, timesheet.ProtocolSchema)
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
