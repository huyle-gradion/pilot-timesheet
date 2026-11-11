package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

const overnightTZ = "Asia/Ho_Chi_Minh"

func writeTestConfigTZ(t *testing.T, tz string) string {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	body := `{
  "timezone": "` + tz + `",
  "state_path": "` + filepath.Join(dir, "state.json") + `",
  "projects": [{"id": "WORK", "name": "Work", "default_classification": "DEV"}]
}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func overnightEntry(id, kind, start, end, desc string) timesheet.EntryRecord {
	return timesheet.EntryRecord{
		BlockID: id, Date: testDate, Start: start, End: end, Description: desc, Kind: kind,
		RecordedAt: "2026-10-27T08:00:00Z",
	}
}

func TestStageSpansOvernightArithmetic(t *testing.T) {
	cases := []struct {
		name, date, tz, start, end string
		want                       [][2]string
	}{
		{"same-day is SplitEntrySpans", testDate, overnightTZ, "03:12", "11:11",
			[][2]string{{"03:12", "07:12"}, {"07:12", "11:11"}}},
		{"overnight at the 240 boundary", testDate, overnightTZ, "23:00", "03:00",
			[][2]string{{"23:00", "03:00"}}},
		{"overnight under the policy", testDate, overnightTZ, "23:00", "01:00",
			[][2]string{{"23:00", "01:00"}}},
		{"crossing chunk is the last 240 minutes", testDate, overnightTZ, "20:00", "03:00",
			[][2]string{{"20:00", "23:00"}, {"23:00", "03:00"}}},
		{"greedy before the crossing chunk", testDate, overnightTZ, "16:00", "02:00",
			[][2]string{{"16:00", "20:00"}, {"20:00", "22:00"}, {"22:00", "02:00"}}},
		{"ends exactly at midnight", testDate, overnightTZ, "19:00", "00:00",
			[][2]string{{"19:00", "23:00"}, {"23:00", "00:00"}}},
		{"pre-midnight part over the policy", testDate, overnightTZ, "18:00", "02:00",
			[][2]string{{"18:00", "22:00"}, {"22:00", "02:00"}}},
		{"post-midnight part at the policy", testDate, overnightTZ, "22:00", "04:00",
			[][2]string{{"22:00", "04:00"}}},
		{"post-midnight part beyond the policy", testDate, overnightTZ, "15:00", "05:00",
			[][2]string{{"15:00", "19:00"}, {"19:00", "23:00"}, {"23:00", "03:00"}}},
		// Europe/Berlin springs forward at 02:00 on 2026-03-29: 21:00→04:00 is
		// 360 real minutes, not 420, and the crossing chunk is 240 real minutes.
		{"DST-adjacent spring forward", "2026-03-28", "Europe/Berlin", "21:00", "04:00",
			[][2]string{{"21:00", "23:00"}, {"23:00", "04:00"}}},
		{"unknown timezone stays single", testDate, "Nowhere/Invalid", "20:00", "03:00",
			[][2]string{{"20:00", "03:00"}}},
		{"unparsable stays single", testDate, overnightTZ, "", "03:00",
			[][2]string{{"", "03:00"}}},
		{"zero duration stays single", testDate, overnightTZ, "09:00", "09:00",
			[][2]string{{"09:00", "09:00"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := timesheet.StageSpans(c.date, c.tz, c.start, c.end)
			if len(got) != len(c.want) {
				t.Fatalf("StageSpans(%s %s-%s) = %v, want %v", c.tz, c.start, c.end, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("StageSpans(%s %s-%s) = %v, want %v", c.tz, c.start, c.end, got, c.want)
				}
			}
		})
	}
}

func TestEffectiveEntrySpansOvernightClaims(t *testing.T) {
	clusters := timesheet.EffectiveEntrySpans([]timesheet.EntryRecord{
		overnightEntry("evidence-22:00-02:00", "work", "22:00", "02:00", "Release night"),
		overnightEntry("meeting-23:30-00:30", "meeting", "23:30", "00:30", "Incident bridge"),
		overnightEntry("work-00:30-01:00", "work", "00:30", "01:00", "Early morning"),
	})
	m := clusterByHead(t, clusters)
	if len(m) != 2 {
		t.Fatalf("clusters = %+v, want the overnight cluster and the early-morning block", clusters)
	}
	night := clusterOf(t, m, "evidence-22:00-02:00")
	assertCluster(t, "evidence-22:00-02:00", night, "22:00", "02:00",
		[]string{"evidence-22:00-02:00", "meeting-23:30-00:30"}, "Release night + Incident bridge")
	if night.Kind != "meeting" || night.BlockID != "meeting-22:00-02:00" {
		t.Fatalf("overnight cluster kind/id = %s/%s, want meeting/meeting-22:00-02:00", night.Kind, night.BlockID)
	}
	assertCluster(t, "work-00:30-01:00", clusterOf(t, m, "work-00:30-01:00"), "00:30", "01:00",
		[]string{"work-00:30-01:00"}, "Early morning")
}

func TestEffectiveEntrySpansExcludesMultiDaySpan(t *testing.T) {
	clusters := timesheet.EffectiveEntrySpans([]timesheet.EntryRecord{
		overnightEntry("marathon-1", "work", "2026-10-26T22:00:00+07:00", "2026-10-28T02:00:00+07:00", "Two-night migration"),
		overnightEntry("good-1", "work", "10:00", "11:00", "Clean block"),
	})
	m := clusterByHead(t, clusters)
	if len(m) != 1 {
		t.Fatalf("clusters = %+v, want only the clean block", clusters)
	}
	clusterOf(t, m, "good-1")
}

func writeLedger(t *testing.T, cfg, date string, entries []timesheet.EntryRecord) {
	t.Helper()
	for i := range entries {
		entries[i].Date = date
	}
	if err := os.MkdirAll(filepath.Dir(timesheet.EntriesPath(statePathFor(cfg), date)), 0o700); err != nil {
		t.Fatal(err)
	}
	writeEntriesFile(t, cfg, date, entries)
}

func stageAndSave(t *testing.T, cfg, date string, entries []timesheet.EntryRecord) (StageReceipt, *timesheet.Day) {
	t.Helper()
	writeLedger(t, cfg, date, entries)
	receipt := runStageCapture(t, []string{"--config", cfg, "--date", date})
	if err := runSave([]string{"--config", cfg, "--date", date}); err != nil {
		t.Fatalf("save: %v", err)
	}
	return receipt, readDurableDay(t, cfg, date)
}

func absoluteSpan(t *testing.T, iv timesheet.Interval, loc *time.Location) (time.Time, time.Time) {
	t.Helper()
	start, err := time.ParseInLocation("2006-01-02 15:04", iv.Date+" "+iv.Start, loc)
	if err != nil {
		t.Fatalf("parse start: %v", err)
	}
	endDate := iv.Date
	if iv.End <= iv.Start {
		d, _ := time.Parse("2006-01-02", iv.Date)
		endDate = d.AddDate(0, 0, 1).Format("2006-01-02")
	}
	end, err := time.ParseInLocation("2006-01-02 15:04", endDate+" "+iv.End, loc)
	if err != nil {
		t.Fatalf("parse end: %v", err)
	}
	return start, end
}

func assertCovers(t *testing.T, day *timesheet.Day, tz, from, to string, maxInterval int) []string {
	t.Helper()
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatal(err)
	}
	type span struct {
		s, e time.Time
		text string
	}
	var spans []span
	total := 0
	for _, iv := range day.Confirmed {
		s, e := absoluteSpan(t, iv, loc)
		minutes, err := iv.Duration(loc)
		if err != nil {
			t.Fatalf("interval %s-%s: %v", iv.Start, iv.End, err)
		}
		if int(e.Sub(s)/time.Minute) != minutes {
			t.Fatalf("interval %s-%s resolves to %d minutes, the model says %d", iv.Start, iv.End, int(e.Sub(s)/time.Minute), minutes)
		}
		if minutes > maxInterval {
			t.Fatalf("interval %s-%s = %d minutes, want <= %d", iv.Start, iv.End, minutes, maxInterval)
		}
		allocs, err := iv.ResolvedAllocations(loc)
		if err != nil {
			t.Fatalf("allocations %s-%s: %v", iv.Start, iv.End, err)
		}
		for _, a := range allocs {
			if a.Minutes > 240 {
				t.Fatalf("interval %s-%s allocates %d minutes on %s, want <= 240 (remote_entry_too_long)", iv.Start, iv.End, a.Minutes, a.Date)
			}
		}
		total += minutes
		spans = append(spans, span{s, e, iv.Start + "-" + iv.End})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].s.Before(spans[j].s) })
	if len(spans) == 0 {
		t.Fatalf("no confirmed intervals")
	}
	for i := 1; i < len(spans); i++ {
		if !spans[i].s.Equal(spans[i-1].e) {
			t.Fatalf("confirmed set is not contiguous and non-overlapping at %s / %s", spans[i-1].text, spans[i].text)
		}
	}
	want := timesheet.Interval{Date: day.Date, Start: from, End: to}
	ws, we := absoluteSpan(t, want, loc)
	if !spans[0].s.Equal(ws) || !spans[len(spans)-1].e.Equal(we) {
		t.Fatalf("confirmed set runs %s..%s, want the recorded span %s-%s", spans[0].text, spans[len(spans)-1].text, from, to)
	}
	if wantTotal := int(we.Sub(ws) / time.Minute); total != wantTotal {
		t.Fatalf("total confirmed minutes = %d, want the recorded span's %d", total, wantTotal)
	}
	out := make([]string, len(spans))
	for i, s := range spans {
		out[i] = s.text
	}
	return out
}

func assertSpans(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("confirmed spans = %v, want %v", got, want)
	}
}

func TestStageOvernightFourHourBoundaries(t *testing.T) {
	cases := []struct {
		start, end  string
		maxInterval int
		want        []string
	}{
		{"23:00", "03:00", 240, []string{"23:00-03:00"}},
		{"20:00", "03:00", 240, []string{"20:00-23:00", "23:00-03:00"}},
		{"18:00", "02:00", 240, []string{"18:00-22:00", "22:00-02:00"}},
		{"22:00", "04:00", 360, []string{"22:00-04:00"}},
	}
	for _, c := range cases {
		t.Run(c.start+"-"+c.end, func(t *testing.T) {
			cfg := writeTestConfigTZ(t, overnightTZ)
			receipt, day := stageAndSave(t, cfg, testDate, []timesheet.EntryRecord{
				overnightEntry("night-1", "work", c.start, c.end, "Overnight release"),
			})
			if len(receipt.Skipped) != 0 {
				t.Fatalf("skipped = %+v, want none", receipt.Skipped)
			}
			assertSpans(t, assertCovers(t, day, overnightTZ, c.start, c.end, c.maxInterval), c.want...)
			if len(receipt.Staged) != len(c.want) {
				t.Fatalf("staged = %+v, want %d chunk refs", receipt.Staged, len(c.want))
			}
		})
	}
}

func TestStageOvernightInteriorMeeting(t *testing.T) {
	cfg := writeTestConfigTZ(t, overnightTZ)
	receipt, day := stageAndSave(t, cfg, testDate, []timesheet.EntryRecord{
		overnightEntry("evidence-21:00-03:00", "work", "21:00", "03:00", "Release night"),
		overnightEntry("meeting-23:30-00:30", "meeting", "23:30", "00:30", "Incident bridge"),
		overnightEntry("meeting-21:30-22:00", "meeting", "21:30", "22:00", "Go/no-go"),
		overnightEntry("work-09:00-10:00", "work", "09:00", "10:00", "Morning block"),
	})
	merged := map[string]bool{}
	for _, s := range receipt.Skipped {
		if !strings.Contains(s.Reason, "merged into block meeting-21:00-03:00") {
			t.Fatalf("skip %+v, want a merge into the overnight cluster", s)
		}
		merged[s.BlockID] = true
	}
	if len(merged) != 2 || !merged["meeting-23:30-00:30"] || !merged["meeting-21:30-22:00"] {
		t.Fatalf("skipped = %+v, want both interior meetings merged", receipt.Skipped)
	}
	night := *day
	night.Confirmed = nil
	for _, iv := range day.Confirmed {
		if iv.Start == "09:00" {
			continue
		}
		for _, name := range []string{"Release night", "Incident bridge", "Go/no-go"} {
			if !strings.Contains(iv.Description, name) {
				t.Fatalf("interval %s-%s description = %q, want every contributor named", iv.Start, iv.End, iv.Description)
			}
		}
		night.Confirmed = append(night.Confirmed, iv)
	}
	assertSpans(t, assertCovers(t, &night, overnightTZ, "21:00", "03:00", 240), "21:00-23:00", "23:00-03:00")
	if len(day.Confirmed) != 3 {
		t.Fatalf("confirmed = %+v, want the two overnight chunks and the morning block", day.Confirmed)
	}
}

func TestStageMultiMidnightSpanSkipped(t *testing.T) {
	cfg := writeTestConfigTZ(t, overnightTZ)
	entries := []timesheet.EntryRecord{
		overnightEntry("marathon-1", "work", "2026-10-26T22:00:00+07:00", "2026-10-28T02:00:00+07:00", "Two-night migration"),
		overnightEntry("good-1", "work", "10:00", "11:00", "Clean block"),
	}
	writeLedger(t, cfg, testDate, entries)
	ledgerPath := timesheet.EntriesPath(statePathFor(cfg), testDate)
	before, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	receipt := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	if len(receipt.Skipped) != 1 || receipt.Skipped[0].BlockID != "marathon-1" ||
		!strings.Contains(receipt.Skipped[0].Reason, "24 hours or more") {
		t.Fatalf("skipped = %+v, want marathon-1 with the multi-midnight reason", receipt.Skipped)
	}
	if len(receipt.Staged) != 1 || receipt.Staged[0].BlockID != "good-1" {
		t.Fatalf("staged = %+v, want only the clean block", receipt.Staged)
	}
	if err := runSave([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if day := readDurableDay(t, cfg, testDate); len(day.Confirmed) != 1 || day.Confirmed[0].Start != "10:00" {
		t.Fatalf("confirmed = %+v, want only the clean block", day.Confirmed)
	}
	after, err := os.ReadFile(ledgerPath)
	if err != nil || string(before) != string(after) {
		t.Fatalf("entries ledger changed by stage/save (err=%v)", err)
	}
}

// TestStageOvernightDSTAdjacentDay: Europe/Berlin springs forward at 02:00 on
// 2026-03-29, so 21:00→04:00 recorded on 2026-03-28 is 360 real minutes. The
// chunks are cut in real time: 120 + 240, and the totals match exactly.
func TestStageOvernightDSTAdjacentDay(t *testing.T) {
	const date, tz = "2026-03-28", "Europe/Berlin"
	cfg := writeTestConfigTZ(t, tz)
	_, day := stageAndSave(t, cfg, date, []timesheet.EntryRecord{
		overnightEntry("night-1", "work", "21:00", "04:00", "Migration window"),
	})
	assertSpans(t, assertCovers(t, day, tz, "21:00", "04:00", 240), "21:00-23:00", "23:00-04:00")
}

func TestNextUnitsMatchOvernightStage(t *testing.T) {
	cfg := writeTestConfigTZ(t, overnightTZ)
	entries := []timesheet.EntryRecord{
		overnightEntry("evidence-21:00-03:00", "work", "21:00", "03:00", "Release night"),
		overnightEntry("meeting-23:30-00:30", "meeting", "23:30", "00:30", "Incident bridge"),
		overnightEntry("work-09:00-14:00", "work", "09:00", "14:00", "Morning block"),
		overnightEntry("night-only", "work", "2026-10-26T22:00:00+07:00", "2026-10-28T02:00:00+07:00", "Two nights"),
	}
	_, day := stageAndSave(t, cfg, testDate, entries)
	stageMap, err := timesheet.ReadStageMap(statePathFor(cfg), testDate)
	if err != nil {
		t.Fatal(err)
	}
	units := ledgerUnits(entries, testDate, overnightTZ)
	if !stageMapMatchesUnits(stageMap, units) {
		t.Fatalf("stage map %v does not match ledger units %+v", stageMap, units)
	}
	if !sameContent(ledgerContentSpans(units), confirmedContentSpans(day.Confirmed)) {
		t.Fatalf("ledger content %+v != confirmed %+v", ledgerContentSpans(units), confirmedContentSpans(day.Confirmed))
	}
}

func TestPlanAcceptsChunkedOvernightDay(t *testing.T) {
	stub := &stubRemote{entries: map[string][]map[string]any{}}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	t.Setenv("SYNTHETIC_GRADION_TOKEN", "synthetic-token")
	cfg := writeTestConfigWithGradion(t, server.URL)
	seedCatalog(t, cfg, "cat-1",
		timesheet.ClassificationOption{ID: "cls-dev", Name: "Development", Code: "DEV", ActivityType: "engineering"},
	)
	stageAndSave(t, cfg, testDate, []timesheet.EntryRecord{
		overnightEntry("night-1", "work", "18:00", "02:00", "Overnight release"),
		overnightEntry("meeting-23:30-00:30", "meeting", "23:30", "00:30", "Incident bridge"),
	})
	preview := runPlanCapture(t, []string{"--config", cfg, "--date", testDate})
	for _, b := range preview.Blockers {
		if b.Code == "remote_entry_too_long" || strings.Contains(b.Code, "overlap") {
			t.Fatalf("blockers = %+v, want no remote_entry_too_long or overlap", preview.Blockers)
		}
	}
}

func TestStageControlDayByteIdentical(t *testing.T) {
	cfg := writeTestConfig(t)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("evidence-02:16-13:00", "02:16", "13:00", "Broad evidence block"),
		timedBlock("meeting-06:40-07:15", "06:40", "07:15", "Workflow training"),
		timedBlock("work-14:00-15:30", "14:00", "15:30", "Bridge PRs"),
		timedBlock("work-16:00-16:00", "16:00", "16:00", "Zero block"),
		timedBlock("work-20:00-23:59", "20:00", "23:59", "Late block"),
	})
	out := captureStdout(t, func() {
		if err := runStage([]string{"--config", cfg, "--date", testDate}); err != nil {
			t.Fatalf("stage: %v", err)
		}
	})
	got := strings.ReplaceAll(out, filepath.Dir(cfg), "$DIR")
	if err := runSave([]string{"--config", cfg, "--date", testDate}); err != nil {
		t.Fatalf("save: %v", err)
	}
	confirmed, err := json.Marshal(readDurableDay(t, cfg, testDate).Confirmed)
	if err != nil {
		t.Fatal(err)
	}
	got += "--- confirmed\n" + string(confirmed) + "\n"
	entries, err := timesheet.ReadEntries(statePathFor(cfg), testDate)
	if err != nil {
		t.Fatal(err)
	}
	units, err := json.Marshal(ledgerUnits(entries.Entries, testDate, "Asia/Ho_Chi_Minh"))
	if err != nil {
		t.Fatal(err)
	}
	got += "--- units\n" + string(units) + "\n"
	want, err := os.ReadFile(filepath.Join("testdata", "overnight_control_day.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("control day changed:\n--- got\n%s\n--- want\n%s", got, want)
	}
}

func TestStageOvernightUnionOverADaySkipped(t *testing.T) {
	cases := []struct {
		name string
		a, b timesheet.EntryRecord
	}{
		{"26 hours", overnightEntry("work-06:00-20:00", "work", "06:00", "20:00", "Long day"),
			overnightEntry("work-19:00-08:00", "work", "19:00", "08:00", "Long night")},
		{"exactly 24 hours", overnightEntry("work-06:00-20:00", "work", "06:00", "20:00", "Long day"),
			overnightEntry("work-19:00-06:00", "work", "19:00", "06:00", "Long night")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			entries := []timesheet.EntryRecord{
				c.a, c.b,
				overnightEntry("work-01:00-02:00", "work", "01:00", "02:00", "Early block"),
			}
			clusters, dayLong := timesheet.ResolveEntrySpans(entries)
			if len(clusters) != 1 || clusters[0].BlockID != "work-01:00-02:00" || len(dayLong) != 2 {
				t.Fatalf("clusters = %+v, dayLong = %v; want only the early block and both long contributors", clusters, dayLong)
			}
			cfg := writeTestConfigTZ(t, overnightTZ)
			receipt, day := stageAndSave(t, cfg, testDate, entries)
			skipped := map[string]bool{}
			for _, s := range receipt.Skipped {
				if !strings.Contains(s.Reason, "24 hours or more") || strings.Contains(s.Reason, "covered by the union span") {
					t.Fatalf("skip %+v, want the day-long union reason", s)
				}
				skipped[s.BlockID] = true
			}
			if len(skipped) != 2 || !skipped[c.a.BlockID] || !skipped[c.b.BlockID] {
				t.Fatalf("skipped = %+v, want both day-long contributors", receipt.Skipped)
			}
			if len(day.Confirmed) != 1 || day.Confirmed[0].Start != "01:00" || day.Confirmed[0].End != "02:00" {
				t.Fatalf("confirmed = %+v, want only the early block (nothing wrapped)", day.Confirmed)
			}
			stageMap, err := timesheet.ReadStageMap(statePathFor(cfg), testDate)
			if err != nil {
				t.Fatal(err)
			}
			units := ledgerUnits(entries, testDate, overnightTZ)
			if !stageMapMatchesUnits(stageMap, units) || !currentDerives(units, day.Confirmed) {
				t.Fatalf("next units %+v do not match the staged/saved day", units)
			}
		})
	}
}

func legacySavedOvernightDay(t *testing.T, cfg string, entries []timesheet.EntryRecord) {
	t.Helper()
	writeLedger(t, cfg, testDate, entries)
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	st := store.State()
	st.Days[testDate] = &timesheet.Day{
		Date: testDate, Timezone: overnightTZ, Revision: 1, NextInterval: 2,
		Confirmed: []timesheet.Interval{{
			ID: "iv-20261026-1", Date: testDate, Start: "21:00", End: "03:00",
			Project: "WORK", Classification: "DEV", Description: "Release night",
		}},
	}
	if err := store.CommitState(st); err != nil {
		t.Fatal(err)
	}
}

func TestFinalizedLegacyOvernightDayStaysCurrent(t *testing.T) {
	entries := []timesheet.EntryRecord{overnightEntry("night-1", "work", "21:00", "03:00", "Release night")}
	cfg := writeTestConfigTZ(t, overnightTZ)
	legacySavedOvernightDay(t, cfg, entries)
	confirmed := readDayConfirmed(t, cfg, testDate)

	o := finalizedDay()
	o.Artifacts.Entries = &timesheet.EntriesFile{Date: testDate, Entries: entries}
	o.Units = ledgerUnits(entries, testDate, overnightTZ)
	o.LegacyUnits = legacyDerivations(entries, testDate, overnightTZ)
	o.Workflow.Confirmed = confirmed
	if n := deriveDayNext(o); n.State == stateStopped && strings.Contains(n.Reason, "no longer derives") {
		t.Fatalf("legacy finalized day stopped: %s", n.Reason)
	}
	changed := []timesheet.EntryRecord{overnightEntry("night-1", "work", "21:00", "02:00", "Release night")}
	o.Units = ledgerUnits(changed, testDate, overnightTZ)
	o.LegacyUnits = legacyDerivations(changed, testDate, overnightTZ)
	if n := deriveDayNext(o); n.State != stateStopped || !strings.Contains(n.Reason, "no longer derives") {
		t.Fatalf("changed ledger = %q/%q, want the no-longer-derives stop", n.State, n.Reason)
	}

	t.Setenv("HOME", t.TempDir())
	err := runAgentSubmit([]string{"--config", cfg, "--date", testDate})
	if err != nil && strings.Contains(err.Error(), "differs from saved intervals") {
		t.Fatalf("submit --chat refused the legacy saved day: %v", err)
	}
}
