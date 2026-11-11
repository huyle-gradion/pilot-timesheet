package main

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

var fixedNow = time.Date(2026, 10, 26, 18, 0, 0, 0, time.UTC)

// pinClock replaces the package clock for one test. Tests that pin it must not
// run in parallel.
func pinClock(t *testing.T) {
	t.Helper()
	previous := clockNow
	clockNow = func() time.Time { return fixedNow }
	t.Cleanup(func() { clockNow = previous })
}

// writeSyntheticConfig writes a configuration with no enabled sources, so a
// command under test can never reach a calendar feed, a GitHub endpoint or an
// inherited credential. It returns the config path and the state path the
// entries files sit beside.
func writeSyntheticConfig(t *testing.T, timezone string) (configPath, statePath string) {
	t.Helper()
	dir := t.TempDir()
	configPath = filepath.Join(dir, "config.json")
	statePath = filepath.Join(dir, "state.json")
	body := `{
  "timezone": "` + timezone + `",
  "state_path": "` + statePath + `",
  "projects": [{"id": "WORK", "name": "Work", "default_classification": "DEV"}]
}`
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, statePath
}

func writeReviewed(t *testing.T, dir, date, blockID, description string) string {
	t.Helper()
	body := `{"date":"` + date + `","blocks":[{"block_id":"` + blockID + `","description":"` + description + `"}]}`
	path := filepath.Join(dir, "reviewed-"+date+"-"+blockID+".json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunWithoutDateResolvesTodayInConfiguredTimezone(t *testing.T) {
	pinClock(t)
	cases := []struct {
		timezone string
		want     string
	}{
		{timezone: "Asia/Ho_Chi_Minh", want: "2026-10-27"},
		{timezone: "UTC", want: "2026-10-26"},
	}
	for _, tc := range cases {
		t.Run(tc.timezone, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			cfg, _ := writeSyntheticConfig(t, tc.timezone)

			out, stderr, err := captureOutput(t, func() error {
				return runRun([]string{"--config", cfg})
			})
			if err != nil {
				t.Fatalf("run without --date: %v", err)
			}
			var draft timesheet.Draft
			if err := json.Unmarshal(out, &draft); err != nil {
				t.Fatalf("run stdout is not the draft JSON: %v\n%s", err, out)
			}
			if draft.Date != tc.want {
				t.Fatalf("draft date = %q, want %q for timezone %s at %s", draft.Date, tc.want, tc.timezone, fixedNow)
			}
			artifacts := filepath.Join(testArtifactDir(t, cfg, tc.want), "draft.json")
			if !strings.Contains(string(stderr), artifacts) {
				t.Fatalf("stderr %q does not name the resolved artifacts path %q", stderr, artifacts)
			}
			if _, err := os.Stat(artifacts); err != nil {
				t.Fatalf("resolved artifacts file: %v", err)
			}
		})
	}
}

func TestDraftAndCollectWithoutDatePrintResolvedDay(t *testing.T) {
	pinClock(t)
	cfg, _ := writeSyntheticConfig(t, "Asia/Ho_Chi_Minh")

	draftOut, _, err := captureOutput(t, func() error {
		return runDraft([]string{"--config", cfg})
	})
	if err != nil {
		t.Fatalf("draft without --date: %v", err)
	}
	var draft timesheet.Draft
	if err := json.Unmarshal(draftOut, &draft); err != nil {
		t.Fatalf("draft stdout is not the draft JSON: %v", err)
	}
	if draft.Date != "2026-10-27" {
		t.Fatalf("draft date = %q, want 2026-10-27 in Asia/Ho_Chi_Minh at %s", draft.Date, fixedNow)
	}

	collectOut, _, err := captureOutput(t, func() error {
		return runCollect([]string{"--config", cfg})
	})
	if err != nil {
		t.Fatalf("collect without --date: %v", err)
	}
	var collect timesheet.CollectResult
	if err := json.Unmarshal(collectOut, &collect); err != nil {
		t.Fatalf("collect stdout is not the collect JSON: %v", err)
	}
	if collect.Date != "2026-10-27" {
		t.Fatalf("collect date = %q, want 2026-10-27 in Asia/Ho_Chi_Minh at %s", collect.Date, fixedNow)
	}
	if !collect.NoSources {
		t.Fatalf("synthetic config reached a source: %+v", collect.Sources)
	}
}

