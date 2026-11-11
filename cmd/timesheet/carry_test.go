package main

import (
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

const carryTarget = "2026-10-27"

func carryEntry(date, id, kind, start, end, desc string) timesheet.EntryRecord {
	e := overnightEntry(id, kind, start, end, desc)
	e.Date = date
	return e
}

func ledgerBytes(t *testing.T, cfg, date string) string {
	t.Helper()
	b, err := os.ReadFile(timesheet.EntriesPath(statePathFor(cfg), date))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func dayMinutes(t *testing.T, day *timesheet.Day, tz string) int {
	t.Helper()
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, iv := range day.Confirmed {
		m, err := iv.Duration(loc)
		if err != nil {
			t.Fatalf("interval %s-%s: %v", iv.Start, iv.End, err)
		}
		total += m
	}
	return total
}

func confirmedSpans(day *timesheet.Day) []string {
	out := make([]string, 0, len(day.Confirmed))
	for _, iv := range day.Confirmed {
		out = append(out, iv.Start+"-"+iv.End)
	}
	sort.Strings(out)
	return out
}

func assertNotStale(t *testing.T, n nextAction) {
	t.Helper()
	if n.State == stateStopped && (strings.Contains(n.Reason, "no longer derives") || strings.Contains(n.Reason, "carr")) {
		t.Fatalf("next reported the day stale: %s", n.Reason)
	}
}

func assertNoIntervalOver(t *testing.T, day *timesheet.Day, tz string, limit int) {
	t.Helper()
	loc, _ := time.LoadLocation(tz)
	for _, iv := range day.Confirmed {
		if m, err := iv.Duration(loc); err != nil || m > limit {
			t.Fatalf("interval %s-%s on %s = %d minutes (err %v), want <= %d", iv.Start, iv.End, day.Date, m, err, limit)
		}
	}
}

func TestCarryOvernightRemainder(t *testing.T) {
	cases := []struct {
		start, end     string
		total          int
		source         []string
		carryS, carryE string
		target         []string
	}{
		{"23:00", "05:00", 360, []string{"23:00-03:00"}, "03:00", "05:00", []string{"03:00-05:00", "10:00-11:00"}},
		{"20:00", "06:00", 600, []string{"20:00-00:00"}, "00:00", "06:00", []string{"00:00-04:00", "04:00-06:00", "10:00-11:00"}},
	}
	for _, c := range cases {
		t.Run(c.start+"-"+c.end, func(t *testing.T) {
			cfg := writeTestConfigTZ(t, overnightTZ)
			receipt, source := stageAndSave(t, cfg, testDate, []timesheet.EntryRecord{
				overnightEntry("night-1", "work", c.start, c.end, "Overnight release"),
			})
			sourceLedger := ledgerBytes(t, cfg, testDate)
			assertSpans(t, confirmedSpans(source), c.source...)
			assertNoIntervalOver(t, source, overnightTZ, 240)
			if len(receipt.CarriedOut) != 1 {
				t.Fatalf("carried_out = %+v, want one carried segment", receipt.CarriedOut)
			}
			out := receipt.CarriedOut[0]
			if out.From != testDate || out.To != carryTarget || out.Start != c.carryS || out.End != c.carryE {
				t.Fatalf("carried_out = %+v, want %s→%s %s-%s", out, testDate, carryTarget, c.carryS, c.carryE)
			}

			targetReceipt, target := stageAndSave(t, cfg, carryTarget, []timesheet.EntryRecord{
				carryEntry(carryTarget, "work-10:00-11:00", "work", "10:00", "11:00", "Own morning work"),
			})
			if len(targetReceipt.CarriedIn) != 1 || targetReceipt.CarriedIn[0].From != testDate {
				t.Fatalf("carried_in = %+v, want the segment carried from %s", targetReceipt.CarriedIn, testDate)
			}
			assertSpans(t, confirmedSpans(target), c.target...)
			assertNoIntervalOver(t, target, overnightTZ, 240)
			carried := 0
			loc, _ := time.LoadLocation(overnightTZ)
			for _, iv := range target.Confirmed {
				if iv.Description == "Overnight release" {
					m, _ := iv.Duration(loc)
					carried += m
				}
			}
			if got := dayMinutes(t, source, overnightTZ) + carried; got != c.total {
				t.Fatalf("source + carried minutes = %d, want the recorded %d", got, c.total)
			}
			if ledgerBytes(t, cfg, testDate) != sourceLedger {
				t.Fatalf("the source ledger was modified")
			}
		})
	}
}

func TestCarryIntoEmptyDay(t *testing.T) {
	cfg := writeTestConfigTZ(t, overnightTZ)
	stageAndSave(t, cfg, testDate, []timesheet.EntryRecord{
		overnightEntry("night-1", "work", "23:00", "05:00", "Overnight release"),
	})
	receipt := runStageCapture(t, []string{"--config", cfg, "--date", carryTarget})
	if len(receipt.Staged) != 1 || len(receipt.CarriedIn) != 1 {
		t.Fatalf("stage = %+v, want the one carried segment staged", receipt)
	}
	if err := runSave([]string{"--config", cfg, "--date", carryTarget}); err != nil {
		t.Fatalf("save: %v", err)
	}
	assertSpans(t, confirmedSpans(readDurableDay(t, cfg, carryTarget)), "03:00-05:00")

	carry := []timesheet.CarrySegment{{
		SourceDate: testDate, SourceBlockID: "night-1", Date: nextDay, Start: "03:00", End: "05:00",
		Minutes: 120, Description: "Overnight release", Kind: "work", Timezone: overnightTZ,
	}}
	o := unclassifiedDay()
	o.Artifacts = timesheet.DayArtifacts{CarryIn: carry}
	o.Units = carryUnits(nil, carry, nextDay, overnightTZ)
	o.DraftPresent, o.DraftValid, o.DraftBlocks = true, true, 0
	if n := deriveDayNext(o); n.State != stateAction || n.Step != "stage" {
		t.Fatalf("carry-only day with an empty draft = %s/%s %q, want the stage action", n.State, n.Step, n.Reason)
	}
}

func carryUnits(entries []timesheet.EntryRecord, carry []timesheet.CarrySegment, date, tz string) []ledgerUnit {
	return ledgerUnits(dayStageEntries(entries, carry), date, tz)
}

func TestCarryMergesWithOverlappingEntries(t *testing.T) {
	cfg := writeTestConfigTZ(t, overnightTZ)
	stageAndSave(t, cfg, testDate, []timesheet.EntryRecord{
		overnightEntry("night-1", "work", "23:00", "05:00", "Overnight release"),
	})
	receipt, target := stageAndSave(t, cfg, carryTarget, []timesheet.EntryRecord{
		carryEntry(carryTarget, "work-04:00-06:00", "work", "04:00", "06:00", "Post-release checks"),
	})
	assertSpans(t, confirmedSpans(target), "03:00-06:00")
	if !strings.Contains(target.Confirmed[0].Description, "Overnight release") ||
		!strings.Contains(target.Confirmed[0].Description, "Post-release checks") {
		t.Fatalf("description = %q, want both contributors", target.Confirmed[0].Description)
	}
	if len(receipt.Skipped) != 1 || !strings.Contains(receipt.Skipped[0].Reason, "merged into block") {
		t.Fatalf("skipped = %+v, want one merged contributor", receipt.Skipped)
	}
}

func TestCarryRestageReplaces(t *testing.T) {
	cfg := writeTestConfigTZ(t, overnightTZ)
	writeLedger(t, cfg, testDate, []timesheet.EntryRecord{
		overnightEntry("night-1", "work", "23:00", "05:00", "Overnight release"),
	})
	runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	writeLedger(t, cfg, testDate, []timesheet.EntryRecord{
		overnightEntry("night-1", "work", "23:00", "06:00", "Overnight release"),
	})
	receipt := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	if len(receipt.CarriedOut) != 1 || receipt.CarriedOut[0].End != "06:00" {
		t.Fatalf("carried_out = %+v, want the one replaced segment ending 06:00", receipt.CarriedOut)
	}
	target := runStageCapture(t, []string{"--config", cfg, "--date", carryTarget})
	if len(target.CarriedIn) != 1 || target.CarriedIn[0].Start != "03:00" || target.CarriedIn[0].End != "06:00" {
		t.Fatalf("carried_in = %+v, want exactly the replaced segment", target.CarriedIn)
	}

	writeLedger(t, cfg, testDate, []timesheet.EntryRecord{
		overnightEntry("night-1", "work", "23:00", "02:00", "Overnight release"),
	})
	if r := runStageCapture(t, []string{"--config", cfg, "--date", testDate}); len(r.CarriedOut) != 0 {
		t.Fatalf("carried_out = %+v, want none", r.CarriedOut)
	}
	if _, _, err := captureOutput(t, func() error { return runStage([]string{"--config", cfg, "--date", carryTarget}) }); err == nil {
		t.Fatalf("stage of a day with neither a ledger nor a carry succeeded")
	}
}

func TestCarryIntoSavedDayStops(t *testing.T) {
	cfg := writeTestConfigTZ(t, overnightTZ)
	_, before := stageAndSave(t, cfg, carryTarget, []timesheet.EntryRecord{
		carryEntry(carryTarget, "work-10:00-11:00", "work", "10:00", "11:00", "Own morning work"),
	})
	receipt, _ := stageAndSave(t, cfg, testDate, []timesheet.EntryRecord{
		overnightEntry("night-1", "work", "23:00", "05:00", "Overnight release"),
	})
	if !strings.Contains(receipt.CarryNote, carryTarget) {
		t.Fatalf("carry_note = %q, want it to name the saved target day", receipt.CarryNote)
	}
	n := nextDoc(t, "--config", cfg, "--date", carryTarget)
	if n.State != stateStopped || !strings.Contains(n.Reason, "carried from "+testDate) || !strings.Contains(n.Reason, "stage --revise") {
		t.Fatalf("next on the saved target = %s %q, want a stop naming the carry and stage --revise", n.State, n.Reason)
	}
	t.Setenv("HOME", t.TempDir())
	if err := runAgentSubmit([]string{"--config", cfg, "--date", carryTarget}); err == nil ||
		!strings.Contains(err.Error(), "differs from saved intervals") {
		t.Fatalf("submit --chat = %v, want a refusal", err)
	}
	after := readDurableDay(t, cfg, carryTarget)
	if after.Revision != before.Revision || strings.Join(confirmedSpans(after), ",") != strings.Join(confirmedSpans(before), ",") {
		t.Fatalf("the saved target day changed: %v -> %v", confirmedSpans(before), confirmedSpans(after))
	}
	if err := runStage([]string{"--config", cfg, "--date", carryTarget, "--revise"}); err != nil {
		t.Fatalf("stage --revise: %v", err)
	}
	assertSpans(t, confirmedSpans(readDurableDay(t, cfg, carryTarget)), "03:00-05:00", "10:00-11:00")
	assertNotStale(t, nextDoc(t, "--config", cfg, "--date", carryTarget))
}

func TestCarryAfterMidnightStampedBlocks(t *testing.T) {
	cfg := writeTestConfigTZ(t, overnightTZ)
	receipt, source := stageAndSave(t, cfg, testDate, []timesheet.EntryRecord{
		overnightEntry("night-1", "work", "22:00", "05:00", "Overnight release"),
		overnightEntry("meeting-01:00-01:30", "meeting", "2026-10-27T01:00:00+07:00", "2026-10-27T01:30:00+07:00", "Incident bridge"),
		overnightEntry("meeting-04:00-04:30", "meeting", "2026-10-27T04:00:00+07:00", "2026-10-27T04:30:00+07:00", "Handover call"),
	})
	for _, iv := range source.Confirmed {
		if iv.Start < "22:00" {
			t.Fatalf("source day staged %s-%s: a next-date block was placed 24 hours early", iv.Start, iv.End)
		}
	}
	assertSpans(t, confirmedSpans(source), "22:00-02:00")
	if len(receipt.CarriedOut) != 1 || receipt.CarriedOut[0].Start != "02:00" || receipt.CarriedOut[0].End != "05:00" {
		t.Fatalf("carried_out = %+v, want 02:00-05:00", receipt.CarriedOut)
	}
	if !strings.Contains(receipt.CarriedOut[0].Description, "Handover call") {
		t.Fatalf("carried description = %q, want the handover block to follow the carry", receipt.CarriedOut[0].Description)
	}

	cfg2 := writeTestConfigTZ(t, overnightTZ)
	r2, d2 := stageAndSave(t, cfg2, testDate, []timesheet.EntryRecord{
		overnightEntry("work-09:00-10:00", "work", "09:00", "10:00", "Own work"),
		overnightEntry("meeting-01:00-01:30", "meeting", "2026-10-27T01:00:00+07:00", "2026-10-27T01:30:00+07:00", "Early call"),
	})
	assertSpans(t, confirmedSpans(d2), "09:00-10:00")
	if len(r2.CarriedOut) != 1 || r2.CarriedOut[0].Start != "01:00" || r2.CarriedOut[0].End != "01:30" {
		t.Fatalf("carried_out = %+v, want the whole 01:00-01:30 block", r2.CarriedOut)
	}
}

func TestCarryKeepsDayLongSkip(t *testing.T) {
	cfg := writeTestConfigTZ(t, overnightTZ)
	receipt, _ := stageAndSave(t, cfg, testDate, []timesheet.EntryRecord{
		overnightEntry("marathon-1", "work", "2026-10-26T22:00:00+07:00", "2026-10-27T23:00:00+07:00", "Too long"),
		overnightEntry("good-1", "work", "10:00", "11:00", "Clean block"),
	})
	if len(receipt.CarriedOut) != 0 || len(receipt.Skipped) != 1 || !strings.Contains(receipt.Skipped[0].Reason, "24 hours or more") {
		t.Fatalf("receipt = %+v, want the day-long skip and no carry", receipt)
	}
}

// TestCarryDSTAtMidnightStaysLoud: America/Santiago falls back at midnight on
// 2026-04-05, so a pre-midnight cut at 23:00 is ambiguous. No carry is made;
// the entry stages as one span and plan blocks it with remote_entry_too_long.
func TestCarryDSTAtMidnightStaysLoud(t *testing.T) {
	const date, tz = "2026-04-04", "America/Santiago"
	stub := &stubRemote{entries: map[string][]map[string]any{}}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	t.Setenv("SYNTHETIC_GRADION_TOKEN", "synthetic-token")
	cfg := writeTestConfigWithGradionTZ(t, server.URL, tz)
	seedCatalog(t, cfg, "cat-1",
		timesheet.ClassificationOption{ID: "cls-dev", Name: "Development", Code: "DEV", ActivityType: "engineering"},
	)
	receipt, day := stageAndSave(t, cfg, date, []timesheet.EntryRecord{
		carryEntry(date, "night-1", "work", "19:00", "06:00", "Migration window"),
	})
	if len(receipt.CarriedOut) != 0 {
		t.Fatalf("carried_out = %+v, want no carry when a boundary is ambiguous", receipt.CarriedOut)
	}
	assertSpans(t, confirmedSpans(day), "19:00-06:00")
	preview := runPlanCapture(t, []string{"--config", cfg, "--date", date})
	loud := false
	for _, b := range preview.Blockers {
		loud = loud || b.Code == "remote_entry_too_long"
	}
	if !loud {
		t.Fatalf("blockers = %+v, want remote_entry_too_long", preview.Blockers)
	}
}

func writeTestConfigWithGradionTZ(t *testing.T, endpoint, tz string) string {
	t.Helper()
	cfg := writeTestConfigWithGradion(t, endpoint)
	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(strings.Replace(string(b), "Asia/Ho_Chi_Minh", tz, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestCarryNextMatchesStage(t *testing.T) {
	cfg := writeTestConfigTZ(t, overnightTZ)
	entries := []timesheet.EntryRecord{
		overnightEntry("night-1", "work", "20:00", "06:00", "Overnight release"),
		overnightEntry("work-09:00-10:00", "work", "09:00", "10:00", "Morning"),
	}
	_, source := stageAndSave(t, cfg, testDate, entries)
	own := []timesheet.EntryRecord{carryEntry(carryTarget, "work-05:00-07:00", "work", "05:00", "07:00", "Checks")}
	_, target := stageAndSave(t, cfg, carryTarget, own)
	state := statePathFor(cfg)
	for _, c := range []struct {
		date    string
		entries []timesheet.EntryRecord
		day     *timesheet.Day
	}{{testDate, entries, source}, {carryTarget, own, target}} {
		art, err := timesheet.ReadDayArtifacts(state, c.date)
		if err != nil {
			t.Fatal(err)
		}
		units := carryUnits(c.entries, art.CarryIn, c.date, overnightTZ)
		if !stageMapMatchesUnits(art.StageMap, units) || !currentDerives(units, c.day.Confirmed) {
			t.Fatalf("%s: next units %+v do not match the staged/saved day %v", c.date, units, confirmedSpans(c.day))
		}
		assertNotStale(t, nextDoc(t, "--config", cfg, "--date", c.date))
	}
}

func TestCarryUpgradeKeepsSavedDaysCurrent(t *testing.T) {
	entries := []timesheet.EntryRecord{overnightEntry("night-1", "work", "15:00", "05:00", "Overnight release")}
	o := finalizedDay()
	o.Artifacts.Entries = &timesheet.EntriesFile{Date: testDate, Entries: entries}
	o.Units = ledgerUnits(entries, testDate, overnightTZ)
	o.LegacyUnits = legacyDerivations(entries, testDate, overnightTZ)
	o.Workflow.Confirmed = []timesheet.Interval{
		{ID: "iv-1", Date: testDate, Start: "15:00", End: "19:00", Project: "WORK", Description: "Overnight release"},
		{ID: "iv-2", Date: testDate, Start: "19:00", End: "23:00", Project: "WORK", Description: "Overnight release"},
		{ID: "iv-3", Date: testDate, Start: "23:00", End: "05:00", Project: "WORK", Description: "Overnight release"},
	}
	assertNotStale(t, deriveDayNext(o))

	cfg := writeTestConfigTZ(t, overnightTZ)
	_, target := stageAndSave(t, cfg, carryTarget, []timesheet.EntryRecord{
		carryEntry(carryTarget, "work-10:00-11:00", "work", "10:00", "11:00", "Own work"),
	})
	writeLedger(t, cfg, testDate, entries)
	assertNotStale(t, nextDoc(t, "--config", cfg, "--date", carryTarget))
	if after := readDurableDay(t, cfg, carryTarget); after.Revision != target.Revision {
		t.Fatalf("the saved target changed revision")
	}
}

func TestCarryTimezoneMismatchRefused(t *testing.T) {
	cfg := writeTestConfigTZ(t, overnightTZ)
	stageAndSave(t, cfg, testDate, []timesheet.EntryRecord{
		overnightEntry("night-1", "work", "23:00", "05:00", "Overnight release"),
	})
	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte(strings.Replace(string(b), overnightTZ, "Europe/Berlin", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	writeLedger(t, cfg, carryTarget, []timesheet.EntryRecord{
		carryEntry(carryTarget, "work-10:00-11:00", "work", "10:00", "11:00", "Own work"),
	})
	if _, _, err := captureOutput(t, func() error { return runStage([]string{"--config", cfg, "--date", carryTarget}) }); err == nil ||
		!strings.Contains(err.Error(), "timezone") {
		t.Fatalf("stage = %v, want a timezone refusal", err)
	}
	if n := nextDoc(t, "--config", cfg, "--date", carryTarget); n.State != stateStopped || !strings.Contains(n.Reason, "timezone") {
		t.Fatalf("next = %s %q, want a timezone stop", n.State, n.Reason)
	}
}

func TestCarryControlDayWritesNoRecord(t *testing.T) {
	cfg := writeTestConfigTZ(t, overnightTZ)
	receipt, _ := stageAndSave(t, cfg, testDate, []timesheet.EntryRecord{
		overnightEntry("work-09:00-17:00", "work", "09:00", "17:00", "Day"),
		overnightEntry("night-1", "work", "22:00", "02:00", "Short night"),
	})
	if len(receipt.CarriedOut) != 0 {
		t.Fatalf("carried_out = %+v, want none", receipt.CarriedOut)
	}
	if _, err := os.Stat(timesheet.CarryPath(statePathFor(cfg), testDate)); !os.IsNotExist(err) {
		t.Fatalf("a carry record exists for a day with nothing to carry (err %v)", err)
	}
}

func currentDerives(units []ledgerUnit, confirmed []timesheet.Interval) bool {
	current, _ := ledgerDerivesSaved(units, nil, confirmed)
	return current
}
