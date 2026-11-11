package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

const testDate = "2026-10-26"

func statePathFor(cfg string) string {
	return filepath.Join(filepath.Dir(cfg), "state.json")
}

func writeTestConfigWithGithub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	body := `{
  "timezone": "Asia/Ho_Chi_Minh",
  "state_path": "` + filepath.Join(dir, "state.json") + `",
  "sources": {
    "enabled": true,
    "github": {"enabled": true, "user": "huyle-gradion", "repositories": ["acme/api"]}
  }
}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestStageSkipsUnanchoredEntriesInBlockIDOrder(t *testing.T) {
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		{DraftBlock: timesheet.DraftBlock{BlockID: "day-b", Kind: "day"}, Description: "Second unanchored"},
		timedBlock("work-1", "09:00", "10:00", "Build the bridge"),
		{DraftBlock: timesheet.DraftBlock{BlockID: "day-a", Kind: "day"}, Description: "First unanchored"},
	})
	receipt := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	if len(receipt.Skipped) != 2 || receipt.Skipped[0].BlockID != "day-a" || receipt.Skipped[1].BlockID != "day-b" {
		t.Fatalf("skipped = %+v, want day-a then day-b", receipt.Skipped)
	}
}

func seedEntries(t *testing.T, cfg, date string, blocks []timesheet.ReviewedBlock) {
	t.Helper()
	_, err := timesheet.RecordEntries(statePathFor(cfg), timesheet.AppVersion,
		timesheet.ReviewedDraft{Date: date, Blocks: blocks}, false, time.Now())
	if err != nil {
		t.Fatalf("seed entries: %v", err)
	}
}

func writeEntriesFile(t *testing.T, cfg, date string, entries []timesheet.EntryRecord) {
	t.Helper()
	f := timesheet.EntriesFile{
		Date: date, AppVersion: "test",
		UpdatedAt: time.Now().UTC().Format(time.RFC3339), Entries: entries,
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(timesheet.EntriesPath(statePathFor(cfg), date), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func seedEvidence(t *testing.T, cfg, date string, ev *timesheet.DayEvidence) {
	t.Helper()
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatalf("seed evidence: %v", err)
	}
	defer func() { _ = store.Close() }()
	if err := store.CommitEvidence(date, ev); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}
}

func readDayConfirmed(t *testing.T, cfg, date string) []timesheet.Interval {
	t.Helper()
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatalf("read confirmed: %v", err)
	}
	defer func() { _ = store.Close() }()
	day, ok := store.State().Days[date]
	if !ok {
		t.Fatalf("read confirmed: day %s is not in the store", date)
	}
	return day.Confirmed
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func runStageCapture(t *testing.T, args []string) StageReceipt {
	t.Helper()
	var receipt StageReceipt
	out := captureStdout(t, func() {
		if err := runStage(args); err != nil {
			t.Fatalf("stage: %v", err)
		}
	})
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		t.Fatalf("stage receipt is not valid JSON: %v\n%s", err, out)
	}
	return receipt
}

func timedBlock(id, start, end, desc string) timesheet.ReviewedBlock {
	return timesheet.ReviewedBlock{
		DraftBlock:  timesheet.DraftBlock{BlockID: id, Kind: "work", Start: start, End: end},
		Description: desc,
	}
}

func TestStageRecordedDayMix(t *testing.T) {
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "Build the bridge PRs: #12 acme/api"),
		timedBlock("meeting-1", "11:00", "12:00", "Standup notes"),
		{DraftBlock: timesheet.DraftBlock{BlockID: "day-1", Kind: "day"}, Description: "Whole-day entry with no anchors"},
	})
	receipt := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	if len(receipt.Staged) != 2 {
		t.Fatalf("staged = %+v, want 2 intervals", receipt.Staged)
	}
	if len(receipt.Skipped) != 1 || receipt.Skipped[0].BlockID != "day-1" {
		t.Fatalf("skipped = %+v, want exactly day-1", receipt.Skipped)
	}
	if !strings.Contains(receipt.Skipped[0].Reason, "never invented") {
		t.Fatalf("skip reason = %q, want the honest never-invented wording", receipt.Skipped[0].Reason)
	}
	if receipt.StagedIntervals != 2 {
		t.Fatalf("staged_intervals = %d, want 2", receipt.StagedIntervals)
	}
	sidecar, err := timesheet.ReadStageMap(statePathFor(cfg), testDate)
	if err != nil || len(sidecar) != 2 {
		t.Fatalf("sidecar = %v, %v; want 2 mappings", sidecar, err)
	}
	for _, ref := range receipt.Staged {
		ids := sidecar[ref.BlockID]
		if len(ids) != 1 || ids[0] != ref.IntervalID || ref.IntervalID == "" {
			t.Fatalf("sidecar mapping for %s = %v, want the staged interval id", ref.BlockID, ids)
		}
	}
}

func TestStageWithoutEntriesFileFails(t *testing.T) {
	cfg := writeTestConfig(t)
	err := runStage([]string{"--config", cfg, "--date", testDate})
	if err == nil || !strings.Contains(err.Error(), "no recorded entries file") {
		t.Fatalf("stage without entries = %v, want the record-first error", err)
	}
}

func TestRestageEditsAndRemovesInPlace(t *testing.T) {
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "First block"),
		timedBlock("work-2", "11:00", "12:00", "Second block"),
	})
	parsed, err := timesheet.LoadConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h, err := openHeadless(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	defer h.done()
	open := func() timesheet.DaySummary {
		res, err := h.call("open_day", timesheet.Request{Day: testDate})
		if err != nil {
			t.Fatal(err)
		}
		return res.(timesheet.DaySummary)
	}

	first, err := applyStage(h, parsed, "stage", testDate, open())
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Staged) != 2 || first.StagedIntervals != 2 {
		t.Fatalf("first stage = %+v, want 2 staged", first)
	}
	ids := map[string]string{}
	for _, ref := range first.Staged {
		ids[ref.BlockID] = ref.IntervalID
	}

	writeEntriesFile(t, cfg, testDate, []timesheet.EntryRecord{
		{BlockID: "work-1", Date: testDate, Start: "09:30", End: "10:30", Description: "First block", Kind: "work"},
		{BlockID: "work-2", Date: testDate, Start: "11:00", End: "12:00", Description: "Second block", Kind: "work"},
	})
	second, err := applyStage(h, parsed, "stage", testDate, open())
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Updated) != 1 || second.Updated[0].BlockID != "work-1" {
		t.Fatalf("second stage updated = %+v, want work-1 edited in place", second.Updated)
	}
	if second.Updated[0].IntervalID != ids["work-1"] {
		t.Fatalf("edited interval id = %q, want stable %q", second.Updated[0].IntervalID, ids["work-1"])
	}
	if second.StagedIntervals != 2 {
		t.Fatalf("staged_intervals after edit = %d, want a stable 2", second.StagedIntervals)
	}

	writeEntriesFile(t, cfg, testDate, []timesheet.EntryRecord{
		{BlockID: "work-2", Date: testDate, Start: "11:00", End: "12:00", Description: "Second block", Kind: "work"},
	})
	third, err := applyStage(h, parsed, "stage", testDate, open())
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Removed) != 1 || third.Removed[0].BlockID != "work-1" {
		t.Fatalf("removed = %+v, want work-1's interval removed", third.Removed)
	}
	if third.StagedIntervals != 1 {
		t.Fatalf("staged_intervals after removal = %d, want 1", third.StagedIntervals)
	}
	sidecar, err := timesheet.ReadStageMap(statePathFor(cfg), testDate)
	if err != nil || len(sidecar) != 1 || len(sidecar["work-1"]) != 0 {
		t.Fatalf("sidecar after removal = %v, %v; want only work-2", sidecar, err)
	}
}

func TestStageCLIRestageKeepsCountStable(t *testing.T) {
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "First block"),
		timedBlock("work-2", "11:00", "12:00", "Second block"),
	})
	first := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	writeEntriesFile(t, cfg, testDate, []timesheet.EntryRecord{
		{BlockID: "work-1", Date: testDate, Start: "09:30", End: "10:30", Description: "First block", Kind: "work"},
		{BlockID: "work-2", Date: testDate, Start: "11:00", End: "12:00", Description: "Second block", Kind: "work"},
	})
	second := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	if second.StagedIntervals != first.StagedIntervals {
		t.Fatalf("staged_intervals %d -> %d, want stable", first.StagedIntervals, second.StagedIntervals)
	}
	for _, ref := range second.Staged {
		found := false
		for _, prev := range first.Staged {
			if ref.BlockID == prev.BlockID && ref.IntervalID == prev.IntervalID {
				found = true
			}
		}
		if !found {
			t.Fatalf("block %s restaged as %q, want a stable interval id", ref.BlockID, ref.IntervalID)
		}
	}
}

func TestStageRefusedAfterFinalize(t *testing.T) {
	cfg := writeTestConfigWithGithub(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "One block"),
	})
	seedEvidence(t, cfg, testDate, &timesheet.DayEvidence{
		Attempts: []timesheet.SourceAttempt{
			{Source: timesheet.SourceGithub, CollectionID: "col-gate-1", AttemptedAt: "2026-10-26T10:00:00Z", Status: timesheet.StatusIncomplete},
		},
	})
	runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	if err := runAcknowledge([]string{"--config", cfg, "--date", testDate, "--source", "github", "--collection-id", "col-gate-1"}); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	if err := runConfirm([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := runSave([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("save: %v", err)
	}
	for name, run := range map[string]func([]string) error{
		"stage":   runStage,
		"confirm": runConfirm,
		"save":    runSave,
	} {
		err := run([]string{"--config", cfg, "--date", testDate})
		if err == nil || !strings.Contains(err.Error(), "already has confirmed intervals") {
			t.Fatalf("%s after save = %v, want the finalized-day refusal", name, err)
		}
	}
	confirmed := readDayConfirmed(t, cfg, testDate)
	if len(confirmed) != 1 || confirmed[0].Description != "One block" {
		t.Fatalf("confirmed = %+v, want the single saved interval untouched", confirmed)
	}
}

func TestAcknowledgeCollectionIDFailClosed(t *testing.T) {
	cfg := writeTestConfigWithGithub(t)
	seedEvidence(t, cfg, testDate, &timesheet.DayEvidence{
		Attempts: []timesheet.SourceAttempt{
			{Source: timesheet.SourceGithub, CollectionID: "col-right", AttemptedAt: "2026-10-26T10:00:00Z", Status: timesheet.StatusIncomplete},
		},
	})
	err := runAcknowledge([]string{"--config", cfg, "--date", testDate, "--source", "github", "--collection-id", "col-wrong"})
	if err == nil || !strings.Contains(err.Error(), "stale_collection") || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("wrong collection_id = %v, want the verbatim stale_collection refusal", err)
	}
	out := captureStdout(t, func() {
		if err := runAcknowledge([]string{"--config", cfg, "--date", testDate, "--source", "github", "--collection-id", "col-right"}); err != nil {
			t.Fatalf("acknowledge with the collected id: %v", err)
		}
	})
	var receipt timesheet.AcknowledgedSource
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		t.Fatalf("acknowledge receipt: %v", err)
	}
	if !receipt.Acknowledged || receipt.CollectionID != "col-right" {
		t.Fatalf("receipt = %+v, want acknowledged col-right", receipt)
	}
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if !store.Evidence(testDate).Attempts[0].Acknowledged {
		t.Fatal("store evidence attempt is not acknowledged")
	}
}

func TestDecideAttendance(t *testing.T) {
	cfg := writeTestConfig(t)
	seedEvidence(t, cfg, testDate, &timesheet.DayEvidence{
		Attempts: []timesheet.SourceAttempt{
			{Source: timesheet.SourceCalendar, CollectionID: "col-cal-1", AttemptedAt: "2026-10-26T10:00:00Z", Status: timesheet.StatusComplete},
		},
		Meetings: []timesheet.ProposedMeeting{
			{Ref: "cal-m1", UID: "uid-1", Start: "2026-10-26T09:00:00+07:00"},
		},
	})
	err := runDecide([]string{"--config", cfg, "--date", testDate, "--meeting-id", "cal-m1", "--decision", "attended", "--collection-id", "col-old"})
	if err == nil || !strings.Contains(err.Error(), "stale_collection") {
		t.Fatalf("decide with stale id = %v, want the stale_collection refusal", err)
	}
	for _, decision := range []string{"attended", "skipped"} {
		out := captureStdout(t, func() {
			if err := runDecide([]string{"--config", cfg, "--date", testDate, "--meeting-id", "cal-m1", "--decision", decision, "--collection-id", "col-cal-1"}); err != nil {
				t.Fatalf("decide %s: %v", decision, err)
			}
		})
		var receipt timesheet.MeetingDecision
		if err := json.Unmarshal([]byte(out), &receipt); err != nil {
			t.Fatalf("decide receipt: %v", err)
		}
		if receipt.Decision != decision || receipt.MeetingID != "cal-m1" {
			t.Fatalf("receipt = %+v, want %s for cal-m1", receipt, decision)
		}
		store, err := timesheet.OpenStore(statePathFor(cfg))
		if err != nil {
			t.Fatal(err)
		}
		got := store.Evidence(testDate).Meetings[0].Decision
		_ = store.Close()
		if got != decision {
			t.Fatalf("stored decision = %q, want %q", got, decision)
		}
	}
}

func TestConfirmSourceGateVerbatimAndSaveDurable(t *testing.T) {
	cfg := writeTestConfigWithGithub(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "Bridge work PRs: #12 acme/api"),
	})
	seedEvidence(t, cfg, testDate, &timesheet.DayEvidence{
		Attempts: []timesheet.SourceAttempt{
			{Source: timesheet.SourceGithub, CollectionID: "col-gate-1", AttemptedAt: "2026-10-26T10:00:00Z", Status: timesheet.StatusIncomplete},
		},
	})
	err := runConfirm([]string{"--config", cfg, "--date", testDate})
	if err == nil || !strings.Contains(err.Error(), "source_unacknowledged") ||
		!strings.Contains(err.Error(), "must be reviewed and explicitly acknowledged before confirmation") {
		t.Fatalf("confirm without acknowledgment = %v, want the child's source-gate error verbatim", err)
	}
	if _, ok := readDayConfirmedIfAny(t, cfg, testDate); ok {
		t.Fatal("a refused confirm must not change the store")
	}
	if err := runAcknowledge([]string{"--config", cfg, "--date", testDate, "--source", "github", "--collection-id", "col-gate-1"}); err != nil {
		t.Fatalf("acknowledge: %v", err)
	}
	var confirmReceipt timesheet.ConfirmReceipt
	out := captureStdout(t, func() {
		if err := runConfirm([]string{"--config", cfg, "--date", testDate}); err != nil {
			t.Fatalf("confirm after acknowledgment: %v", err)
		}
	})
	if err := json.Unmarshal([]byte(out), &confirmReceipt); err != nil {
		t.Fatalf("confirm receipt: %v", err)
	}
	if confirmReceipt.Intervals != 1 || confirmReceipt.Durable {
		t.Fatalf("confirm receipt = %+v, want 1 in-memory interval", confirmReceipt)
	}
	var saveReceipt timesheet.SaveReceipt
	out = captureStdout(t, func() {
		if err := runSave([]string{"--config", cfg, "--date", testDate}); err != nil {
			t.Fatalf("save: %v", err)
		}
	})
	if err := json.Unmarshal([]byte(out), &saveReceipt); err != nil {
		t.Fatalf("save receipt: %v", err)
	}
	confirmed := readDayConfirmed(t, cfg, testDate)
	if len(confirmed) != 1 {
		t.Fatalf("durable confirmed = %+v, want 1 interval", confirmed)
	}
	iv := confirmed[0]
	if iv.Date != testDate || iv.Start != "09:00" || iv.End != "10:00" ||
		iv.Project != "" || iv.Description != "Bridge work PRs: #12 acme/api" {
		t.Fatalf("durable interval = %+v, want the staged entry carried into Confirmed", iv)
	}
}

func readDayConfirmedIfAny(t *testing.T, cfg, date string) ([]timesheet.Interval, bool) {
	t.Helper()
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	day, ok := store.State().Days[date]
	if !ok {
		return nil, false
	}
	return day.Confirmed, len(day.Confirmed) > 0
}

func TestFinalizeBlockedByStoreLock(t *testing.T) {
	cfg := writeTestConfig(t)
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	err = runStage([]string{"--config", cfg, "--date", testDate})
	if err == nil || !strings.Contains(err.Error(), "locked by another running process") {
		t.Fatalf("stage under a held lock = %v, want the store_locked error", err)
	}
}

func TestFinalizeFlagErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := writeTestConfig(t)
	cases := []struct {
		name string
		run  func([]string) error
		args []string
		want string
	}{
		{"stage missing config", runStage, []string{"--date", testDate}, "no configuration at"},
		{"stage bad date", runStage, []string{"--config", cfg, "--date", "2026-13-01"}, "must be YYYY-MM-DD"},
		{"stage positional", runStage, []string{"--config", cfg, "--date", testDate, "extra"}, "positional"},
		{"confirm missing date", runConfirm, []string{"--config", cfg}, "confirm requires --date"},
		{"save missing config", runSave, []string{"--date", testDate}, "no configuration at"},
		{"acknowledge bad source", runAcknowledge, []string{"--config", cfg, "--date", testDate, "--source", "slack", "--collection-id", "c"}, "--source must be github or calendar"},
		{"acknowledge missing collection", runAcknowledge, []string{"--config", cfg, "--date", testDate, "--source", "github"}, "--collection-id"},
		{"decide bad decision", runDecide, []string{"--config", cfg, "--date", testDate, "--meeting-id", "m", "--decision", "maybe", "--collection-id", "c"}, "--decision must be attended or skipped"},
		{"decide missing meeting", runDecide, []string{"--config", cfg, "--date", testDate, "--decision", "attended", "--collection-id", "c"}, "--meeting-id"},
	}
	for _, tc := range cases {
		err := tc.run(tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
}
