package main

import (
	"sort"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func clusterByHead(t *testing.T, clusters []timesheet.EntryCluster) map[string]timesheet.EntryCluster {
	t.Helper()
	out := map[string]timesheet.EntryCluster{}
	for _, c := range clusters {
		if len(c.BlockIDs) == 0 {
			t.Fatalf("cluster has no block ids: %+v", clusters)
		}
		out[c.BlockIDs[0]] = c
	}
	return out
}

func clusterOf(t *testing.T, m map[string]timesheet.EntryCluster, head string) timesheet.EntryCluster {
	t.Helper()
	got, ok := m[head]
	if !ok {
		t.Fatalf("no cluster headed by %s in %+v", head, m)
	}
	return got
}

func assertCluster(t *testing.T, head string, got timesheet.EntryCluster, start, end string, ids []string, desc string) {
	t.Helper()
	if got.Start != start || got.End != end {
		t.Fatalf("%s span = %s-%s, want %s-%s", head, got.Start, got.End, start, end)
	}
	if len(got.BlockIDs) != len(ids) {
		t.Fatalf("%s contributors = %v, want %v", head, got.BlockIDs, ids)
	}
	for i := range ids {
		if got.BlockIDs[i] != ids[i] {
			t.Fatalf("%s contributors = %v, want %v", head, got.BlockIDs, ids)
		}
	}
	if got.Description != desc {
		t.Fatalf("%s description = %q, want %q", head, got.Description, desc)
	}
}

func TestEffectiveEntrySpansMeetingInsideEvidence(t *testing.T) {
	clusters := timesheet.EffectiveEntrySpans([]timesheet.EntryRecord{
		{BlockID: "evidence-02:16-10:15", Start: "02:16", End: "10:15", Description: "Broad evidence"},
		{BlockID: "meeting-06:40-07:15", Start: "06:40", End: "07:15", Description: "Workflow training"},
	})
	m := clusterByHead(t, clusters)
	if len(m) != 1 {
		t.Fatalf("clusters = %+v, want one", clusters)
	}
	assertCluster(t, "evidence-02:16-10:15", clusterOf(t, m, "evidence-02:16-10:15"), "02:16", "10:15",
		[]string{"evidence-02:16-10:15", "meeting-06:40-07:15"}, "Broad evidence + Workflow training")
}

func TestEffectiveEntrySpansPartialOverlap(t *testing.T) {
	clusters := timesheet.EffectiveEntrySpans([]timesheet.EntryRecord{
		{BlockID: "b-late", Start: "10:00", End: "12:00", Description: "Ops deploy"},
		{BlockID: "a-early", Start: "09:00", End: "11:00", Description: "Bridge PRs"},
	})
	m := clusterByHead(t, clusters)
	if len(m) != 1 {
		t.Fatalf("clusters = %+v, want one", clusters)
	}
	assertCluster(t, "a-early", clusterOf(t, m, "a-early"), "09:00", "12:00",
		[]string{"a-early", "b-late"}, "Bridge PRs + Ops deploy")
}

func TestEffectiveEntrySpansDuplicateMerges(t *testing.T) {
	clusters := timesheet.EffectiveEntrySpans([]timesheet.EntryRecord{
		{BlockID: "dup-b", Start: "06:40", End: "07:15", Description: "Same work"},
		{BlockID: "dup-a", Start: "06:40", End: "07:15", Description: "Same work"},
	})
	m := clusterByHead(t, clusters)
	if len(m) != 1 {
		t.Fatalf("clusters = %+v, want one", clusters)
	}
	assertCluster(t, "dup-a", clusterOf(t, m, "dup-a"), "06:40", "07:15",
		[]string{"dup-a", "dup-b"}, "Same work")
}

func TestEffectiveEntrySpansExcludesUnusableEntries(t *testing.T) {
	clusters := timesheet.EffectiveEntrySpans([]timesheet.EntryRecord{
		{BlockID: "zero-1", Start: "09:00", End: "09:00", Description: "No duration"},
		{BlockID: "night-1", Start: "23:00", End: "01:00", Description: "Overnight watch"},
		{BlockID: "good-1", Start: "10:00", End: "11:00", Description: "Clean block"},
	})
	m := clusterByHead(t, clusters)
	if len(m) != 2 {
		t.Fatalf("clusters = %+v, want the clean block and the overnight block", clusters)
	}
	assertCluster(t, "good-1", clusterOf(t, m, "good-1"), "10:00", "11:00",
		[]string{"good-1"}, "Clean block")
	assertCluster(t, "night-1", clusterOf(t, m, "night-1"), "23:00", "01:00",
		[]string{"night-1"}, "Overnight watch")
}

func TestStageDay21ShapeEndToEnd(t *testing.T) {
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("evidence-02:16-13:00", "02:16", "13:00", "Broad evidence block"),
		timedBlock("meeting-06:40-07:15", "06:40", "07:15", "Workflow training"),
		timedBlock("meeting-09:00-09:30", "09:00", "09:30", "Standup"),
	})
	receipt := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	if receipt.StagedIntervals != 3 {
		t.Fatalf("staged_intervals = %d, want 3 (one union span 02:16-13:00 chunked 240+240+164)", receipt.StagedIntervals)
	}
	if err := runConfirm([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := runSave([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("save: %v", err)
	}
	day := readDurableDay(t, cfg, testDate)
	if len(day.Confirmed) != 3 {
		spans := make([]string, 0, len(day.Confirmed))
		for _, iv := range day.Confirmed {
			spans = append(spans, iv.Start+"-"+iv.End)
		}
		t.Fatalf("confirmed spans = %v, want the three chunks of 02:16-13:00", spans)
	}
	type bound struct{ start, end, minutes int }
	clock := func(s string) int {
		m, ok := 0, false
		if len(s) == 5 {
			h := int(s[0]-'0')*10 + int(s[1]-'0')
			mm := int(s[3]-'0')*10 + int(s[4]-'0')
			m, ok = h*60+mm, true
		}
		if !ok {
			t.Fatalf("unparsable span %q", s)
		}
		return m
	}
	var bounds []bound
	total := 0
	for _, iv := range day.Confirmed {
		s, e := clock(iv.Start), clock(iv.End)
		mins := e - s
		if mins <= 0 || mins > 240 {
			t.Fatalf("interval %s-%s = %d minutes, want (0, 240]", iv.Start, iv.End, mins)
		}
		for _, name := range []string{"Broad evidence block", "Workflow training", "Standup"} {
			if !strings.Contains(iv.Description, name) {
				t.Fatalf("interval %s-%s description = %q, want every contributor named", iv.Start, iv.End, iv.Description)
			}
		}
		bounds = append(bounds, bound{s, e, mins})
		total += mins
	}
	for i := 1; i < len(bounds); i++ {
		for j := 0; j < i; j++ {
			if bounds[i].start < bounds[j].end && bounds[j].start < bounds[i].end {
				t.Fatalf("intervals overlap: %+v", bounds)
			}
		}
	}
	sort.Slice(bounds, func(i, j int) bool { return bounds[i].start < bounds[j].start })
	if bounds[0].start != clock("02:16") || bounds[len(bounds)-1].end != clock("13:00") {
		t.Fatalf("confirmed set = %+v, want it bounded by the union span 02:16-13:00", bounds)
	}
	for i := 1; i < len(bounds); i++ {
		if bounds[i].start != bounds[i-1].end {
			t.Fatalf("confirmed set = %+v, want contiguous chunks across the union span", bounds)
		}
	}
	if total != clock("13:00")-clock("02:16") {
		t.Fatalf("total confirmed minutes = %d, want the union span's %d", total, clock("13:00")-clock("02:16"))
	}
}

func TestEffectiveEntrySpansTouchingUnchanged(t *testing.T) {
	clusters := timesheet.EffectiveEntrySpans([]timesheet.EntryRecord{
		{BlockID: "a", Start: "09:00", End: "10:00", Description: "First"},
		{BlockID: "b", Start: "10:00", End: "11:00", Description: "Second"},
	})
	m := clusterByHead(t, clusters)
	if len(m) != 2 {
		t.Fatalf("clusters = %+v, want two", clusters)
	}
	assertCluster(t, "a", clusterOf(t, m, "a"), "09:00", "10:00", []string{"a"}, "First")
	assertCluster(t, "b", clusterOf(t, m, "b"), "10:00", "11:00", []string{"b"}, "Second")
}

func TestEffectiveEntrySpansDisjointUnchanged(t *testing.T) {
	clusters := timesheet.EffectiveEntrySpans([]timesheet.EntryRecord{
		{BlockID: "a", Start: "09:00", End: "10:00", Description: "First"},
		{BlockID: "b", Start: "11:00", End: "11:15", Description: "Second"},
	})
	m := clusterByHead(t, clusters)
	if len(m) != 2 {
		t.Fatalf("clusters = %+v, want two", clusters)
	}
	assertCluster(t, "a", clusterOf(t, m, "a"), "09:00", "10:00", []string{"a"}, "First")
	assertCluster(t, "b", clusterOf(t, m, "b"), "11:00", "11:15", []string{"b"}, "Second")
}

func TestStageUnparsableTimesSkipReason(t *testing.T) {
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("bad-1", "23:00", "1am", "Block with a malformed end time"),
		timedBlock("good-1", "09:00", "10:00", "Clean block"),
	})
	receipt := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	if len(receipt.Skipped) != 1 || receipt.Skipped[0].BlockID != "bad-1" {
		t.Fatalf("skipped = %+v, want exactly bad-1", receipt.Skipped)
	}
	reason := receipt.Skipped[0].Reason
	if !strings.Contains(reason, "cannot be parsed") || !strings.Contains(reason, "1am") {
		t.Fatalf("skip reason = %q, want the unparsable-times reason naming the bad end time", reason)
	}
	if strings.Contains(reason, "fully claimed") {
		t.Fatalf("skip reason = %q, want the fully-claimed reason reserved for genuinely claimed entries", reason)
	}
	if len(receipt.Staged) != 1 || receipt.Staged[0].BlockID != "good-1" {
		t.Fatalf("staged = %+v, want only the clean block", receipt.Staged)
	}
}

