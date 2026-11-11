package timesheet

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

const snapshotDate = "2026-10-27"

func snapshotStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := t.TempDir() + "/state.json"
	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, path
}

func snapshotConfig(path string) *Config {
	return &Config{
		Timezone:  "Asia/Ho_Chi_Minh",
		StatePath: path,
		Sources: &SourcesConfig{
			Enabled:  true,
			Github:   &GitHubSourceConfig{Enabled: true, User: "u", Repositories: []string{"acme/api"}},
			Calendar: &CalendarSourceConfig{Enabled: true},
		},
	}
}

func sampleEntry(blockID, start, end, description string) EntryRecord {
	return EntryRecord{
		BlockID: blockID, Date: snapshotDate, Start: start, End: end,
		Description: description, Kind: "work",
	}
}

func snapshotDay(date string) *Day {
	return &Day{
		Date: date, Timezone: "Asia/Ho_Chi_Minh", Revision: 2, NextInterval: 2,
		Confirmed: []Interval{{
			ID: "iv-1", Date: date, Start: "09:00", End: "10:00",
			Project: "WORK", Description: "Synthetic work",
		}},
		History: []HistoryEntry{{
			Revision: 2, ConfirmedAt: "2026-10-27T10:00:00Z", Intervals: 1, TotalMinutes: 60,
		}},
	}
}

func TestReadWorkflowSnapshotExposesDurableFacts(t *testing.T) {
	store, path := snapshotStore(t)
	cfg := snapshotConfig(path)
	state := &State{
		Days: map[string]*Day{snapshotDate: snapshotDay(snapshotDate)},
		Remote: &RemoteState{
			Observations: map[string]*RemoteObservation{
				snapshotDate: {ObservationID: "obs-2", Date: snapshotDate, ObservedAt: "2026-10-27T10:06:00Z", Complete: true},
				"2026-10-20": {ObservationID: "obs-old", Date: "2026-10-20", ObservedAt: "2026-10-20T10:00:00Z"},
			},
			Resolutions: []RemoteResolution{
				{Date: snapshotDate, At: "2026-10-27T10:02:00Z", Decision: DecisionForeign, ObservationID: "obs-1", RemoteID: "r-1"},
				{Date: snapshotDate, At: "2026-10-27T10:07:00Z", Decision: DecisionAdopt, ObservationID: "obs-2", RemoteID: "r-2", LocalID: "iv-1"},
				{Date: "2026-10-20", At: "2026-10-20T11:00:00Z", Decision: DecisionForeign, ObservationID: "obs-old", RemoteID: "r-old"},
			},
		},
		Plans: map[string]*PlanRecord{
			"plan-old": {PlanID: "plan-old", Date: "2026-10-20", State: PlanReady, CreatedAt: "2026-10-20T10:00:00Z"},
			"plan-1":   {PlanID: "plan-1", Date: snapshotDate, State: PlanBlocked, CreatedAt: "2026-10-27T10:01:00Z", Blockers: []Blocker{{Code: "unresolved_identity"}}},
			"plan-2": {PlanID: "plan-2", Date: snapshotDate, State: PlanBlocked, CreatedAt: "2026-10-27T10:05:00Z",
				Blockers: []Blocker{{Code: "unresolved_identity", LocalID: "iv-1"}}},
		},
	}
	if err := store.CommitState(state); err != nil {
		t.Fatalf("commit synthetic state: %v", err)
	}
	if err := store.CommitEvidence(snapshotDate, &DayEvidence{Attempts: []SourceAttempt{
		{Source: SourceGithub, CollectionID: "col-gh", Status: StatusIncomplete, AttemptedAt: "2026-10-27T09:00:00Z"},
		{Source: SourceCalendar, CollectionID: "col-cal", Status: StatusComplete, AttemptedAt: "2026-10-27T09:00:00Z", Acknowledged: true},
	}}); err != nil {
		t.Fatalf("commit evidence: %v", err)
	}
	_ = store.Close()
	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	snap, err := ReadWorkflowSnapshot(cfg, reopened, snapshotDate)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !snap.Finalized || len(snap.Confirmed) != 1 || snap.Confirmed[0].Description != "Synthetic work" {
		t.Fatalf("day facts = %+v, want the durable finalized day's intervals", snap)
	}
	if snap.Plan == nil || snap.Plan.PlanID != "plan-2" || snap.PlanAmbiguous {
		t.Fatalf("plan = %+v ambiguous=%v, want the newest plan for the date", snap.Plan, snap.PlanAmbiguous)
	}
	if len(snap.Plan.Blockers) != 1 || snap.Plan.Blockers[0].LocalID != "iv-1" {
		t.Fatalf("blockers = %+v, want the newest record's blockers", snap.Plan.Blockers)
	}
	if snap.Observation == nil || snap.Observation.ObservationID != "obs-2" || !snap.Observation.Complete {
		t.Fatalf("observation = %+v, want this date's persisted observation", snap.Observation)
	}
	if snap.Resolution == nil || snap.Resolution.At != "2026-10-27T10:07:00Z" || snap.Resolution.Decision != DecisionAdopt {
		t.Fatalf("resolution = %+v, want the newest resolution for the date", snap.Resolution)
	}

	wantSources := []SourceSnapshot{
		{Source: SourceGithub, Status: StatusIncomplete, CollectionID: "col-gh", Attempted: true},
		{Source: SourceCalendar, Status: StatusComplete, CollectionID: "col-cal", Attempted: true, Complete: true, Acknowledged: true},
	}
	if !reflect.DeepEqual(snap.Sources, wantSources) {
		t.Fatalf("sources = %+v, want %+v", snap.Sources, wantSources)
	}
}