func TestRecordWithoutDateUsesResolvedDay(t *testing.T) {
	pinClock(t)
	cfg, statePath := writeSyntheticConfig(t, "Asia/Ho_Chi_Minh")
	dir := t.TempDir()

	file := writeReviewed(t, dir, "2026-10-27", "b1", "morning work")
	out, _, err := captureOutput(t, func() error {
		return runRecord([]string{"--config", cfg, "--file", file})
	})
	if err != nil {
		t.Fatalf("record without --date: %v", err)
	}
	var receipt timesheet.RecordReceipt
	if err := json.Unmarshal(out, &receipt); err != nil {
		t.Fatalf("record stdout is not the receipt JSON: %v", err)
	}
	if receipt.Date != "2026-10-27" {
		t.Fatalf("receipt date = %q, want the resolved day 2026-10-27", receipt.Date)
	}
	entries, err := timesheet.ReadEntries(statePath, "2026-10-27")
	if err != nil || entries == nil || len(entries.Entries) != 1 {
		t.Fatalf("resolved day's entries = %+v, %v", entries, err)
	}

	other := writeReviewed(t, dir, "2026-10-26", "b2", "yesterday work")
	_, _, err = captureOutput(t, func() error {
		return runRecord([]string{"--config", cfg, "--file", other})
	})
	if err == nil || !strings.Contains(err.Error(), "2026-10-27") {
		t.Fatalf("record of a wrong-day draft: err = %v, want an error naming the resolved day 2026-10-27", err)
	}
}

