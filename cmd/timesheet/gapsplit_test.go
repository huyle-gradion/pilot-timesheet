package main

import (
	"sort"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// clusterByHead indexes one day's stage clusters by the head block id the
// stage map and the receipt name.
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

// assertCluster pins one cluster's union span, its contributors in cluster
// order (head first) and the joined description its intervals carry.
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

// TestEffectiveEntrySpansMeetingInsideEvidence is the live 2026-10-21 shape:
// workflow training 06:40-07:15 inside evidence 02:16-10:15. The two entries
// strictly overlap, so they are one cluster over the union span and the staged
// interval names both instead of staging the same minutes twice.
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

// TestEffectiveEntrySpansPartialOverlap: two entries that partly overlap merge
// to the union span, and both descriptions are kept in one text.
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

// TestEffectiveEntrySpansDuplicateMerges: a duplicate span is an overlap, so
// the two entries are one cluster under the lowest block id. The merged-away
// id is reported by the caller, never silently dropped.
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

// TestEffectiveEntrySpansExcludesUnusableEntries: a zero-duration entry is not
// a cluster candidate; the caller owns its skip reason, so it may not leak into
// a cluster. An overnight entry is a candidate (issue #176): its cluster ends
// on the next day's clock.
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

// TestStageDay21ShapeEndToEnd is the live 2026-10-21 ledger shape rewritten
// to the merged reading: evidence 02:16-13:00 and the two meetings inside it
// are one connected cluster (issue #183 rule 6), so the day stages as ONE
// interval set over the union span, chunked to the four-hour policy, and
// confirm plus save keep the day gap-free and non-overlapping.
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
	// Chunk-ceiling, non-overlap and contiguity: the confirmed minutes must
	// cover the union span exactly, with nothing lost, doubled or bridged.
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

// TestEffectiveEntrySpansTouchingUnchanged: end == start merely touches, so
// the two entries keep their own clusters (nothing is bridged).
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

// TestEffectiveEntrySpansDisjointUnchanged: entries that do not overlap keep
// their single spans (the bridge adds nothing on clean days).
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

// TestStageUnparsableTimesSkipReason: an entry whose anchors are not times the
// review carries is skipped with its OWN reason naming the bad times — never
// with the fully-claimed reason, which only genuine duplicates may carry
// (issue #173). The day's other entries still stage.
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

// TestStageZeroDurationEntrySkipReason: a 09:00→09:00 entry is NOT an overnight
// span — it has zero duration, and the interval op refuses start == end. Passing
// it through the overnight branch aborted the whole stage command with
// `invalid_interval` and blocked every other entry of the day (issue #173). It is
// skipped with its own reason, and a valid block on the same day still stages.
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

// TestStageOvernightBlockEndToEnd is the issue #173 regression: the gap-aware
// resolver excludes a span whose end is not after its start, and the derivation
// then reported the block as "fully claimed by more specific entries" and
// dropped it — silent loss of a real overnight block. A 23:00→01:00 block must
// stage as ONE overnight interval, survive confirm and save, and never carry
// the false fully-claimed reason.
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
	// The saved revision's view counts only the part on the starting local day
	// (23:00-24:00); the 01:00 tail is the next day's allocation — the same
	// cross-midnight split every overnight interval gets.
	if n := len(day.History); n == 0 || day.History[n-1].TotalMinutes != 60 {
		t.Fatalf("history = %+v, want the saved revision's 60 minutes on the starting day", day.History)
	}
}
