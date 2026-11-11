package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// TestRunStatusReportsEnvironmentNames (issue #272): status carries what the
// retired serve-only check_environment op reported, the required variable
// NAMES and which are missing, never a value.
func TestRunStatusReportsEnvironmentNames(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	body := `{"timezone": "UTC", "state_path": "` + filepath.Join(dir, "state.json") + `",
  "projects": [{"id": "WORK", "name": "Work", "default_classification": "DEV"}],
  "required_env": ["SYNTHETIC_PRESENT_TOKEN", "SYNTHETIC_ABSENT_TOKEN"]}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SYNTHETIC_PRESENT_TOKEN", "synthetic-secret-value")
	out, _, err := captureOutput(t, func() error { return runStatus([]string{"--config", cfg, "--date", "2026-10-24"}) })
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Environment timesheet.EnvCheckResult `json:"environment"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.Environment.Missing, ",") != "SYNTHETIC_ABSENT_TOKEN" ||
		strings.Join(got.Environment.Present, ",") != "SYNTHETIC_PRESENT_TOKEN" {
		t.Fatalf("environment = %+v\n%s", got.Environment, out)
	}
	if strings.Contains(string(out), "synthetic-secret-value") || strings.Contains(string(out), "protocol_schema") {
		t.Fatalf("status leaked a value or still names the retired protocol schema:\n%s", out)
	}
}