func TestStatusWithoutDateReportsResolvedDay(t *testing.T) {
	pinClock(t)
	cfg, _ := writeSyntheticConfig(t, "Asia/Ho_Chi_Minh")
	dir := t.TempDir()

	file := writeReviewed(t, dir, "2026-10-27", "b1", "morning work")
	if _, _, err := captureOutput(t, func() error {
		return runRecord([]string{"--config", cfg, "--file", file})
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	type statusSummary struct {
		Status   timesheet.StatusResult `json:"status"`
		Date     string                 `json:"date"`
		Entries  int                    `json:"entries"`
		Recorded []string               `json:"recorded"`
	}
	out, _, err := captureOutput(t, func() error {
		return runStatus([]string{"--config", cfg})
	})
	if err != nil {
		t.Fatalf("status without --date: %v", err)
	}
	var summary statusSummary
	if err := json.Unmarshal(out, &summary); err != nil {
		t.Fatalf("status stdout is not the summary JSON: %v\n%s", err, out)
	}
	if summary.Date != "2026-10-27" {
		t.Fatalf("status date = %q, want the resolved day 2026-10-27", summary.Date)
	}
	if summary.Entries != 1 {
		t.Fatalf("status entries = %d, want 1", summary.Entries)
	}
	if len(summary.Recorded) != 1 || !strings.Contains(summary.Recorded[0], "morning work") {
		t.Fatalf("status recorded = %v, want the day's entry", summary.Recorded)
	}
	found := false
	for _, day := range summary.Status.Days {
		if day.Date == "2026-10-27" {
			found = true
		}
	}
	if !found {
		t.Fatalf("status does not carry the opened day: %+v", summary.Status.Days)
	}

	datedOut, _, err := captureOutput(t, func() error {
		return runStatus([]string{"--config", cfg, "--date", "2026-10-26"})
	})
	if err != nil {
		t.Fatalf("status --date: %v", err)
	}
	var dated statusSummary
	if err := json.Unmarshal(datedOut, &dated); err != nil {
		t.Fatalf("dated status stdout is not the summary JSON: %v", err)
	}
	if dated.Date != "2026-10-26" || dated.Entries != 0 {
		t.Fatalf("status --date = %+v, want date 2026-10-26 with no entries", dated)
	}
}

func TestDayScopedCommandWithoutDateStillRejects(t *testing.T) {
	cfg, _ := writeSyntheticConfig(t, "Asia/Ho_Chi_Minh")
	err := runStage([]string{"--config", cfg})
	if err == nil || !strings.Contains(err.Error(), "stage requires --date") {
		t.Fatalf("stage without --date: err = %v, want %q", err, "stage requires --date")
	}
}

func TestSuppliedDateMustParseAsADate(t *testing.T) {
	pinClock(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	cfg, statePath := writeSyntheticConfig(t, "Asia/Ho_Chi_Minh")
	dir := t.TempDir()
	reviewed := writeReviewed(t, dir, "2026-10-27", "b1", "work")

	commands := []struct {
		name string
		run  func([]string) error
		args []string
	}{
		{name: "collect", run: runCollect, args: []string{"--config", cfg}},
		{name: "draft", run: runDraft, args: []string{"--config", cfg}},
		{name: "run", run: runRun, args: []string{"--config", cfg}},
		{name: "record", run: runRecord, args: []string{"--config", cfg, "--file", reviewed}},
		{name: "status", run: runStatus, args: []string{"--config", cfg}},
	}
	values := []struct{ label, value string }{
		{label: "whitespace", value: " "},
		{label: "empty", value: ""},
	}
	for _, command := range commands {
		for _, value := range values {
			t.Run(command.name+"/"+value.label, func(t *testing.T) {
				args := append(append([]string{}, command.args...), "--date", value.value)
				out, _, err := captureOutput(t, func() error {
					return command.run(args)
				})
				want := command.name + ": --date must be YYYY-MM-DD"
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("%s --date %q: err = %v, want %q", command.name, value.value, err, want)
				}
				if len(out) != 0 {
					t.Fatalf("%s --date %q printed output, want none: %s", command.name, value.value, out)
				}
			})
		}
	}

	t.Run("stage/whitespace", func(t *testing.T) {
		err := runStage([]string{"--config", cfg, "--date", " "})
		if err == nil || !strings.Contains(err.Error(), "stage: --date must be YYYY-MM-DD") {
			t.Fatalf("stage --date %q: err = %v, want %q", " ", err, "stage: --date must be YYYY-MM-DD")
		}
	})

	if _, err := os.Stat(filepath.Join(tmp, "timesheet")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a rejected --date wrote an artifact directory: %v", err)
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a rejected --date opened the store: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(statePath), "entries")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a rejected --date wrote entries: %v", err)
	}
}

func TestDateFlagHelpNamesOmission(t *testing.T) {
	cfg, _ := writeSyntheticConfig(t, "Asia/Ho_Chi_Minh")
	cases := []struct {
		name string
		run  func([]string) error
	}{
		{name: "collect", run: runCollect},
		{name: "draft", run: runDraft},
		{name: "run", run: runRun},
		{name: "record", run: runRecord},
		{name: "status", run: runStatus},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, err := captureOutput(t, func() error {
				return tc.run([]string{"--config", cfg, "--help"})
			})
			if !errors.Is(err, flag.ErrHelp) {
				t.Fatalf("--help should surface flag.ErrHelp, got %v", err)
			}
			help := string(stderr)
			if !strings.Contains(help, "-date string") {
				t.Fatalf("--help does not describe --date:\n%s", help)
			}
			if strings.Contains(help, "day to operate on, YYYY-MM-DD (required)") {
				t.Fatalf("--help calls an optional --date required:\n%s", help)
			}
			if !strings.Contains(help, "day to operate on, YYYY-MM-DD (optional; omitted means today in the configured timezone)") {
				t.Fatalf("--help does not say what omission means:\n%s", help)
			}
		})
	}
}
