package main

// The setup subcommand creates the fixed store-path files under
// $HOME/.timesheet exactly once: config.json and .env.local. It validates the
// generated configuration before its first target effect, creates the directory
// 0700 when absent, and hands each file to the no-replace publisher. A present
// final path — valid, invalid, unsafe or symlinked — is reported and left
// byte-identical; setup never deletes, replaces, repairs or chmods it, and a
// present env file is never read or parsed.
//
// Project ids and names are never invented. They come verbatim from repeatable
// --project values or from a loadable --seed-config; without either source and
// with the config missing, setup creates nothing and returns an actionable
// usage error. The timezone comes from an explicit --tz or the host's IANA
// zone; setup never guesses a zone and never falls back to UTC silently.

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// setupUsageError marks caller-input problems that main reports with exit 2.
// The type survives wrapping so the exit code never depends on the message.
type setupUsageError struct{ message string }

func (e *setupUsageError) Error() string { return e.message }

func setupUsagef(format string, args ...any) error {
	return &setupUsageError{message: fmt.Sprintf(format, args...)}
}

// projectSeed is one --project value. Fields are copied verbatim; nothing is
// synthesized from the cwd, the git remote or a default classification.
type projectSeed struct {
	id             string
	name           string
	classification string
}

type setupOptions struct {
	projects       []projectSeed
	seedConfigPath string
	timezone       string
}

// setupProjectFlag collects repeatable --project values without parsing them
// in Set, so a malformed value stays a setupUsageError instead of the flag
// package's plain wrapped error.
type setupProjectFlag []string

func (f *setupProjectFlag) String() string { return strings.Join(*f, ",") }