func TestReadWorkflowSnapshotUnattemptedAndDisabledSources(t *testing.T) {
	store, path := snapshotStore(t)
	cfg := snapshotConfig(path)
	if err := store.CommitState(&State{Days: map[string]*Day{}}); err != nil {
		t.Fatalf("commit state: %v", err)
	}
	snap, err := ReadWorkflowSnapshot(cfg, store, snapshotDate)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Finalized || snap.Plan != nil || snap.Observation != nil || snap.Resolution != nil {
		t.Fatalf("snapshot = %+v, want the empty-day facts", snap)
	}
	want := []SourceSnapshot{
		{Source: SourceGithub, Status: "unattempted"},
		{Source: SourceCalendar, Status: "unattempted"},
	}
	if !reflect.DeepEqual(snap.Sources, want) {
		t.Fatalf("sources = %+v, want every enabled source reported unattempted", snap.Sources)
	}

	cfg.Sources = nil
	snap, err = ReadWorkflowSnapshot(cfg, store, snapshotDate)
	if err != nil {
		t.Fatalf("snapshot without sources: %v", err)
	}
	if len(snap.Sources) != 0 {
		t.Fatalf("sources = %+v, want none for a config with no enabled source", snap.Sources)
	}
}

func TestReadWorkflowSnapshotPlanTieIsNamed(t *testing.T) {
	store, path := snapshotStore(t)
	cfg := snapshotConfig(path)
	if err := store.CommitState(&State{
		Days: map[string]*Day{},
		Plans: map[string]*PlanRecord{
			"plan-a": {PlanID: "plan-a", Date: snapshotDate, State: PlanBlocked, CreatedAt: "2026-10-27T10:00:00Z"},
			"plan-b": {PlanID: "plan-b", Date: snapshotDate, State: PlanReady, CreatedAt: "2026-10-27T10:00:00Z"},
		},
	}); err != nil {
		t.Fatalf("commit state: %v", err)
	}
	snap, err := ReadWorkflowSnapshot(cfg, store, snapshotDate)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !snap.PlanAmbiguous {
		t.Fatalf("snapshot = %+v, want the same-timestamp plans flagged ambiguous rather than guessed", snap)
	}
	if snap.Plan == nil || snap.Plan.PlanID != "plan-a" {
		t.Fatalf("plan = %+v, want the deterministic pick plan-a", snap.Plan)
	}
}

func TestReadWorkflowSnapshotRejectsUnparsableTimestamp(t *testing.T) {
	store, path := snapshotStore(t)
	cfg := snapshotConfig(path)
	if err := store.CommitState(&State{
		Days:  map[string]*Day{},
		Plans: map[string]*PlanRecord{"plan-x": {PlanID: "plan-x", Date: snapshotDate, State: PlanReady, CreatedAt: "not-a-time"}},
	}); err != nil {
		t.Fatalf("commit state: %v", err)
	}
	if _, err := ReadWorkflowSnapshot(cfg, store, snapshotDate); err == nil {
		t.Fatal("an unparsable plan timestamp must fail the derivation instead of being guessed")
	}
}

