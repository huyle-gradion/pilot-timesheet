package main

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func pinSetupHome(t *testing.T) (home, dir string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TZ", "")
	t.Chdir(t.TempDir())
	return home, filepath.Join(home, ".timesheet")
}

func runSetupOutput(t *testing.T, args ...string) (string, error) {
	t.Helper()
	stdout, _, err := captureOutput(t, func() error { return runSetup(args) })
	return string(stdout), err
}

func setupMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

func readSetupBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func TestSetupNeedsNoProject(t *testing.T) {
	_, dir := pinSetupHome(t)
	t.Setenv("TZ", "UTC")
	if _, err := runSetupOutput(t); err != nil {
		t.Fatalf("setup without arguments: %v", err)
	}
	configPath := filepath.Join(dir, "config.json")
	if _, err := timesheet.LoadConfig(configPath); err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
	if raw := string(readSetupBytes(t, configPath)); strings.Contains(raw, `"project`) {
		t.Fatalf("generated config names a project:\n%s", raw)
	}
}

func TestSetupHappyPath(t *testing.T) {
	_, dir := pinSetupHome(t)
	configPath := filepath.Join(dir, "config.json")
	envPath := filepath.Join(dir, ".env.local")

	out, err := runSetupOutput(t, "--tz", "Europe/Amsterdam")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if got := setupMode(t, dir); got != 0o700 {
		t.Fatalf("dir mode = %04o, want 0700", got)
	}
	if got := setupMode(t, configPath); got != 0o600 {
		t.Fatalf("config mode = %04o, want 0600", got)
	}
	if got := setupMode(t, envPath); got != 0o600 {
		t.Fatalf("env mode = %04o, want 0600", got)
	}

	cfg, err := timesheet.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
	if cfg.Timezone != "Europe/Amsterdam" {
		t.Fatalf("timezone = %q", cfg.Timezone)
	}
	if want := filepath.Join(dir, setupStateName); cfg.StatePath != want || !filepath.IsAbs(cfg.StatePath) {
		t.Fatalf("state_path = %q, want the absolute %q", cfg.StatePath, want)
	}
	if raw := string(readSetupBytes(t, configPath)); strings.Contains(raw, "default_classification") {
		t.Fatalf("generated config invents a classification:\n%s", raw)
	}

	envBytes := readSetupBytes(t, envPath)
	if strings.Contains(string(envBytes), "=") {
		t.Fatalf("env template contains an assignment:\n%s", envBytes)
	}
	for _, name := range []string{"GITHUB_TOKEN", "GCAL_ICS_URL"} {
		if !strings.Contains(string(envBytes), "# "+name) {
			t.Fatalf("env template lacks the commented name %s:\n%s", name, envBytes)
		}
	}

	for _, want := range []string{"created " + dir, "created " + configPath, "created " + envPath, "next: credential values"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout %q lacks %q", out, want)
		}
	}
	if !strings.Contains(out, "then run 'timesheet log'") || strings.Contains(out, "'timesheet run'") {
		t.Fatalf("stdout %q does not point at 'timesheet log'", out)
	}
	if _, statErr := os.Stat(cfg.StatePath); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("setup created a state file at %s; it must only configure", cfg.StatePath)
	}
}

func TestSetupTimezoneFallsBackToHostTZ(t *testing.T) {
	_, dir := pinSetupHome(t)
	t.Setenv("TZ", "Asia/Tokyo")
	if _, err := runSetupOutput(t); err != nil {
		t.Fatalf("setup: %v", err)
	}
	cfg, err := timesheet.LoadConfig(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("generated config: %v", err)
	}
	if cfg.Timezone != "Asia/Tokyo" {
		t.Fatalf("timezone = %q, want the host TZ value", cfg.Timezone)
	}
}

func TestSetupRepeatIsNoOpWithoutSeedParams(t *testing.T) {
	_, dir := pinSetupHome(t)
	configPath := filepath.Join(dir, "config.json")
	envPath := filepath.Join(dir, ".env.local")
	if _, err := runSetupOutput(t, "--tz", "UTC"); err != nil {
		t.Fatalf("first setup: %v", err)
	}
	beforeConfig := readSetupBytes(t, configPath)
	beforeEnv := readSetupBytes(t, envPath)

	out, err := runSetupOutput(t)
	if err != nil {
		t.Fatalf("repeat setup: %v", err)
	}
	if !bytes.Equal(beforeConfig, readSetupBytes(t, configPath)) {
		t.Fatal("repeat setup changed config.json")
	}
	if !bytes.Equal(beforeEnv, readSetupBytes(t, envPath)) {
		t.Fatal("repeat setup changed .env.local")
	}
	if strings.Contains(out, "created ") {
		t.Fatalf("repeat setup created something:\n%s", out)
	}
	for _, want := range []string{"present " + configPath, "loads OK", "present " + envPath, "present, not read"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout %q lacks %q", out, want)
		}
	}
}

