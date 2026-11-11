package main

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func TestSplitEntrySpansArithmetic(t *testing.T) {
	cases := []struct {
		start, end string
		want       [][2]string
	}{
		{"03:12", "11:11", [][2]string{{"03:12", "07:12"}, {"07:12", "11:11"}}},
		{"09:00", "13:00", [][2]string{{"09:00", "13:00"}}},
		{"09:00", "13:01", [][2]string{{"09:00", "13:00"}, {"13:00", "13:01"}}},
		{"09:00", "10:00", [][2]string{{"09:00", "10:00"}}},
		{"23:00", "22:00", [][2]string{{"23:00", "22:00"}}},
		{"", "10:00", [][2]string{{"", "10:00"}}},
	}
	for _, c := range cases {
		got := timesheet.SplitEntrySpans(c.start, c.end)
		if len(got) != len(c.want) {
			t.Fatalf("SplitEntrySpans(%q, %q) = %v, want %v", c.start, c.end, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("SplitEntrySpans(%q, %q)[%d] = %v, want %v", c.start, c.end, i, got[i], c.want[i])
			}
		}
	}
}

func TestStageSplitsFourHourBlocks(t *testing.T) {
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("evidence-1", "03:12", "11:11", "Long evidence block"),
		timedBlock("meeting-1", "15:30", "15:45", "Standup"),
	})
	entriesPath := timesheet.EntriesPath(statePathFor(cfg), testDate)
	before, err := os.ReadFile(entriesPath)
	if err != nil {
		t.Fatalf("read entries: %v", err)
	}
	receipt := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	byBlock := map[string][]StageBlockRef{}
	for _, ref := range receipt.Staged {
		byBlock[ref.BlockID] = append(byBlock[ref.BlockID], ref)
	}
	if len(byBlock["evidence-1"]) != 2 {
		t.Fatalf("evidence-1 refs = %+v, want 2 chunks", byBlock["evidence-1"])
	}
	for i, ref := range byBlock["evidence-1"] {
		if ref.Chunk != i+1 || ref.Chunks != 2 {
			t.Fatalf("chunk ref %+v, want chunk %d of 2", ref, i+1)
		}
	}
	if len(byBlock["meeting-1"]) != 1 || byBlock["meeting-1"][0].Chunk != 0 || byBlock["meeting-1"][0].Chunks != 0 {
		t.Fatalf("meeting-1 refs = %+v, want a single unsplit interval", byBlock["meeting-1"])
	}
	if receipt.StagedIntervals != 3 {
		t.Fatalf("staged_intervals = %d, want 3 (2 chunks + 1 meeting)", receipt.StagedIntervals)
	}
	sidecar, err := timesheet.ReadStageMap(statePathFor(cfg), testDate)
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if len(sidecar["evidence-1"]) != 2 || sidecar["evidence-1"][0] == sidecar["evidence-1"][1] {
		t.Fatalf("sidecar evidence-1 = %v, want 2 distinct chunk ids", sidecar["evidence-1"])
	}
	if len(sidecar["meeting-1"]) != 1 {
		t.Fatalf("sidecar meeting-1 = %v, want 1 id", sidecar["meeting-1"])
	}
	after, err := os.ReadFile(entriesPath)
	if err != nil || string(before) != string(after) {
		t.Fatalf("entries ledger changed by the split (err=%v)", err)
	}
}

func seedOverlongConfirmedDay(t *testing.T, cfg string) int {
	t.Helper()
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()
	st := store.State()
	st.Days[testDate] = &timesheet.Day{
		Date: testDate, Timezone: "UTC", Revision: 1, NextInterval: 2,
		Confirmed: []timesheet.Interval{{
			ID: "iv-overlong-1", Date: testDate, Start: "09:00", End: "14:00",
			Project: "WORK", Description: "Over-long confirmed interval",
		}},
	}
	if err := store.CommitState(st); err != nil {
		t.Fatalf("seed over-long confirmed day: %v", err)
	}
	return 1
}

