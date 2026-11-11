package timesheet

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Store owns the durable local state file and an exclusive advisory lock for
// the whole lifetime of the process that opened it. A second process that tries to write the same state
// fails with store_locked instead of racing or losing the first writer's work.
//
// durableSchema is the schema actually on disk: the loaded schema until a
// successful write, then 2. An in-memory upgrade never writes by itself.
type Store struct {
	path          string
	lock          *os.File
	state         *State
	uncertain     *AppError
	durableSchema int
}

// storeFaultHook is nil in normal operation. Deterministic tests set it to
// exercise pre-rename and post-rename failure boundaries. It is not reachable
// from configuration, environment, or the protocol.
var storeFaultHook func(stage string) error

func injectStoreFault(stage string) error {
	if storeFaultHook != nil {
		return storeFaultHook(stage)
	}
	return nil
}

// OpenStore creates the state directory if needed, takes the single-writer lock,
// and loads confirmed state. A missing state file is a new store, not an error.
func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, newError(CodeConfigInvalid, "state path is required")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, newError(CodeWriteFailed, "state directory cannot be created")
	}
	lockPath := path + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, newError(CodeWriteFailed, "state lock file cannot be opened")
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, newError(CodeStoreLocked, "local state is locked by another running process")
		}
		return nil, newError(CodeStoreLocked, "local state lock cannot be acquired")
	}

	st := &Store{path: path, lock: lockFile, state: newState(), durableSchema: stateSchema}
	if err := st.load(); err != nil {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
		return nil, err
	}
	return st, nil
}

func newState() *State {
	return &State{Schema: stateSchema, Days: map[string]*Day{}, Evidence: map[string]*DayEvidence{}}
}

// load reads the durable state. A schema-1 or schema-2 file is validated in its
// own shape and upgraded in memory through an explicit conversion; the file is
// not rewritten here. Any other schema fails closed. A file with trailing data,
// unknown fields, a persisted authorization claim, or a schema-3-only section
// under an older schema declaration is rejected without mutation.
func (s *Store) load() error {
	info, err := os.Stat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || info.IsDir() {
		return newError(CodeStateInvalid, "local state file cannot be read")
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return newError(CodeStateInvalid, "local state file cannot be read")
	}
	var header struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return newError(CodeStateInvalid, "local state file is not valid JSON for the supported schema")
	}
	loaded := header.Schema
	// The schema-2 conversion drops the obsolete always-false persisted plan
	// authorization flags after validating them. A persisted true claim is a
	// state-invalid failure, never permission.
	if loaded == schemaTwoState {
		data, err = convertSchemaTwoState(data)
		if err != nil {
			return err
		}
	}
	var state State
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&state); err != nil {
		return newError(CodeStateInvalid, "local state file is not valid JSON for the supported schema")
	}
	if err := ensureJSONEOF(dec); err != nil {
		return newError(CodeStateInvalid, "local state file has trailing data")
	}
	if loaded == legacyStateSchema {
		state.Schema = legacyStateSchema
	}
	switch loaded {
	case legacyStateSchema:
		if state.Migration != nil || state.Remote != nil || len(state.Plans) > 0 ||
			len(state.Approvals) > 0 || len(state.Intents) > 0 || len(state.Executions) > 0 {
			return newError(CodeStateInvalid, "local state declares schema 1 but carries later-schema sections")
		}
		if err := validateLegacyState(&state); err != nil {
			return err
		}
	case schemaTwoState:
		if err := validateStateBody(&state); err != nil {
			return err
		}
	case stateSchema:
		if err := validateState(&state); err != nil {
			return err
		}
	default:
		return newError(CodeStateInvalid, "local state schema is not supported")
	}
	if state.Days == nil {
		state.Days = map[string]*Day{}
	}
	if state.Evidence == nil {
		state.Evidence = map[string]*DayEvidence{}
	}
	// Upgrade is in-memory only. A legacy file stays its own schema on disk
	// until a later successful durable save or transaction activates schema 3.
	state.Schema = stateSchema
	s.state = &state
	s.durableSchema = loaded
	return nil
}

