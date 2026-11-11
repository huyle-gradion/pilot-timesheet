package main

// The defaulting rules of the store-path flags: --config resolves explicit
// --config > ./timesheet.json in the cwd > $HOME/.timesheet/config.json, and
// --env-file resolves explicit > $HOME/.timesheet/.env.local when it exists.
// A missing configuration is an error naming both candidates and the fixes; a
// missing default env file means no file values, never an error. An explicit
// flag always wins verbatim, the tiers are never merged, and no ancestor is
// searched. os.UserHomeDir reads $HOME on the platforms this targets (Linux,
// macOS), so t.Setenv("HOME", ...) pins every case; t.Chdir pins the cwd.

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

	// Explicit wins verbatim, even when a default also exists.
	explicit := filepath.Join(t.TempDir(), "elsewhere.json")
	got, err := resolveConfigPath("run", explicit)
	if err != nil || got != explicit {
		t.Fatalf("explicit config: got %q, err %v", got, err)
	}

	// A missing configuration is an error naming both candidates and the fix.
	// The fix names the live first-run command, never the retired `setup`.
	_, err = resolveConfigPath("run", "")
	if err == nil || !strings.Contains(err.Error(), "no configuration at") ||
		!strings.Contains(err.Error(), filepath.Join(home, ".timesheet", "config.json")) ||
		!strings.Contains(err.Error(), "./timesheet.json") ||
		!strings.Contains(err.Error(), "pass --config") ||
		!strings.Contains(err.Error(), "run 'timesheet init'") ||
		strings.Contains(err.Error(), "timesheet setup") {
		t.Fatalf("missing default config: err = %v", err)
	}

	// An existing home default is used as-is.
	def := filepath.Join(home, ".timesheet", "config.json")
	writeDefaultsConfig(t, def)
	got, err = resolveConfigPath("run", "")
	if err != nil || got != def {
		t.Fatalf("default config: got %q, err %v", got, err)
	}
}

// TestResolveConfigPathProjectLocalTier pins the middle tier: the cwd file wins
// over the home default, an explicit flag still wins over both, and no ancestor
// directory is consulted.
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

	// Project-local wins over the home default.
	t.Chdir(parent)
	got, err := resolveConfigPath("run", "")
	if err != nil || got != projectLocalConfigPath {
		t.Fatalf("project-local config: got %q, err %v", got, err)
	}

	// Explicit wins over project-local.
	explicit := filepath.Join(t.TempDir(), "explicit.json")
	got, err = resolveConfigPath("run", explicit)
	if err != nil || got != explicit {
		t.Fatalf("explicit over project-local: got %q, err %v", got, err)
	}

	// A parent's timesheet.json is not an ancestor fallback: the child resolves
	// the home default.
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

	// Explicit wins verbatim; an unreadable explicit file fails later, in
	// LoadEnvFile, exactly as before.
	explicit := filepath.Join(t.TempDir(), "other.env")
	got, err := resolveEnvFile("run", explicit)
	if err != nil || got != explicit {
		t.Fatalf("explicit env file: got %q, err %v", got, err)
	}

	// A missing default means no file values, not an error.
	got, err = resolveEnvFile("run", "")
	if err != nil || got != "" {
		t.Fatalf("missing default env file: got %q, err %v", got, err)
	}

	// An existing default is used, and no project-local env file exists.
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
	// Config resolves from the default; env stays empty when its default is
	// absent — one call, both rules.
	cfg, env, err := resolveStoreFlags("plan", "", "")
	if err != nil || cfg != filepath.Join(dir, "config.json") || env != "" {
		t.Fatalf("store flags: cfg=%q env=%q err=%v", cfg, env, err)
	}
}

// TestResolveStoreFlagsReportsChoiceOnce pins the shared stderr report: the
// selected tier is named exactly once per invocation, on stderr, and never on
// stdout (which run/draft reserve for JSON).
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

// TestResolveStoreFlagsExplicitReportedVerbatim: an explicit --config is still
// reported once, labelled explicit, and the project-local file is not consulted.
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

// TestLoadConfigKeepsRelativeStatePath is the no-silent-semantic-change guard:
// the loader is untouched, so a relative state_path keeps resolving against the
// process cwd exactly as before. Only setup-generated configs bake an absolute
// path.
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
