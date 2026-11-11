package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func TestMigrateCLIDispatch(t *testing.T) {
	if os.Getenv("TIMESHEET_MIGRATE_CLI_CHILD") == "1" {
		separator := slices.Index(os.Args, "--")
		if separator < 0 {
			t.Fatal("missing CLI arguments")
		}
		os.Args = append([]string{"timesheet"}, os.Args[separator+1:]...)
		main()
		os.Exit(0)
	}
	home := t.TempDir()
	run := func(args ...string) (string, string, int) {
		t.Helper()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, executable, append([]string{"-test.run=^TestMigrateCLIDispatch$", "--"}, args...)...)
		cmd.Dir = home
		cmd.Env = []string{"TIMESHEET_MIGRATE_CLI_CHILD=1", "HOME=" + home, "TMPDIR=" + home, "XDG_CONFIG_HOME=" + home}
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err = cmd.Run()
		code := 0
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return stdout.String(), stderr.String(), code
	}

	cfg, _ := migrationConfig(t, true)
	stdout, stderr, code := run("migrate", "preview", "--config", cfg, "--date", "2026-10-20")
	var preview timesheet.MigrationPreview
	if code != 0 || json.Unmarshal([]byte(stdout), &preview) != nil || preview.MigrationID == "" {
		t.Fatalf("preview: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	stdout, stderr, code = run("migrate", "apply", "--config", cfg, "--date", "2026-10-20", "--migration-id", "bogus")
	if code != 1 || stdout != "" || !strings.Contains(stderr, timesheet.CodeMigrationStale) {
		t.Fatalf("stale apply: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	stdout, stderr, code = run("migrate", "apply", "--config", cfg, "--date", "2026-10-20", "--migration-id", preview.MigrationID)
	var receipt timesheet.MigrationApplyReceipt
	if code != 0 || json.Unmarshal([]byte(stdout), &receipt) != nil || receipt.Status != timesheet.MigrationApplied {
		t.Fatalf("apply: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	disabled, _ := migrationConfig(t, false)
	if stdout, stderr, code = run("migrate", "preview", "--config", disabled, "--date", "2026-10-20"); code != 1 || stdout != "" ||
		!strings.Contains(stderr, timesheet.CodeMigrationDisabled) {
		t.Fatalf("disabled preview: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if stdout, stderr, code = run("migrate"); code != 1 || stdout != "" || !strings.Contains(stderr, "migrate preview") {
		t.Fatalf("bare migrate: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

const legacyRecord = `{
  "schema_version": 1,
  "date": "2026-10-20",
  "revision": 1,
  "updated_at": "2026-10-20T18:00:00Z",
  "confirmed": [
    {"id": "legacy-1", "date": "2026-10-20", "start_time": "09:00", "end_time": "10:30", "project": "OLD-1", "description": "Synthetic legacy work", "confirmed_at": "2026-10-20T17:00:00Z", "revision": 1, "kind": "work", "manual": true}
  ]
}`

func migrationConfig(t *testing.T, enabled bool) (cfgPath, statePath string) {
	t.Helper()
	dir := t.TempDir()
	legacy := filepath.Join(dir, "legacy")
	if err := os.MkdirAll(filepath.Join(legacy, "workflow"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "workflow", "2026-10-20.json"), []byte(legacyRecord), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath = filepath.Join(dir, "state.json")
	cfg := map[string]any{
		"timezone":   "UTC",
		"state_path": statePath,
		"projects":   []map[string]string{{"id": "P1", "name": "Synthetic", "default_classification": "DEV"}},
		"migration": map[string]any{
			"enabled": enabled, "legacy_root": legacy, "legacy_timezone": "Europe/Amsterdam",
			"project_map": map[string]string{"OLD-1": "P1"}, "classification_map": map[string]string{},
		},
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath = filepath.Join(dir, "timesheet.json")
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, statePath
}

func runMigrateJSON(t *testing.T, v any, args ...string) error {
	t.Helper()
	out, _, err := captureOutput(t, func() error { return runMigrate(args) })
	if err == nil && v != nil {
		if jerr := json.Unmarshal(out, v); jerr != nil {
			t.Fatalf("migrate %v: stdout is not JSON: %v\n%s", args, jerr, out)
		}
	}
	return err
}

func fileBytes(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

func TestMigrateCommandsAreStateless(t *testing.T) {
	disabled, _ := migrationConfig(t, false)
	var appErr *timesheet.AppError
	if err := runMigrateJSON(t, nil, "preview", "--config", disabled, "--date", "2026-10-20"); !errors.As(err, &appErr) || appErr.Code != timesheet.CodeMigrationDisabled {
		t.Fatalf("preview with migration disabled = %v, want migration_disabled", err)
	}

	cfg, state := migrationConfig(t, true)
	var preview timesheet.MigrationPreview
	if err := runMigrateJSON(t, &preview, "preview", "--config", cfg, "--date", "2026-10-20"); err != nil {
		t.Fatal(err)
	}
	if !preview.Ready || preview.MigrationID == "" || preview.NoChange {
		t.Fatalf("preview = %+v", preview)
	}

	before := fileBytes(t, state)
	err := runMigrateJSON(t, nil, "apply", "--config", cfg, "--date", "2026-10-20", "--migration-id", "bogus")
	if !errors.As(err, &appErr) || appErr.Code != timesheet.CodeMigrationStale {
		t.Fatalf("apply with a bogus id = %v, want migration_stale", err)
	}
	if fileBytes(t, state) != before {
		t.Fatal("a stale apply wrote the store")
	}
	if err := runMigrate([]string{"apply", "--config", cfg, "--date", "2026-10-20"}); err == nil || !strings.Contains(err.Error(), "--migration-id") {
		t.Fatalf("apply without --migration-id = %v", err)
	}

	var receipt timesheet.MigrationApplyReceipt
	if err := runMigrateJSON(t, &receipt, "apply", "--config", cfg, "--date", "2026-10-20", "--migration-id", preview.MigrationID); err != nil {
		t.Fatal(err)
	}
	if receipt.Status != timesheet.MigrationApplied || receipt.ReceiptID != preview.MigrationID {
		t.Fatalf("apply = %+v", receipt)
	}

	var again timesheet.MigrationPreview
	if err := runMigrateJSON(t, &again, "preview", "--config", cfg, "--date", "2026-10-20"); err != nil || !again.NoChange {
		t.Fatalf("repeat preview = %+v, %v; want no_change", again, err)
	}
	var noop timesheet.MigrationApplyReceipt
	if err := runMigrateJSON(t, &noop, "apply", "--config", cfg, "--date", "2026-10-20", "--migration-id", again.MigrationID); err != nil || noop.Status != timesheet.MigrationNoChange {
		t.Fatalf("repeat apply = %+v, %v; want no_change", noop, err)
	}
}

func TestMigrateDateRangeExpands(t *testing.T) {
	cfg, _ := migrationConfig(t, true)
	var ranged, listed timesheet.MigrationPreview
	if err := runMigrateJSON(t, &ranged, "preview", "--config", cfg, "--date", "2026-10-19..2026-10-21"); err != nil {
		t.Fatal(err)
	}
	if err := runMigrateJSON(t, &listed, "preview", "--config", cfg,
		"--date", "2026-10-21", "--date", "2026-10-19", "--date", "2026-10-20"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(ranged.Dates, ",") != "2026-10-19,2026-10-20,2026-10-21" || ranged.MigrationID != listed.MigrationID {
		t.Fatalf("range %v (%s) vs list %v (%s)", ranged.Dates, ranged.MigrationID, listed.Dates, listed.MigrationID)
	}
	for _, bad := range []string{"2026-10-21..2026-10-19", "2026-10-20..", "2026-01-01..2026-12-31"} {
		if err := runMigrate([]string{"preview", "--config", cfg, "--date", bad}); err == nil {
			t.Errorf("--date %q was accepted", bad)
		}
	}
	if err := runMigrate([]string{"preview", "--config", cfg}); err == nil || !strings.Contains(err.Error(), "--date") {
		t.Fatalf("preview without --date = %v", err)
	}
	if err := runMigrate([]string{"rollback", "--config", cfg}); err == nil {
		t.Fatal("an unknown migrate subcommand was accepted")
	}
}