// convertSchemaTwoState is the explicit in-memory conversion of an accepted
// schema-2 file. It rejects authorization claims and schema-3-only sections,
// then strips the obsolete always-false persisted plan flags
// (approved/approval_available) so the strict decode accepts the rest of the
// file unchanged. Old plan identity, migration history, observations and
// outcomes are all preserved.
func convertSchemaTwoState(data []byte) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, newError(CodeStateInvalid, "local state file is not valid JSON for the supported schema")
	}
	for _, section := range []string{"approvals", "intents", "executions"} {
		if _, has := obj[section]; has {
			return nil, newError(CodeStateInvalid, "local state declares schema 2 but carries schema 3 sections")
		}
	}
	plansRaw, has := obj["plans"]
	if !has {
		return data, nil
	}
	var plans map[string]json.RawMessage
	if err := json.Unmarshal(plansRaw, &plans); err != nil {
		return nil, newError(CodeStateInvalid, "local state file is not valid JSON for the supported schema")
	}
	changed := false
	for key, raw := range plans {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, newError(CodeStateInvalid, "local state file is not valid JSON for the supported schema")
		}
		for _, flag := range []string{"approved", "approval_available"} {
			value, ok := fields[flag]
			if !ok {
				continue
			}
			var b bool
			if err := json.Unmarshal(value, &b); err != nil || b {
				return nil, newError(CodeStateInvalid, "local state claims an approval authority this build does not have")
			}
			delete(fields, flag)
			changed = true
		}
		if changed {
			rebuilt, err := json.Marshal(fields)
			if err != nil {
				return nil, newError(CodeStateInvalid, "local state file cannot be converted")
			}
			plans[key] = rebuilt
		}
	}
	if !changed {
		return data, nil
	}
	plansBytes, err := json.Marshal(plans)
	if err != nil {
		return nil, newError(CodeStateInvalid, "local state file cannot be converted")
	}
	obj["plans"] = plansBytes
	return json.Marshal(obj)
}

// validateEvidence checks one persisted day's evidence collection for
// structural soundness. Like the rest of load validation it fails closed.
func validateEvidence(ev *DayEvidence) error {
	if ev == nil {
		return newError(CodeStateInvalid, "local state contains a null evidence record")
	}
	seen := map[string]bool{}
	for _, a := range ev.Attempts {
		if a.Source != SourceGithub && a.Source != SourceCalendar {
			return newError(CodeStateInvalid, "local state contains an evidence attempt with an unknown source")
		}
		if a.CollectionID == "" || a.AttemptedAt == "" || !validEvidenceStatus(a.Status) {
			return newError(CodeStateInvalid, "local state contains an incomplete evidence attempt record")
		}
		if seen[a.Source] {
			return newError(CodeStateInvalid, "local state contains duplicate evidence attempts for one source")
		}
		seen[a.Source] = true
	}
	for _, t := range ev.Topics {
		if t.Ref == "" || t.Provider == "" {
			return newError(CodeStateInvalid, "local state contains an evidence topic without identity")
		}
		for _, item := range t.Items {
			if item.Ref == "" || item.Time == "" {
				return newError(CodeStateInvalid, "local state contains an evidence item without identity")
			}
		}
		// Overflow items are durable evidence like Items and pass the same
		// identity check; a negative loss count is structurally invalid.
		for _, item := range t.OverflowItems {
			if item.Ref == "" || item.Time == "" {
				return newError(CodeStateInvalid, "local state contains an evidence item without identity")
			}
		}
		if t.DroppedItems < 0 {
			return newError(CodeStateInvalid, "local state contains an evidence topic with a negative dropped-item count")
		}
	}
	for _, m := range ev.Meetings {
		if m.Ref == "" || m.UID == "" || m.Start == "" {
			return newError(CodeStateInvalid, "local state contains a proposed meeting without identity")
		}
		if m.Decision != "" && !validDecision(m.Decision) {
			return newError(CodeStateInvalid, "local state contains an invalid attendance decision")
		}
	}
	return nil
}

func validateState(state *State) error {
	if state.Schema != stateSchema {
		return newError(CodeStateInvalid, "local state schema is not supported")
	}
	return validateStateBody(state)
}

// validateLegacyState validates an R1/R2 schema-1 file in its own shape: days and
// evidence only, with no R3 sections.
func validateLegacyState(state *State) error {
	if state.Schema != legacyStateSchema {
		return newError(CodeStateInvalid, "local state schema is not supported")
	}
	return validateStateBody(state)
}

