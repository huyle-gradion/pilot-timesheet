package timesheet

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

type ReconcileMatch struct {
	LocalID         string `json:"local_id"`
	RemoteID        string `json:"remote_id,omitempty"`
	Date            string `json:"date,omitempty"`
	StartTime       string `json:"start_time,omitempty"`
	EndTime         string `json:"end_time,omitempty"`
	Changed         bool   `json:"changed,omitempty"`
	Accepted        bool   `json:"accepted,omitempty"`
	ContentObserved bool   `json:"content_observed,omitempty"`
}

// ReconcileCandidate is an unbound remote entry that overlaps a local interval or
// repeats its text. It is a review suggestion, never a match.
type ReconcileCandidate struct {
	LocalID  string `json:"local_id"`
	RemoteID string `json:"remote_id"`
	Reason   string `json:"reason"`
}

type ReconcileReceipt struct {
	Date               string               `json:"date"`
	Revision           int                  `json:"revision"`
	ObservationID      string               `json:"observation_id"`
	ObservedAt         string               `json:"observed_at"`
	Complete           bool                 `json:"complete"`
	Failed             bool                 `json:"failed,omitempty"`
	Detail             string               `json:"detail,omitempty"`
	Coverage           []string             `json:"coverage,omitempty"`
	Mapped             []ReconcileMatch     `json:"mapped"`
	Changed            []ReconcileMatch     `json:"changed"`
	Missing            []ReconcileMatch     `json:"missing"`
	Unbound            []RemoteEntry        `json:"unbound"`
	Candidates         []ReconcileCandidate `json:"candidates"`
	Orphaned           []ReconcileMatch     `json:"orphaned"`
	UnresolvedOutcomes []Blocker            `json:"unresolved_outcomes,omitempty"`
	Blockers           []Blocker            `json:"blockers,omitempty"`
	Ready              bool                 `json:"ready"`
}

type ResolutionReceipt struct {
	Date          string `json:"date"`
	Revision      int    `json:"revision"`
	ObservationID string `json:"observation_id"`
	Decision      string `json:"decision"`
	RemoteID      string `json:"remote_id"`
	LocalID       string `json:"local_id,omitempty"`
	Bound         bool   `json:"bound"`
	Complete      bool   `json:"observation_complete"`
}

// PlanPreview is the exact rendered plan. Approved and approval_available are
// DERIVED approval-ledger facts (R4): approved reports whether an unconsumed,
// unrevoked native approval currently binds this exact plan_id;
// approval_available reports whether the plan is ready to be approved. They are
// never durable authorization flags on a plan record.
type PlanPreview struct {
	Date               string            `json:"date"`
	PlanID             string            `json:"plan_id"`
	State              string            `json:"state"`
	RemoteComplete     bool              `json:"remote_complete"`
	RemoteObservation  string            `json:"remote_observation,omitempty"`
	RemoteObservations map[string]string `json:"remote_observations,omitempty"`
	RemoteFingerprint  string            `json:"remote_fingerprint,omitempty"`
	SourceRevisions    map[string]int    `json:"source_revisions,omitempty"`
	Operations         []PlanOperation   `json:"operations"`
	Blockers           []Blocker         `json:"blockers"`
	SurfacedRemotes    []RemoteEntry     `json:"surfaced_remotes,omitempty"`
	UnresolvedOutcomes []Blocker         `json:"unresolved_outcomes,omitempty"`
	Approved           bool              `json:"approved"`
	ApprovalAvailable  bool              `json:"approval_available"`
	Persisted          bool              `json:"persisted"`
}

// contributingInterval is one confirmed interval allocation that renders on the
// previewed date. The origin interval identity and revision are retained so a
// cross-midnight allocation never collapses two dates onto one mapping.
type contributingInterval struct {
	OriginDate     string
	OriginRevision int
	Timezone       string
	Interval       Interval
	Allocation     Allocation
}

// collectContributions returns every durable confirmed interval that allocates
// onto date, including intervals that started on an earlier day.
func collectContributions(st *State, date string) ([]contributingInterval, error) {
	var out []contributingInterval
	dates := make([]string, 0, len(st.Days))
	for d := range st.Days {
		dates = append(dates, d)
	}
	sort.Strings(dates)
	for _, d := range dates {
		day := st.Days[d]
		loc, err := dayLocation(day.Timezone)
		if err != nil {
			return nil, err
		}
		for _, iv := range day.Confirmed {
			allocations, err := iv.ResolvedAllocations(loc)
			if err != nil {
				return nil, err
			}
			for _, a := range allocations {
				if a.Date != date {
					continue
				}
				out = append(out, contributingInterval{
					OriginDate: d, OriginRevision: day.Revision, Timezone: day.Timezone,
					Interval: iv.clone(), Allocation: a,
				})
			}
		}
	}
	return out, nil
}

func contributionRevisions(contributions []contributingInterval) map[string]int {
	revisions := make(map[string]int)
	for _, c := range contributions {
		revisions[c.OriginDate] = c.OriginRevision
	}
	return revisions
}

// selectedChoiceFor resolves one interval's explicit selection reference into
// the semantic choice it bound (R4F1). The referenced STORED catalog is only
// the historical rendering the operator selected from; it binds what was
// selected, never current truth. Stored/live metadata alone is a retrieval
// hint, so a missing or semantically incomplete reference fails closed and
// requires explicit reselection instead of a name/code/default substitution.
// A nil result without a blocker means the interval carries no selection and
// the caller's own classification check already skips it.
func selectedChoiceFor(st *State, iv Interval) (*ClassificationChoice, *Blocker) {
	if strings.TrimSpace(iv.RemoteClassificationID) == "" {
		return nil, nil
	}
	bad := func(detail string) *Blocker {
		return &Blocker{Code: CodeSelectionUnverifiable, Date: iv.Date, LocalID: iv.ID,
			Detail: detail}
	}
	if st.Remote == nil || strings.TrimSpace(iv.RemoteCatalogID) == "" {
		return nil, bad("no stored selection catalog backs this interval's classification; select_classification again")
	}
	cat := st.Remote.Catalogs[iv.RemoteCatalogID]
	if cat == nil {
		return nil, bad("the stored selection reference is gone (pruned or unknown catalog); select_classification again")
	}
	for _, opt := range cat.Options {
		if opt.ID != iv.RemoteClassificationID {
			continue
		}
		choice := choiceFromOption(opt)
		if choice.Name == "" || choice.Code == "" || choice.ActivityType == "" {
			return nil, bad("the selected option's stored semantics (name/code/activity type) cannot be established; select_classification again")
		}
		return &choice, nil
	}
	return nil, bad("the selected classification is not part of the referenced stored catalog; select_classification again")
}

