package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func TestStageMergedClusterEndToEnd(t *testing.T) {
	cfg := writeTestConfig(t)
	if err := os.MkdirAll(filepath.Dir(timesheet.EntriesPath(statePathFor(cfg), testDate)), 0o700); err != nil {
		t.Fatalf("entries dir: %v", err)
	}
	writeEntriesFile(t, cfg, testDate, []timesheet.EntryRecord{
		{BlockID: "work-09:00-11:00", Date: testDate, Start: "09:00", End: "11:00", Kind: "work", Description: "Bridge PRs"},
		{BlockID: "work-10:00-12:00", Date: testDate, Start: "10:00", End: "12:00", Kind: "work", Description: "Ops deploy"},
	})
	receipt := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	if len(receipt.Staged) != 1 || receipt.Staged[0].BlockID != "work-09:00-12:00" || receipt.Staged[0].Chunk != 0 {
		t.Fatalf("staged = %+v, want one unsplit interval under the union-derived id", receipt.Staged)
	}
	if receipt.StagedIntervals != 1 {
		t.Fatalf("staged_intervals = %d, want 1", receipt.StagedIntervals)
	}
	if len(receipt.Skipped) != 1 || receipt.Skipped[0].BlockID != "work-10:00-12:00" {
		t.Fatalf("skipped = %+v, want the merged-away contributor", receipt.Skipped)
	}
	reason := receipt.Skipped[0].Reason
	if !strings.Contains(reason, "work-09:00-12:00") || strings.Contains(reason, "fully claimed") {
		t.Fatalf("skip reason = %q, want it naming the merged block and never the fully-claimed reason", reason)
	}
	if err := runConfirm([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := runSave([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("save: %v", err)
	}
	day := readDurableDay(t, cfg, testDate)
	if len(day.Confirmed) != 1 {
		t.Fatalf("confirmed = %+v, want the single merged interval", day.Confirmed)
	}
	iv := day.Confirmed[0]
	if iv.Start != "09:00" || iv.End != "12:00" {
		t.Fatalf("confirmed interval = %s-%s, want the union span", iv.Start, iv.End)
	}
	if !strings.Contains(iv.Description, "Bridge PRs") || !strings.Contains(iv.Description, "Ops deploy") {
		t.Fatalf("confirmed description = %q, want both contributors named", iv.Description)
	}
}

func TestRestageMergedContributorRemovesItsOldInterval(t *testing.T) {
	cfg := writeTestConfig(t)
	if err := os.MkdirAll(filepath.Dir(timesheet.EntriesPath(statePathFor(cfg), testDate)), 0o700); err != nil {
		t.Fatalf("entries dir: %v", err)
	}
	writeEntriesFile(t, cfg, testDate, []timesheet.EntryRecord{
		{BlockID: "work-09:00-11:00", Date: testDate, Start: "09:00", End: "11:00", Kind: "work", Description: "Bridge PRs"},
		{BlockID: "work-12:00-13:00", Date: testDate, Start: "12:00", End: "13:00", Kind: "work", Description: "Ops deploy"},
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
		t.Fatalf("first stage: %v", err)
	}
	if len(first.Staged) != 2 {
		t.Fatalf("first stage = %+v, want two intervals", first.Staged)
	}
	headInterval := ""
	for _, ref := range first.Staged {
		if ref.BlockID == "work-09:00-11:00" {
			headInterval = ref.IntervalID
		}
	}

	writeEntriesFile(t, cfg, testDate, []timesheet.EntryRecord{
		{BlockID: "work-09:00-11:00", Date: testDate, Start: "09:00", End: "11:00", Kind: "work", Description: "Bridge PRs"},
		{BlockID: "work-12:00-13:00", Date: testDate, Start: "10:00", End: "10:30", Kind: "work", Description: "Ops deploy"},
	})
	second, err := applyStage(h, parsed, "stage", testDate, open())
	if err != nil {
		t.Fatalf("second stage: %v", err)
	}
	if len(second.Skipped) != 1 || second.Skipped[0].BlockID != "work-12:00-13:00" {
		t.Fatalf("skipped = %+v, want the merged-away contributor", second.Skipped)
	}
	if !strings.Contains(second.Skipped[0].Reason, "work-09:00-11:00") {
		t.Fatalf("skip reason = %q, want it naming the merged block", second.Skipped[0].Reason)
	}
	if len(second.Removed) != 1 || second.Removed[0].BlockID != "work-12:00-13:00" {
		t.Fatalf("removed = %+v, want the contributor's stale interval removed", second.Removed)
	}
	if len(second.Updated) != 1 || second.Updated[0].BlockID != "work-09:00-11:00" || second.Updated[0].IntervalID != headInterval {
		t.Fatalf("updated = %+v, want the merged block edited in place under %q", second.Updated, headInterval)
	}
	if second.StagedIntervals != 1 {
		t.Fatalf("staged_intervals = %d, want the single merged interval", second.StagedIntervals)
	}
	sidecar, err := timesheet.ReadStageMap(statePathFor(cfg), testDate)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if len(sidecar) != 1 || len(sidecar["work-09:00-11:00"]) != 1 || sidecar["work-09:00-11:00"][0] != headInterval {
		t.Fatalf("sidecar = %v, want only the merged block's in-place interval %q", sidecar, headInterval)
	}
}

func TestClassifyBindingCoversMergedContributors(t *testing.T) {
	cfg := writeTestConfig(t)
	if err := os.MkdirAll(filepath.Dir(timesheet.EntriesPath(statePathFor(cfg), testDate)), 0o700); err != nil {
		t.Fatalf("entries dir: %v", err)
	}
	entries := []timesheet.EntryRecord{
		{BlockID: "a", Date: testDate, Start: "09:00", End: "11:00", Kind: "work", Description: "A"},
		{BlockID: "b", Date: testDate, Start: "10:00", End: "12:00", Kind: "work", Description: "B"},
	}
	writeEntriesFile(t, cfg, testDate, entries)
	seedCatalog(t, cfg, "cat-1", timesheet.ClassificationOption{ID: "cls-dev", Name: "Development", Code: "DEV"})
	stage := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	if len(stage.Staged) != 1 {
		t.Fatalf("stage = %+v, want the merged interval", stage.Staged)
	}
	interval := stage.Staged[0].IntervalID
	choices := writeChoicesFile(t, cfg, `{"selections":{"`+interval+`":{"catalog_id":"cat-1","classification_id":"cls-dev"}}}`)
	runClassifyCapture(t, []string{"--config", cfg, "--date", testDate, "--file", choices})

	entries[1].End = "13:00"
	entries[1].Description = "Unreviewed changed contributor"
	writeEntriesFile(t, cfg, testDate, entries)

	for _, name := range []string{"confirm", "save"} {
		run := func() error {
			if name == "confirm" {
				return runConfirm([]string{"--config", cfg, "--date", testDate})
			}
			return runSave([]string{"--config", cfg, "--date", testDate})
		}
		_, _, err := captureOutput(t, run)
		if err == nil {
			t.Fatalf("%s after changing a merged contributor succeeded; want it to refuse the stale selection", name)
		}
		if !strings.Contains(err.Error(), "changed since classify") || !strings.Contains(err.Error(), "re-run stage + classify") {
			t.Fatalf("%s error = %q, want the re-run-stage-and-classify refusal", name, err)
		}
	}
}

func TestSaveSkipNoticePrintsEachReason(t *testing.T) {
	cfg := writeTestConfig(t)
	if err := os.MkdirAll(filepath.Dir(timesheet.EntriesPath(statePathFor(cfg), testDate)), 0o700); err != nil {
		t.Fatalf("entries dir: %v", err)
	}
	writeEntriesFile(t, cfg, testDate, []timesheet.EntryRecord{
		{BlockID: "a", Date: testDate, Start: "09:00", End: "11:00", Kind: "work", Description: "A"},
		{BlockID: "b", Date: testDate, Start: "10:00", End: "12:00", Kind: "work", Description: "B"},
		{BlockID: "zero", Date: testDate, Start: "16:00", End: "16:00", Kind: "work", Description: "No duration"},
	})
	_, stderr, err := captureOutput(t, func() error {
		return runSave([]string{"--config", cfg, "--date", testDate})
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	notice := string(stderr)
	if strings.Contains(notice, "without time anchors") {
		t.Fatalf("save notice = %q, want neutral wording that does not assert a cause", notice)
	}
	if !strings.Contains(notice, "skipped block b: merged into block") {
		t.Fatalf("save notice = %q, want the merged contributor's own reason", notice)
	}
	if !strings.Contains(notice, "skipped block zero: entry has zero duration") {
		t.Fatalf("save notice = %q, want the zero-duration entry's own reason", notice)
	}
}
