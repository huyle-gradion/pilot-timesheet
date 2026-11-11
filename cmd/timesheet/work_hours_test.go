package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func writeWorkHoursRunConfig(t *testing.T, workHours string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	body := `{
  "timezone": "Asia/Ho_Chi_Minh",
  "state_path": "` + filepath.Join(dir, "state.json") + `",
  "projects": [{"id": "WORK", "name": "Work", "default_classification": "DEV"}],
  "sources": {"enabled": true, "calendar": {"enabled": true, "user": "me@example.com"}}`
	if workHours != "" {
		body += `,
  "work_hours": ` + workHours
	}
	body += "\n}\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestDraftAnnotatesOutsideWorkHours(t *testing.T) {
	cfg := writeWorkHoursRunConfig(t, `{"days": ["mon", "tue", "wed", "thu", "fri"], "windows": [{"start": "13:00", "end": "18:00"}]}`)
	srv, _ := startFeed(t)
	envFile := writeRunEnvFile(t, srv.URL)

	out, _, err := captureOutput(t, func() error {
		return runDraft([]string{"--config", cfg, "--env-file", envFile, "--date", "2026-10-20"})
	})
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	var draft timesheet.Draft
	if err := json.Unmarshal(out, &draft); err != nil {
		t.Fatalf("draft JSON: %v", err)
	}
	if len(draft.Blocks) != 1 {
		t.Fatalf("blocks = %d, want the one synthetic meeting: %s", len(draft.Blocks), out)
	}
	b := draft.Blocks[0]
	if !b.OutsideWorkHours || b.WorkHoursReason != "before_window" {
		t.Fatalf("block = %+v, want outside_work_hours with before_window", b)
	}
}

func TestDraftWithoutWorkHoursUnannotated(t *testing.T) {
	cfg := writeWorkHoursRunConfig(t, "")
	srv, _ := startFeed(t)
	envFile := writeRunEnvFile(t, srv.URL)

	out, _, err := captureOutput(t, func() error {
		return runDraft([]string{"--config", cfg, "--env-file", envFile, "--date", "2026-10-20"})
	})
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if bytes.Contains(out, []byte("outside_work_hours")) || bytes.Contains(out, []byte("work_hours_reason")) {
		t.Fatalf("no-policy draft carries annotation fields: %s", out)
	}
	var draft timesheet.Draft
	if err := json.Unmarshal(out, &draft); err != nil {
		t.Fatalf("draft JSON: %v", err)
	}
	if len(draft.Blocks) == 0 || draft.Blocks[0].OutsideWorkHours || draft.Blocks[0].WorkHoursReason != "" {
		t.Fatalf("no-policy draft annotated a block: %+v", draft.Blocks)
	}
}

func TestParseHeadlessFlagsAllowsOmittedDateForDailyDriver(t *testing.T) {
	for _, name := range []string{"collect", "draft", "run", "status"} {
		if _, err := parseHeadlessFlags(name, []string{"--config", "/tmp/config.json"}); err != nil {
			t.Fatalf("%s without --date: %v", name, err)
		}
		if _, err := parseHeadlessFlags(name, []string{"--config", "/tmp/config.json", "--date", "26-10-2026"}); err == nil {
			t.Fatalf("%s must still reject a malformed --date", name)
		}
		if _, err := parseHeadlessFlags(name, []string{"--config", "/tmp/config.json", "--date", "2026-10-26"}); err != nil {
			t.Fatalf("%s with a valid --date: %v", name, err)
		}
	}
}