func confirmedIntervalByID(st *State, id string) (Interval, string, int, bool) {
	dates := make([]string, 0, len(st.Days))
	for d := range st.Days {
		dates = append(dates, d)
	}
	sort.Strings(dates)
	for _, d := range dates {
		day := st.Days[d]
		for _, iv := range day.Confirmed {
			if iv.ID == id {
				return iv.clone(), d, day.Revision, true
			}
		}
	}
	return Interval{}, "", 0, false
}

// bindingFor returns the stable binding for one ALLOCATION: the origin interval
// identity together with the local date that allocation renders on. A
// cross-midnight interval has one binding per allocation date, so an interval
// identity alone is not a remote identity.
func bindingFor(r *RemoteState, localID, date string) (int, *RemoteBinding) {
	if r == nil {
		return -1, nil
	}
	for i := range r.Bindings {
		if r.Bindings[i].LocalID == localID && r.Bindings[i].Date == date {
			return i, &r.Bindings[i]
		}
	}
	return -1, nil
}

// bindingAllocationExists reports whether a binding still names a reachable local
// allocation: the interval exists AND still renders on the binding's date. An
// edit that moves work off that date orphans the mapping even though the interval
// identity survives.
func bindingAllocationExists(st *State, b RemoteBinding) bool {
	iv, origin, _, ok := confirmedIntervalByID(st, b.LocalID)
	if !ok {
		return false
	}
	day, ok := st.Days[origin]
	if !ok {
		return false
	}
	loc, err := dayLocation(day.Timezone)
	if err != nil {
		return false
	}
	allocations, err := iv.ResolvedAllocations(loc)
	if err != nil {
		return false
	}
	for _, a := range allocations {
		if a.Date == b.Date {
			return true
		}
	}
	return false
}

func bindingByRemote(r *RemoteState, remoteID string) (int, *RemoteBinding) {
	if r == nil {
		return -1, nil
	}
	for i := range r.Bindings {
		if r.Bindings[i].RemoteID == remoteID {
			return i, &r.Bindings[i]
		}
	}
	return -1, nil
}

func entryByID(entries []RemoteEntry, id string) (RemoteEntry, bool) {
	for _, e := range entries {
		if e.RemoteID == id {
			return e, true
		}
	}
	return RemoteEntry{}, false
}

// entryContentEqual compares the observed remote content that matters for an
// identity/content decision, restricted to the fields the binding's baseline
// actually observed. Billable stays unknown (nil), never false.
func entryContentEqual(a, b RemoteEntry, fields []string) bool {
	if len(fields) == 0 {
		fields = remoteContentFields
	}
	for _, f := range fields {
		switch f {
		case "date":
			if a.Date != b.Date {
				return false
			}
		case "start_time":
			if a.StartTime != b.StartTime {
				return false
			}
		case "end_time":
			if a.EndTime != b.EndTime {
				return false
			}
		case "classification_id":
			if a.ClassificationID != b.ClassificationID {
				return false
			}
		case "description":
			if a.Description != b.Description {
				return false
			}
		case "task":
			if a.Task != b.Task {
				return false
			}
		case "status":
			if a.Status != b.Status {
				return false
			}
		}
	}
	return true
}

// localIntentEqual compares only the fields the local model owns for ONE
// allocation. A remote-only field (task, status, billable) never makes unchanged
// local work look changed, and the later date of a cross-midnight interval never
// compares equal to the origin date's remote entry.
func localIntentEqual(iv Interval, allocation Allocation, entry RemoteEntry) bool {
	start, end, ok := allocationRemoteSpan(allocation)
	if !ok {
		return false
	}
	return entry.Date == allocation.Date && entry.StartTime == start && entry.EndTime == end &&
		entry.ClassificationID == iv.RemoteClassificationID && entry.Description == iv.Description
}

func hasResolution(r *RemoteState, observationID, decision, remoteID string) bool {
	if r == nil {
		return false
	}
	for _, res := range r.Resolutions {
		if res.ObservationID == observationID && res.Decision == decision && res.RemoteID == remoteID {
			return true
		}
	}
	return false
}

func unresolvedOutcomesFor(st *State, dates map[string]bool) []Blocker {
	var out []Blocker
	if st.Remote != nil {
		for _, o := range st.Remote.Outcomes {
			if !outcomeBlocks(o) {
				continue
			}
			if !outcomeRelevant(st, o, dates) {
				continue
			}
			out = append(out, Blocker{
				Code: "unresolved_write", Date: o.Date, LocalID: o.LocalID, RemoteID: o.RemoteID,
				Detail: unresolvedOutcomeDetail(o),
			})
		}
	}
	// R4 native interrupted submissions block the same way: a durable
	// in_flight/uncertain/unidentified write whose allocation date or any
	// contributing origin date touches a relevant date is unresolved replay
	// risk until an explicit resolve_write decision. Never replayed, never
	// auto-cleared.
	for _, it := range st.Intents {
		if !it.unresolved() {
			continue
		}
		if len(dates) > 0 && !intentRelevant(it, dates) {
			continue
		}
		out = append(out, Blocker{
			Code: "unresolved_write", Date: it.Date, LocalID: it.LocalID, RemoteID: it.RemoteID,
			Detail: "a native submission write is unresolved (" + it.ErrorCode + "); use submission_status and resolve_write; writes are never replayed",
		})
	}
	return out
}

func intentTouchesDates(it WriteIntent, dates map[string]bool) bool {
	for _, d := range it.Operation.OriginDates {
		if dates[d] {
			return true
		}
	}
	return false
}

// intentRelevant scopes one native unresolved write by its ALLOCATION date and
// every contributing origin date, not only the durable contributor row (R4F2).
func intentRelevant(it WriteIntent, dates map[string]bool) bool {
	if dates[it.Date] || intentTouchesDates(it, dates) {
		return true
	}
	return false
}

