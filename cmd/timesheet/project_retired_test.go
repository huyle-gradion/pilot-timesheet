package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// Issue #185 / ADR 0012: the project is retired from the command surface.

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
	// A value that merely follows another flag is not the retired flag.
	if err := retiredFlagError("record", []string{"--descriptions", "--project-notes.json"}); err != nil {
		t.Fatalf("an unrelated value = %v", err)
	}
}

// writeNoProjectConfig is a configuration with no project at all, the shape
// init writes after ADR 0012.
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

// TestNoProjectConfigStagesClassifiesSavesAndPlans runs the deterministic
// verbs on a configuration without a project: nothing asks for one.
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

// TestLegacyProjectDayStaysSubmittedUnderANoProjectConfig opens the real
// schema-3 store (captured from the previous build after a submission) under a
// configuration without its project. The submitted day stays current: the
// plan has nothing to write, and the legacy labels survive the upgrade.
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