func validateStateBody(state *State) error {
	if state.Days == nil {
		state.Days = map[string]*Day{}
	}
	for key, day := range state.Days {
		if day == nil || day.Date != key {
			return newError(CodeStateInvalid, "local state contains an inconsistent day record")
		}
		if _, err := parseDate(day.Date); err != nil {
			return newError(CodeStateInvalid, "local state contains an invalid day date")
		}
		if day.Revision < 0 || day.NextInterval < 1 {
			return newError(CodeStateInvalid, "local state contains an invalid revision counter")
		}
		loc, err := dayLocation(day.Timezone)
		if err != nil {
			return newError(CodeStateInvalid, "local state day has no usable timezone")
		}
		seen := map[string]bool{}
		for _, iv := range day.Confirmed {
			if iv.ID == "" || seen[iv.ID] {
				return newError(CodeStateInvalid, "local state contains a duplicate or missing interval id")
			}
			seen[iv.ID] = true
			if iv.Date != key {
				return newError(CodeStateInvalid, "local state interval does not match its day record")
			}
			if iv.Project == "" || iv.Description == "" {
				return newError(CodeStateInvalid, "local state interval is incomplete")
			}
			if err := validateRemoteClassificationFields(iv); err != nil {
				return err
			}
			if _, _, err := iv.resolveEndpoints(loc); err != nil {
				return newError(CodeStateInvalid, "local state interval is invalid in its recorded timezone")
			}
		}
	}
	for key, ev := range state.Evidence {
		if _, err := parseDate(key); err != nil {
			return newError(CodeStateInvalid, "local state contains evidence under an invalid date key")
		}
		if err := validateEvidence(ev); err != nil {
			return err
		}
	}
	if err := validateMigrationState(state.Migration); err != nil {
		return err
	}
	if err := validateRemoteState(state.Remote); err != nil {
		return err
	}
	if err := validatePlans(state.Plans); err != nil {
		return err
	}
	if err := validateApprovals(state.Approvals); err != nil {
		return err
	}
	if err := validateIntents(state.Intents); err != nil {
		return err
	}
	return validateExecutions(state.Executions)
}

// validateApprovals validates the native approval ledger. Unique occurrence
// IDs, valid lifecycle states and timestamps; the ledger is the only approval
// authority and is never pruned while any row is consumed or revoked audit.
func validateApprovals(rows []ApprovalRecord) error {
	if len(rows) > maxStoredApprovals {
		return newError(CodeStateInvalid, "local state contains too many native approvals")
	}
	seen := map[string]bool{}
	for _, a := range rows {
		if a.ApprovalID == "" || seen[a.ApprovalID] {
			return newError(CodeStateInvalid, "local state contains a duplicate or missing native approval id")
		}
		seen[a.ApprovalID] = true
		if a.PlanID == "" {
			return newError(CodeStateInvalid, "local state contains an approval without a plan identity")
		}
		if _, err := parseDate(a.Date); err != nil {
			return newError(CodeStateInvalid, "local state contains an approval under an invalid date")
		}
		switch a.State {
		case ApprovalActive:
			if a.ExecutionID != "" || a.ConsumedAt != "" || a.RevokedAt != "" {
				return newError(CodeStateInvalid, "local state contains an active approval with consumption markers")
			}
		case ApprovalExecuting:
			if a.ExecutionID == "" || a.ConsumedAt == "" || a.RevokedAt != "" {
				return newError(CodeStateInvalid, "local state contains an executing approval without its consumption markers")
			}
		case ApprovalRevoked:
			if a.RevokedAt == "" || a.ExecutionID != "" || a.ConsumedAt != "" {
				return newError(CodeStateInvalid, "local state contains an inconsistent revoked approval")
			}
		default:
			return newError(CodeStateInvalid, "local state contains an approval in an unknown state")
		}
		if a.ContextDigest == "" {
			return newError(CodeStateInvalid, "local state contains an approval without its bound context digest")
		}
	}
	return nil
}