// outcomeAllocationDates resolves the actual (origin interval ID, allocation
// date) footprint a migrated outcome's local identity renders on, through the
// shared contribution helpers. A legacy row may carry its origin's date while
// its write actually targets a later spill day (R4F5/R4F6).
func outcomeAllocationDates(st *State, o WriteOutcome) map[string]bool {
	out := map[string]bool{}
	if st == nil || strings.TrimSpace(o.LocalID) == "" {
		return out
	}
	for _, dy := range st.Days {
		if dy == nil {
			continue
		}
		for _, cand := range dy.Confirmed {
			if cand.ID != o.LocalID {
				continue
			}
			loc, err := dayLocation(dy.Timezone)
			if err != nil {
				continue
			}
			allocs, err := cand.ResolvedAllocations(loc)
			if err != nil {
				continue
			}
			for _, a := range allocs {
				out[a.Date] = true
			}
		}
	}
	return out
}

// outcomeRelevant reports whether a pending migrated outcome can touch the
// given date set: by its recorded write date, by its actual allocation dates
// resolved through the local interval, or conservatively when the recorded
// date is unknown. An unrelated dirty or plan date never inherits this block.
func outcomeRelevant(st *State, o WriteOutcome, dates map[string]bool) bool {
	if o.Date == "" {
		// Unknown pending provenance stays blocking wherever any date is asked.
		return true
	}
	if dates[o.Date] {
		return true
	}
	alloc := outcomeAllocationDates(st, o)
	for d := range alloc {
		if dates[d] {
			return true
		}
	}
	return false
}

// allocationRemoteSpan renders one local allocation as the exact same-day remote
// span a plan operation would write. An allocation that ends at the local day
// boundary is written as 00:00, the documented "ends at midnight" form. A
// whole-day allocation has no representable same-day span and is reported
// unsupported instead of being altered or declared ready.
func allocationRemoteSpan(a Allocation) (string, string, bool) {
	start := strings.TrimSpace(a.Start)
	end := strings.TrimSpace(a.End)
	if end == "24:00" {
		end = "00:00"
	}
	startMin, err := parseClock(start)
	if err != nil {
		return "", "", false
	}
	endMin, err := parseClock(end)
	if err != nil {
		return "", "", false
	}
	if a.Minutes <= 0 || startMin == endMin || (endMin < startMin && endMin != 0) {
		return "", "", false
	}
	return start, end, true
}

// allocationSpan resolves one allocation's absolute span in the origin day's
// timezone. Elapsed minutes are authoritative, so a DST-shifted boundary does
// not fabricate a wall-clock end.
func allocationSpan(a Allocation, loc *time.Location) (time.Time, time.Time, bool) {
	if a.Minutes <= 0 {
		return time.Time{}, time.Time{}, false
	}
	start, err := resolveLocal(a.Date, a.Start, loc, "allocation start")
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	return start, start.Add(time.Duration(a.Minutes) * time.Minute), true
}

func remoteSpan(e RemoteEntry, loc *time.Location) (time.Time, time.Time, bool) {
	iv := Interval{Date: e.Date, Start: e.StartTime, End: e.EndTime}
	start, end, err := iv.resolveEndpoints(loc)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	return start, end, true
}

// hasForeign reports whether the exact current observation already carries a
// foreign decision for this remote entry. Foreign resolves equal-text identity
// hints only; it never authorizes a physical overlap.
func hasForeign(r *RemoteState, observationID, remoteID string) bool {
	return hasResolution(r, observationID, DecisionForeign, remoteID)
}

// boundCollision explains an unbound local allocation that overlaps an observed
// entry already bound to another local allocation (or to a removed one). The
// bound remote ID is never proof that the slot is free.
func boundCollision(st *State, b RemoteBinding) (code, detail string) {
	if !bindingAllocationExists(st, b) {
		return "orphaned_mapping", "an observed remote entry is still bound to a local allocation that no longer exists; the slot is not free, and the mapping is never erased or reassigned to unblock a create"
	}
	return "identity_conflict", "an observed remote entry is bound to another local allocation; a create there would log the same remote time twice"
}

