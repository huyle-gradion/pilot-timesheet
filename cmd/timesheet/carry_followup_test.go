package main

import (
	"os"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func TestCarryStaleSourceStops(t *testing.T) {
	own := []timesheet.EntryRecord{carryEntry(carryTarget, "work-10:00-11:00", "work", "10:00", "11:00", "Own work")}
	stageCmd := "stage --date " + testDate

	t.Run("source edited, not re-staged", func(t *testing.T) {
		cfg := writeTestConfigTZ(t, overnightTZ)
		writeLedger(t, cfg, testDate, []timesheet.EntryRecord{overnightEntry("night-1", "work", "23:00", "05:00", "Overnight release")})
		runStageCapture(t, []string{"--config", cfg, "--date", testDate})
		writeLedger(t, cfg, testDate, []timesheet.EntryRecord{overnightEntry("night-1", "work", "23:00", "02:00", "Overnight release")})
		writeLedger(t, cfg, carryTarget, own)
		if _, _, err := captureOutput(t, func() error { return runStage([]string{"--config", cfg, "--date", carryTarget}) }); err == nil ||
			!strings.Contains(err.Error(), stageCmd) {
			t.Fatalf("stage of the target = %v, want a refusal naming %q", err, stageCmd)
		}
		if err := runSave([]string{"--config", cfg, "--date", carryTarget}); err == nil {
			t.Fatalf("save of the target accepted a stale carry")
		}
		if n := nextDoc(t, "--config", cfg, "--date", carryTarget); n.State != stateStopped || !strings.Contains(n.Reason, stageCmd) {
			t.Fatalf("next = %s %q, want a stop naming %q", n.State, n.Reason, stageCmd)
		}
		runStageCapture(t, []string{"--config", cfg, "--date", testDate})
		if r := runStageCapture(t, []string{"--config", cfg, "--date", carryTarget}); len(r.CarriedIn) != 0 {
			t.Fatalf("carried_in = %+v after the source re-stage, want none", r.CarriedIn)
		}
	})

	t.Run("source edited after the target saved", func(t *testing.T) {
		cfg := writeTestConfigTZ(t, overnightTZ)
		stageAndSave(t, cfg, testDate, []timesheet.EntryRecord{overnightEntry("night-1", "work", "23:00", "05:00", "Overnight release")})
		stageAndSave(t, cfg, carryTarget, own)
		writeLedger(t, cfg, testDate, []timesheet.EntryRecord{overnightEntry("night-1", "work", "23:00", "02:00", "Overnight release")})
		if n := nextDoc(t, "--config", cfg, "--date", carryTarget); n.State != stateStopped || !strings.Contains(n.Reason, testDate) {
			t.Fatalf("next = %s %q, want a stop naming the source", n.State, n.Reason)
		}
		t.Setenv("HOME", t.TempDir())
		if err := runAgentSubmit([]string{"--config", cfg, "--date", carryTarget}); err == nil || !strings.Contains(err.Error(), testDate) {
			t.Fatalf("submit --chat = %v, want a refusal naming the source", err)
		}
	})

	t.Run("source deleted", func(t *testing.T) {
		cfg := writeTestConfigTZ(t, overnightTZ)
		writeLedger(t, cfg, testDate, []timesheet.EntryRecord{overnightEntry("night-1", "work", "23:00", "05:00", "Overnight release")})
		runStageCapture(t, []string{"--config", cfg, "--date", testDate})
		if err := os.Remove(timesheet.EntriesPath(statePathFor(cfg), testDate)); err != nil {
			t.Fatal(err)
		}
		writeLedger(t, cfg, carryTarget, own)
		if _, _, err := captureOutput(t, func() error { return runStage([]string{"--config", cfg, "--date", carryTarget}) }); err == nil ||
			!strings.Contains(err.Error(), "no entries ledger") {
			t.Fatalf("stage of the target = %v, want the missing-source refusal", err)
		}
		if n := nextDoc(t, "--config", cfg, "--date", carryTarget); n.State != stateStopped || !strings.Contains(n.Reason, "no entries ledger") {
			t.Fatalf("next = %s %q, want the missing-source stop", n.State, n.Reason)
		}
	})
}

func TestSubmitChatChecksOwnCarryRecord(t *testing.T) {
	cfg := writeTestConfigTZ(t, overnightTZ)
	stageAndSave(t, cfg, testDate, []timesheet.EntryRecord{overnightEntry("night-1", "work", "23:00", "05:00", "Overnight release")})
	if err := os.Remove(timesheet.CarryPath(statePathFor(cfg), testDate)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	if err := runAgentSubmit([]string{"--config", cfg, "--date", testDate}); err == nil || !strings.Contains(err.Error(), "carry record") {
		t.Fatalf("submit --chat = %v, want the carry-record refusal", err)
	}
}

func TestCarryFridayIntoSaturday(t *testing.T) {
	const friday, saturday, monday = "2026-10-30", "2026-10-31", "2026-11-02"
	cfg := writeTestConfigTZ(t, overnightTZ)
	night := carryEntry(friday, "night-1", "work", "20:00", "06:00", "Release night")
	stageAndSave(t, cfg, friday, []timesheet.EntryRecord{night})

	loaded, err := timesheet.LoadConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	hasCarry := func(d string) bool {
		in, err := timesheet.ReadCarryInto(loaded.StatePath, d)
		return err == nil && len(in) > 0
	}
	if next, ok := carryFollowUp(friday, monday, true, []string{friday}, hasCarry); !ok || next != saturday {
		t.Fatalf("follow-up after Friday = %q %v, want Saturday", next, ok)
	}
	if _, ok := carryFollowUp(friday, saturday, false, []string{friday}, hasCarry); ok {
		t.Fatalf("Saturday was followed up before log_after on Saturday itself")
	}
	if _, ok := carryFollowUp(saturday, monday, true, []string{friday, saturday}, hasCarry); ok {
		t.Fatalf("Sunday followed up without a carry")
	}

	if n, m := dayLedgerTotals(loaded, friday); n != 1 || m != 240 {
		t.Fatalf("Friday totals = %d entries, %d min; want 1 entry, 240 min staged", n, m)
	}
	stageAndSave(t, cfg, saturday, nil)
	if n, m := dayLedgerTotals(loaded, saturday); n != 1 || m != 360 {
		t.Fatalf("Saturday totals = %d entries, %d min; want the 1 carried segment, 360 min", n, m)
	}

	art, err := timesheet.ReadDayArtifacts(loaded.StatePath, saturday)
	if err != nil {
		t.Fatal(err)
	}
	var head, list, tail strings.Builder
	a := nextAction{Day: saturday, State: stateHumanGates, Human: &nextHumanGates{Carried: carriedView(art.CarryIn)}}
	renderHumanGates(&head, &list, &tail, a, nil, stdoutStyles())
	if !strings.Contains(list.String(), "carried "+friday+" -> "+saturday) || !strings.Contains(list.String(), "00:00-06:00 (360 min)") {
		t.Fatalf("gate view = %q, want the carried segment", list.String())
	}
}
