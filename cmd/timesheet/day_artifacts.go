package main

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func dayArtifactsDir(statePath, day string) (string, error) {
	abs, err := filepath.Abs(statePath)
	if err != nil {
		return "", fmt.Errorf("resolve the store's day folder from %s: %w", statePath, err)
	}
	return filepath.Join(filepath.Dir(abs), "days", day), nil
}

func legacyDayArtifactsDir(statePath, day string) (string, error) {
	abs, err := filepath.Abs(statePath)
	if err != nil {
		return "", fmt.Errorf("resolve artifact store identity: %w", err)
	}
	identity := fmt.Sprintf("%x", sha256.Sum256([]byte(abs)))
	return filepath.Join(os.TempDir(), "timesheet", identity, day), nil
}

// legacyDayArtifactsStop returns a one-line stop reason when the day's working
// files still sit in the pre-#233 temp location and the store's day folder
// does not exist yet; otherwise "". A day in progress is never silently
// restarted: the owner moves the files once with the named command.
func legacyDayArtifactsStop(statePath, day, dir string) (string, error) {
	if _, err := os.Stat(dir); err == nil || !errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	legacy, err := legacyDayArtifactsDir(statePath, day)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(legacy)
	if err != nil || len(entries) == 0 {
		return "", nil
	}
	return "the day's working files are in the old temp location " + legacy +
		"; move them into the store once and rerun: " + shellJoin([]string{"mkdir", "-p", filepath.Dir(dir)}) +
		" && " + shellJoin([]string{"mv", legacy, dir}), nil
}

func ensureDayArtifactsDir(verb, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("%s: the day folder %s cannot be created: %w", verb, dir, err)
	}
	return nil
}