// normalizeText supports only a deliberately weak "same text" candidate hint. It
// never establishes a match.
func normalizeText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// reconcileDate performs one fresh read-only observation, persists it, and
// surfaces mapped/changed/missing/orphaned state, unbound remotes and
// unresolved-write history. It never binds an unmapped entry.
func (s *Session) reconcileDate(date string) (ReconcileReceipt, error) {
	if _, err := parseDate(date); err != nil {
		return ReconcileReceipt{}, err
	}
	reader, err := NewGradionReader(s.cfg, s.env)
	if err != nil {
		return ReconcileReceipt{}, err
	}
	defer func() { _ = reader.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), gradionTimeout)
	defer cancel()
	obs := reader.ObserveDate(ctx, date)

	next := s.store.State().cloneState()
	if next.Remote == nil {
		next.Remote = &RemoteState{}
	}
	if next.Remote.Observations == nil {
		next.Remote.Observations = map[string]*RemoteObservation{}
	}
	next.Remote.Observations[date] = obs
	if err := s.store.CommitState(next); err != nil {
		return ReconcileReceipt{}, err
	}

	st := s.store.State()
	receipt := ReconcileReceipt{
		Date: date, ObservationID: obs.ObservationID, ObservedAt: obs.ObservedAt,
		Complete: obs.Complete, Failed: obs.Failed, Detail: obs.Detail,
		Coverage: obs.Coverage, Revision: 0,
		Mapped: []ReconcileMatch{}, Changed: []ReconcileMatch{}, Missing: []ReconcileMatch{},
		Unbound: []RemoteEntry{}, Candidates: []ReconcileCandidate{}, Orphaned: []ReconcileMatch{},
	}
	if day, ok := st.Days[date]; ok {
		receipt.Revision = day.Revision
	}
	contributions, err := collectContributions(st, date)
	if err != nil {
		return ReconcileReceipt{}, err
	}
	block := func(b Blocker) { receipt.Blockers = append(receipt.Blockers, b) }
	if obs.Failed {
		block(Blocker{Code: "remote_unreadable", Date: date, Detail: obs.Detail})
	} else if !obs.Complete {
		block(Blocker{Code: "remote_incomplete", Date: date, Detail: obs.Detail})
	}

	// Mapped allocations: report unchanged, changed or missing. Identity is the
	// origin interval AND its allocation date, never the interval alone.
	for _, c := range contributions {
		iv := c.Interval
		_, b := bindingFor(st.Remote, iv.ID, c.Allocation.Date)
		if b == nil {
			continue
		}
		m := ReconcileMatch{LocalID: iv.ID, RemoteID: b.RemoteID, Date: b.Date}
		e, found := entryByID(obs.Entries, b.RemoteID)
		if found {
			m.StartTime, m.EndTime, m.ContentObserved = e.StartTime, e.EndTime, e.ContentObserved
			changed := b.LastObserved != nil && !entryContentEqual(*b.LastObserved, e, b.BaselineFields)
			m.Changed = changed
			if changed {
				m.Accepted = hasResolution(st.Remote, obs.ObservationID, DecisionAcceptObserved, b.RemoteID)
				receipt.Changed = append(receipt.Changed, m)
				if !m.Accepted {
					block(Blocker{Code: "remote_changed", Date: date, LocalID: iv.ID, RemoteID: b.RemoteID,
						Detail: "the mapped remote content changed since the last observation; accept_observed is required"})
				}
			} else {
				receipt.Mapped = append(receipt.Mapped, m)
			}
		} else {
			if obs.Complete {
				receipt.Missing = append(receipt.Missing, m)
				block(Blocker{Code: "remote_missing", Date: date, LocalID: iv.ID, RemoteID: b.RemoteID,
					Detail: "a mapped remote entry is absent from a complete observation"})
			}
		}
	}

	for _, e := range obs.Entries {
		if _, b := bindingByRemote(st.Remote, e.RemoteID); b != nil {
			continue
		}
		receipt.Unbound = append(receipt.Unbound, e)
	}
	ambiguous := false
	for _, c := range contributions {
		iv := c.Interval
		alloc := c.Allocation
		if _, b := bindingFor(st.Remote, iv.ID, alloc.Date); b != nil {
			continue
		}
		loc, err := dayLocation(c.Timezone)
		if err != nil {
			continue
		}
		start, end, ok := allocationSpan(alloc, loc)
		for _, e := range obs.Entries {
			if _, taken := bindingByRemote(st.Remote, e.RemoteID); taken != nil {
				// A bound remote is not a free slot. It only conflicts when it
				// physically occupies this allocation's time; an unrelated
				// mapping elsewhere must not block this date's work.
				if ok {
					if rStart, rEnd, rOK := remoteSpan(e, loc); rOK && rStart.Before(end) && start.Before(rEnd) {
						code, detail := boundCollision(st, *taken)
						receipt.Candidates = append(receipt.Candidates, ReconcileCandidate{
							LocalID: iv.ID, RemoteID: e.RemoteID, Reason: "bound_overlap"})
						block(Blocker{Code: code, Date: date, LocalID: iv.ID, RemoteID: e.RemoteID, Detail: detail})
					}
				}
				continue
			}
			if ok {
				if rStart, rEnd, rOK := remoteSpan(e, loc); rOK && rStart.Before(end) && start.Before(rEnd) {
					ambiguous = true
					receipt.Candidates = append(receipt.Candidates, ReconcileCandidate{
						LocalID: iv.ID, RemoteID: e.RemoteID, Reason: "overlap"})
					continue
				}
			}
			if e.Description != "" && normalizeText(e.Description) == normalizeText(iv.Description) &&
				!hasForeign(st.Remote, obs.ObservationID, e.RemoteID) {
				ambiguous = true
				receipt.Candidates = append(receipt.Candidates, ReconcileCandidate{
					LocalID: iv.ID, RemoteID: e.RemoteID, Reason: "equal_text"})
			}
		}
	}
	if ambiguous {
		block(Blocker{Code: "unresolved_identity", Date: date,
			Detail: "an unbound remote entry overlaps or repeats local work; a fresh read never binds it, choose an explicit resolution"})
	}

	if st.Remote != nil {
		for _, b := range st.Remote.Bindings {
			if bindingAllocationExists(st, b) {
				continue
			}
			receipt.Orphaned = append(receipt.Orphaned, ReconcileMatch{LocalID: b.LocalID, RemoteID: b.RemoteID, Date: b.Date})
		}
	}

	dateSet := map[string]bool{date: true}
	for _, c := range contributions {
		dateSet[c.OriginDate] = true
	}
	receipt.UnresolvedOutcomes = unresolvedOutcomesFor(st, dateSet)
	receipt.Blockers = append(receipt.Blockers, receipt.UnresolvedOutcomes...)
	receipt.Ready = len(receipt.Blockers) == 0
	return receipt, nil
}

