package timesheet

// The workflow snapshot is ONE read-only view over facts the binary already
// persists, for the `next` planner: the saved day's intervals, the
// per-source latest attempt (including the acknowledgment flag the status
// surface does not carry), the newest immutable plan record for the date, the
// date's latest remote observation, the newest per-date resolution, and the
// unresolved-write counters.
//
// It defines NO new durable data and mutates nothing: every field is a plain
// read of Store.State() / Store.Evidence(). It is a derivation aid, never a
// safety device — the authoritative gates still live inside the verbs
// (checkSourceGate runs inside confirm/save; openFinalizeDay refuses a
// finalized day), so no derivation can bypass them.

import (
	"maps"
	"sort"
	"strings"
	"time"
)

// statusUnattempted mirrors the status surface's value for an enabled source
// with no recorded attempt (session.go's daySourceStatuses).
const statusUnattempted = "unattempted"

// SourceSnapshot is one enabled source's latest attempt for the date. The same
// shape daySourceStatuses reports, plus the durable Acknowledged flag.
type SourceSnapshot struct {
	Source       string
	Status       string
	CollectionID string
	Attempted    bool
	Complete     bool
	Acknowledged bool
}

// PlanSnapshot is the newest durable plan record for one date.
type PlanSnapshot struct {
	PlanID    string
	State     string
	CreatedAt string
	Blockers  []Blocker
}

// ObservationSnapshot is the date's persisted latest remote observation.
type ObservationSnapshot struct {
	ObservationID string
	ObservedAt    string
	Complete      bool
	Failed        bool
}

// ResolutionSnapshot is the newest explicit human resolution for one date.
type ResolutionSnapshot struct {
	ObservationID string
	Decision      string
	At            string
}

// WorkflowSnapshot is every durable fact the planner derives from.
type WorkflowSnapshot struct {
	ConfiguredDefaultPresent bool
	ConfiguredDefaultError   string
	Date                     string
	Revision                 int
	Finalized                bool
	Confirmed                []Interval
	Sources                  []SourceSnapshot
	Plan                     *PlanSnapshot
	// PlanCurrent binds the newest plan to the exact current contribution origins
	// and revisions, including prior-day intervals allocating onto this date.
	PlanCurrent bool
	// PlanAmbiguous is true when two plan records for the date share the
	// newest CreatedAt; recency is then underivable, and the planner must stop
	// rather than guess which record is current.
	PlanAmbiguous    bool
	Observation      *ObservationSnapshot
	Resolution       *ResolutionSnapshot
	UnresolvedWrites int
	NativeUnresolved int
	// SubmittedHistory counts other days' provider-confirmed interval
	// submissions; zero means there is no history to classify from (#235).
	SubmittedHistory int
	// Timezone is the day's recorded timezone, or the configured one for a day
	// with no durable record yet: exactly what open_day reports, so a read-only
	// derivation chunks overnight spans as stage does (#176).
	Timezone string
}

// ReadWorkflowSnapshot reads the day's durable facts. An unparsable durable
// timestamp is an error: the caller stops with the reason rather than
// inventing an ordering.
func ReadWorkflowSnapshot(cfg *Config, store *Store, date string) (WorkflowSnapshot, error) {
	snap := WorkflowSnapshot{Date: date, Timezone: cfg.Timezone}
	state := store.State()
	snap.ConfiguredDefaultPresent = cfg.Gradion != nil && cfg.Gradion.DefaultClassification != nil
	if snap.ConfiguredDefaultPresent {
		if _, err := resolveConfiguredDefault(cfg, state); err != nil {
			snap.ConfiguredDefaultError = err.Error()
		}
	}
	snap.SubmittedHistory = len(submittedSelections(state, date))
	if day := state.Days[date]; day != nil {
		snap.Timezone = day.Timezone
		snap.Revision = day.Revision
		snap.Confirmed = append([]Interval(nil), day.Confirmed...)
		snap.Finalized = len(snap.Confirmed) > 0
	}

	githubOn, calendarOn := cfg.EnabledSources()
	ev := store.Evidence(date)
	for _, src := range []struct {
		name    string
		enabled bool
	}{
		{SourceGithub, githubOn},
		{SourceCalendar, calendarOn},
	} {
		if !src.enabled {
			continue
		}
		a := latestAttempt(ev, src.name)
		if a == nil {
			snap.Sources = append(snap.Sources, SourceSnapshot{Source: src.name, Status: statusUnattempted})
			continue
		}
		snap.Sources = append(snap.Sources, SourceSnapshot{
			Source:       src.name,
			Status:       a.Status,
			CollectionID: a.CollectionID,
			Attempted:    true,
			Complete:     a.Status == StatusComplete,
			Acknowledged: a.Acknowledged,
		})
	}

	plan, ambiguous, err := newestPlanForDate(state.Plans, date)
	if err != nil {
		return WorkflowSnapshot{}, err
	}
	snap.Plan, snap.PlanAmbiguous = plan, ambiguous
	if plan != nil {
		contributions, err := collectContributions(state, date)
		if err != nil {
			return WorkflowSnapshot{}, err
		}
		snap.PlanCurrent = maps.Equal(state.Plans[plan.PlanID].SourceRevisions, contributionRevisions(contributions))
	}

	if state.Remote != nil {
		if o := state.Remote.Observations[date]; o != nil {
			snap.Observation = &ObservationSnapshot{
				ObservationID: o.ObservationID, ObservedAt: o.ObservedAt,
				Complete: o.Complete, Failed: o.Failed,
			}
		}
		var newest time.Time
		for _, r := range state.Remote.Resolutions {
			if r.Date != date {
				continue
			}
			at, err := time.Parse(time.RFC3339, r.At)
			if err != nil {
				return WorkflowSnapshot{}, newError(CodeStateInvalid,
					"local state carries a remote resolution for "+date+" with an unreadable timestamp")
			}
			if snap.Resolution == nil || !at.Before(newest) {
				newest = at
				snap.Resolution = &ResolutionSnapshot{ObservationID: r.ObservationID, Decision: r.Decision, At: r.At}
			}
		}
		for _, o := range state.Remote.Outcomes {
			if outcomeBlocks(o) {
				snap.UnresolvedWrites++
			}
		}
	}
	for i := range state.Intents {
		if state.Intents[i].unresolved() {
			snap.NativeUnresolved++
		}
	}
	return snap, nil
}

