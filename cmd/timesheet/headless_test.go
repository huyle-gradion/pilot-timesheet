package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func writeTestConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	body := `{
  "timezone": "Asia/Ho_Chi_Minh",
  "state_path": "` + filepath.Join(dir, "state.json") + `"
}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// Regression: status --date with no recorded entries file must report zero
// entries, not dereference the missing file (previously a nil panic).
func TestRunStatusDateWithoutEntriesFile(t *testing.T) {
	cfg := writeTestConfig(t)
	if err := runStatus([]string{"--config", cfg, "--date", "2026-10-24"}); err != nil {
		t.Fatalf("status with no entries file failed: %v", err)
	}
}

func TestRunRecordRejectsDateMismatch(t *testing.T) {
	cfg := writeTestConfig(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "reviewed.json")
	body := `{"date":"2026-10-20","blocks":[{"block_id":"b","description":"x"}]}`
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runRecord([]string{"--config", cfg, "--date", "2026-10-24", "--file", file})
	if err == nil {
		t.Fatal("expected a date mismatch error")
	}
}

// --- DESC-ONLY-1: record --draft + --descriptions (issue #152) ---------------

func writeRecordInputs(t *testing.T, dir string) (draftPath, descPath string) {
	t.Helper()
	draftPath = filepath.Join(dir, "draft.json")
	descPath = filepath.Join(dir, "descriptions.json")
	draft := `{
  "date": "2026-10-26",
  "timezone": "Asia/Ho_Chi_Minh",
  "app_version": "test",
  "blocks": [
    {
      "block_id": "meeting-09:00-10:00",
      "kind": "meeting",
      "start": "09:00",
      "end": "10:00",
      "title": "Standup",
      "topic_candidates": ["Calendar filtering"],
      "prs": ["#12 acme/api"],
      "prs_inline": "PRs: #12 acme/api"
    },
    {
      "block_id": "evidence-20:00-21:00",
      "kind": "evidence",
      "start": "20:00",
      "end": "21:00",
      "topic_candidates": ["Evening research"]
    }
  ]
}`
	desc := `{
  "notes": "quiet day",
  "descriptions": {
    "meeting-09:00-10:00": "Standup and calendar filtering",
    "evidence-20:00-21:00": "Evening research notes"
  }
}`
	if err := os.WriteFile(draftPath, []byte(draft), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(descPath, []byte(desc), 0o600); err != nil {
		t.Fatal(err)
	}
	return draftPath, descPath
}

func TestRunRecordComposesFromDraftAndDescriptions(t *testing.T) {
	cfg := writeTestConfig(t)
	dir := t.TempDir()
	draftPath, descPath := writeRecordInputs(t, dir)
	if err := runRecord([]string{
		"--config", cfg, "--date", "2026-10-26",
		"--draft", draftPath, "--descriptions", descPath,
	}); err != nil {
		t.Fatalf("record --draft --descriptions: %v", err)
	}
	// state.json sits next to the config's state_path; read the entries back
	// through the documented path derivation.
	statePath := filepath.Join(filepath.Dir(cfg), "state.json")
	f, err := timesheet.ReadEntries(statePath, "2026-10-26")
	if err != nil || f == nil || len(f.Entries) != 2 {
		t.Fatalf("read entries: %+v %v", f, err)
	}
	byID := map[string]string{}
	for _, e := range f.Entries {
		byID[e.BlockID] = e.Description
	}
	if byID["meeting-09:00-10:00"] != "Standup and calendar filtering PRs: #12 acme/api" {
		t.Fatalf("meeting description = %q, want composed with appended prs_inline", byID["meeting-09:00-10:00"])
	}
	if byID["evidence-20:00-21:00"] != "Evening research notes" {
		t.Fatalf("evidence description = %q", byID["evidence-20:00-21:00"])
	}
}

func TestRunRecordFlagCombinations(t *testing.T) {
	cfg := writeTestConfig(t)
	dir := t.TempDir()
	draftPath, descPath := writeRecordInputs(t, dir)
	file := filepath.Join(dir, "reviewed.json")
	if err := os.WriteFile(file, []byte(`{"date":"2026-10-26","blocks":[{"block_id":"b","description":"x"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "both modes",
			args: []string{"--config", cfg, "--date", "2026-10-26", "--file", file, "--draft", draftPath, "--descriptions", descPath},
			want: "not both input modes",
		},
		{
			name: "draft without descriptions",
			args: []string{"--config", cfg, "--date", "2026-10-26", "--draft", draftPath},
			want: "both --draft and --descriptions",
		},
		{
			name: "descriptions without draft",
			args: []string{"--config", cfg, "--date", "2026-10-26", "--descriptions", descPath},
			want: "both --draft and --descriptions",
		},
		{
			name: "neither mode",
			args: []string{"--config", cfg, "--date", "2026-10-26"},
			want: "either --file or both --draft and --descriptions",
		},
	}
	for _, tc := range cases {
		err := runRecord(tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestRunRecordComposeValidationErrors(t *testing.T) {
	cfg := writeTestConfig(t)
	dir := t.TempDir()
	draftPath, _ := writeRecordInputs(t, dir)

	// Missing description names the offending block_id.
	missing := filepath.Join(dir, "missing.json")
	if err := os.WriteFile(missing, []byte(`{"descriptions": {"evidence-20:00-21:00": "Evening research notes"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runRecord([]string{"--config", cfg, "--date", "2026-10-26", "--draft", draftPath, "--descriptions", missing})
	if err == nil || !strings.Contains(err.Error(), "meeting-09:00-10:00") {
		t.Fatalf("missing description: err = %v, want the named block_id", err)
	}

	// Unknown block_id is rejected.
	unknown := filepath.Join(dir, "unknown.json")
	if err := os.WriteFile(unknown, []byte(`{"descriptions": {"meeting-09:00-10:00": "x", "evidence-20:00-21:00": "y", "nope": "z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err = runRecord([]string{"--config", cfg, "--date", "2026-10-26", "--draft", draftPath, "--descriptions", unknown})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("unknown block: err = %v, want the unknown id", err)
	}

	// notes must be a JSON string, never an array.
	notesArray := filepath.Join(dir, "notes-array.json")
	if err := os.WriteFile(notesArray, []byte(`{"notes": ["a"], "descriptions": {"meeting-09:00-10:00": "x", "evidence-20:00-21:00": "y"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err = runRecord([]string{"--config", cfg, "--date", "2026-10-26", "--draft", draftPath, "--descriptions", notesArray})
	if err == nil || !strings.Contains(err.Error(), `"notes" must be one JSON string`) {
		t.Fatalf("array notes: err = %v, want the string-only validation error", err)
	}
}
