package main

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

type setupOptions struct {
	seedConfigPath string
	timezone       string
}

func parseSetupFlags(args []string) (setupOptions, error) {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	seedConfig := flags.String("seed-config", "", "loadable configuration to seed required_env and sources from")
	tz := flags.String("tz", "", "IANA timezone for the generated configuration (default: the host zone)")
	if err := flags.Parse(args); err != nil {
		return setupOptions{}, err
	}
	if flags.NArg() != 0 {
		return setupOptions{}, setupUsagef("setup does not accept positional arguments")
	}
	opts := setupOptions{seedConfigPath: strings.TrimSpace(*seedConfig), timezone: strings.TrimSpace(*tz)}
	return opts, nil
}

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
			lines = append(lines, fmt.Sprintf("created %s (mode 0600; timezone %s from %s; state_path %s)",
				configPath, timezone, timezoneSource, filepath.Join(dir, setupStateName)))
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