func TestSetupCreatesOnlyTheMissingEnvFile(t *testing.T) {
	_, dir := pinSetupHome(t)
	configPath := filepath.Join(dir, "config.json")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{
  "timezone": "Asia/Ho_Chi_Minh",
  "state_path": "` + filepath.Join(dir, "state.json") + `",
  "projects": [{"id": "WORK", "name": "Work"}]
}`
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	before := readSetupBytes(t, configPath)

	out, err := runSetupOutput(t)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !bytes.Equal(before, readSetupBytes(t, configPath)) {
		t.Fatal("existing config was changed")
	}
	if !strings.Contains(out, "present "+configPath) {
		t.Fatalf("stdout %q lacks the present config line", out)
	}
	if !strings.Contains(out, "created "+filepath.Join(dir, ".env.local")) {
		t.Fatalf("stdout %q lacks the created env line", out)
	}
}

func TestSetupNeverReplacesExistingInvalidConfig(t *testing.T) {
	_, dir := pinSetupHome(t)
	configPath := filepath.Join(dir, "config.json")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := readSetupBytes(t, configPath)

	out, err := runSetupOutput(t)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !bytes.Equal(before, readSetupBytes(t, configPath)) {
		t.Fatal("setup replaced an invalid existing config")
	}
	for _, want := range []string{"exists but does not load", "config: timezone is required", "fix it yourself"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout %q lacks %q", out, want)
		}
	}
	if strings.Contains(out, "deleted") || strings.Contains(out, "removed") {
		t.Fatalf("stdout %q implies a deletion", out)
	}
}

func TestSetupPreservesExistingModesAndReportsUnsafePermissions(t *testing.T) {
	_, dir := pinSetupHome(t)
	configPath := filepath.Join(dir, "config.json")
	envPath := filepath.Join(dir, ".env.local")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{
  "timezone": "Asia/Ho_Chi_Minh",
  "state_path": "/tmp/state.json",
  "projects": [{"id": "WORK", "name": "Work"}]
}`
	if err := os.WriteFile(configPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte("# keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(configPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(envPath, 0o640); err != nil {
		t.Fatal(err)
	}
	beforeConfig := readSetupBytes(t, configPath)
	beforeEnv := readSetupBytes(t, envPath)

	out, err := runSetupOutput(t)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if got := setupMode(t, configPath); got != 0o644 {
		t.Fatalf("config mode = %04o, want the existing 0644", got)
	}
	if got := setupMode(t, envPath); got != 0o640 {
		t.Fatalf("env mode = %04o, want the existing 0640", got)
	}
	if !bytes.Equal(beforeConfig, readSetupBytes(t, configPath)) || !bytes.Equal(beforeEnv, readSetupBytes(t, envPath)) {
		t.Fatal("setup modified or chmodded an existing file")
	}
	if strings.Count(out, "unsafe: group/other permissions") != 2 {
		t.Fatalf("stdout %q does not report both unsafe modes", out)
	}
}

func TestSetupReportsSymlinkedFinalWithoutFollowingIt(t *testing.T) {
	_, dir := pinSetupHome(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "real-config.json")
	body := `{
  "timezone": "Asia/Ho_Chi_Minh",
  "state_path": "/tmp/state.json",
  "projects": [{"id": "REAL", "name": "Real"}]
}`
	if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	if err := os.Symlink(target, configPath); err != nil {
		t.Fatal(err)
	}

	out, err := runSetupOutput(t)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !strings.Contains(out, "symlink; not followed; left untouched") {
		t.Fatalf("stdout %q does not report the symlink", out)
	}
	if !bytes.Equal([]byte(body), readSetupBytes(t, target)) {
		t.Fatal("setup followed and modified the symlink target")
	}
	info, err := os.Lstat(configPath)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config path is no longer a symlink: %v, %v", info, err)
	}
}

func TestSetupRefusesSymlinkedTargetDirectory(t *testing.T) {
	home, _ := pinSetupHome(t)
	realDir := t.TempDir()
	if err := os.Symlink(realDir, filepath.Join(home, ".timesheet")); err != nil {
		t.Fatal(err)
	}

	_, err := runSetupOutput(t)
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v, want the unsafe-target refusal", err)
	}
	entries, readErr := os.ReadDir(realDir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("setup wrote through the symlinked target: %v", entries)
	}
}