// validateIntents validates the durable write-intent ledger. A confirmed row
// must carry verified observation content; a resolved row carries an explicit
// resolution and observation identity; anything unresolved (including
// in_flight) is a blocker and is never pruned.
func validateIntents(rows []WriteIntent) error {
	if len(rows) > maxStoredIntents {
		return newError(CodeStateInvalid, "local state contains too many native write intents")
	}
	seen := map[string]bool{}
	for _, it := range rows {
		if it.WriteRef == "" || seen[it.WriteRef] {
			return newError(CodeStateInvalid, "local state contains a duplicate or missing write reference")
		}
		seen[it.WriteRef] = true
		if it.ExecutionID == "" || it.ApprovalID == "" || it.PlanID == "" {
			return newError(CodeStateInvalid, "local state contains a write intent without its submission identity")
		}
		if _, err := parseDate(it.Date); err != nil {
			return newError(CodeStateInvalid, "local state contains a write intent under an invalid date")
		}
		if it.OpIndex < 0 || it.LocalID == "" {
			return newError(CodeStateInvalid, "local state contains an incomplete write intent")
		}
		if it.Action != "create" && it.Action != "edit" {
			return newError(CodeStateInvalid, "local state contains a write intent with an unknown action")
		}
		if it.Action == "edit" && it.RemoteID == "" {
			return newError(CodeStateInvalid, "local state contains an edit write intent without its intended remote identity")
		}
		if it.OpDigest == "" || it.Preconditions == "" {
			return newError(CodeStateInvalid, "local state contains a write intent without its exact preconditions")
		}
		switch it.Status {
		case IntentInFlight:
		case IntentConfirmed:
			if it.Observed == nil || it.Observed.RemoteID == "" {
				return newError(CodeStateInvalid, "local state contains a confirmed write intent without verified content")
			}
		case IntentUncertain, IntentUnidentified:
			if it.ErrorCode == "" {
				return newError(CodeStateInvalid, "local state contains an unresolved write intent without its stable reason")
			}
		default:
			return newError(CodeStateInvalid, "local state contains a write intent in an unknown attempt state")
		}
		if it.Resolved {
			if (it.Resolution != DecisionWriteAdopt && it.Resolution != DecisionWriteNotApplied) ||
				it.ResolvedAt == "" || it.ResolutionObservationID == "" {
				return newError(CodeStateInvalid, "local state contains a resolved write intent without its complete decision record")
			}
			// A resolved row must be one that was unresolved: a confirmed write
			// is history, never a pending decision.
			if it.Status == IntentConfirmed {
				return newError(CodeStateInvalid, "local state contains a resolution recorded on an already confirmed write")
			}
		} else if it.Resolution != "" || it.ResolvedAt != "" || it.ResolutionObservationID != "" {
			return newError(CodeStateInvalid, "local state contains a write intent with resolution fields but no resolved marker")
		}
		if it.AttemptedAt == "" && it.Status != IntentInFlight {
			return newError(CodeStateInvalid, "local state contains a dispatched write intent without its attempt timestamp")
		}
	}
	return nil
}

// validateExecutions validates native submission audit rows.
func validateExecutions(rows []ExecutionRecord) error {
	if len(rows) > maxStoredExecutions {
		return newError(CodeStateInvalid, "local state contains too many native submission records")
	}
	seen := map[string]bool{}
	for _, e := range rows {
		if e.ExecutionID == "" || seen[e.ExecutionID] {
			return newError(CodeStateInvalid, "local state contains a duplicate or missing submission record id")
		}
		seen[e.ExecutionID] = true
		if e.ApprovalID == "" || e.PlanID == "" {
			return newError(CodeStateInvalid, "local state contains a submission record without its approval identity")
		}
		if _, err := parseDate(e.Date); err != nil {
			return newError(CodeStateInvalid, "local state contains a submission record under an invalid date")
		}
		switch e.State {
		case ExecutionStarted:
			if e.FinishedAt != "" {
				return newError(CodeStateInvalid, "local state contains a started submission record with a finish timestamp")
			}
		case ExecutionCompleted, ExecutionStopped:
			if e.FinishedAt == "" {
				return newError(CodeStateInvalid, "local state contains a finished submission record without its finish timestamp")
			}
		default:
			return newError(CodeStateInvalid, "local state contains a submission record in an unknown state")
		}
	}
	return nil
}

