package timesheet

// R3 explicit legacy migration.
//
// The authoritative legacy daily format is workflow schema 1 at
// <configured legacy records root>/workflow/YYYY-MM-DD.json. This file reads
// those files as data with a private, read-only shape. It deliberately does not
// import the old workflow package and never calls its store constructor: the old
// constructor creates directories and locks, and migration must not write into
// the legacy root.
//
// The read is explicitly NOT an atomic multi-file snapshot of an independent
// writer. The documented precondition is that legacy writers are quiescent; a
// changed source byte set is detected by re-hashing every file immediately before
// activation and fails the whole candidate instead of importing a mixed batch.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MigrationConflict is one reason a candidate cannot be activated. Conflicts are
// bounded and carry a stable code and a date, never raw file contents.
type MigrationConflict struct {
	Code   string `json:"code"`
	Date   string `json:"date,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// MigrationCounts is the bounded shape of what a candidate would import.
type MigrationCounts struct {
	Days            int `json:"days"`
	Intervals       int `json:"intervals"`
	Classified      int `json:"classified"`
	Mappings        int `json:"mappings"`
	Outcomes        int `json:"outcomes"`
	Carries         int `json:"carries"`
	LegacyPlans     int `json:"legacy_plans"`
	LegacyApprovals int `json:"legacy_approvals"`
	Revalidate      int `json:"revalidate"`
	Unresolved      int `json:"unresolved"`
}

// MigrationPreview is the read-only validation receipt for an explicit date
// selection. It never activates anything. NoChange reports that the exact source
// bytes for every requested date were already imported; conflicts report why the
// whole candidate is rejected.
type MigrationPreview struct {
	MigrationID        string              `json:"migration_id"`
	Dates              []string            `json:"dates"`
	Timezone           string              `json:"timezone"`
	Ready              bool                `json:"ready"`
	NoChange           bool                `json:"no_change"`
	ExistingReceipt    string              `json:"existing_receipt,omitempty"`
	SourceIdentity     string              `json:"source_identity"`
	Counts             MigrationCounts     `json:"counts"`
	Conflicts          []MigrationConflict `json:"conflicts"`
	ConflictsTruncated int                 `json:"conflicts_truncated,omitempty"`
	SourceHashes       map[string]string   `json:"source_hashes"`
}

// MigrationApplyReceipt is the activation receipt. Status is applied or
// no_change; no_change never writes and never duplicates a receipt.
type MigrationApplyReceipt struct {
	MigrationID string          `json:"migration_id"`
	Status      string          `json:"status"`
	ReceiptID   string          `json:"receipt_id"`
	Dates       []string        `json:"dates"`
	Counts      MigrationCounts `json:"counts"`
	Unresolved  int             `json:"unresolved_outcomes"`
}

// --- private legacy document shape -----------------------------------------
//
// These structs are a read-only provenance mirror of workflow schema 1. Only the
// attributes R3 preserves or reports are decoded; unknown additive legacy fields
// are ignored (not rejected), exactly as a forward-compatible reader must.

type legacyRecord struct {
	SchemaVersion  int              `json:"schema_version"`
	Date           string           `json:"date"`
	Revision       int              `json:"revision"`
	UpdatedAt      string           `json:"updated_at"`
	Evidence       []legacyEvidence `json:"evidence"`
	Confirmed      []legacyInterval `json:"confirmed"`
	RemoteMappings []legacyMapping  `json:"remote_mappings"`
	Outcomes       []legacyOutcome  `json:"outcomes"`
	Review         *legacyReview    `json:"review"`
	Carries        []legacyCarry    `json:"carries"`
	Plans          []legacyPlan     `json:"plans"`
	Approvals      []legacyApproval `json:"approvals"`
	CollectedRaw   json.RawMessage  `json:"collected_evidence"`
}

type legacyEvidence struct {
	ID         string `json:"id"`
	Source     string `json:"source"`
	Timestamp  string `json:"timestamp"`
	EndTime    string `json:"end_time"`
	Title      string `json:"title"`
	Ref        string `json:"ref"`
	ObservedAt string `json:"observed_at"`
}

type legacyReview struct {
	Reviewed      bool               `json:"reviewed"`
	Empty         bool               `json:"empty"`
	ReviewedAt    string             `json:"reviewed_at"`
	ExcludedRaw   []legacyExclusion  `json:"excluded_evidence"`
	AttendanceRaw []legacyAttendance `json:"attendance"`
	Acknowledged  []string           `json:"acknowledged_sources"`
}

type legacyExclusion struct {
	EvidenceID string `json:"evidence_id"`
	Reason     string `json:"reason"`
	ExcludedAt string `json:"excluded_at"`
}

type legacyAttendance struct {
	EvidenceID string `json:"evidence_id"`
	Attended   bool   `json:"attended"`
	DecidedAt  string `json:"decided_at"`
}

// remoteContentFields is the observed-content field vocabulary used to compare a
// binding baseline with a fresh observation. Fields a legacy record never
// recorded are unknown, not contradictions.
var remoteContentFields = []string{
	"date", "start_time", "end_time", "classification_id", "description", "task", "status",
}

func observedFieldsPresent(present map[string]json.RawMessage) []string {
	var out []string
	for _, f := range remoteContentFields {
		if _, ok := present[f]; ok {
			out = append(out, f)
		}
	}
	return out
}

func rawLen(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return 0
	}
	return len(items)
}

type legacyInterval struct {
	ID                 string   `json:"id"`
	Date               string   `json:"date"`
	StartTime          string   `json:"start_time"`
	EndTime            string   `json:"end_time"`
	Project            string   `json:"project"`
	Description        string   `json:"description"`
	Attendance         bool     `json:"attendance"`
	ConfirmedAt        string   `json:"confirmed_at"`
	Revision           int      `json:"revision"`
	Kind               string   `json:"kind"`
	EvidenceIDs        []string `json:"evidence_ids"`
	Manual             bool     `json:"manual"`
	SplitGroupID       string   `json:"split_group_id"`
	PartIndex          int      `json:"part_index"`
	Carryover          bool     `json:"carryover"`
	ClassificationID   string   `json:"classification_id"`
	ClassificationCode string   `json:"classification_code"`
	ClassificationName string   `json:"classification_name"`
}

type legacyMapping struct {
	LocalID          string          `json:"local_id"`
	RemoteID         string          `json:"remote_id"`
	Date             string          `json:"date"`
	StartTime        string          `json:"start_time"`
	EndTime          string          `json:"end_time"`
	ClassificationID string          `json:"classification_id"`
	Description      string          `json:"description"`
	LastObservedRaw  json.RawMessage `json:"last_observed"`
	CreatedAt        string          `json:"created_at"`
	UpdatedAt        string          `json:"updated_at"`
}

type legacyRemoteState struct {
	RemoteID         string `json:"remote_id"`
	Date             string `json:"date"`
	StartTime        string `json:"start_time"`
	EndTime          string `json:"end_time"`
	ClassificationID string `json:"classification_id"`
	Description      string `json:"description"`
	ObservedAt       string `json:"observed_at"`
	Task             string `json:"task"`
	Billable         *bool  `json:"billable"`
	ContentObserved  bool   `json:"content_observed"`
}

type legacyOutcome struct {
	OperationKey     string `json:"operation_key"`
	LocalID          string `json:"local_id"`
	RemoteID         string `json:"remote_id"`
	Action           string `json:"action"`
	Status           string `json:"status"`
	Date             string `json:"date"`
	StartTime        string `json:"start_time"`
	EndTime          string `json:"end_time"`
	Description      string `json:"description"`
	ErrorCode        string `json:"error_code"`
	OriginErrorCode  string `json:"origin_error_code"`
	ClassificationID string `json:"classification_id"`
	Resolved         bool   `json:"resolved"`
	RecordedAt       string `json:"recorded_at"`
	IntentAt         string `json:"intent_at"`
}

type legacyCarry struct {
	SourceDate   string   `json:"source_date"`
	Date         string   `json:"date"`
	Start        string   `json:"start"`
	End          string   `json:"end"`
	Minutes      int      `json:"minutes"`
	SplitGroupID string   `json:"split_group_id"`
	PartIndex    int      `json:"part_index"`
	Kind         string   `json:"kind"`
	Project      string   `json:"project"`
	Description  string   `json:"description"`
	EvidenceIDs  []string `json:"evidence_ids"`
}

type legacyPlan struct {
	PlanID           string                `json:"plan_id"`
	Date             string                `json:"date"`
	LocalFingerprint string                `json:"local_fingerprint"`
	RecordRevision   int                   `json:"record_revision"`
	RemoteObservedAt string                `json:"remote_observed_at"`
	RemoteComplete   bool                  `json:"remote_complete"`
	Operations       []legacyPlanOperation `json:"operations"`
	CreatedAt        string                `json:"created_at"`
}

type legacyPlanOperation struct {
	LocalID          string             `json:"local_id"`
	Kind             string             `json:"kind"`
	RemoteID         string             `json:"remote_id"`
	Date             string             `json:"date"`
	StartTime        string             `json:"start_time"`
	EndTime          string             `json:"end_time"`
	ClassificationID string             `json:"classification_id"`
	Description      string             `json:"description"`
	Task             string             `json:"task"`
	Billable         *bool              `json:"billable"`
	Reason           string             `json:"reason"`
	ExpectedRemote   *legacyRemoteState `json:"expected_remote"`
}

type legacyApproval struct {
	ApprovalID        string `json:"approval_id"`
	PlanID            string `json:"plan_id"`
	Date              string `json:"date"`
	RecordRevision    int    `json:"record_revision"`
	RemoteObservedAt  string `json:"remote_observed_at"`
	ApprovedAt        string `json:"approved_at"`
	Superseded        bool   `json:"superseded"`
	InvalidatedAt     string `json:"invalidated_at"`
	InvalidatedReason string `json:"invalidated_reason"`
}

// --- reading -----------------------------------------------------------------

type migrationSource struct {
	Date   string
	Hash   string
	Record legacyRecord
}

type migrationCandidate struct {
	Dates             []string
	Timezone          string
	SourceHashes      map[string]string
	SourceIdentity    string
	MigrationID       string
	TargetFingerprint string
	Ready             bool
	NoChange          bool
	ExistingReceipt   string
	Conflicts         []MigrationConflict
	ConflictsDropped  int
	Days              map[string]*Day
	Bindings          []RemoteBinding
	Outcomes          []WriteOutcome
	Carries           []MigratedCarry
	Receipt           MigrationReceipt
	Counts            MigrationCounts
}

// normalizeMigrationDates validates an explicit, bounded date selection. It
// sorts and de-duplicates but never silently truncates: an over-bound request is
// rejected.
func normalizeMigrationDates(dates []string) ([]string, error) {
	if len(dates) == 0 {
		return nil, newError(CodeMigrationInvalid, "dates is required: select the legacy dates explicitly")
	}
	if len(dates) > maxMigrationDates {
		return nil, newError(CodeMigrationInvalid, fmt.Sprintf(
			"dates exceeds the %d-date bound; split the selection deliberately", maxMigrationDates))
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(dates))
	for _, d := range dates {
		if _, err := parseDate(d); err != nil {
			return nil, newError(CodeMigrationInvalid, "dates contains a value that is not a real YYYY-MM-DD date")
		}
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	sort.Strings(out)
	return out, nil
}

// readMigrationSource reads exactly one configured, date-named regular file. It
// performs no discovery, accepts no request-supplied path, rejects a symlinked
// workflow directory or source file, and never writes into the legacy root.
func readMigrationSource(cfg *Config, date string) (migrationSource, *MigrationConflict) {
	root := cfg.Migration.LegacyRoot
	conflict := func(code, detail string) *MigrationConflict {
		return &MigrationConflict{Code: code, Date: date, Detail: detail}
	}
	resolvedRoot, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return migrationSource{}, conflict("legacy_root_unreadable", "the configured legacy records root cannot be resolved")
	}
	info, err := os.Stat(resolvedRoot)
	if err != nil || !info.IsDir() {
		return migrationSource{}, conflict("legacy_root_unreadable", "the configured legacy records root is not a directory")
	}
	workflowDir := filepath.Join(resolvedRoot, "workflow")
	if dirInfo, err := os.Lstat(workflowDir); err != nil || !dirInfo.IsDir() {
		return migrationSource{}, conflict("legacy_workflow_missing", "the legacy workflow directory is missing or is not a real directory")
	}
	path := filepath.Join(workflowDir, date+".json")
	if rel, err := filepath.Rel(resolvedRoot, path); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return migrationSource{}, conflict("legacy_source_escape", "the legacy source path escapes the configured root")
	}
	fileInfo, err := os.Lstat(path)
	if err != nil {
		return migrationSource{}, conflict("legacy_source_missing", "no legacy record exists for this date")
	}
	if !fileInfo.Mode().IsRegular() {
		return migrationSource{}, conflict("legacy_source_unreadable", "the legacy record is not a regular file")
	}
	if fileInfo.Size() > maxLegacyRecordBytes {
		return migrationSource{}, conflict("legacy_source_too_large", "the legacy record exceeds the bounded read size")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return migrationSource{}, conflict("legacy_source_unreadable", "the legacy record cannot be read")
	}
	sum := sha256.Sum256(data)
	var rec legacyRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return migrationSource{}, conflict("legacy_schema_unknown", "the legacy record is not valid JSON for schema 1")
	}
	if rec.SchemaVersion != 1 {
		return migrationSource{}, conflict("legacy_schema_unknown", fmt.Sprintf(
			"the legacy record declares schema_version %d; only schema 1 is supported", rec.SchemaVersion))
	}
	if rec.Date != date {
		return migrationSource{}, conflict("legacy_date_mismatch", "the legacy record's date does not match its file name")
	}
	return migrationSource{Date: date, Hash: hex.EncodeToString(sum[:]), Record: rec}, nil
}

// --- candidate construction --------------------------------------------------

func buildMigrationCandidate(cfg *Config, st *State, dates []string) (*migrationCandidate, error) {
	if cfg.Migration == nil || !cfg.Migration.Enabled {
		return nil, newError(CodeMigrationDisabled, "explicit legacy migration is not enabled in configuration")
	}
	tz := cfg.Migration.LegacyTimezone
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, newError(CodeConfigInvalid, "migration legacy timezone is not a recognised IANA timezone")
	}
	cand := &migrationCandidate{
		Dates:        append([]string(nil), dates...),
		Timezone:     tz,
		SourceHashes: map[string]string{},
		Days:         map[string]*Day{},
		Receipt: MigrationReceipt{
			Dates:           append([]string(nil), dates...),
			Timezone:        tz,
			SourceHashes:    map[string]string{},
			LegacyRevisions: map[string]int{},
			LegacyUpdatedAt: map[string]string{},
		},
	}
	cand.TargetFingerprint = targetFingerprint(st)

	addConflict := func(c MigrationConflict) {
		if len(cand.Conflicts) >= maxMigrationConflicts {
			cand.ConflictsDropped++
			return
		}
		cand.Conflicts = append(cand.Conflicts, c)
	}
	// capacity reports an exceeded audit bound ONCE. Audit rows are never
	// silently dropped on a successful activation: the whole candidate fails.
	capacityReported := map[string]bool{}
	capacity := func(category string) {
		if capacityReported[category] {
			return
		}
		capacityReported[category] = true
		addConflict(MigrationConflict{Code: "migration_capacity",
			Detail: "the selection exceeds the preserved " + category + " audit bound; no rows are silently dropped and nothing was activated, so split the selection deliberately"})
	}

	sources := make([]migrationSource, 0, len(dates))
	for _, date := range dates {
		src, conflict := readMigrationSource(cfg, date)
		if conflict != nil {
			addConflict(*conflict)
			continue
		}
		sources = append(sources, src)
		cand.SourceHashes[date] = src.Hash
		cand.Receipt.SourceHashes[date] = src.Hash
	}
	if len(sources) != len(dates) {
		cand.SourceIdentity = migrationSourceIdentity(cand.Dates, tz, cand.SourceHashes)
		cand.MigrationID = migrationID(cand.SourceIdentity, cand.TargetFingerprint)
		return cand, nil
	}

	cand.SourceIdentity = migrationSourceIdentity(cand.Dates, tz, cand.SourceHashes)
	cand.MigrationID = migrationID(cand.SourceIdentity, cand.TargetFingerprint)

	// Repeat-safe source identity. Any date already imported must match the exact
	// bytes previously imported under the SAME declared legacy timezone; a changed
	// source or a different interpretation of the same bytes is a conflict, never
	// a silent overwrite or a time-slot merge. Comparison is per date against the
	// receipt covering that date, so a repeated subset of a larger imported batch
	// stays valid when its bytes and timezone agree.
	imported := map[string]MigrationReceipt{}
	for _, r := range receiptsOf(st) {
		for _, d := range r.Dates {
			imported[d] = r
		}
	}
	var already []string
	for _, date := range cand.Dates {
		if r, ok := imported[date]; ok {
			if r.Timezone != cand.Timezone {
				addConflict(MigrationConflict{Code: "interpretation_conflict", Date: date,
					Detail: "this date was already imported under the legacy timezone " + r.Timezone + "; the same bytes under " + cand.Timezone + " are a different interpretation, and the recorded day/receipt timezone is preserved"})
			} else if r.SourceHashes[date] != cand.SourceHashes[date] {
				addConflict(MigrationConflict{Code: "source_changed", Date: date,
					Detail: "this date was already imported from different source bytes; existing work and receipts are preserved and are never overwritten"})
			}
			already = append(already, date)
		}
	}
	if len(already) == len(cand.Dates) {
		if len(cand.Conflicts) > 0 {
			return cand, nil
		}
		cand.NoChange = true
		cand.Ready = false
		if r, ok := imported[cand.Dates[0]]; ok {
			cand.ExistingReceipt = r.MigrationID
		}
		return cand, nil
	}
	if len(already) > 0 && len(already) < len(cand.Dates) {
		addConflict(MigrationConflict{Code: "mixed_source",
			Detail: "the selection mixes already-imported dates with new dates; select only new dates or only one identical repeated selection"})
	}

	// Destination occupancy: a migrated date may only land on empty durable
	// state. Any existing day for the date is incompatible target data.
	for _, date := range cand.Dates {
		if _, ok := st.Days[date]; ok {
			addConflict(MigrationConflict{Code: "destination_occupied", Date: date,
				Detail: "the durable store already has work for this date; migration never overwrites or merges it"})
		}
	}

	usedIDs := map[string]bool{}
	for _, day := range st.Days {
		for _, iv := range day.Confirmed {
			usedIDs[iv.ID] = true
		}
	}
	// Preserve one-to-one identities: a remote ID may not already be bound.
	boundRemotes := map[string]bool{}
	boundLocals := map[string]bool{}
	if st.Remote != nil {
		for _, b := range st.Remote.Bindings {
			boundRemotes[b.RemoteID] = true
			boundLocals[b.LocalID] = true
		}
	}

	type builtDay struct {
		day      *Day
		audits   []LegacyIntervalAudit
		spans    []migrationSpan
		remapped map[string]string // legacy id -> new id
	}
	var built []builtDay
	outcomeCount := 0
	carryCount := 0

	for _, src := range sources {
		day := &Day{Date: src.Date, Timezone: tz, Revision: src.Record.Revision, NextInterval: 1}
		bd := builtDay{day: day, remapped: map[string]string{}}
		if day.Revision < 0 {
			addConflict(MigrationConflict{Code: "unrepresentable_revision", Date: src.Date,
				Detail: "the legacy record has a negative revision counter"})
			day.Revision = 0
		}
		cand.Receipt.LegacyRevisions[src.Date] = src.Record.Revision
		if strings.TrimSpace(src.Record.UpdatedAt) != "" {
			cand.Receipt.LegacyUpdatedAt[src.Date] = src.Record.UpdatedAt
		}
		for _, le := range src.Record.Evidence {
			if len(cand.Receipt.LegacyEvidence) >= maxMigrationEvidenceRows {
				capacity("evidence")
				break
			}
			cand.Receipt.LegacyEvidence = append(cand.Receipt.LegacyEvidence, LegacyEvidenceAudit{
				ID: le.ID, Date: src.Date, Source: le.Source, Timestamp: le.Timestamp, EndTime: le.EndTime,
				Title: le.Title, Ref: le.Ref, ObservedAt: le.ObservedAt,
			})
		}
		if src.Record.Review != nil {
			review := LegacyReviewAudit{
				Date:       src.Date,
				Reviewed:   src.Record.Review.Reviewed,
				Empty:      src.Record.Review.Empty,
				ReviewedAt: src.Record.Review.ReviewedAt,
				Excluded:   len(src.Record.Review.ExcludedRaw),
				Attendance: len(src.Record.Review.AttendanceRaw),
			}
			review.Acknowledged = append([]string(nil), src.Record.Review.Acknowledged...)
			for _, ex := range src.Record.Review.ExcludedRaw {
				review.ExcludedRows = append(review.ExcludedRows, LegacyExcludedAudit{
					EvidenceID: ex.EvidenceID, Reason: ex.Reason, ExcludedAt: ex.ExcludedAt,
				})
			}
			for _, at := range src.Record.Review.AttendanceRaw {
				review.AttendanceRows = append(review.AttendanceRows, LegacyAttendanceAudit{
					EvidenceID: at.EvidenceID, Attended: at.Attended, DecidedAt: at.DecidedAt,
				})
			}
			cand.Receipt.LegacyReviews = append(cand.Receipt.LegacyReviews, review)
		}
		cand.Receipt.LegacyCollectedEvidence += rawLen(src.Record.CollectedRaw)

		// A duplicate local ID inside ONE source record is ambiguous identity.
		// It is rejected before aliasing or binding construction: neither row is
		// guessed from its time slot and no mapping is redirected.
		legacyIDs := map[string]bool{}
		ambiguousIDs := map[string]bool{}
		for _, li := range src.Record.Confirmed {
			id := strings.TrimSpace(li.ID)
			if id == "" {
				continue
			}
			if legacyIDs[id] {
				ambiguousIDs[id] = true
				continue
			}
			legacyIDs[id] = true
		}
		if len(ambiguousIDs) > 0 {
			addConflict(MigrationConflict{Code: "ambiguous_local_id", Date: src.Date,
				Detail: "a source record repeats a confirmed local interval id; the duplicate identity cannot be resolved automatically, so no alias or binding is guessed"})
		}

		for _, li := range src.Record.Confirmed {
			if li.Date != src.Date {
				addConflict(MigrationConflict{Code: "interval_date_mismatch", Date: src.Date,
					Detail: "a confirmed legacy interval belongs to a different date"})
				continue
			}
			if ambiguousIDs[strings.TrimSpace(li.ID)] {
				continue
			}
			description := strings.TrimSpace(li.Description)
			if description == "" {
				addConflict(MigrationConflict{Code: "unrepresentable_interval", Date: src.Date,
					Detail: "a confirmed legacy interval has an empty description"})
				continue
			}
			if len([]rune(description)) > maxDescriptionRunes {
				addConflict(MigrationConflict{Code: "unrepresentable_interval", Date: src.Date,
					Detail: "a confirmed legacy description exceeds the new description bound"})
				continue
			}
			if controlRune.MatchString(description) {
				addConflict(MigrationConflict{Code: "unrepresentable_interval", Date: src.Date,
					Detail: "a confirmed legacy description contains control characters"})
				continue
			}
			if len(li.EvidenceIDs) > maxEvidenceRefs {
				addConflict(MigrationConflict{Code: "too_many_evidence_refs", Date: src.Date,
					Detail: "a confirmed legacy interval carries more evidence references than the new bound; no reference is silently dropped"})
				continue
			}
			refs := make([]string, 0, len(li.EvidenceIDs))
			badRef := false
			for _, id := range li.EvidenceIDs {
				id = strings.TrimSpace(id)
				if id == "" || len([]rune(id)) > maxEvidenceRefRunes || controlRune.MatchString(id) {
					badRef = true
					break
				}
				refs = append(refs, id)
			}
			if badRef {
				addConflict(MigrationConflict{Code: "unrepresentable_interval", Date: src.Date,
					Detail: "a confirmed legacy interval carries an invalid evidence reference"})
				continue
			}

			rawClass := strings.TrimSpace(li.ClassificationID)
			localClass := ""
			if rawClass != "" {
				if mapped, ok := cfg.Migration.ClassificationMap[rawClass]; ok {
					localClass = mapped
				} else if identifierRE.MatchString(rawClass) {
					localClass = rawClass
				} else {
					addConflict(MigrationConflict{Code: "unrepresentable_classification", Date: src.Date,
						Detail: "a legacy classification value is not representable; provision migration.classification_map or correct it explicitly"})
					continue
				}
			}

			iv := Interval{
				Date:                     src.Date,
				Start:                    strings.TrimSpace(li.StartTime),
				End:                      strings.TrimSpace(li.EndTime),
				Classification:           localClass,
				Description:              description,
				EvidenceRefs:             refs,
				MigratedClassificationID: rawClass,
			}
			start, end, err := iv.resolveEndpoints(loc)
			if err != nil {
				addConflict(MigrationConflict{Code: "unrepresentable_interval", Date: src.Date,
					Detail: "a confirmed legacy interval is invalid in the explicit legacy timezone (" + errorCode(err) + ")"})
				continue
			}
			iv.ID = assignIntervalID(li.ID, src.Date, usedIDs)
			if li.ID != "" && li.ID != iv.ID {
				cand.Receipt.IDCrosswalk = append(cand.Receipt.IDCrosswalk, LegacyIDCrosswalk{
					Date: src.Date, LegacyID: li.ID, NewID: iv.ID,
				})
			}
			if li.ID != "" {
				bd.remapped[li.ID] = iv.ID
			}
			day.Confirmed = append(day.Confirmed, iv)
			bd.spans = append(bd.spans, migrationSpan{ID: iv.ID, Date: src.Date, Start: start, End: end})
			bd.audits = append(bd.audits, LegacyIntervalAudit{
				NewID: iv.ID, LegacyID: li.ID, Date: src.Date,
				ConfirmedAt: li.ConfirmedAt, Revision: li.Revision,
				Attendance: li.Attendance, Kind: li.Kind, Manual: li.Manual,
				Carryover: li.Carryover, SplitGroupID: li.SplitGroupID, PartIndex: li.PartIndex,
				LegacyProject: strings.TrimSpace(li.Project), LegacyClassID: li.ClassificationID,
				LegacyClassCode: li.ClassificationCode, LegacyClassLabel: li.ClassificationName,
			})
			if rawClass != "" {
				cand.Receipt.Revalidate = append(cand.Receipt.Revalidate, iv.ID)
				cand.Counts.Classified++
			}
		}

		// Remote mappings are preserved as stable one-to-one bindings. A mapping
		// whose local identity is dangling or already bound is a conflict.
		for _, lm := range src.Record.RemoteMappings {
			localID := lm.LocalID
			if remapped, ok := bd.remapped[localID]; ok {
				localID = remapped
			}
			if _, ok := indexOfInterval(day.Confirmed, localID); !ok {
				addConflict(MigrationConflict{Code: "dangling_mapping", Date: src.Date,
					Detail: "a legacy remote mapping names a local interval that is not part of this candidate"})
				continue
			}
			if _, err := parseDate(lm.Date); err != nil {
				addConflict(MigrationConflict{Code: "dangling_mapping", Date: src.Date,
					Detail: "a legacy remote mapping has no usable date"})
				continue
			}
			if strings.TrimSpace(lm.RemoteID) == "" || strings.TrimSpace(lm.LocalID) == "" {
				addConflict(MigrationConflict{Code: "dangling_mapping", Date: src.Date,
					Detail: "a legacy remote mapping lacks a stable identity"})
				continue
			}
			if boundRemotes[lm.RemoteID] || boundLocals[localID] {
				addConflict(MigrationConflict{Code: "identity_conflict", Date: src.Date,
					Detail: "a legacy remote mapping would not be one-to-one with the durable store"})
				continue
			}
			boundRemotes[lm.RemoteID] = true
			boundLocals[localID] = true
			b := RemoteBinding{
				LocalID: localID, RemoteID: lm.RemoteID, Date: lm.Date,
				StartTime: lm.StartTime, EndTime: lm.EndTime,
				ClassificationID: lm.ClassificationID, Description: lm.Description,
				CreatedAt: lm.CreatedAt, UpdatedAt: lm.UpdatedAt, Origin: "migration",
				MigrationID: cand.MigrationID,
			}
			if len(bytes.TrimSpace(lm.LastObservedRaw)) > 0 && !bytes.Equal(bytes.TrimSpace(lm.LastObservedRaw), []byte("null")) {
				var last legacyRemoteState
				var present map[string]json.RawMessage
				if json.Unmarshal(lm.LastObservedRaw, &last) == nil && json.Unmarshal(lm.LastObservedRaw, &present) == nil {
					if strings.TrimSpace(last.RemoteID) != "" {
						if _, err := parseDate(last.Date); err == nil {
							b.LastObserved = &RemoteEntry{
								RemoteID: last.RemoteID, Date: last.Date,
								StartTime: last.StartTime, EndTime: last.EndTime,
								ClassificationID: last.ClassificationID, Description: last.Description,
								Task: last.Task, Billable: cloneBool(last.Billable),
								ContentObserved: last.ContentObserved,
							}
							b.BaselineFields = observedFieldsPresent(present)
						}
					}
				}
			}
			cand.Bindings = append(cand.Bindings, b)
			cand.Counts.Mappings++
		}

		for _, lo := range src.Record.Outcomes {
			if lo.Status == "" {
				addConflict(MigrationConflict{Code: "malformed_outcome", Date: src.Date,
					Detail: "a legacy operation outcome has no status"})
				continue
			}
			if !knownOutcomeStatus(lo.Status) {
				addConflict(MigrationConflict{Code: "malformed_outcome", Date: src.Date,
					Detail: "a legacy operation outcome has an unrecognised status; it is never imported as safe history"})
				continue
			}
			localID := lo.LocalID
			if remapped, ok := bd.remapped[localID]; ok {
				localID = remapped
			}
			o := WriteOutcome{
				OperationKey: lo.OperationKey, LocalID: localID, RemoteID: lo.RemoteID,
				Action: lo.Action, Status: lo.Status, Date: lo.Date,
				StartTime: lo.StartTime, EndTime: lo.EndTime, Description: lo.Description,
				ErrorCode: lo.ErrorCode, OriginErrorCode: lo.OriginErrorCode,
				ClassificationID: lo.ClassificationID, Resolved: lo.Resolved,
				RecordedAt: lo.RecordedAt, IntentAt: lo.IntentAt, MigrationID: cand.MigrationID,
			}
			cand.Outcomes = append(cand.Outcomes, o)
			outcomeCount++
			if outcomeBlocks(o) {
				cand.Counts.Unresolved++
			}
		}

		for i, lc := range src.Record.Carries {
			if _, err := parseDate(lc.Date); err != nil {
				addConflict(MigrationConflict{Code: "malformed_carry", Date: src.Date,
					Detail: "a legacy carry fragment has no usable target date"})
				continue
			}
			if lc.Start != "" {
				if _, err := parseClock(lc.Start); err != nil {
					addConflict(MigrationConflict{Code: "malformed_carry", Date: src.Date,
						Detail: "a legacy carry fragment has an invalid start"})
					continue
				}
			}
			if lc.End != "" {
				if _, err := parseClock(lc.End); err != nil {
					addConflict(MigrationConflict{Code: "malformed_carry", Date: src.Date,
						Detail: "a legacy carry fragment has an invalid end"})
					continue
				}
			}
			// The fragment keeps its legacy project verbatim as an inert label
			// (ADR 0012); no configured project is required.
			project := strings.TrimSpace(lc.Project)
			fragment := fmt.Sprintf("carry-%s-%d", src.Date, i+1)
			if len(lc.EvidenceIDs) > maxEvidenceRefs {
				addConflict(MigrationConflict{Code: "too_many_evidence_refs", Date: src.Date,
					Detail: "a legacy carry fragment carries more evidence references than the new bound"})
				continue
			}
			cand.Carries = append(cand.Carries, MigratedCarry{
				FragmentID: fragment, SourceDate: lc.SourceDate, Date: lc.Date,
				Start: lc.Start, End: lc.End, Minutes: lc.Minutes,
				SplitGroupID: lc.SplitGroupID, PartIndex: lc.PartIndex, Kind: lc.Kind,
				Project: project, Description: lc.Description,
				EvidenceIDs: append([]string(nil), lc.EvidenceIDs...),
				Confirmed:   false, MigrationID: cand.MigrationID,
			})
			carryCount++
		}

		for _, lp := range src.Record.Plans {
			if len(cand.Receipt.LegacyPlanAudits) >= maxMigrationPlanRows {
				capacity("legacy_plan")
				break
			}
			audit := LegacyPlanAudit{
				PlanID: lp.PlanID, Date: lp.Date, Operations: len(lp.Operations),
				LocalFingerprint: lp.LocalFingerprint, RecordRevision: lp.RecordRevision,
				RemoteObservedAt: lp.RemoteObservedAt, RemoteComplete: lp.RemoteComplete,
				CreatedAt: lp.CreatedAt, Inert: true,
			}
			for _, op := range lp.Operations {
				row := LegacyOperationAudit{
					LocalID: op.LocalID, Kind: op.Kind, RemoteID: op.RemoteID, Date: op.Date,
					StartTime: op.StartTime, EndTime: op.EndTime,
					ClassificationID: op.ClassificationID, Description: op.Description,
					Task: op.Task, Billable: cloneBool(op.Billable), Reason: op.Reason,
				}
				if op.ExpectedRemote != nil {
					row.ExpectedRemote = &LegacyRemoteAudit{
						RemoteID: op.ExpectedRemote.RemoteID, Date: op.ExpectedRemote.Date,
						StartTime: op.ExpectedRemote.StartTime, EndTime: op.ExpectedRemote.EndTime,
						ClassificationID: op.ExpectedRemote.ClassificationID,
						Description:      op.ExpectedRemote.Description, Task: op.ExpectedRemote.Task,
						Billable:        cloneBool(op.ExpectedRemote.Billable),
						ObservedAt:      op.ExpectedRemote.ObservedAt,
						ContentObserved: op.ExpectedRemote.ContentObserved,
					}
				}
				audit.OperationRows = append(audit.OperationRows, row)
			}
			cand.Receipt.LegacyPlanAudits = append(cand.Receipt.LegacyPlanAudits, audit)
		}
		for _, la := range src.Record.Approvals {
			if len(cand.Receipt.LegacyApprovalsAudit) >= maxMigrationApprovalRows {
				capacity("legacy_approval")
				break
			}
			cand.Receipt.LegacyApprovalsAudit = append(cand.Receipt.LegacyApprovalsAudit, LegacyApprovalAudit{
				ApprovalID: la.ApprovalID, PlanID: la.PlanID, Date: la.Date,
				RecordRevision: la.RecordRevision, RemoteObservedAt: la.RemoteObservedAt,
				ApprovedAt: la.ApprovedAt, Superseded: la.Superseded,
				InvalidatedAt: la.InvalidatedAt, InvalidatedReason: la.InvalidatedReason,
				Inert: true, Usable: false,
			})
		}

		// Advance the local allocation counter past any preserved legacy id shape.
		prefix := "iv-" + strings.ReplaceAll(src.Date, "-", "") + "-"
		for _, iv := range day.Confirmed {
			if strings.HasPrefix(iv.ID, prefix) {
				if n, err := strconv.Atoi(strings.TrimPrefix(iv.ID, prefix)); err == nil && n >= day.NextInterval {
					day.NextInterval = n + 1
				}
			}
		}
		if day.NextInterval < 1 {
			day.NextInterval = 1
		}
		built = append(built, bd)
	}

	// Overlapping confirmed work is rejected for the whole candidate, including
	// overlap with existing durable work on neighbouring dates.
	spans := make([]migrationSpan, 0)
	for _, bd := range built {
		spans = append(spans, bd.spans...)
	}
	for _, day := range st.Days {
		locD, err := dayLocation(day.Timezone)
		if err != nil {
			continue
		}
		for _, iv := range day.Confirmed {
			s, e, err := iv.resolveEndpoints(locD)
			if err != nil {
				continue
			}
			spans = append(spans, migrationSpan{ID: iv.ID, Date: day.Date, Start: s, End: e})
		}
	}
	if blocker := detectOverlap(spans); blocker != nil {
		addConflict(MigrationConflict{Code: "overlap", Date: blocker.Date, Detail: blocker.Detail})
	}

	for _, bd := range built {
		cand.Days[bd.day.Date] = bd.day
		for _, audit := range bd.audits {
			if len(cand.Receipt.LegacyIntervals) >= maxMigrationIntervalRows {
				capacity("legacy_interval")
				break
			}
			cand.Receipt.LegacyIntervals = append(cand.Receipt.LegacyIntervals, audit)
		}
		cand.Counts.Intervals += len(bd.day.Confirmed)
	}
	cand.Counts.Days = len(cand.Days)
	cand.Counts.Outcomes = outcomeCount
	cand.Counts.Carries = carryCount
	cand.Counts.LegacyPlans = len(cand.Receipt.LegacyPlanAudits)
	cand.Counts.LegacyApprovals = len(cand.Receipt.LegacyApprovalsAudit)
	cand.Counts.Revalidate = len(cand.Receipt.Revalidate)
	cand.Receipt.Intervals = cand.Counts.Intervals
	cand.Receipt.Classified = cand.Counts.Classified
	cand.Receipt.Mappings = cand.Counts.Mappings
	cand.Receipt.Outcomes = cand.Counts.Outcomes
	cand.Receipt.Carries = cand.Counts.Carries
	cand.Receipt.LegacyPlans = cand.Counts.LegacyPlans
	cand.Receipt.LegacyApprovals = cand.Counts.LegacyApprovals
	cand.Receipt.MigrationID = cand.MigrationID
	cand.Receipt.SourceIdentity = cand.SourceIdentity
	bindMigrationContent(cand)

	cand.Ready = len(cand.Conflicts) == 0
	return cand, nil
}

// bindMigrationContent folds the exact payload apply would commit (days,
// bindings, outcomes, carries, receipt and conflicts) into the migration_id.
// Each command reloads the configuration, so a classification map
// edited between preview and apply changes this payload and therefore the id:
// apply can only commit what the preview showed (PR #275 review). The payload
// is built deterministically and carries no timestamps of its own (AppliedAt
// is set at merge), and the provisional id it embeds is itself a function of
// the source identity and the target fingerprint.
func bindMigrationContent(cand *migrationCandidate) {
	payload, err := json.Marshal(struct {
		Days      map[string]*Day     `json:"days"`
		Bindings  []RemoteBinding     `json:"bindings"`
		Outcomes  []WriteOutcome      `json:"outcomes"`
		Carries   []MigratedCarry     `json:"carries"`
		Receipt   MigrationReceipt    `json:"receipt"`
		Conflicts []MigrationConflict `json:"conflicts"`
	}{cand.Days, cand.Bindings, cand.Outcomes, cand.Carries, cand.Receipt, cand.Conflicts})
	if err != nil {
		payload = nil
	}
	id := sha256Hex([]byte(cand.MigrationID + "|" + sha256Hex(payload)))
	cand.MigrationID = id
	cand.Receipt.MigrationID = id
	for i := range cand.Bindings {
		cand.Bindings[i].MigrationID = id
	}
	for i := range cand.Outcomes {
		cand.Outcomes[i].MigrationID = id
	}
	for i := range cand.Carries {
		cand.Carries[i].MigrationID = id
	}
}

type migrationSpan struct {
	ID    string
	Date  string
	Start time.Time
	End   time.Time
}

// detectOverlap is the same absolute-time overlap rule the local workflow uses.
func detectOverlap(spans []migrationSpan) *Blocker {
	if len(spans) < 2 {
		return nil
	}
	sorted := append([]migrationSpan(nil), spans...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].Start.Equal(sorted[j].Start) {
			return sorted[i].Start.Before(sorted[j].Start)
		}
		return sorted[i].ID < sorted[j].ID
	})
	maxEnd := sorted[0].End
	maxID := sorted[0].ID
	for _, span := range sorted[1:] {
		if span.Start.Before(maxEnd) {
			return &Blocker{Code: "overlap", Date: span.Date, LocalID: span.ID,
				Detail: "overlapping confirmed work between " + fmtID(maxID) + " and " + fmtID(span.ID)}
		}
		if span.End.After(maxEnd) {
			maxEnd = span.End
			maxID = span.ID
		}
	}
	return nil
}

func indexOfInterval(intervals []Interval, id string) (int, bool) {
	if id == "" {
		return -1, false
	}
	for i := range intervals {
		if intervals[i].ID == id {
			return i, true
		}
	}
	return -1, false
}

func validMigrationID(id string) bool {
	if id == "" || len(id) > 128 || controlRune.MatchString(id) {
		return false
	}
	return true
}

func assignIntervalID(legacyID, date string, used map[string]bool) string {
	legacyID = strings.TrimSpace(legacyID)
	if validMigrationID(legacyID) && !used[legacyID] {
		used[legacyID] = true
		return legacyID
	}
	prefix := "iv-" + strings.ReplaceAll(date, "-", "") + "-"
	for n := 1; ; n++ {
		id := prefix + strconv.Itoa(n)
		if !used[id] {
			used[id] = true
			return id
		}
	}
}

func receiptsOf(st *State) []MigrationReceipt {
	if st == nil || st.Migration == nil {
		return nil
	}
	return st.Migration.Receipts
}

// knownOutcomeStatus reports whether a legacy outcome status is one of the
// canonical workflow statuses. An unrecognised value is never treated as safe.
func knownOutcomeStatus(status string) bool {
	switch status {
	case "confirmed", "failed", "skipped", "in_flight", "uncertain", "unidentified":
		return true
	}
	return false
}

// outcomeBlocks reports whether a preserved legacy write outcome must block
// readiness. It follows the canonical legacy predicate
// (internal/submit/resolve.go: IsUnresolved): a resolved row never blocks, and
// only in_flight/uncertain/unidentified rows block while unresolved. Confirmed,
// failed and skipped are TERMINAL history, not pending writes, even though their
// manual-resolution flag is false. An unknown/malformed status is treated as
// unresolved rather than silently becoming safe.
func outcomeBlocks(o WriteOutcome) bool {
	if o.Resolved {
		return false
	}
	switch o.Status {
	case "in_flight", "uncertain", "unidentified":
		return true
	case "confirmed", "failed", "skipped":
		return false
	}
	return true
}

// unresolvedOutcomeDetail explains one blocking preserved outcome truthfully.
func unresolvedOutcomeDetail(o WriteOutcome) string {
	if !knownOutcomeStatus(o.Status) {
		return "a preserved write outcome has an unrecognised status and is treated as unresolved; R3 never replays or clears it"
	}
	return "a preserved " + o.Status + " write outcome is unresolved; R3 never replays or clears it"
}

// --- identity ----------------------------------------------------------------

func migrationSourceIdentity(dates []string, tz string, hashes map[string]string) string {
	type identity struct {
		Dates    []string          `json:"dates"`
		Timezone string            `json:"timezone"`
		Hashes   map[string]string `json:"hashes"`
	}
	data, err := json.Marshal(identity{Dates: dates, Timezone: tz, Hashes: hashes})
	if err != nil {
		return ""
	}
	return sha256Hex(data)
}

func migrationID(sourceIdentity, targetFingerprint string) string {
	return sha256Hex([]byte(sourceIdentity + "|" + targetFingerprint))
}

func targetFingerprint(st *State) string {
	if st == nil {
		return sha256Hex(nil)
	}
	cp := st.cloneState()
	cp.SavedAt = ""
	cp.Schema = stateSchema
	data, err := json.Marshal(cp)
	if err != nil {
		return ""
	}
	return sha256Hex(data)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// --- activation --------------------------------------------------------------

// mergeMigrationCandidate applies a validated candidate to a clone of the
// durable snapshot. Nothing is written here; the caller commits the whole
// candidate through Store.CommitState.
func mergeMigrationCandidate(st *State, cand *migrationCandidate) *State {
	next := st.cloneState()
	if next.Migration == nil {
		next.Migration = &MigrationState{Sources: map[string]string{}}
	}
	if next.Migration.Sources == nil {
		next.Migration.Sources = map[string]string{}
	}
	if next.Remote == nil {
		next.Remote = &RemoteState{}
	}
	for date, day := range cand.Days {
		next.Days[date] = day.clone()
	}
	next.Remote.Bindings = append(next.Remote.Bindings, cand.Bindings...)
	next.Remote.Outcomes = append(next.Remote.Outcomes, cand.Outcomes...)
	next.Migration.Carries = append(next.Migration.Carries, cand.Carries...)
	receipt := cand.Receipt
	receipt.AppliedAt = time.Now().UTC().Format(time.RFC3339)
	next.Migration.Receipts = append(next.Migration.Receipts, receipt)
	next.Migration.Sources[cand.SourceIdentity] = cand.MigrationID
	return next
}
