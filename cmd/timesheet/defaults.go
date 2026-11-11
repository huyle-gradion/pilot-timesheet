package main

// The store-path flags default to fixed locations under $HOME/.timesheet so
// the daily commands never have to repeat them: --config resolves to
// ./timesheet.json when the current directory carries one (project-local), and
// otherwise to $HOME/.timesheet/config.json; --env-file defaults to
// $HOME/.timesheet/.env.local. An explicit flag always wins verbatim, so
// tests, scripted callers and alternate setups keep working unchanged.
//
// The three tiers are never merged: exactly one configuration is loaded. The
// selected path is reported once per invocation on stderr by the shared
// resolveStoreFlags call, so no parser hand-prints it and every command
// reports the same choice.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultConfigName = "config.json"
	defaultEnvName    = ".env.local"
	// projectLocalConfigPath is looked up relative to the process cwd. It is
	// not an ancestor walk: only the current directory is consulted.
	projectLocalConfigPath = "./timesheet.json"
)

// configChoice records how the configuration path was selected. It exists so
// the chosen-path report can name the tier without re-deriving it.
type configChoice struct {
	path string
	tier string // "explicit", "project-local" or "home"
}

// homeTimesheetPath returns <home>/.timesheet/<name>. When the home directory
// cannot be resolved the error names the fix, so the message is actionable
// wherever the caller surfaces it.
func homeTimesheetPath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot resolve the default $HOME/.timesheet/%s ($HOME is not set); pass the flag explicitly", name)
	}
	return filepath.Join(home, ".timesheet", name), nil
}

// resolveConfigChoice applies the three tiers: the explicit --config verbatim,
// then ./timesheet.json in the cwd when it exists, then the home default. A
// missing configuration is an error naming both candidates and the fixes.
func resolveConfigChoice(name, explicit string) (configChoice, error) {
	if strings.TrimSpace(explicit) != "" {
		return configChoice{path: explicit, tier: "explicit"}, nil
	}
	if _, err := os.Stat(projectLocalConfigPath); err == nil {
		return configChoice{path: projectLocalConfigPath, tier: "project-local"}, nil
	}
	def, err := homeTimesheetPath(defaultConfigName)
	if err != nil {
		return configChoice{}, fmt.Errorf("%s: %v", name, err)
	}
	if _, err := os.Stat(def); err != nil {
		return configChoice{}, fmt.Errorf("%s: no configuration at %s or %s; pass --config <path> or run 'timesheet init'", name, projectLocalConfigPath, def)
	}
	return configChoice{path: def, tier: "home"}, nil
}

// resolveConfigPath returns the configuration path a command loads. LoadConfig's
// own read failure carries no path, so an unconfigured machine would otherwise
// fail with a message that does not say what to create or pass.
func resolveConfigPath(name, explicit string) (string, error) {
	choice, err := resolveConfigChoice(name, explicit)
	if err != nil {
		return "", err
	}
	return choice.path, nil
}

// resolveEnvChoice returns the environment-file path: the explicit --env-file
// verbatim (an unreadable explicit file still fails in LoadEnvFile), or the
// default $HOME/.timesheet/.env.local when it exists. A missing or unresolvable
// default is NOT an error: the command runs with no file values, exactly as an
// omitted --env-file always has, and ops needing credentials fail closed with
// their own errors naming the fix. There is deliberately no project-local tier
// for the env file.
func resolveEnvChoice(explicit string) (path, tier string, err error) {
	if strings.TrimSpace(explicit) != "" {
		return explicit, "explicit", nil
	}
	def, err := homeTimesheetPath(defaultEnvName)
	if err != nil {
		return "", "", nil
	}
	if _, err := os.Stat(def); err != nil {
		return "", "", nil
	}
	return def, "home", nil
}

// resolveEnvFile keeps the single-value shape the existing callers and tests
// use; the tier is only needed for the shared report.
func resolveEnvFile(name, explicit string) (string, error) {
	path, _, err := resolveEnvChoice(explicit)
	if err != nil {
		return "", err
	}
	return path, nil
}

// noteConfigChoice prints the selected paths once per invocation. It lives here
// so the defaulting rules and their report have one owner; parsers never
// hand-print a discovered path.
func noteConfigChoice(name string, config configChoice, envPath, envTier string) {
	switch config.tier {
	case "project-local":
		fmt.Fprintf(os.Stderr, "%s: using config %s (project-local)\n", name, config.path)
	case "home":
		fmt.Fprintf(os.Stderr, "%s: using config %s (home default)\n", name, config.path)
	default:
		fmt.Fprintf(os.Stderr, "%s: using config %s (explicit)\n", name, config.path)
	}
	if envPath == "" {
		return
	}
	tier := "explicit"
	if envTier == "home" {
		tier = "home default"
	}
	fmt.Fprintf(os.Stderr, "%s: using env file %s (%s)\n", name, envPath, tier)
}

// resolveStoreFlags resolves both store-path flags together and reports the
// selection once. Every parser calls it right after flag parsing, so the rest
// of the command sees resolved paths only and no command ever re-implements
// the defaulting rules.
func resolveStoreFlags(name, configPath, envFile string) (string, string, error) {
	config, err := resolveConfigChoice(name, configPath)
	if err != nil {
		return "", "", err
	}
	env, envTier, err := resolveEnvChoice(envFile)
	if err != nil {
		return "", "", err
	}
	noteConfigChoice(name, config, env, envTier)
	return config.path, env, nil
}