func validateMigrationState(m *MigrationState) error {
	if m == nil {
		return nil
	}
	receipts := map[string]bool{}
	for _, r := range m.Receipts {
		if r.MigrationID == "" || receipts[r.MigrationID] {
			return newError(CodeStateInvalid, "local state contains a duplicate or missing migration receipt id")
		}
		receipts[r.MigrationID] = true
		if len(r.Dates) == 0 || r.AppliedAt == "" {
			return newError(CodeStateInvalid, "local state contains an incomplete migration receipt")
		}
		seenDates := map[string]bool{}
		for _, d := range r.Dates {
			if _, err := parseDate(d); err != nil || seenDates[d] {
				return newError(CodeStateInvalid, "local state contains an invalid migration receipt date")
			}
			seenDates[d] = true
		}
		if _, err := dayLocation(r.Timezone); err != nil {
			return newError(CodeStateInvalid, "local state contains a migration receipt without a usable timezone")
		}
	}
	for key, id := range m.Sources {
		if key == "" || !receipts[id] {
			return newError(CodeStateInvalid, "local state contains a migration source map without a receipt")
		}
	}
	fragments := map[string]bool{}
	for _, c := range m.Carries {
		if c.FragmentID == "" || fragments[c.FragmentID] {
			return newError(CodeStateInvalid, "local state contains a duplicate or missing carry fragment id")
		}
		fragments[c.FragmentID] = true
		if c.Confirmed {
			return newError(CodeStateInvalid, "local state promotes an unconfirmed legacy carry into confirmed work")
		}
		if _, err := parseDate(c.Date); err != nil {
			return newError(CodeStateInvalid, "local state contains a carry fragment under an invalid date")
		}
		if c.Start != "" && c.End != "" {
			if _, err := parseClock(c.Start); err != nil {
				return newError(CodeStateInvalid, "local state contains a carry fragment with an invalid start")
			}
			if _, err := parseClock(c.End); err != nil {
				return newError(CodeStateInvalid, "local state contains a carry fragment with an invalid end")
			}
		}
	}
	return nil
}

func validateRemoteState(r *RemoteState) error {
	if r == nil {
		return nil
	}
	// Identity is (local interval, allocation date): a cross-midnight interval
	// legitimately has one binding per date, while a remote ID still may not be
	// bound twice.
	type allocationKey struct{ local, date string }
	locals := map[allocationKey]bool{}
	remotes := map[string]bool{}
	for _, b := range r.Bindings {
		if b.LocalID == "" || b.RemoteID == "" {
			return newError(CodeStateInvalid, "local state contains a remote binding without identity")
		}
		key := allocationKey{local: b.LocalID, date: b.Date}
		if locals[key] || remotes[b.RemoteID] {
			return newError(CodeStateInvalid, "local state contains a non-one-to-one remote binding")
		}
		locals[key] = true
		remotes[b.RemoteID] = true
		if _, err := parseDate(b.Date); err != nil {
			return newError(CodeStateInvalid, "local state contains a remote binding under an invalid date")
		}
		if b.LastObserved != nil {
			if err := validateRemoteEntry(*b.LastObserved); err != nil {
				return err
			}
		}
		for _, f := range b.BaselineFields {
			known := false
			for _, allowed := range remoteContentFields {
				if f == allowed {
					known = true
					break
				}
			}
			if !known {
				return newError(CodeStateInvalid, "local state contains an unknown remote baseline field")
			}
		}
	}
	for key, obs := range r.Observations {
		if obs == nil || obs.Date != key || obs.ObservationID == "" {
			return newError(CodeStateInvalid, "local state contains an inconsistent remote observation")
		}
		if _, err := parseDate(key); err != nil {
			return newError(CodeStateInvalid, "local state contains a remote observation under an invalid date")
		}
		if len(obs.Entries) > maxRemoteEntries {
			return newError(CodeStateInvalid, "local state contains a remote observation above the entry bound")
		}
		seen := map[string]bool{}
		for _, e := range obs.Entries {
			if err := validateRemoteEntry(e); err != nil {
				return err
			}
			if seen[e.RemoteID] {
				return newError(CodeStateInvalid, "local state contains duplicate observed remote ids")
			}
			seen[e.RemoteID] = true
		}
	}
	for _, res := range r.Resolutions {
		if _, err := parseDate(res.Date); err != nil {
			return newError(CodeStateInvalid, "local state contains a resolution under an invalid date")
		}
		if !validResolution(res.Decision) || res.RemoteID == "" || res.ObservationID == "" {
			return newError(CodeStateInvalid, "local state contains an incomplete remote resolution")
		}
		if res.Decision == DecisionForeign && res.LocalID != "" {
			return newError(CodeStateInvalid, "local state contains a foreign resolution that names a local id")
		}
		if res.Decision != DecisionForeign && res.LocalID == "" {
			return newError(CodeStateInvalid, "local state contains a binding resolution without a local id")
		}
	}
	for _, o := range r.Outcomes {
		if o.Status == "" {
			return newError(CodeStateInvalid, "local state contains a write outcome without a status")
		}
	}
	for key, cat := range r.Catalogs {
		if cat == nil || cat.CatalogID != key || cat.CatalogID == "" {
			return newError(CodeStateInvalid, "local state contains an inconsistent classification catalog")
		}
		if len(cat.Options) > maxCatalogOptions {
			return newError(CodeStateInvalid, "local state contains a catalog above the option bound")
		}
		for _, opt := range cat.Options {
			if opt.ID == "" {
				return newError(CodeStateInvalid, "local state contains a catalog option without an id")
			}
		}
	}
	return nil
}