func (f *setupProjectFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

func parseProjectSeed(text string) (projectSeed, error) {
	parts := strings.Split(text, "=")
	if len(parts) < 2 || len(parts) > 3 {
		return projectSeed{}, setupUsagef("init: --project expects '<id>=<name>[=<classification>]' (quote values that contain spaces)")
	}
	seed := projectSeed{id: strings.TrimSpace(parts[0]), name: parts[1]}
	if len(parts) == 3 {
		seed.classification = strings.TrimSpace(parts[2])
	}
	if seed.id == "" || strings.TrimSpace(seed.name) == "" {
		return projectSeed{}, setupUsagef("init: --project needs a non-empty id and name in '<id>=<name>[=<classification>]'")
	}
	return seed, nil
}

func parseSetupFlags(args []string) (setupOptions, error) {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var rawProjects setupProjectFlag
	flags.Var(&rawProjects, "project", "project as '<id>=<name>[=<classification>]' (repeatable; quote values that contain spaces)")
	seedConfig := flags.String("seed-config", "", "loadable configuration to seed projects and required_env from")
	tz := flags.String("tz", "", "IANA timezone for the generated configuration (default: the host zone)")
	if err := flags.Parse(args); err != nil {
		return setupOptions{}, err
	}
	if flags.NArg() != 0 {
		return setupOptions{}, setupUsagef("init does not accept positional arguments")
	}
	opts := setupOptions{seedConfigPath: strings.TrimSpace(*seedConfig), timezone: strings.TrimSpace(*tz)}
	for _, raw := range rawProjects {
		seed, err := parseProjectSeed(raw)
		if err != nil {
			return setupOptions{}, err
		}
		opts.projects = append(opts.projects, seed)
	}
	if len(opts.projects) > 0 && opts.seedConfigPath != "" {
		return setupOptions{}, setupUsagef("init: pass either --project or --seed-config, not both")
	}
	return opts, nil
}

// runSetup is the CLI entry point; tests redirect os.Stdout around it.
func runSetup(args []string) error {
	opts, err := parseSetupFlags(args)
	if err != nil {
		return err
	}
	return executeSetup(opts, os.Stdout)
}

func executeSetup(opts setupOptions, out io.Writer) error {
	dir, err := homeTimesheetPath(".")
	if err != nil {
		return fmt.Errorf("init: %v", err)
	}

	dirInfo, dirErr := os.Lstat(dir)
	dirExists := true
	switch {
	case errors.Is(dirErr, fs.ErrNotExist):
		dirExists = false
	case dirErr != nil:
		return fmt.Errorf("init: cannot inspect %s: %v", dir, dirErr)
	case dirInfo.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("init: %s is a symlink; refusing to follow it (nothing was created or changed)", dir)
	case !dirInfo.IsDir():
		return fmt.Errorf("init: %s is not a directory; refusing to modify it (nothing was created or changed)", dir)
	}

	configPath := filepath.Join(dir, defaultConfigName)
	configExists, err := setupTargetExists(configPath)
	if err != nil {
		return err
	}
	envPath := filepath.Join(dir, defaultEnvName)
	envExists, err := setupTargetExists(envPath)
	if err != nil {
		return err
	}
	needConfig := !configExists
	needEnv := !envExists

	// A repeat run with both final files present is a no-op even without seed
	// parameters: nothing is read, validated or rewritten.
	if !needConfig && !needEnv {
		configLine, err := presentSetupFile(configPath, true)
		if err != nil {
			return err
		}
		envLine, err := presentSetupFile(envPath, false)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, configLine)
		fmt.Fprintln(out, envLine)
		return nil
	}

	if needConfig && len(opts.projects) == 0 && opts.seedConfigPath == "" {
		return setupUsagef("init: no files created. The new configuration needs at least one project; pass --project '<id>=<name>[=<classification>]' (quoted if it contains spaces, repeatable) or --seed-config <path>")
	}

	// --seed-config is loaded first, before any generated bytes or target
	// effect, so a bad seed reports by field with nothing created.
	var seed *timesheet.Config
	if opts.seedConfigPath != "" {
		absSeed, absErr := filepath.Abs(opts.seedConfigPath)
		if absErr != nil {
			return fmt.Errorf("init: --seed-config %s: %v", opts.seedConfigPath, absErr)
		}
		seed, err = timesheet.LoadConfig(absSeed)
		if err != nil {
			return fmt.Errorf("init: --seed-config %s: %v", opts.seedConfigPath, err)
		}
		opts.seedConfigPath = absSeed
	}

	var configBytes []byte
	var timezone, timezoneSource string
	if needConfig {
		configBytes, timezone, timezoneSource, err = setupConfigBytes(opts, seed, dir)
		if err != nil {
			return err
		}
	}
	var envBytes []byte
	if needEnv {
		names := defaultSetupEnvNames()
		if seed != nil {
			names = seed.RequiredEnv
		}
		envBytes = setupEnvTemplate(names)
	}

	dirCreated := false
	if !dirExists {
		if err := os.Mkdir(dir, 0o700); err == nil {
			dirCreated = true
		} else if !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("init: cannot create %s: %v", dir, err)
		}
	}

	var lines []string
	if dirCreated {
		lines = append(lines, fmt.Sprintf("created %s (dir, mode 0700)", dir))
	}

	createdFiles := 0
	if needConfig {
		created, err := publishSetupFile(dir, defaultConfigName, configBytes)
		if err != nil {
			return err
		}
		if created {
			createdFiles++
			lines = append(lines, fmt.Sprintf("created %s (mode 0600; timezone %s from %s; state_path %s; %s)",
				configPath, timezone, timezoneSource, filepath.Join(dir, setupStateName), describeSetupProjects(seed, opts.projects)))
		} else {
			line, err := presentSetupFile(configPath, true)
			if err != nil {
				return err
			}
			lines = append(lines, line)
		}
	} else {
		line, err := presentSetupFile(configPath, true)
		if err != nil {
			return err
		}
		lines = append(lines, line)
	}

	if needEnv {
		created, err := publishSetupFile(dir, defaultEnvName, envBytes)
		if err != nil {
			return err
		}
		if created {
			createdFiles++
			lines = append(lines, fmt.Sprintf("created %s (mode 0600; commented credential names only; no value is written)", envPath))
		} else {
			line, err := presentSetupFile(envPath, false)
			if err != nil {
				return err
			}
			lines = append(lines, line)
		}
	} else {
		line, err := presentSetupFile(envPath, false)
		if err != nil {
			return err
		}
		lines = append(lines, line)
	}

	if createdFiles > 0 {
		if needEnv {
			lines = append(lines, "next: credential values are yours to add, then run 'timesheet log'")
		} else {
			lines = append(lines, "next: run 'timesheet log'")
		}
	}

	for _, line := range lines {
		fmt.Fprintln(out, line)
	}
	return nil
}
