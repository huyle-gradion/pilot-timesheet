package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultConfigName      = "config.json"
	defaultEnvName         = ".env.local"
	projectLocalConfigPath = "./timesheet.json"
)

type configChoice struct {
	path string
	tier string // "explicit", "project-local" or "home"
}

func homeTimesheetPath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot resolve the default $HOME/.timesheet/%s ($HOME is not set); pass the flag explicitly", name)
	}
	return filepath.Join(home, ".timesheet", name), nil
}

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

func resolveConfigPath(name, explicit string) (string, error) {
	choice, err := resolveConfigChoice(name, explicit)
	if err != nil {
		return "", err
	}
	return choice.path, nil
}

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

func resolveEnvFile(name, explicit string) (string, error) {
	path, _, err := resolveEnvChoice(explicit)
	if err != nil {
		return "", err
	}
	return path, nil
}

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