func validateRemoteEntry(e RemoteEntry) error {
	if e.RemoteID == "" {
		return newError(CodeStateInvalid, "local state contains an observed remote entry without an id")
	}
	if _, err := parseDate(e.Date); err != nil {
		return newError(CodeStateInvalid, "local state contains an observed remote entry under an invalid date")
	}
	if _, err := parseClock(e.StartTime); err != nil {
		return newError(CodeStateInvalid, "local state contains an observed remote entry with an invalid start")
	}
	if _, err := parseClock(e.EndTime); err != nil {
		return newError(CodeStateInvalid, "local state contains an observed remote entry with an invalid end")
	}
	return nil
}

func validatePlans(plans map[string]*PlanRecord) error {
	for key, p := range plans {
		if p == nil || p.PlanID != key || p.PlanID == "" {
			return newError(CodeStateInvalid, "local state contains an inconsistent plan record")
		}
		if p.State != PlanReady && p.State != PlanBlocked {
			return newError(CodeStateInvalid, "local state contains a plan in an unknown state")
		}
		if _, err := parseDate(p.Date); err != nil {
			return newError(CodeStateInvalid, "local state contains a plan under an invalid date")
		}
		for _, op := range p.Operations {
			if op.Kind != "create" && op.Kind != "edit" {
				return newError(CodeStateInvalid, "local state contains a plan with an unknown operation kind")
			}
			if op.LocalID == "" || op.Date == "" || op.ClassificationID == "" {
				return newError(CodeStateInvalid, "local state contains an incomplete plan operation")
			}
			if _, err := parseClock(op.StartTime); err != nil {
				return newError(CodeStateInvalid, "local state contains a plan operation with an invalid start")
			}
			if _, err := parseClock(op.EndTime); err != nil {
				return newError(CodeStateInvalid, "local state contains a plan operation with an invalid end")
			}
			if op.Kind == "edit" && op.RemoteID == "" {
				return newError(CodeStateInvalid, "local state contains an edit plan operation without a remote id")
			}
		}
	}
	return nil
}

func validResolution(decision string) bool {
	switch decision {
	case DecisionAdopt, DecisionAcceptObserved, DecisionForeign:
		return true
	}
	return false
}

// validateRemoteClassificationFields checks the R3 additive interval fields. A
// reviewed remote choice must name both the option and the catalog identity it
// was reviewed in; a migrated hint is audit-only and never a selection.
func validateRemoteClassificationFields(iv Interval) error {
	if iv.RemoteClassificationID != "" {
		if len(iv.RemoteClassificationID) > 128 || controlRune.MatchString(iv.RemoteClassificationID) ||
			!projectIDRE.MatchString(iv.RemoteClassificationID) {
			return newError(CodeStateInvalid, "local state contains an invalid remote classification id")
		}
		if strings.TrimSpace(iv.RemoteCatalogID) == "" {
			return newError(CodeStateInvalid, "local state contains a remote classification choice without a catalog identity")
		}
	}
	if iv.RemoteCatalogID != "" && iv.RemoteClassificationID == "" {
		return newError(CodeStateInvalid, "local state contains a catalog identity without a classification choice")
	}
	if iv.MigratedClassificationID != "" && controlRune.MatchString(iv.MigratedClassificationID) {
		return newError(CodeStateInvalid, "local state contains an invalid migrated classification hint")
	}
	return nil
}