func TestReadDayArtifactsParsesContentOnce(t *testing.T) {
	_, path := snapshotStore(t)
	empty, err := ReadDayArtifacts(path, snapshotDate)
	if err != nil {
		t.Fatalf("artifacts: %v", err)
	}
	if empty.EntriesPresent || empty.ChoicesPresent ||
		empty.Entries != nil || empty.StageMap != nil || empty.Choices != nil {
		t.Fatalf("artifacts = %+v, want every artifact absent", empty)
	}

	if _, err := RecordEntries(path, AppVersion, ReviewedDraft{Date: snapshotDate, Blocks: []ReviewedBlock{
		{DraftBlock: DraftBlock{BlockID: "work-1", Kind: "work", Start: "09:00", End: "10:00"}, Description: "Synthetic work"},
	}}, false, time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := WriteStageMap(path, AppVersion, snapshotDate, map[string][]string{"work-1": {"iv-1"}},
		time.Date(2026, 10, 27, 9, 5, 0, 0, time.UTC)); err != nil {
		t.Fatalf("stage map: %v", err)
	}
	if err := WriteClassificationChoices(path, AppVersion, snapshotDate,
		map[string]ClassificationChoiceEntry{"iv-1": {CatalogID: "cat-1", ClassificationID: "cls-1", ContentDigest: "digest"}},
		time.Date(2026, 10, 27, 9, 10, 0, 0, time.UTC)); err != nil {
		t.Fatalf("choices: %v", err)
	}

	got, err := ReadDayArtifacts(path, snapshotDate)
	if err != nil {
		t.Fatalf("artifacts: %v", err)
	}
	if !got.EntriesPresent || len(got.Entries.Entries) != 1 || got.Entries.Entries[0].Description != "Synthetic work" {
		t.Fatalf("entries = %+v, want one recorded block", got.Entries)
	}
	if !reflect.DeepEqual(got.StageMap, map[string][]string{"work-1": {"iv-1"}}) {
		t.Fatalf("stage map = %+v, want the stored mapping", got.StageMap)
	}
	if !got.ChoicesPresent || len(got.Choices) != 1 || got.Choices["iv-1"].ClassificationID != "cls-1" {
		t.Fatalf("choices = %+v, want the stored selections", got.Choices)
	}

	if err := os.WriteFile(StagePath(path, snapshotDate), []byte(`{"date":"2026-10-27"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDayArtifacts(path, snapshotDate); err == nil {
		t.Fatal("a corrupt stage map must fail the observation instead of being ignored")
	}
}

func TestStagedLedgerDigestBindsContent(t *testing.T) {
	blocks := map[string]EntryRecord{
		"iv-1": sampleEntry("work-1", "09:00", "10:00", "Build the bridge"),
		"iv-2": sampleEntry("work-2", "11:00", "12:00", "Review the deck"),
	}
	base := StagedLedgerDigest(snapshotDate, blocks)
	if len(base) != 64 {
		t.Fatalf("digest = %q, want a sha256 hex binding", base)
	}
	if again := StagedLedgerDigest(snapshotDate, blocks); again != base {
		t.Fatalf("digest is not deterministic: %q vs %q", base, again)
	}
	reordered := map[string]EntryRecord{"iv-2": blocks["iv-2"], "iv-1": blocks["iv-1"]}
	if got := StagedLedgerDigest(snapshotDate, reordered); got != base {
		t.Fatalf("digest depends on map order: %q vs %q", got, base)
	}

	edited := map[string]EntryRecord{
		"iv-1": sampleEntry("work-1", "09:00", "10:00", "Build the bridge, edited"),
		"iv-2": blocks["iv-2"],
	}
	if got := StagedLedgerDigest(snapshotDate, edited); got == base {
		t.Fatal("editing a description must change the ledger digest")
	}
	moved := map[string]EntryRecord{
		"iv-1": sampleEntry("work-1", "09:30", "10:30", "Build the bridge"),
		"iv-2": blocks["iv-2"],
	}
	if got := StagedLedgerDigest(snapshotDate, moved); got == base {
		t.Fatal("editing a time must change the ledger digest")
	}
	shorter := map[string]EntryRecord{"iv-1": blocks["iv-1"]}
	if got := StagedLedgerDigest(snapshotDate, shorter); got == base {
		t.Fatal("removing a staged interval must change the ledger digest")
	}
	if empty := StagedLedgerDigest(snapshotDate, map[string]EntryRecord{}); empty == base || empty == "" {
		t.Fatalf("empty digest = %q, want a distinct stable value", empty)
	}
}

func TestReadWorkflowSnapshotDoesNotMutateState(t *testing.T) {
	store, path := snapshotStore(t)
	cfg := snapshotConfig(path)
	if err := store.CommitState(&State{
		Days:  map[string]*Day{snapshotDate: snapshotDay(snapshotDate)},
		Plans: map[string]*PlanRecord{"plan-1": {PlanID: "plan-1", Date: snapshotDate, State: PlanReady, CreatedAt: "2026-10-27T10:05:00Z"}},
	}); err != nil {
		t.Fatalf("commit state: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadWorkflowSnapshot(cfg, store, snapshotDate); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(before)) != strings.TrimSpace(string(after)) {
		t.Fatalf("the read-only snapshot rewrote durable state:\n%s", strings.TrimSpace(string(after)))
	}
}
