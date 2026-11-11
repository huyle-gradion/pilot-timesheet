package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func writeDefaultsConfig(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolveConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())

	explicit := filepath.Join(t.TempDir(), "elsewhere.json")
	got, err := resolveConfigPath("run", explicit)
	if err != nil || got != explicit {
		t.Fatalf("explicit config: got %q, err %v", got, err)
	}

	_, err = resolveConfigPath("run", "")
	if err == nil || !strings.Contains(err.Error(), "no configuration at") ||
		!strings.Contains(err.Error(), filepath.Join(home, ".timesheet", "config.json")) ||
		!strings.Contains(err.Error(), "./timesheet.json") ||
		!strings.Contains(err.Error(), "pass --config") ||
		!strings.Contains(err.Error(), "run 'timesheet init'") ||
		strings.Contains(err.Error(), "timesheet setup") {
		t.Fatalf("missing default config: err = %v", err)
	}

	def := filepath.Join(home, ".timesheet", "config.json")
	writeDefaultsConfig(t, def)
	got, err = resolveConfigPath("run", "")
	if err != nil || got != def {
		t.Fatalf("default config: got %q, err %v", got, err)
	}
}

func TestResolveConfigPathProjectLocalTier(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	homeConfig := filepath.Join(home, ".timesheet", "config.json")
	writeDefaultsConfig(t, homeConfig)

	parent := t.TempDir()
	writeDefaultsConfig(t, filepath.Join(parent, "timesheet.json"))
	child := filepath.Join(parent, "child")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}

	t.Chdir(parent)
	got, err := resolveConfigPath("run", "")
	if err != nil || got != projectLocalConfigPath {
		t.Fatalf("project-local config: got %q, err %v", got, err)
	}

	explicit := filepath.Join(t.TempDir(), "explicit.json")
	got, err = resolveConfigPath("run", explicit)
	if err != nil || got != explicit {
		t.Fatalf("explicit over project-local: got %q, err %v", got, err)
	}

	t.Chdir(child)
	got, err = resolveConfigPath("run", "")
	if err != nil || got != homeConfig {
		t.Fatalf("no ancestor lookup: got %q, err %v", got, err)
	}
}

func TestResolveEnvFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())

	explicit := filepath.Join(t.TempDir(), "other.env")
	got, err := resolveEnvFile("run", explicit)
	if err != nil || got != explicit {
		t.Fatalf("explicit env file: got %q, err %v", got, err)
	}

	got, err = resolveEnvFile("run", "")
	if err != nil || got != "" {
		t.Fatalf("missing default env file: got %q, err %v", got, err)
	}

	def := filepath.Join(home, ".timesheet", ".env.local")
	if err := os.MkdirAll(filepath.Dir(def), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(def, []byte("A=b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = resolveEnvFile("run", "")
	if err != nil || got != def {
		t.Fatalf("default env file: got %q, err %v", got, err)
	}
}

func TestResolveStoreFlags(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())
	dir := filepath.Join(home, ".timesheet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, env, err := resolveStoreFlags("plan", "", "")
	if err != nil || cfg != filepath.Join(dir, "config.json") || env != "" {
		t.Fatalf("store flags: cfg=%q env=%q err=%v", cfg, env, err)
	}
}

func TestResolveStoreFlagsReportsChoiceOnce(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := t.TempDir()
	t.Chdir(cwd)
	dir := filepath.Join(home, ".timesheet")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env.local"), []byte("A=b\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, err := captureOutput(t, func() error {
		_, _, err := resolveStoreFlags("status", "", "")
		return err
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(stdout) != 0 {
		t.Fatalf("stdout = %q, want nothing", stdout)
	}
	if got := strings.Count(string(stderr), "status: using config "); got != 1 {
		t.Fatalf("config report lines = %d, want 1 in %q", got, stderr)
	}
	if !strings.Contains(string(stderr), "(home default)") {
		t.Fatalf("stderr = %q, want the home-default tier", stderr)
	}
	if got := strings.Count(string(stderr), "status: using env file "); got != 1 {
		t.Fatalf("env report lines = %d, want 1 in %q", got, stderr)
	}
}

func TestResolveStoreFlagsExplicitReportedVerbatim(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := os.WriteFile(filepath.Join(cwd, "timesheet.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	explicit := filepath.Join(t.TempDir(), "chosen.json")

	_, stderr, err := captureOutput(t, func() error {
		_, _, err := resolveStoreFlags("stage", explicit, "")
		return err
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.Contains(string(stderr), "stage: using config "+explicit+" (explicit)") {
		t.Fatalf("stderr = %q, want the explicit chosen path", stderr)
	}
}

func TestResolveStoreFlagsMissingConfigNamesCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	_, _, err := resolveStoreFlags("reconcile", "", "")
	if err == nil || !strings.HasPrefix(err.Error(), "reconcile: no configuration at") {
		t.Fatalf("config error should name the command, got %v", err)
	}
}

func TestLoadConfigKeepsRelativeStatePath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	body := `{
  "timezone": "Asia/Ho_Chi_Minh",
  "state_path": "state.json",
  "projects": [{"id": "WORK", "name": "Work"}]
}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	cfg, err := timesheet.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.StatePath != "state.json" {
		t.Fatalf("StatePath = %q, want the relative value untouched", cfg.StatePath)
	}
}

func TestResolveEnvFileIgnoresCwdEnvFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := os.WriteFile(filepath.Join(cwd, ".env.local"), []byte("A=b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveEnvFile("run", ""); err != nil || got != "" {
		t.Fatalf("cwd env file: got %q, err %v; want no env file", got, err)
	}
}