func TestStageZeroDurationEntrySkipReason(t *testing.T) {
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("zero-1", "09:00", "09:00", "Block with no duration"),
		timedBlock("good-1", "10:00", "11:00", "Clean block"),
	})
	receipt := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	if len(receipt.Skipped) != 1 || receipt.Skipped[0].BlockID != "zero-1" {
		t.Fatalf("skipped = %+v, want exactly zero-1", receipt.Skipped)
	}
	reason := receipt.Skipped[0].Reason
	if !strings.Contains(reason, "zero duration") || !strings.Contains(reason, "09:00") {
		t.Fatalf("skip reason = %q, want the zero-duration reason naming the entry's times", reason)
	}
	if strings.Contains(reason, "fully claimed") {
		t.Fatalf("skip reason = %q, want the fully-claimed reason reserved for genuinely claimed entries", reason)
	}
	if len(receipt.Staged) != 1 || receipt.Staged[0].BlockID != "good-1" {
		t.Fatalf("staged = %+v, want only the clean block (no whole-day abort)", receipt.Staged)
	}
	if receipt.StagedIntervals != 1 {
		t.Fatalf("staged_intervals = %d, want 1", receipt.StagedIntervals)
	}
}

func TestStageOvernightBlockEndToEnd(t *testing.T) {
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("night-1", "23:00", "01:00", "Overnight release watch"),
	})
	receipt := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	for _, skip := range receipt.Skipped {
		if strings.Contains(skip.Reason, "fully claimed") {
			t.Fatalf("overnight block skipped as %q, want it staged as one interval", skip.Reason)
		}
	}
	if len(receipt.Skipped) != 0 {
		t.Fatalf("skipped = %+v, want no skips", receipt.Skipped)
	}
	if len(receipt.Staged) != 1 {
		t.Fatalf("staged = %+v, want the block's single overnight interval", receipt.Staged)
	}
	if ref := receipt.Staged[0]; ref.BlockID != "night-1" || ref.Chunk != 0 || ref.Chunks != 0 {
		t.Fatalf("staged ref = %+v, want night-1 as one unsplit interval", ref)
	}
	if err := runConfirm([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := runSave([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("save: %v", err)
	}
	day := readDurableDay(t, cfg, testDate)
	if len(day.Confirmed) != 1 {
		t.Fatalf("confirmed = %+v, want the single overnight interval", day.Confirmed)
	}
	if iv := day.Confirmed[0]; iv.Start != "23:00" || iv.End != "01:00" {
		t.Fatalf("confirmed interval = %s-%s, want the recorded span 23:00-01:00", iv.Start, iv.End)
	}
	if n := len(day.History); n == 0 || day.History[n-1].TotalMinutes != 60 {
		t.Fatalf("history = %+v, want the saved revision's 60 minutes on the starting day", day.History)
	}
}