// resolveRemote records one explicit human identity/content decision against the
// CURRENT observation. It rejects a stale revision or observation id, never
// steals another allocation's remote identity, and never clears a migrated
// unresolved-write blocker.
func (s *Session) resolveRemote(day *Day, date string, revision int, observationID, remoteID, localID, decision string) (ResolutionReceipt, error) {
	if !validResolution(decision) {
		return ResolutionReceipt{}, newError(CodeBadRequest, "decision must be adopt, accept_observed, or foreign")
	}
	remoteID = strings.TrimSpace(remoteID)
	if remoteID == "" {
		return ResolutionReceipt{}, newError(CodeBadRequest, "remote_id is required")
	}
	observationID = strings.TrimSpace(observationID)
	if observationID == "" {
		return ResolutionReceipt{}, newError(CodeBadRequest, "observation_id is required: a resolution must bind the observation it reviewed")
	}
	localID = strings.TrimSpace(localID)
	if decision == DecisionForeign && localID != "" {
		return ResolutionReceipt{}, newError(CodeBadRequest, "local_id must be omitted for a foreign decision")
	}
	if decision != DecisionForeign && localID == "" {
		return ResolutionReceipt{}, newError(CodeBadRequest, "local_id is required for adopt and accept_observed")
	}
	if day != nil && day.Revision != revision {
		return ResolutionReceipt{}, newError(CodeStaleRevision, "the day changed since the review; review again before resolving")
	}
	st := s.store.State()
	if day == nil {
		durableRevision := 0
		if d, ok := st.Days[date]; ok {
			durableRevision = d.Revision
		}
		if durableRevision != revision {
			return ResolutionReceipt{}, newError(CodeStaleRevision, "the saved day changed since the review; review again before resolving")
		}
	}
	if st.Remote == nil {
		return ResolutionReceipt{}, newError(CodeStaleObservation, "no remote observation exists for this date; reconcile first")
	}
	obs := st.Remote.Observations[date]
	if obs == nil || obs.ObservationID != observationID || obs.Failed {
		return ResolutionReceipt{}, newError(CodeStaleObservation, "observation_id is not the current successful observation for this date; reconcile again")
	}
	entry, ok := entryByID(obs.Entries, remoteID)
	if !ok {
		return ResolutionReceipt{}, newError(CodeUnknownRemote, "remote_id is not present in the current observation; reconcile again")
	}

	next := st.cloneState()
	if next.Remote == nil {
		next.Remote = &RemoteState{}
	}
	receipt := ResolutionReceipt{
		Date: date, Revision: revision, ObservationID: observationID,
		Decision: decision, RemoteID: remoteID, LocalID: localID, Complete: obs.Complete,
	}
	now := time.Now().UTC().Format(time.RFC3339)

	switch decision {
	case DecisionAdopt:
		iv, _, _, ok := confirmedIntervalByID(st, localID)
		if !ok {
			return ResolutionReceipt{}, newError(CodeUnknownLocal, "local_id does not name confirmed durable work")
		}
		contributions, err := collectContributions(st, date)
		if err != nil {
			return ResolutionReceipt{}, err
		}
		var allocation *Allocation
		for _, c := range contributions {
			if c.Interval.ID == localID {
				a := c.Allocation
				allocation = &a
				break
			}
		}
		if allocation == nil {
			return ResolutionReceipt{}, newError(CodeUnknownLocal, "local_id does not allocate onto this date")
		}
		if _, existing := bindingFor(st.Remote, localID, allocation.Date); existing != nil {
			return ResolutionReceipt{}, newError(CodeIdentityConflict, "this local allocation is already bound to a remote identity; use accept_observed for its mapped remote")
		}
		if _, taken := bindingByRemote(st.Remote, remoteID); taken != nil {
			return ResolutionReceipt{}, newError(CodeIdentityConflict, "that remote entry is already bound to another local allocation; adoption cannot steal it")
		}
		if entry.Date != allocation.Date {
			return ResolutionReceipt{}, newError(CodeIdentityConflict, "the observed entry is dated on a different day than the selected allocation; that is a local identity decision, not a time-slot match")
		}
		next.Remote.Bindings = append(next.Remote.Bindings, RemoteBinding{
			LocalID: localID, RemoteID: remoteID, Date: allocation.Date,
			StartTime: allocation.Start, EndTime: allocation.End,
			ClassificationID: iv.RemoteClassificationID, Description: iv.Description,
			LastObserved:   cloneEntry(&entry),
			BaselineFields: append([]string(nil), remoteContentFields...),
			CreatedAt:      now, UpdatedAt: now, Origin: "adoption",
		})
		receipt.Bound = true
	case DecisionAcceptObserved:
		idx, b := bindingFor(st.Remote, localID, date)
		if b == nil {
			return ResolutionReceipt{}, newError(CodeIdentityConflict, "accept_observed requires an existing mapping for this local allocation on this date")
		}
		if b.RemoteID != remoteID {
			return ResolutionReceipt{}, newError(CodeIdentityConflict, "accept_observed accepts content for the same mapped remote identity, not a time-slot substitution")
		}
		next.Remote.Bindings[idx].LastObserved = cloneEntry(&entry)
		next.Remote.Bindings[idx].BaselineFields = append([]string(nil), remoteContentFields...)
		next.Remote.Bindings[idx].UpdatedAt = now
		receipt.Bound = true
	case DecisionForeign:
		if _, taken := bindingByRemote(st.Remote, remoteID); taken != nil {
			return ResolutionReceipt{}, newError(CodeIdentityConflict, "foreign cannot erase an active binding")
		}
	}

	next.Remote.Resolutions = append(next.Remote.Resolutions, RemoteResolution{
		Date: date, Revision: revision, ObservationID: observationID, Decision: decision,
		RemoteID: remoteID, LocalID: localID, ObservationComplete: obs.Complete, At: now,
	})
	if len(next.Remote.Resolutions) > maxResolutions {
		next.Remote.Resolutions = next.Remote.Resolutions[len(next.Remote.Resolutions)-maxResolutions:]
	}
	if err := s.store.CommitState(next); err != nil {
		return ResolutionReceipt{}, err
	}
	return receipt, nil
}