// newestPlanForDate returns the newest immutable plan record for one date. The
// pick is deterministic (lowest PlanID) even when two records share the newest
// CreatedAt, but ambiguous is then true: recency is underivable and the caller
// stops instead of reusing or re-approving a stale plan_id.
func newestPlanForDate(plans map[string]*PlanRecord, date string) (*PlanSnapshot, bool, error) {
	var newest *PlanRecord
	var newestAt time.Time
	for _, p := range plans {
		if p == nil || p.Date != date {
			continue
		}
		at, err := time.Parse(time.RFC3339, p.CreatedAt)
		if err != nil {
			return nil, false, newError(CodeStateInvalid,
				"local state carries a plan for "+date+" with an unreadable created_at")
		}
		switch {
		case newest == nil, at.After(newestAt):
			newest, newestAt = p, at
		case at.Equal(newestAt) && p.PlanID < newest.PlanID:
			newest = p
		}
	}
	if newest == nil {
		return nil, false, nil
	}
	ambiguous := false
	for _, p := range plans {
		if p == nil || p.Date != date || p.PlanID == newest.PlanID {
			continue
		}
		at, err := time.Parse(time.RFC3339, p.CreatedAt)
		if err != nil {
			return nil, false, newError(CodeStateInvalid,
				"local state carries a plan for "+date+" with an unreadable created_at")
		}
		if at.Equal(newestAt) {
			ambiguous = true
		}
	}
	return &PlanSnapshot{
		PlanID:    newest.PlanID,
		State:     newest.State,
		CreatedAt: newest.CreatedAt,
		Blockers:  append([]Blocker(nil), newest.Blockers...),
	}, ambiguous, nil
}

// DayArtifacts is the day's ledger and its two sidecars, each already owned by
// the binary and each loaded by its authoritative reader: entries/<date>.json,
// .stage.json and .classify.json. Every file is read and validated exactly
// once; a missing file is reported as absent and a corrupt one is an error, so
// the planner never works from a second, possibly stale, read.
type DayArtifacts struct {
	EntriesPresent  bool
	Entries         *EntriesFile
	StageMapPresent bool
	StageMap        map[string][]string
	ChoicesPresent  bool
	Choices         map[string]ClassificationChoiceEntry
}

// ReadDayArtifacts loads the day's ledger and sidecars through the same readers
// the verbs use. Presence is content-presence, not a timestamp: the planner
// compares the actual blocks and selections.
func ReadDayArtifacts(statePath, date string) (DayArtifacts, error) {
	var out DayArtifacts
	entries, err := ReadEntries(statePath, date)
	if err != nil {
		return DayArtifacts{}, err
	}
	out.Entries, out.EntriesPresent = entries, entries != nil

	stageMap, err := ReadStageMap(statePath, date)
	if err != nil {
		return DayArtifacts{}, err
	}
	out.StageMap = stageMap

	choices, err := ReadClassificationChoices(statePath, date)
	if err != nil {
		return DayArtifacts{}, err
	}
	out.Choices, out.ChoicesPresent = choices, choices != nil
	return out, nil
}

// StagedLedgerDigest binds a classification judgment answer to the exact staged
// content it was chosen for: the SHA-256 hex of one line per staged interval,
// sorted by interval id, each carrying the interval id and its block content
// digest. Any change to any staged block — a description edit, a time edit, a
// new or removed block, a different merge — changes the digest. The model
// copies it verbatim from the judgment report into the answer file, so a stale
// answer file can never be handed to classify.
func StagedLedgerDigest(date string, blocks map[string]EntryRecord) string {
	ids := make([]string, 0, len(blocks))
	for id := range blocks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	for _, id := range ids {
		b.WriteString(id)
		b.WriteString(" ")
		b.WriteString(BlockContentDigest(date, blocks[id]))
		b.WriteString("\n")
	}
	return sha256Hex([]byte(b.String()))
}