func TestSetupConcurrentRunsPublishOneFileEach(t *testing.T) {
	_, dir := pinSetupHome(t)
	opts := setupOptions{timezone: "Europe/Amsterdam"}
	var buffers [2]bytes.Buffer
	var errs [2]error
	var wg sync.WaitGroup
	for i := range buffers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = executeSetup(opts, &buffers[i])
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent setup %d: %v", i, err)
		}
	}
	combined := buffers[0].String() + buffers[1].String()
	for _, path := range []string{filepath.Join(dir, "config.json"), filepath.Join(dir, ".env.local")} {
		if got := strings.Count(combined, "created "+path); got != 1 {
			t.Fatalf("%s created lines = %d, want 1:\n%s", path, got, combined)
		}
		if got := strings.Count(combined, "present "+path+" ("); got != 1 {
			t.Fatalf("%s present lines = %d, want the loser's EEXIST report:\n%s", path, got, combined)
		}
	}
	if _, err := timesheet.LoadConfig(filepath.Join(dir, "config.json")); err != nil {
		t.Fatalf("concurrent config does not load: %v", err)
	}
}

func TestSetupLeavesExistingFilesAndLeftoversAlone(t *testing.T) {
	_, dir := pinSetupHome(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, ".config.json.tmp-999999-1")
	if err := os.WriteFile(orphan, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	notOurs := filepath.Join(dir, ".config.json.tmp-nope")
	if err := os.WriteFile(notOurs, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runSetupOutput(t, "--tz", "UTC")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if strings.Contains(out, "reaped") || strings.Contains(out, "removed") {
		t.Fatalf("stdout %q implies a cleanup", out)
	}
	for _, path := range []string{orphan, keep, notOurs} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("setup removed %s: %v", path, statErr)
		}
	}
	if got := readSetupBytes(t, orphan); !bytes.Equal(got, []byte("stale")) {
		t.Fatalf("leftover temp was modified: %q", got)
	}
	if _, statErr := os.Stat(filepath.Join(dir, defaultConfigName)); statErr != nil {
		t.Fatalf("setup did not publish the missing config: %v", statErr)
	}
}

func TestSetupSeedConfigCarriesEnvAndRelativePathsButNoProjects(t *testing.T) {
	_, dir := pinSetupHome(t)
	seedDir := t.TempDir()
	seedPath := filepath.Join(seedDir, "seed.json")
	body := `{
  "timezone": "America/New_York",
  "state_path": "relative-state.json",
  "projects": [{"id": "SEED", "name": "Seed Project", "default_classification": "DEV"}],
  "required_env": ["SEED_TOKEN"],
  "migration": {"enabled": true, "legacy_root": "legacy", "legacy_timezone": "UTC", "project_map": {"OLD": "SEED"}}
}`
	if err := os.WriteFile(seedPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runSetupOutput(t, "--seed-config", seedPath, "--tz", "Asia/Tokyo")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	configPath := filepath.Join(dir, "config.json")
	cfg, err := timesheet.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("generated config: %v", err)
	}
	if cfg.Timezone != "Asia/Tokyo" {
		t.Fatalf("timezone = %q, want the explicit --tz", cfg.Timezone)
	}
	if cfg.StatePath != filepath.Join(dir, setupStateName) {
		t.Fatalf("state_path = %q, want the target's absolute state path", cfg.StatePath)
	}
	if raw := string(readSetupBytes(t, configPath)); strings.Contains(raw, `"project`) {
		t.Fatalf("the seed's retired project keys were copied forward:\n%s", raw)
	}
	if len(cfg.RequiredEnv) != 1 || cfg.RequiredEnv[0] != "SEED_TOKEN" {
		t.Fatalf("required_env = %v", cfg.RequiredEnv)
	}
	if cfg.Migration == nil || cfg.Migration.LegacyRoot != filepath.Join(seedDir, "legacy") {
		t.Fatalf("migration.legacy_root = %+v, want the seed-dir-relative path resolved", cfg.Migration)
	}
	envBytes := readSetupBytes(t, filepath.Join(dir, ".env.local"))
	if !strings.Contains(string(envBytes), "# SEED_TOKEN") || strings.Contains(string(envBytes), "GITHUB_TOKEN") {
		t.Fatalf("env template does not use the seed's required_env:\n%s", envBytes)
	}
	if !strings.Contains(out, "created ") {
		t.Fatalf("stdout %q lacks the created lines", out)
	}
}