// State exposes the in-memory durable snapshot. Callers must not mutate it
// outside Store.Commit.
func (s *Store) State() *State { return s.state }

// SavedRevision returns the durable revision for date, or 0 for an unknown day.
func (s *Store) SavedRevision(date string) int {
	if day, ok := s.state.Days[date]; ok {
		return day.Revision
	}
	return 0
}

// Commit persists one day after verifying that the caller loaded the same
// durable revision. A failure before rename rolls the in-memory snapshot back and
// leaves the previous file intact. A failure after rename keeps the new in-memory
// snapshot but returns write_uncertain: the file may be active, its crash
// durability is unconfirmed, and further saves are blocked until restart.
func (s *Store) Commit(expectedSavedRevision int, day *Day) error {
	if day == nil {
		return newError(CodeInternal, "no day to save")
	}
	if s.uncertain != nil {
		return s.uncertain
	}
	existing, existed := s.state.Days[day.Date]
	diskRevision := 0
	if existed {
		diskRevision = existing.Revision
	}
	if diskRevision != expectedSavedRevision {
		return newError(CodeStaleRevision, "local state changed in another writer; reopen the day before saving")
	}
	if day.Revision < expectedSavedRevision {
		return newError(CodeStaleRevision, "day revision is older than the saved state")
	}

	snapshot := day.clone()
	previousSavedAt := s.state.SavedAt
	s.state.Days[day.Date] = snapshot
	s.state.SavedAt = time.Now().UTC().Format(time.RFC3339)
	activated, err := s.write()
	if err != nil {
		if activated {
			s.durableSchema = stateSchema
			s.uncertain = newError(CodeWriteUncertain,
				"local state may have been activated, but directory durability is unconfirmed; close and restart before saving again")
			return s.uncertain
		}
		s.state.SavedAt = previousSavedAt
		if existed {
			s.state.Days[day.Date] = existing
		} else {
			delete(s.state.Days, day.Date)
		}
		return err
	}
	s.durableSchema = stateSchema
	return nil
}

// write reports whether rename activated the new file. Any failure with
// activated=true happened after activation and must not be treated as a safe
// rollback or retried blindly.
func (s *Store) write() (bool, error) {
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return false, newError(CodeWriteFailed, "local state cannot be encoded")
	}
	data = append(data, '\n')
	dir := filepath.Dir(s.path)

	tmp, err := os.CreateTemp(dir, ".timesheet-state-*.tmp")
	if err != nil {
		return false, newError(CodeWriteFailed, "local state cannot be written")
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return false, newError(CodeWriteFailed, "local state permissions cannot be set")
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return false, newError(CodeWriteFailed, "local state cannot be written")
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return false, newError(CodeWriteFailed, "local state cannot be flushed")
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return false, newError(CodeWriteFailed, "local state cannot be closed")
	}
	if err := injectStoreFault("before_rename"); err != nil {
		_ = os.Remove(tmpName)
		return false, newError(CodeWriteFailed, "local state cannot be activated")
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
		return false, newError(CodeWriteFailed, "local state cannot be activated")
	}
	if err := injectStoreFault("directory_open"); err != nil {
		return true, newError(CodeWriteFailed, "local state was activated but its directory cannot be opened")
	}
	dirFile, err := os.Open(dir)
	if err != nil {
		return true, newError(CodeWriteFailed, "local state was activated but its directory cannot be opened")
	}
	if err := injectStoreFault("directory_sync"); err != nil {
		_ = dirFile.Close()
		return true, newError(CodeWriteFailed, "local state was activated but its directory cannot be synchronized")
	}
	if err := dirFile.Sync(); err != nil {
		_ = dirFile.Close()
		return true, newError(CodeWriteFailed, "local state was activated but its directory cannot be synchronized")
	}
	if err := injectStoreFault("directory_close"); err != nil {
		_ = dirFile.Close()
		return true, newError(CodeWriteFailed, "local state was activated but its directory cannot be closed")
	}
	if err := dirFile.Close(); err != nil {
		return true, newError(CodeWriteFailed, "local state was activated but its directory cannot be closed")
	}
	return true, nil
}

// Uncertain reports whether a post-rename failure left this session unable to
// make further durability claims without a restart.
func (s *Store) Uncertain() bool { return s.uncertain != nil }