// previewPlan reads the saved durable work that renders on day, performs a fresh
// read-only observation of every allocation date it plans against, and renders
// one exact per-allocation create/edit operation plus explicit blockers. It never
// plans from an unsaved day and never mutates work.
func (s *Session) previewPlan(date string) (PlanPreview, error) {
	if _, err := parseDate(date); err != nil {
		return PlanPreview{}, err
	}
	st := s.store.State()
	contributions, err := collectContributions(st, date)
	if err != nil {
		return PlanPreview{}, err
	}
	if len(contributions) == 0 {
		return PlanPreview{}, newError(CodeNoConfirmedWork, "there is no saved confirmed work that renders on this date")
	}

	origins := map[string]bool{}
	for _, c := range contributions {
		origins[c.OriginDate] = true
	}

	// Every plan operation is one ALLOCATION, so the dates that must be observed
	// are the allocation dates, not merely the origin dates. A spill-day preview
	// therefore observes the spill day itself.
	relevant := map[string]bool{}
	for _, c := range contributions {
		relevant[c.Allocation.Date] = true
	}
	relevantDates := make([]string, 0, len(relevant))
	for d := range relevant {
		relevantDates = append(relevantDates, d)
	}
	sort.Strings(relevantDates)

	// R4F2: guard every open session whose durable saved OR current (staged /
	// unsaved-confirmed) allocation footprint touches the plan's affected
	// allocation dates. Prior-day spill additions, changed timezone-resolved
	// allocations, removals/moves of old contributions and unsaved
	// confirmations are all dirty footprints of this kind; the guarded plan
	// stays blocked until the session is saved or cancelled, and unrelated
	// dirty dates never block this preview. The guard covers both approval and
	// consumption because both re-render this exact preview.
	affected := make(map[string]bool, len(relevant)+len(origins))
	for d := range relevant {
		affected[d] = true
	}
	for d := range origins {
		affected[d] = true
	}
	for openDate, open := range s.days {
		if open == nil || (open.Revision == open.SavedRevision && intervalsEqual(open.Staged, open.Confirmed)) {
			continue
		}
		loc, err := dayLocation(open.Timezone)
		if err != nil {
			return PlanPreview{}, err
		}
		touchedIn := func(list []Interval, listDate string, loc *time.Location) bool {
			for _, iv := range list {
				if iv.Date != listDate {
					continue
				}
				allocs, err := iv.ResolvedAllocations(loc)
				if err != nil {
					continue
				}
				for _, a := range allocs {
					if affected[a.Date] {
						return true
					}
				}
			}
			return false
		}
		dirty := touchedIn(open.Confirmed, openDate, loc) || touchedIn(open.Staged, openDate, loc)
		// R4F2 continuation: an unsaved confirmation can empty BOTH current
		// session lists (staged and confirmed) while this plan still renders
		// the durable saved work. The saved origin's footprint is therefore
		// part of the dirty session's footprint: a final removal that was
		// confirmed but not saved must never authorize the stale saved view.
		// The saved view is resolved with its own timezone, and this read never
		// mutates the durable snapshot or persists the draft.
		if !dirty {
			if saved := st.Days[openDate]; saved != nil {
				savedLoc, err := dayLocation(saved.Timezone)
				if err != nil {
					return PlanPreview{}, err
				}
				dirty = touchedIn(saved.Confirmed, openDate, savedLoc)
			}
		}
		if dirty {
			return PlanPreview{}, newError(CodeDirtySession,
				"an open session on "+openDate+" has unsaved changes whose saved or staged allocation footprint touches this plan's affected date(s); save or cancel it before previewing a plan")
		}
	}

	reader, err := NewGradionReader(s.cfg, s.env)
	if err != nil {
		return PlanPreview{}, err
	}
	defer func() { _ = reader.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), gradionTimeout)
	defer cancel()

	observations := map[string]*RemoteObservation{}
	next := st.cloneState()
	if next.Remote == nil {
		next.Remote = &RemoteState{}
	}
	if next.Remote.Observations == nil {
		next.Remote.Observations = map[string]*RemoteObservation{}
	}
	allComplete := true
	for _, d := range relevantDates {
		obs := reader.ObserveDate(ctx, d)
		next.Remote.Observations[d] = obs
		observations[d] = obs
		if !obs.Complete {
			allComplete = false
		}
	}
	// Persist the observations and the plan in one durable transaction.
	st = next
	if st.Plans == nil {
		st.Plans = map[string]*PlanRecord{}
	}

	preview := PlanPreview{
		Date: date, Operations: []PlanOperation{}, Blockers: []Blocker{},
		SurfacedRemotes: []RemoteEntry{}, SourceRevisions: map[string]int{},
		RemoteComplete:     allComplete,
		RemoteObservations: map[string]string{},
	}
	seenBlocker := map[string]bool{}
	block := func(b Blocker) {
		key := b.Code + "|" + b.Date + "|" + b.LocalID + "|" + b.RemoteID
		if seenBlocker[key] {
			return
		}
		seenBlocker[key] = true
		preview.Blockers = append(preview.Blockers, b)
	}
	preview.SourceRevisions = contributionRevisions(contributions)

	observedByDate := map[string]map[string]RemoteEntry{}
	for _, d := range relevantDates {
		obs := observations[d]
		preview.RemoteObservations[d] = obs.ObservationID
		entries := map[string]RemoteEntry{}
		for _, e := range obs.Entries {
			entries[e.RemoteID] = e
		}
		observedByDate[d] = entries
		if obs.Failed {
			block(Blocker{Code: "remote_unreadable", Date: d, Detail: obs.Detail})
		} else if !obs.Complete {
			block(Blocker{Code: "remote_incomplete", Date: d, Detail: obs.Detail})
		}
	}
	if len(relevantDates) > 0 {
		preview.RemoteObservation = preview.RemoteObservations[relevantDates[0]]
	}

	boundIDs := map[string]bool{}
	if st.Remote != nil {
		for _, b := range st.Remote.Bindings {
			boundIDs[b.RemoteID] = true
		}
	}
	for _, d := range relevantDates {
		for _, e := range observations[d].Entries {
			if boundIDs[e.RemoteID] {
				continue
			}
			preview.SurfacedRemotes = append(preview.SurfacedRemotes, e)
		}
	}
	// Preserved unresolved write history blocks readiness. The dates asked are
	// the plan's affected allocation dates plus its contributing origin dates
	// (R4F5): a pending migrated outcome may record its origin's date while its
	// write targets a later spill allocation, and that row must gate this
	// plan's approval and submission even when the spill day currently reads
	// empty.
	preview.UnresolvedOutcomes = unresolvedOutcomesFor(st, affected)
	for _, b := range preview.UnresolvedOutcomes {
		block(b)
	}

	type plannedOp struct {
		op    PlanOperation
		order []string
	}
	var ops []plannedOp
	for _, c := range contributions {
		iv := c.Interval
		origin := c.OriginDate
		alloc := c.Allocation
		allocDate := alloc.Date
		obs := observations[allocDate]
		if obs == nil {
			continue
		}

		loc, err := dayLocation(c.Timezone)
		if err != nil {
			continue
		}
		startText, endText, representable := allocationRemoteSpan(alloc)
		if !representable {
			block(Blocker{Code: "unrepresentable_projection", Date: allocDate, LocalID: iv.ID,
				Detail: "this local allocation has no representable same-day remote span; the work is never altered, split differently or declared ready"})
			continue
		}
		// Defense in depth for the Gradion four-hour policy (issue #167): the
		// stage derivation normally splits long blocks into compliant chunks,
		// but any interval whose remote span still exceeds the policy — however
		// it reached the confirmed set — blocks the plan instead of reaching
		// approve. Checked on the interval (before the classification filter)
		// so an over-long unclassified interval surfaces both blockers.
		if alloc.Minutes > maxRemoteEntryMinutes {
			block(Blocker{Code: "remote_entry_too_long", Date: allocDate, LocalID: iv.ID,
				Detail: fmt.Sprintf("interval %s spans %d minutes; the Gradion remote rejects entries longer than %d minutes (live-verified policy, issue #167); re-run stage so the block is split into chunks of at most %d minutes",
					iv.ID, alloc.Minutes, maxRemoteEntryMinutes, maxRemoteEntryMinutes)})
		}
		if strings.TrimSpace(iv.RemoteClassificationID) == "" {
			block(Blocker{Code: "classification_unselected", Date: allocDate, LocalID: iv.ID,
				Detail: "no explicit reviewed remote classification has been selected for this interval; a default or migrated hint is only a suggestion"})
		}

		_, b := bindingFor(st.Remote, iv.ID, allocDate)
		if b != nil {
			e, found := observedByDate[allocDate][b.RemoteID]
			if !found {
				if obs.Complete {
					block(Blocker{Code: "remote_missing", Date: allocDate, LocalID: iv.ID, RemoteID: b.RemoteID,
						Detail: "the mapped remote entry is absent from a complete observation; it is never recreated blindly"})
				}
				continue
			}
			changed := b.LastObserved != nil && !entryContentEqual(*b.LastObserved, e, b.BaselineFields)
			if changed && !hasResolution(st.Remote, obs.ObservationID, DecisionAcceptObserved, b.RemoteID) {
				block(Blocker{Code: "remote_changed", Date: allocDate, LocalID: iv.ID, RemoteID: b.RemoteID,
					Detail: "the mapped remote content changed and has not been explicitly accepted for this observation"})
				continue
			}
			if localIntentEqual(iv, alloc, e) {
				continue // unchanged mapped work yields no write operation
			}
			if strings.TrimSpace(iv.RemoteClassificationID) == "" {
				continue
			}
			choice, bad := selectedChoiceFor(st, iv)
			if bad != nil {
				block(*bad)
				continue
			}
			ops = append(ops, plannedOp{op: PlanOperation{
				LocalID: iv.ID, Kind: "edit", RemoteID: b.RemoteID, Date: allocDate,
				StartTime: startText, EndTime: endText, Minutes: alloc.Minutes,
				ClassificationID: iv.RemoteClassificationID, Description: iv.Description,
				Task: e.Task, CatalogID: iv.RemoteCatalogID,
				CrossesMidnight:    iv.CrossesMidnight(),
				OriginDates:        []string{origin},
				OriginRevisions:    map[string]int{origin: c.OriginRevision},
				ExpectedRemote:     cloneEntry(&e),
				PreserveRemoteOnly: []string{"task", "billable"},
				// The bounded deterministic audit reason is part of the edit's
				// exact approved body and therefore of the canonical plan
				// content: a change to the reason changes the plan identity.
				Reason:         planEditReason(allocDate, iv.ID),
				SelectedChoice: choice,
			}, order: []string{allocDate, startText, iv.ID}})
			continue
		}

		// Unmapped allocation: a create is only safe with complete coverage and
		// no relevant identity/overlap ambiguity.
		if !obs.Complete {
			continue
		}
		ambiguous := false
		start, end, spanOK := allocationSpan(alloc, loc)
		for _, e := range observations[allocDate].Entries {
			if _, taken := bindingByRemote(st.Remote, e.RemoteID); taken != nil {
				// A bound remote is not a free slot. An unrelated mapping does
				// not block this date, but a real physical overlap does, and the
				// mapping is never erased or reassigned to unblock it.
				if spanOK {
					if rStart, rEnd, ok := remoteSpan(e, loc); ok && rStart.Before(end) && start.Before(rEnd) {
						ambiguous = true
						code, detail := boundCollision(st, *taken)
						block(Blocker{Code: code, Date: allocDate, LocalID: iv.ID, RemoteID: e.RemoteID, Detail: detail})
					}
				}
				continue
			}
			if spanOK {
				if rStart, rEnd, ok := remoteSpan(e, loc); ok && rStart.Before(end) && start.Before(rEnd) {
					ambiguous = true
					block(Blocker{Code: "unresolved_identity", Date: allocDate, LocalID: iv.ID, RemoteID: e.RemoteID,
						Detail: "an unbound remote entry overlaps this local allocation; resolve the identity explicitly before treating a create as safe"})
					continue
				}
			}
			if e.Description != "" && normalizeText(e.Description) == normalizeText(iv.Description) &&
				!hasForeign(st.Remote, obs.ObservationID, e.RemoteID) {
				ambiguous = true
				block(Blocker{Code: "unresolved_identity", Date: allocDate, LocalID: iv.ID, RemoteID: e.RemoteID,
					Detail: "an unbound remote entry repeats this local description; equal text is only a candidate, never a match"})
			}
		}
		if ambiguous || strings.TrimSpace(iv.RemoteClassificationID) == "" {
			continue
		}
		choice, bad := selectedChoiceFor(st, iv)
		if bad != nil {
			block(*bad)
			continue
		}
		ops = append(ops, plannedOp{op: PlanOperation{
			LocalID: iv.ID, Kind: "create", Date: allocDate,
			StartTime: startText, EndTime: endText, Minutes: alloc.Minutes,
			ClassificationID: iv.RemoteClassificationID, Description: iv.Description,
			CatalogID: iv.RemoteCatalogID, CrossesMidnight: iv.CrossesMidnight(),
			OriginDates: []string{origin}, OriginRevisions: map[string]int{origin: c.OriginRevision},
			SelectedChoice: choice,
		}, order: []string{allocDate, startText, iv.ID}})
	}

	sort.SliceStable(ops, func(i, j int) bool {
		a, b := ops[i].order, ops[j].order
		for k := 0; k < len(a) && k < len(b); k++ {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return len(a) < len(b)
	})
	for _, o := range ops {
		preview.Operations = append(preview.Operations, o.op)
	}
	sort.SliceStable(preview.SurfacedRemotes, func(i, j int) bool {
		return preview.SurfacedRemotes[i].RemoteID < preview.SurfacedRemotes[j].RemoteID
	})

	preview.RemoteFingerprint = remoteFingerprint(relevantDates, observations)
	preview.PlanID = planID(date, preview.SourceRevisions, preview.Operations, preview.RemoteFingerprint, preview.Blockers)
	if len(preview.Blockers) == 0 {
		preview.State = PlanReady
	} else {
		preview.State = PlanBlocked
	}

	record := &PlanRecord{
		PlanID: preview.PlanID, Date: date, State: preview.State,
		RemoteObservation: preview.RemoteObservation, RemoteObservations: preview.RemoteObservations,
		RemoteComplete:    preview.RemoteComplete,
		RemoteFingerprint: preview.RemoteFingerprint, SourceRevisions: preview.SourceRevisions,
		Operations: preview.Operations, Blockers: preview.Blockers,
		SurfacedRemotes: preview.SurfacedRemotes, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if existing, exists := st.Plans[preview.PlanID]; exists {
		// Immutable history under an existing ID is never overwritten. The
		// identity includes readiness preconditions, so a mismatch is a real
		// collision rather than an expected state transition.
		if !planRecordMatches(existing, preview) {
			return PlanPreview{}, newError(CodeInternal, "the stored plan identity collides with different immutable content")
		}
	} else {
		st.Plans[preview.PlanID] = record
		prunePlans(st.Plans)
		preview.Persisted = true
	}
	if err := s.store.CommitState(st); err != nil {
		return PlanPreview{}, err
	}
	preview.ApprovalAvailable = preview.State == PlanReady
	preview.Approved = s.approvalBoundFor(preview.PlanID)
	return preview, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func prunePlans(plans map[string]*PlanRecord) {
	if len(plans) <= maxStoredPlans {
		return
	}
	type aged struct {
		id string
		at string
	}
	all := make([]aged, 0, len(plans))
	for id, p := range plans {
		all = append(all, aged{id: id, at: p.CreatedAt})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at < all[j].at })
	for i := 0; i < len(all)-maxStoredPlans; i++ {
		delete(plans, all[i].id)
	}
}

// remoteFingerprint is the relevant remote-content/coverage identity. It excludes
// observation timestamps so identical fresh observations keep canonical content
// stable; actual remote or coverage changes invalidate it.
func remoteFingerprint(dates []string, observations map[string]*RemoteObservation) string {
	type perDate struct {
		Date     string        `json:"date"`
		Complete bool          `json:"complete"`
		Failed   bool          `json:"failed"`
		Entries  []RemoteEntry `json:"entries"`
	}
	payload := make([]perDate, 0, len(dates))
	for _, d := range dates {
		obs := observations[d]
		if obs == nil {
			payload = append(payload, perDate{Date: d})
			continue
		}
		payload = append(payload, perDate{Date: d, Complete: obs.Complete, Failed: obs.Failed, Entries: obs.Entries})
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return sha256Hex(data)
}

// canonicalOperation is the hash-visible content of one operation. Audit-only
// fields (catalog identity, created/observed timestamps) are deliberately absent
// so a fresh identical observation does not change the plan identity.
type canonicalOperation struct {
	LocalID            string   `json:"local_id"`
	Kind               string   `json:"kind"`
	RemoteID           string   `json:"remote_id,omitempty"`
	Date               string   `json:"date"`
	StartTime          string   `json:"start_time"`
	EndTime            string   `json:"end_time"`
	ClassificationID   string   `json:"classification_id"`
	Description        string   `json:"description"`
	CrossesMidnight    bool     `json:"crosses_midnight,omitempty"`
	PreserveRemoteOnly []string `json:"preserve_remote_only,omitempty"`
	// Reason is a previewed edit's audit reason, inside the exact approved
	// body. It is canonical content, not an audit-only timestamp.
	Reason         string       `json:"reason,omitempty"`
	ExpectedRemote *RemoteEntry `json:"expected_remote,omitempty"`
	// SelectedChoice is the explicitly selected classification's relevant
	// semantic identity (id/code/name/activity type) as rendered from the
	// stored selection reference (R4F1). It is canonical plan content: a
	// changed or unverifiable selection produces a different or blocked
	// preview, never a silently swapped body.
	SelectedChoice *ClassificationChoice `json:"selected_choice,omitempty"`
}

// planID is the canonical content identity of one preview. It includes the
// readiness preconditions (the exact blockers) as well as the work, source
// revisions and observed remote content, so a blocked preview and the ready
// preview that follows an explicit decision are distinct identities. Audit
// timestamps are still excluded.
func planID(date string, revisions map[string]int, ops []PlanOperation, remoteFP string, blockers []Blocker) string {
	canon := make([]canonicalOperation, 0, len(ops))
	for _, op := range ops {
		var expected *RemoteEntry
		if op.ExpectedRemote != nil {
			e := *op.ExpectedRemote
			expected = &e
		}
		canon = append(canon, canonicalOperation{
			LocalID: op.LocalID, Kind: op.Kind, RemoteID: op.RemoteID, Date: op.Date,
			StartTime: op.StartTime, EndTime: op.EndTime,
			ClassificationID: op.ClassificationID, Description: op.Description,
			CrossesMidnight: op.CrossesMidnight, PreserveRemoteOnly: op.PreserveRemoteOnly,
			Reason: op.Reason, ExpectedRemote: expected, SelectedChoice: op.SelectedChoice,
		})
	}
	canonBlockers := append([]Blocker(nil), blockers...)
	sort.SliceStable(canonBlockers, func(i, j int) bool {
		a, b := canonBlockers[i], canonBlockers[j]
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		if a.Date != b.Date {
			return a.Date < b.Date
		}
		if a.LocalID != b.LocalID {
			return a.LocalID < b.LocalID
		}
		if a.RemoteID != b.RemoteID {
			return a.RemoteID < b.RemoteID
		}
		return a.Detail < b.Detail
	})
	payload := struct {
		Date       string               `json:"date"`
		Revisions  map[string]int       `json:"source_revisions"`
		Operations []canonicalOperation `json:"operations"`
		Remote     string               `json:"remote_fingerprint"`
		Blockers   []Blocker            `json:"readiness_blockers"`
	}{Date: date, Revisions: revisions, Operations: canon, Remote: remoteFP, Blockers: canonBlockers}
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return sha256Hex(data)
}

// planRecordMatches reports whether a stored immutable plan is the same content
// as a recomputed preview under the same identity. A mismatch means the ID
// collided with unrelated durable history and must not be overwritten.
func planRecordMatches(p *PlanRecord, preview PlanPreview) bool {
	if p == nil || p.State != preview.State {
		return false
	}
	if len(p.Blockers) != len(preview.Blockers) || len(p.Operations) != len(preview.Operations) {
		return false
	}
	for i := range p.Blockers {
		if p.Blockers[i] != preview.Blockers[i] {
			return false
		}
	}
	for i := range p.Operations {
		a, b := p.Operations[i], preview.Operations[i]
		if a.LocalID != b.LocalID || a.Kind != b.Kind || a.RemoteID != b.RemoteID ||
			a.Date != b.Date || a.StartTime != b.StartTime || a.EndTime != b.EndTime ||
			a.Minutes != b.Minutes || a.ClassificationID != b.ClassificationID ||
			a.Description != b.Description || a.Reason != b.Reason {
			return false
		}
		// Choice binding compares by semantic value, never by pointer.
		if (a.SelectedChoice == nil) != (b.SelectedChoice == nil) {
			return false
		}
		if a.SelectedChoice != nil && *a.SelectedChoice != *b.SelectedChoice {
			return false
		}
	}
	return true
}

// planEditReason is the bounded deterministic audit reason for a previewed
// edit. It follows the legacy convention's honest machine-readable phrasing
// ("timesheet review update <date>") and is deliberately revision-independent,
// so a revision bump does not change the plan's content hash and cannot
// invalidate an otherwise-identical plan.
func planEditReason(date, localID string) string {
	date = strings.TrimSpace(date)
	if date == "" {
		date = "unknown"
	}
	localID = strings.TrimSpace(localID)
	if localID == "" {
		localID = "unknown"
	}
	return "timesheet review update " + date + " " + localID
}
