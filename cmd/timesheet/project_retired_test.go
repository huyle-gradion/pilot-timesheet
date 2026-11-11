package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func TestRetiredProjectFlagFailsWithOneLine(t *testing.T) {
	for _, command := range []string{"init", "log", "next", "tui", "judge", "stage", "classify", "save"} {
		for _, args := range [][]string{
			{"--date", testDate, "--project", "WORK"},
			{"--project=WORK"},
			{"-project", "WORK"},
		} {
			err := retiredFlagError(command, args)
			if err == nil {
				t.Fatalf("%s %v: no error, want the retired-flag line", command, args)
			}
			msg := err.Error()
			if strings.Contains(msg, "\n") || !strings.Contains(msg, "--project") || !strings.Contains(msg, "removed") {
				t.Fatalf("%s %v: %q, want one line naming the removed --project", command, args, msg)
			}
		}
	}
	if err := retiredFlagError("stage", []string{"--date", testDate}); err != nil {
		t.Fatalf("stage without --project = %v", err)
	}
	if err := retiredFlagError("next", []string{"--do", "--project", "WORK"}); err == nil {
		t.Fatal("next --do --project: no error")
	}
	for _, args := range [][]string{
		{"--config", "--project"},
		{"--env-file", "-project", "--date", testDate},
		{"--date", testDate, "--", "--project"},
	} {
		if err := retiredFlagError("log", args); err != nil {
			t.Fatalf("log %v = %v, want no retired flag", args, err)
		}
	}
	for _, command := range []string{"record", "status", "migrate", "eval", "submit", "daily"} {
		if err := retiredFlagError(command, []string{"--project", "WORK"}); err != nil {
			t.Fatalf("%s: %v, want the command's own parser to answer", command, err)
		}
	}
}

func TestRetiredConfigKeysAreRemovedOnceOrIgnoredWithOneLine(t *testing.T) {
	body := `{
  "timezone": "Asia/Ho_Chi_Minh",
  "state_path": "/tmp/state.json",
  "projects": [{"id": "WORK", "name": "Work"}]
}
`
	run := func(t *testing.T, mode os.FileMode) (string, []string) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		var lines []string
		old := timesheet.RetiredConfigHook
		timesheet.RetiredConfigHook = retiredConfigHook(func(line string) { lines = append(lines, line) })
		t.Cleanup(func() { timesheet.RetiredConfigHook = old })
		for i := 0; i < 3; i++ {
			if _, err := timesheet.LoadConfig(path); err != nil {
				t.Fatalf("load %d: %v", i, err)
			}
		}
		return path, lines
	}
	t.Run("writable: removed with a backup", func(t *testing.T) {
		path, lines := run(t, 0o600)
		if len(lines) != 1 || !strings.Contains(lines[0], "projects") || !strings.Contains(lines[0], path+".bak") {
			t.Fatalf("lines = %q, want one line naming the removed key and the backup", lines)
		}
		if got, _ := os.ReadFile(path); strings.Contains(string(got), "projects") {
			t.Fatalf("config still lists projects:\n%s", got)
		}
	})
	t.Run("read-only: ignored, file untouched", func(t *testing.T) {
		path, lines := run(t, 0o400)
		if len(lines) != 1 || !strings.Contains(lines[0], "ignored") || !strings.Contains(lines[0], "by hand") {
			t.Fatalf("lines = %q, want one line saying the keys are ignored", lines)
		}
		if got, _ := os.ReadFile(path); string(got) != body {
			t.Fatal("a read-only config changed")
		}
	})
}

func writeNoProjectConfig(t *testing.T, endpoint string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	body := `{
  "timezone": "Asia/Ho_Chi_Minh",
  "state_path": "` + filepath.Join(dir, "state.json") + `",
  "gradion": {"enabled": true, "endpoint": "` + endpoint + `", "token_env": "SYNTHETIC_GRADION_TOKEN"}
}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestNoProjectConfigStagesClassifiesSavesAndPlans(t *testing.T) {
	stub := &stubRemote{entries: map[string][]map[string]any{testDate: {}}}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	t.Setenv("SYNTHETIC_GRADION_TOKEN", "synthetic-token")
	cfg := writeNoProjectConfig(t, server.URL)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{timedBlock("work-1", "09:00", "10:00", "Build the bridge")})
	seedCatalog(t, cfg, "cat-1", timesheet.ClassificationOption{ID: "cls-dev", Name: "Development", Code: "DEV", ActivityType: "engineering"})
	stage := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	if len(stage.Staged) != 1 {
		t.Fatalf("stage = %+v", stage)
	}
	choices := writeChoicesFile(t, cfg, `{"selections": {"`+stage.Staged[0].IntervalID+`": {"catalog_id": "cat-1", "classification_id": "cls-dev"}}}`)
	runClassifyCapture(t, []string{"--config", cfg, "--date", testDate, "--file", choices})
	if err := runSave([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("save: %v", err)
	}
	confirmed := readDayConfirmed(t, cfg, testDate)
	if len(confirmed) != 1 || confirmed[0].Project != "" || confirmed[0].RemoteClassificationID != "cls-dev" {
		t.Fatalf("confirmed = %+v, want one classified interval with no project", confirmed)
	}
	preview := runPlanCapture(t, []string{"--config", cfg, "--date", testDate})
	if preview.State != "ready" || len(preview.Operations) != 1 {
		t.Fatalf("plan = %+v, want one ready create", preview)
	}
}

func TestLegacyProjectDayStaysSubmittedUnderANoProjectConfig(t *testing.T) {
	stub := &stubRemote{entries: map[string][]map[string]any{testDate: {
		remoteWeekEntry("r-new-1", "08:00", "12:00", "cls-dev", "Implement synthetic design"),
		remoteWeekEntry("r-new-2", "12:00", "13:00", "cls-dev", "Implement synthetic design"),
		remoteWeekEntry("r-new-3", "14:00", "15:00", "cls-dev", "Review the deck"),
	}}}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	t.Setenv("SYNTHETIC_GRADION_TOKEN", "synthetic-token")
	cfg := writeNoProjectConfig(t, server.URL)
	fixture, err := os.ReadFile(filepath.Join("..", "..", "internal", "timesheet", "testdata", "state_schema3_with_projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePathFor(cfg), fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	preview := runPlanCapture(t, []string{"--config", cfg, "--date", testDate})
	if len(preview.Blockers) != 0 || len(preview.Operations) != 0 {
		t.Fatalf("plan = %+v, want the submitted day in sync with nothing to write", preview)
	}
	for _, iv := range readDayConfirmed(t, cfg, testDate) {
		if iv.Project != "WORK" || iv.RemoteClassificationID != "cls-dev" {
			t.Fatalf("interval = %+v, want its legacy project and classification kept", iv)
		}
	}
}