func TestSetupRejectsBadInputBeforeCreatingAnything(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"invalid timezone", []string{"--tz", "Not/AZone"}, "not a recognised IANA timezone"},
		{"pseudo zone", []string{"--tz", "Local"}, "not a recognised IANA timezone"},
		{"positional", []string{"extra"}, "positional"},
	}
	for _, tc := range cases {
		_, dir := pinSetupHome(t)
		_, err := runSetupOutput(t, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
		if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("%s created %s despite the error", tc.name, dir)
		}
	}
}

func TestSetupInvalidSeedCreatesNothing(t *testing.T) {
	_, dir := pinSetupHome(t)
	seedPath := filepath.Join(t.TempDir(), "seed.json")
	if err := os.WriteFile(seedPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := runSetupOutput(t, "--seed-config", seedPath)
	if err == nil || !strings.Contains(err.Error(), "config: timezone is required") {
		t.Fatalf("err = %v, want the seed's field error", err)
	}
	if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("setup created %s despite the invalid seed", dir)
	}
}

func TestSetupHelpIsNotAUsageFailure(t *testing.T) {
	pinSetupHome(t)
	if err := runSetup([]string{"--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("--help = %v, want flag.ErrHelp", err)
	}
}

func TestSetupEnvTemplateNeverCarriesValues(t *testing.T) {
	_, dir := pinSetupHome(t)
	if _, err := runSetupOutput(t, "--tz", "UTC"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	envBytes := readSetupBytes(t, filepath.Join(dir, ".env.local"))
	if strings.ContainsAny(string(envBytes), "\"'") || strings.Contains(string(envBytes), "http") {
		t.Fatalf("env template carries a value-like literal:\n%s", envBytes)
	}
}

func TestHostTimezoneFrom(t *testing.T) {
	dir := t.TempDir()
	etcTimezone := filepath.Join(dir, "timezone")
	if err := os.WriteFile(etcTimezone, []byte("Asia/Tokyo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	zoneinfo := filepath.Join(dir, "zoneinfo", "Europe", "Amsterdam")
	if err := os.MkdirAll(filepath.Dir(zoneinfo), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zoneinfo, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	localtime := filepath.Join(dir, "localtime")
	if err := os.Symlink(zoneinfo, localtime); err != nil {
		t.Fatal(err)
	}

	if got, err := hostTimezoneFrom(":Europe/Amsterdam", etcTimezone, localtime); err != nil || got != "Europe/Amsterdam" {
		t.Fatalf("TZ tier: got %q, err %v", got, err)
	}
	if got, err := hostTimezoneFrom("", etcTimezone, localtime); err != nil || got != "Asia/Tokyo" {
		t.Fatalf("timezone-file tier: got %q, err %v", got, err)
	}
	if got, err := hostTimezoneFrom("", filepath.Join(dir, "missing"), localtime); err != nil || got != "Europe/Amsterdam" {
		t.Fatalf("localtime tier: got %q, err %v", got, err)
	}
	if _, err := hostTimezoneFrom("", filepath.Join(dir, "missing"), filepath.Join(dir, "missing")); err == nil {
		t.Fatal("unresolvable host zone returned no error")
	}
	if _, err := hostTimezoneFrom("Local", filepath.Join(dir, "missing"), filepath.Join(dir, "missing")); err == nil {
		t.Fatal("TZ=Local was accepted as a host zone")
	}
	if _, err := hostTimezoneFrom(":", filepath.Join(dir, "missing"), filepath.Join(dir, "missing")); err == nil {
		t.Fatal("an empty TZ value was accepted as a host zone")
	}
}

func TestSetupRemovesStagingFiles(t *testing.T) {
	_, dir := pinSetupHome(t)
	if _, err := runSetupOutput(t, "--tz", "UTC"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), setupTempMarker) {
			t.Fatalf("setup left the staging file %s behind", e.Name())
		}
	}
}

func TestSetupRepeatIgnoresSeedConfig(t *testing.T) {
	_, dir := pinSetupHome(t)
	if _, err := runSetupOutput(t, "--tz", "UTC"); err != nil {
		t.Fatalf("first setup: %v", err)
	}
	if _, err := runSetupOutput(t, "--seed-config", filepath.Join(dir, "missing-seed.json")); err != nil {
		t.Fatalf("repeat setup read the seed: %v", err)
	}
}

func TestSetupLeavesPresentEnvUnreadWhenCreatingConfig(t *testing.T) {
	_, dir := pinSetupHome(t)
	envPath := filepath.Join(dir, ".env.local")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte("A=b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runSetupOutput(t, "--tz", "UTC")
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if !strings.Contains(out, "present "+envPath+" (mode 0600; present, not read;") {
		t.Fatalf("stdout %q does not report the env file as present and unread", out)
	}
}