// Close releases the single-writer lock.
func (s *Store) Close() error {
	if s.lock == nil {
		return nil
	}
	_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
	err := s.lock.Close()
	s.lock = nil
	return err
}

var _ io.Closer = (*Store)(nil)

// Evidence exposes one day's persisted collection results.
func (s *Store) Evidence(date string) *DayEvidence {
	if s.state.Evidence == nil {
		return nil
	}
	return s.state.Evidence[date]
}

// CommitEvidence replaces one day's evidence record atomically, using the
// same write path as Commit. It never touches intervals, revisions, history,
// or save receipts, so F1-F4 behavior is unchanged. A post-activation
// failure behaves exactly like a work save: write_uncertain and a blocked
// session until restart.
func (s *Store) CommitEvidence(date string, ev *DayEvidence) error {
	if s.state.Evidence == nil {
		s.state.Evidence = map[string]*DayEvidence{}
	}
	if _, err := parseDate(date); err != nil {
		return newError(CodeStateInvalid, "evidence date is not a calendar date")
	}
	if err := validateEvidence(ev); err != nil {
		return err
	}
	if s.uncertain != nil {
		return s.uncertain
	}
	previous := s.state.Evidence[date]
	previousSavedAt := s.state.SavedAt
	s.state.Evidence[date] = ev.cloneEvidence()
	activated, err := s.write()
	if err != nil && activated {
		s.durableSchema = stateSchema
		s.uncertain = newError(CodeWriteUncertain,
			"local state may have been activated, but directory durability is unconfirmed; close and restart before writing again")
		return s.uncertain
	}
	if err != nil {
		// Rollback must reproduce absence exactly: restoring a nil pointer
		// would persist an explicit null evidence entry for a date that
		// previously had none, and that null entry fails state validation
		// after a later save plus restart. Delete the key instead.
		if previous == nil {
			delete(s.state.Evidence, date)
		} else {
			s.state.Evidence[date] = previous
		}
		s.state.SavedAt = previousSavedAt
		return err
	}
	s.durableSchema = stateSchema
	return nil
}

// CommitState atomically activates a complete candidate state through the SAME
// write path as a work save: one temporary file, one rename, then directory
// synchronization. A failure before rename restores the previous in-memory
// snapshot and leaves the previous file intact; a failure after rename is
// write_uncertain and blocks further claims until restart. There is no separate
// unsafe persistence path.
func (s *Store) CommitState(next *State) error {
	if next == nil {
		return newError(CodeInternal, "no state to commit")
	}
	if s.uncertain != nil {
		return s.uncertain
	}
	next.Schema = stateSchema
	if err := validateState(next); err != nil {
		return err
	}
	next.SavedAt = time.Now().UTC().Format(time.RFC3339)
	previous := s.state
	previousSchema := s.durableSchema
	s.state = next
	activated, err := s.write()
	if err != nil {
		if activated {
			s.durableSchema = stateSchema
			s.uncertain = newError(CodeWriteUncertain,
				"local state may have been activated, but directory durability is unconfirmed; close and restart before writing again")
			return s.uncertain
		}
		s.state = previous
		s.durableSchema = previousSchema
		return err
	}
	s.durableSchema = stateSchema
	return nil
}

// DurableSchema reports the schema actually on disk (1 until the first
// successful write upgrades an R1/R2 file).
func (s *Store) DurableSchema() int { return s.durableSchema }

func (e *DayEvidence) cloneEvidence() *DayEvidence {
	out := &DayEvidence{
		Attempts: append([]SourceAttempt(nil), e.Attempts...),
		Topics:   append([]TopicCandidate(nil), e.Topics...),
		Meetings: append([]ProposedMeeting(nil), e.Meetings...),
	}
	for i, a := range out.Attempts {
		out.Attempts[i].Families = append([]FamilyReport(nil), a.Families...)
	}
	for i, t := range out.Topics {
		out.Topics[i].Items = append([]EvidenceItem(nil), t.Items...)
		out.Topics[i].OverflowItems = append([]EvidenceItem(nil), t.OverflowItems...)
	}
	return out
}

// EvidenceUncertain reports whether a failed evidence write left durability
// unresolved; the session blocks further evidence writes and saves.
func (s *Store) EvidenceUnderUncertainty() bool { return s.uncertain != nil }