func TestStageReviseReplacesFinalizedDayWithChunks(t *testing.T) {
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("evidence-1", "03:12", "11:11", "Long evidence block"),
		timedBlock("meeting-1", "15:30", "15:45", "Standup"),
	})
	before := seedOverlongConfirmedDay(t, cfg)

	err := runStage([]string{"--config", cfg, "--date", testDate})
	if err == nil || !strings.Contains(err.Error(), "--revise") {
		t.Fatalf("plain stage on finalized day = %v, want the refusal naming --revise", err)
	}

	receipt := runStageCapture(t, []string{"--config", cfg, "--date", testDate, "--revise"})
	if !receipt.Refinalized || receipt.Replaced != 1 {
		t.Fatalf("revise receipt = %+v, want refinalized with 1 replaced interval", receipt)
	}
	if len(receipt.Removed) != 1 || receipt.Removed[0].IntervalID != "iv-overlong-1" {
		t.Fatalf("revise removed = %+v, want the replaced durable interval iv-overlong-1", receipt.Removed)
	}
	if receipt.StagedIntervals != 3 {
		t.Fatalf("revise staged_intervals = %d, want 3 (2 chunks + 1 meeting)", receipt.StagedIntervals)
	}
	day := readDurableDay(t, cfg, testDate)
	if day.Revision != before+1 {
		t.Fatalf("revision = %d, want %d", day.Revision, before+1)
	}
	if len(day.Confirmed) != 3 {
		t.Fatalf("confirmed = %d intervals, want 3", len(day.Confirmed))
	}
	spans := map[string]bool{}
	for _, iv := range day.Confirmed {
		spans[iv.Start+"-"+iv.End] = true
	}
	if !spans["03:12-07:12"] || !spans["07:12-11:11"] || !spans["15:30-15:45"] {
		t.Fatalf("confirmed spans = %v, want the 240/235 chunks and the meeting", spans)
	}

	if _, err := timesheet.RecordEntries(statePathFor(cfg), timesheet.AppVersion,
		timesheet.ReviewedDraft{Date: testDate, Blocks: []timesheet.ReviewedBlock{
			timedBlock("evidence-1", "03:12", "11:11", "Long evidence block"),
		}}, true, time.Now()); err != nil {
		t.Fatalf("re-record ledger without the meeting: %v", err)
	}
	second := runStageCapture(t, []string{"--config", cfg, "--date", testDate, "--revise"})
	removed := map[string]bool{}
	for _, ref := range second.Removed {
		removed[ref.IntervalID] = true
	}
	for _, iv := range day.Confirmed {
		if !removed[iv.ID] {
			t.Fatalf("second revise removed = %+v, want the durable interval %s it replaced", second.Removed, iv.ID)
		}
	}
	day = readDurableDay(t, cfg, testDate)
	if len(day.Confirmed) != 2 || second.StagedIntervals != 2 {
		t.Fatalf("after ledger removal: confirmed = %d, staged = %d; want only the 2 chunks", len(day.Confirmed), second.StagedIntervals)
	}
}

func TestPlanBlocksOverlongOperation(t *testing.T) {
	stub := &stubRemote{entries: map[string][]map[string]any{}}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	t.Setenv("SYNTHETIC_GRADION_TOKEN", "synthetic-token")
	cfg := writeTestConfigWithGradion(t, server.URL)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("evidence-1", "09:00", "14:00", "Over-long confirmed interval"),
	})
	seedOverlongConfirmedDay(t, cfg)
	seedCatalog(t, cfg, "cat-1",
		timesheet.ClassificationOption{ID: "cls-dev", Name: "Development", Code: "DEV", ActivityType: "engineering"},
	)
	preview := runPlanCapture(t, []string{"--config", cfg, "--date", testDate})
	if preview.State != "blocked" {
		t.Fatalf("plan state = %q, want blocked", preview.State)
	}
	found := false
	for _, b := range preview.Blockers {
		if b.Code == "remote_entry_too_long" && b.LocalID == "iv-overlong-1" &&
			strings.Contains(b.Detail, "300 minutes") && strings.Contains(b.Detail, "240") {
			found = true
		}
	}
	if !found {
		t.Fatalf("blockers = %+v, want remote_entry_too_long naming the interval, minutes and policy", preview.Blockers)
	}
}

func TestSplitDeterminism(t *testing.T) {
	for i := 0; i < 2; i++ {
		got := timesheet.SplitEntrySpans("03:12", "11:11")
		want := [][2]string{{"03:12", "07:12"}, {"07:12", "11:11"}}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("run %d: SplitEntrySpans = %v, want %v", i, got, want)
		}
	}
}
