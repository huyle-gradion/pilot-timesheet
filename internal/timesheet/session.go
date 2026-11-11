package timesheet

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Request is the strict JSON request envelope of the in-process dispatcher
// (HandleLine) that the commands and the TUI drive. Unknown fields
// are rejected. Paths are never request fields: the caller supplies them as flags.
type Request struct {
	ID           string   `json:"id"`
	Op           string   `json:"op"`
	Day          string   `json:"day,omitempty"`
	Revision     *int     `json:"revision,omitempty"`
	IntervalID   string   `json:"interval_id,omitempty"`
	Text         string   `json:"text,omitempty"`
	EvidenceRefs []string `json:"evidence_refs,omitempty"`
	Source       string   `json:"source,omitempty"`
	CollectionID string   `json:"collection_id,omitempty"`
	MeetingID    string   `json:"meeting_id,omitempty"`
	Decision     string   `json:"decision,omitempty"`

	// R3 fields. They are additive request fields; each operation rejects any
	// field it does not use.
	Dates            []string `json:"dates,omitempty"`
	MigrationID      string   `json:"migration_id,omitempty"`
	Query            string   `json:"query,omitempty"`
	CatalogID        string   `json:"catalog_id,omitempty"`
	ClassificationID string   `json:"classification_id,omitempty"`
	ObservationID    string   `json:"observation_id,omitempty"`
	LocalID          string   `json:"local_id,omitempty"`
	RemoteID         string   `json:"remote_id,omitempty"`

	// R4 native approval/verified-submission fields. plan_id, approval_id and
	// write_ref identify one exact immutable thing; a request can never point
	// at "the current plan" or a fuzzy date.
	PlanID     string `json:"plan_id,omitempty"`
	ApprovalID string `json:"approval_id,omitempty"`
	WriteRef   string `json:"write_ref,omitempty"`
}

// ProtocolError is a stable, compact error. It never carries stack traces,
// configuration dumps, credential values, or file contents.
type ProtocolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Response echoes the request ID and carries exactly one of result or error.
type Response struct {
	ID     string         `json:"id"`
	OK     bool           `json:"ok"`
	Result any            `json:"result,omitempty"`
	Error  *ProtocolError `json:"error,omitempty"`
}

// DaySummary is returned after opening or mutating a day.
type DaySummary struct {
	Date          string `json:"date"`
	Timezone      string `json:"timezone"`
	Revision      int    `json:"revision"`
	SavedRevision int    `json:"saved_revision"`
	Dirty         bool   `json:"dirty"`
	Confirmed     View   `json:"confirmed"`
	Staged        View   `json:"staged"`
}

// IntervalMutation is returned after staging an add, edit, or removal.
type IntervalMutation struct {
	Date          string `json:"date"`
	Revision      int    `json:"revision"`
	SavedRevision int    `json:"saved_revision"`
	Dirty         bool   `json:"dirty"`
	IntervalID    string `json:"interval_id,omitempty"`
	Staged        View   `json:"staged"`
}

// ConfirmReceipt reports an in-memory confirmation. It is durable only after a
// successful save.
type ConfirmReceipt struct {
	Date          string `json:"date"`
	Revision      int    `json:"revision"`
	SavedRevision int    `json:"saved_revision"`
	Dirty         bool   `json:"dirty"`
	Durable       bool   `json:"durable"`
	Intervals     int    `json:"intervals"`
	TotalMinutes  int    `json:"total_minutes"`
	Confirmed     View   `json:"confirmed"`
}

// SaveReceipt reports a durable local save, with no remote effect.
type SaveReceipt struct {
	Date          string `json:"date"`
	Revision      int    `json:"revision"`
	SavedRevision int    `json:"saved_revision"`
	SavedAt       string `json:"saved_at"`
	Intervals     int    `json:"intervals"`
	TotalMinutes  int    `json:"total_minutes"`
	Confirmed     View   `json:"confirmed"`
}

// CrossMidnight explains where both dates' minutes are recorded.
type CrossMidnight struct {
	IntervalID   string       `json:"interval_id"`
	Start        string       `json:"start"`
	End          string       `json:"end"`
	TotalMinutes int          `json:"total_minutes"`
	Allocations  []Allocation `json:"allocations"`
}

// Review is the human-readable local review payload. Evidence is the day's
// structured collection view (topic candidates, proposed meetings, and
// per-source completeness). It contains bounded structured provider identities
// and links only — never provider documents, raw payloads, or credential
// values.
type Review struct {
	Date          string          `json:"date"`
	Timezone      string          `json:"timezone"`
	Revision      int             `json:"revision"`
	SavedRevision int             `json:"saved_revision"`
	Dirty         bool            `json:"dirty"`
	Confirmed     View            `json:"confirmed"`
	Staged        View            `json:"staged"`
	History       []HistoryEntry  `json:"history,omitempty"`
	CrossMidnight []CrossMidnight `json:"cross_midnight,omitempty"`
	Evidence      *EvidenceReview `json:"evidence,omitempty"`
	Notes         []string        `json:"notes,omitempty"`
}

// EvidenceReview is the local review surface for R2 collection state.
type EvidenceReview struct {
	Sources  []ComposedSourceStatus `json:"sources"`
	Topics   []TopicCandidate       `json:"topics"`
	Meetings []ProposedMeeting      `json:"meetings"`
}

// DaySourceStatus is one enabled source's latest collection attempt state for
// the day (issue #162): it surfaces the attempt's status and collection_id so
// acknowledge/decide can copy the id without reading state.json. A source with
// no recorded attempt reports "unattempted" and no collection_id.
type DaySourceStatus struct {
	Source       string `json:"source"`
	Status       string `json:"status"`
	CollectionID string `json:"collection_id,omitempty"`
}

// DayStatus is a compact status entry for one loaded day.
type DayStatus struct {
	Date               string            `json:"date"`
	Timezone           string            `json:"timezone"`
	Revision           int               `json:"revision"`
	SavedRevision      int               `json:"saved_revision"`
	Dirty              bool              `json:"dirty"`
	ConfirmedIntervals int               `json:"confirmed_intervals"`
	StagedIntervals    int               `json:"staged_intervals"`
	ConfirmedMinutes   int               `json:"confirmed_minutes"`
	StagedMinutes      int               `json:"staged_minutes"`
	Sources            []DaySourceStatus `json:"sources,omitempty"`
}

// StatusResult reports loaded days and the build and store identity. It exposes no
// configuration values, credential state or remote endpoint.
type StatusResult struct {
	AppVersion          string      `json:"app_version"`
	StoreSchema         int         `json:"store_schema"`
	DurableSchema       int         `json:"durable_schema"`
	DurabilityUncertain bool        `json:"durability_uncertain"`
	MigrationReceipts   int         `json:"migration_receipts"`
	RemoteBindings      int         `json:"remote_bindings"`
	UnresolvedWrites    int         `json:"unresolved_writes"`
	Plans               int         `json:"plans"`
	Approvals           int         `json:"approvals"`
	NativeUnresolved    int         `json:"native_unresolved_writes"`
	Days                []DayStatus `json:"days"`
}

// EnvCheckResult reports required variable NAMES only, never values.
type EnvCheckResult struct {
	Required []string `json:"required"`
	Missing  []string `json:"missing"`
	Present  []string `json:"present"`
}

type closeResult struct {
	Closed bool `json:"closed"`
}

// Session is one in-process daily workflow. It owns working copies of opened
// days; only explicit save commits them to the Store.
type Session struct {
	cfg   *Config
	env   map[string]string
	store *Store
	days  map[string]*Day
}

// NewSession creates a session over an opened store.
func NewSession(cfg *Config, env map[string]string, store *Store) *Session {
	return &Session{
		cfg: cfg, env: env, store: store,
		days: map[string]*Day{},
	}
}

func errorResponse(id string, err error) Response {
	var app *AppError
	if errors.As(err, &app) {
		return Response{ID: id, OK: false, Error: &ProtocolError{Code: app.Code, Message: app.Message}}
	}
	return Response{ID: id, OK: false, Error: &ProtocolError{
		Code: CodeInternal, Message: "request failed without a safe diagnostic",
	}}
}

// HandleLine decodes and dispatches one request. The bool reports close.
func (s *Session) HandleLine(line []byte) (Response, bool) {
	req, err := decodeRequest(line)
	if err != nil {
		return errorResponse("", err), false
	}
	if err := validateRequestID(req.ID); err != nil {
		return errorResponse("", err), false
	}
	switch req.Op {
	case "open_day":
		if err := rejectFields(req, "day"); err != nil {
			return errorResponse(req.ID, err), false
		}
		result, err := s.openDay(req)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, result), false

	case "add_interval":
		if err := rejectFields(req, "day", "revision", "text", "evidence_refs"); err != nil {
			return errorResponse(req.ID, err), false
		}
		result, err := s.addInterval(req)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, result), false

	case "edit_interval":
		if err := rejectFields(req, "day", "revision", "interval_id", "text"); err != nil {
			return errorResponse(req.ID, err), false
		}
		result, err := s.editInterval(req)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, result), false

	case "remove_interval":
		if err := rejectFields(req, "day", "revision", "interval_id"); err != nil {
			return errorResponse(req.ID, err), false
		}
		result, err := s.removeInterval(req)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, result), false

	case "review":
		if err := rejectFields(req, "day"); err != nil {
			return errorResponse(req.ID, err), false
		}
		result, err := s.review(req)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, result), false

	case "confirm":
		if err := rejectFields(req, "day", "revision"); err != nil {
			return errorResponse(req.ID, err), false
		}
		result, err := s.confirm(req)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, result), false

	case "save":
		if err := rejectFields(req, "day", "revision"); err != nil {
			return errorResponse(req.ID, err), false
		}
		result, err := s.save(req)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, result), false

	case "status":
		if err := rejectFields(req); err != nil {
			return errorResponse(req.ID, err), false
		}
		result, err := s.status()
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, result), false

	case "check_environment":
		if err := rejectFields(req); err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, s.checkEnvironment()), false

	case "close":
		if err := rejectFields(req); err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, closeResult{Closed: true}), true

	case "collect":
		if err := rejectFields(req, "day"); err != nil {
			return errorResponse(req.ID, err), false
		}
		result, err := s.collectDay(req)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, result), false

	case "acknowledge_source":
		if err := rejectFields(req, "day", "source", "collection_id"); err != nil {
			return errorResponse(req.ID, err), false
		}
		result, err := s.acknowledgeSource(req)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, result), false

	case "decide_meeting":
		if err := rejectFields(req, "day", "meeting_id", "decision", "collection_id"); err != nil {
			return errorResponse(req.ID, err), false
		}
		result, err := s.decideMeeting(req)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, result), false

	case "migration_preview":
		if err := rejectFields(req, "dates"); err != nil {
			return errorResponse(req.ID, err), false
		}
		result, err := s.migrationPreview(req)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, result), false

	case "migration_apply":
		if err := rejectFields(req, "dates", "migration_id"); err != nil {
			return errorResponse(req.ID, err), false
		}
		result, err := s.migrationApply(req)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, result), false

	case "classifications":
		if err := rejectFields(req, "query"); err != nil {
			return errorResponse(req.ID, err), false
		}
		result, err := s.classifications(req)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, result), false

	case "select_classification":
		if err := rejectFields(req, "day", "revision", "interval_id", "catalog_id", "classification_id"); err != nil {
			return errorResponse(req.ID, err), false
		}
		result, err := s.selectClassification(req)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, result), false

	case "reconcile_remote":
		if err := rejectFields(req, "day"); err != nil {
			return errorResponse(req.ID, err), false
		}
		date, err := requiredDay(req)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		result, err := s.reconcileDate(date)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, result), false

	case "resolve_remote":
		if err := rejectFields(req, "day", "revision", "observation_id", "remote_id", "local_id", "decision"); err != nil {
			return errorResponse(req.ID, err), false
		}
		result, err := s.resolveRemoteRequest(req)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, result), false

	case "preview_plan":
		if err := rejectFields(req, "day"); err != nil {
			return errorResponse(req.ID, err), false
		}
		date, err := requiredDay(req)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		result, err := s.previewPlan(date)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, result), false

	case "approve_plan":
		if err := rejectFields(req, "day", "plan_id"); err != nil {
			return errorResponse(req.ID, err), false
		}
		result, err := s.approvePlan(req)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, result), false

	case "revoke_approval":
		if err := rejectFields(req, "day", "approval_id"); err != nil {
			return errorResponse(req.ID, err), false
		}
		result, err := s.revokeApproval(req)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, result), false

	case "submit_plan":
		if err := rejectFields(req, "day", "approval_id"); err != nil {
			return errorResponse(req.ID, err), false
		}
		result, err := s.submitPlan(req)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, result), false

	case "submission_status":
		if err := rejectFields(req, "day"); err != nil {
			return errorResponse(req.ID, err), false
		}
		result, err := s.submissionStatus(req)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, result), false

	case "resolve_write":
		if err := rejectFields(req, "write_ref", "observation_id", "decision", "remote_id"); err != nil {
			return errorResponse(req.ID, err), false
		}
		result, err := s.resolveWrite(req)
		if err != nil {
			return errorResponse(req.ID, err), false
		}
		return successResponse(req.ID, result), false

	default:
		return errorResponse(req.ID, newError(CodeUnknownOp, "op is not supported by this build")), false
	}
}

func successResponse(id string, result any) Response {
	return Response{ID: id, OK: true, Result: result}
}

func decodeRequest(line []byte) (Request, error) {
	var req Request
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return Request{}, newError(CodeBadRequest, "request is not valid JSON for the supported schema")
	}
	if err := ensureJSONEOF(dec); err != nil {
		return Request{}, newError(CodeBadRequest, "request has trailing data")
	}
	if strings.TrimSpace(req.Op) == "" {
		return Request{}, newError(CodeBadRequest, "op is required")
	}
	return req, nil
}

func validateRequestID(id string) error {
	if id == "" {
		return newError(CodeBadRequest, "id is required")
	}
	if len(id) > 128 || controlRune.MatchString(id) {
		return newError(CodeBadRequest, "id is invalid")
	}
	return nil
}

func rejectFields(req Request, allowed ...string) error {
	allowedSet := map[string]bool{"id": true, "op": true}
	for _, field := range allowed {
		allowedSet[field] = true
	}
	set := map[string]bool{}
	if req.Day != "" {
		set["day"] = true
	}
	if req.Revision != nil {
		set["revision"] = true
	}
	if req.IntervalID != "" {
		set["interval_id"] = true
	}
	if req.Text != "" {
		set["text"] = true
	}
	if req.EvidenceRefs != nil {
		set["evidence_refs"] = true
	}
	if req.Source != "" {
		set["source"] = true
	}
	if req.CollectionID != "" {
		set["collection_id"] = true
	}
	if req.MeetingID != "" {
		set["meeting_id"] = true
	}
	if req.Decision != "" {
		set["decision"] = true
	}
	if req.Dates != nil {
		set["dates"] = true
	}
	if req.MigrationID != "" {
		set["migration_id"] = true
	}
	if req.Query != "" {
		set["query"] = true
	}
	if req.CatalogID != "" {
		set["catalog_id"] = true
	}
	if req.ClassificationID != "" {
		set["classification_id"] = true
	}
	if req.ObservationID != "" {
		set["observation_id"] = true
	}
	if req.LocalID != "" {
		set["local_id"] = true
	}
	if req.RemoteID != "" {
		set["remote_id"] = true
	}
	for field := range set {
		if !allowedSet[field] {
			return newError(CodeBadRequest, "field is not allowed for this op: "+field)
		}
	}
	return nil
}

func requiredDay(req Request) (string, error) {
	if strings.TrimSpace(req.Day) == "" {
		return "", newError(CodeBadRequest, "day is required")
	}
	return req.Day, nil
}

func requiredRevision(req Request) (int, error) {
	if req.Revision == nil {
		return 0, newError(CodeBadRequest, "revision is required")
	}
	if *req.Revision < 0 {
		return 0, newError(CodeBadRequest, "revision must not be negative")
	}
	return *req.Revision, nil
}

func (s *Session) loadedDay(date string) (*Day, error) {
	if _, err := parseDate(date); err != nil {
		return nil, err
	}
	day, ok := s.days[date]
	if !ok {
		return nil, newError(CodeUnknownDay, "day is not open; call open_day first")
	}
	return day, nil
}

func checkRevision(day *Day, revision int) error {
	if revision != day.Revision {
		return newError(CodeStaleRevision, fmt.Sprintf(
			"expected revision %d but the day is at revision %d; review again before changing it",
			revision, day.Revision))
	}
	return nil
}

func (s *Session) openDay(req Request) (DaySummary, error) {
	date, err := requiredDay(req)
	if err != nil {
		return DaySummary{}, err
	}
	if _, err := parseDate(date); err != nil {
		return DaySummary{}, err
	}
	day, ok := s.days[date]
	if !ok {
		if saved, exists := s.store.State().Days[date]; exists {
			day = saved.clone()
		} else {
			day = &Day{Date: date, Timezone: s.cfg.Timezone, Revision: 0, NextInterval: 1}
		}
		day.Staged = cloneIntervals(day.Confirmed)
		day.SavedRevision = day.Revision
		s.days[date] = day
	}
	return s.summary(day)
}

func (s *Session) addInterval(req Request) (IntervalMutation, error) {
	date, err := requiredDay(req)
	if err != nil {
		return IntervalMutation{}, err
	}
	revision, err := requiredRevision(req)
	if err != nil {
		return IntervalMutation{}, err
	}
	day, err := s.loadedDay(date)
	if err != nil {
		return IntervalMutation{}, err
	}
	if err := checkRevision(day, revision); err != nil {
		return IntervalMutation{}, err
	}
	if strings.TrimSpace(req.Text) == "" {
		return IntervalMutation{}, newError(CodeBadRequest, "text is required")
	}
	loc, err := dayLocation(day.Timezone)
	if err != nil {
		return IntervalMutation{}, err
	}
	iv, err := ParseInterval(req.Text, day.Date, loc)
	if err != nil {
		return IntervalMutation{}, err
	}
	refs, err := normalizeEvidenceRefs(req.EvidenceRefs)
	if err != nil {
		return IntervalMutation{}, err
	}
	iv.EvidenceRefs = refs
	iv.ID = day.nextIntervalID()

	previous := cloneIntervals(day.Staged)
	day.Staged = append(day.Staged, iv)
	if err := s.validateStaged(day); err != nil {
		day.Staged = previous
		return IntervalMutation{}, err
	}
	staged, err := s.viewFor(day, day.Staged)
	if err != nil {
		day.Staged = previous
		return IntervalMutation{}, err
	}
	return IntervalMutation{
		Date:          day.Date,
		Revision:      day.Revision,
		SavedRevision: day.SavedRevision,
		Dirty:         !intervalsEqual(day.Staged, day.Confirmed),
		IntervalID:    iv.ID,
		Staged:        staged,
	}, nil
}

func (s *Session) editInterval(req Request) (IntervalMutation, error) {
	date, err := requiredDay(req)
	if err != nil {
		return IntervalMutation{}, err
	}
	revision, err := requiredRevision(req)
	if err != nil {
		return IntervalMutation{}, err
	}
	if req.IntervalID == "" {
		return IntervalMutation{}, newError(CodeBadRequest, "interval_id is required")
	}
	if strings.TrimSpace(req.Text) == "" {
		return IntervalMutation{}, newError(CodeBadRequest, "text is required")
	}
	day, err := s.loadedDay(date)
	if err != nil {
		return IntervalMutation{}, err
	}
	if err := checkRevision(day, revision); err != nil {
		return IntervalMutation{}, err
	}
	index := indexOf(day.Staged, req.IntervalID)
	if index < 0 {
		return IntervalMutation{}, newError(CodeUnknownInterval, "interval is not in the staged day; review again")
	}
	loc, err := dayLocation(day.Timezone)
	if err != nil {
		return IntervalMutation{}, err
	}
	previous := cloneIntervals(day.Staged)
	replacement, err := ParseInterval(req.Text, day.Date, loc)
	if err != nil {
		return IntervalMutation{}, err
	}
	replacement.ID = req.IntervalID
	replacement.EvidenceRefs = append([]string(nil), day.Staged[index].EvidenceRefs...)
	day.Staged[index] = replacement
	if err := s.validateStaged(day); err != nil {
		day.Staged = previous
		return IntervalMutation{}, err
	}
	staged, err := s.viewFor(day, day.Staged)
	if err != nil {
		day.Staged = previous
		return IntervalMutation{}, err
	}
	return IntervalMutation{
		Date:          day.Date,
		Revision:      day.Revision,
		SavedRevision: day.SavedRevision,
		Dirty:         !intervalsEqual(day.Staged, day.Confirmed),
		IntervalID:    req.IntervalID,
		Staged:        staged,
	}, nil
}

func (s *Session) removeInterval(req Request) (IntervalMutation, error) {
	date, err := requiredDay(req)
	if err != nil {
		return IntervalMutation{}, err
	}
	revision, err := requiredRevision(req)
	if err != nil {
		return IntervalMutation{}, err
	}
	if req.IntervalID == "" {
		return IntervalMutation{}, newError(CodeBadRequest, "interval_id is required")
	}
	day, err := s.loadedDay(date)
	if err != nil {
		return IntervalMutation{}, err
	}
	if err := checkRevision(day, revision); err != nil {
		return IntervalMutation{}, err
	}
	index := indexOf(day.Staged, req.IntervalID)
	if index < 0 {
		return IntervalMutation{}, newError(CodeUnknownInterval, "interval is not in the staged day; review again")
	}
	previous := cloneIntervals(day.Staged)
	day.Staged = append(day.Staged[:index], day.Staged[index+1:]...)
	if err := s.validateStaged(day); err != nil {
		day.Staged = previous
		return IntervalMutation{}, err
	}
	staged, err := s.viewFor(day, day.Staged)
	if err != nil {
		day.Staged = previous
		return IntervalMutation{}, err
	}
	return IntervalMutation{
		Date:          day.Date,
		Revision:      day.Revision,
		SavedRevision: day.SavedRevision,
		Dirty:         !intervalsEqual(day.Staged, day.Confirmed),
		IntervalID:    req.IntervalID,
		Staged:        staged,
	}, nil
}

func (s *Session) review(req Request) (Review, error) {
	date, err := requiredDay(req)
	if err != nil {
		return Review{}, err
	}
	day, err := s.loadedDay(date)
	if err != nil {
		return Review{}, err
	}
	confirmed, err := s.viewFor(day, day.Confirmed)
	if err != nil {
		return Review{}, err
	}
	staged, err := s.viewFor(day, day.Staged)
	if err != nil {
		return Review{}, err
	}
	review := Review{
		Date:          day.Date,
		Timezone:      day.Timezone,
		Revision:      day.Revision,
		SavedRevision: day.SavedRevision,
		Dirty:         !intervalsEqual(day.Staged, day.Confirmed),
		Confirmed:     confirmed,
		Staged:        staged,
		History:       historyWithoutLegacyLabels(day.History),
		Notes: []string{
			"Confirmed work is local only; remote submission is not implemented in this build.",
			"Staged changes become durable only after confirm and save.",
			"This session view includes unsaved confirmations on other open days; save receipts use durable state only.",
			"Evidence topic candidates and proposed meetings never imply worked duration; attendance and completion are only ever explicit local decisions.",
		},
	}
	review.Evidence = composedEvidenceReview(s.cfg, s.store.Evidence(day.Date))
	loc, err := dayLocation(day.Timezone)
	if err != nil {
		return Review{}, err
	}
	for _, iv := range day.Staged {
		if !iv.CrossesMidnight() {
			continue
		}
		total, err := iv.Duration(loc)
		if err != nil {
			return Review{}, err
		}
		allocations, err := iv.ResolvedAllocations(loc)
		if err != nil {
			return Review{}, err
		}
		review.CrossMidnight = append(review.CrossMidnight, CrossMidnight{
			IntervalID:   iv.ID,
			Start:        iv.Date + " " + iv.Start,
			End:          iv.End,
			TotalMinutes: total,
			Allocations:  allocations,
		})
	}
	for _, seg := range review.Staged.Segments {
		if seg.Locked {
			review.Notes = append(review.Notes,
				"Segments marked locked started on an earlier day and are edited there.")
			break
		}
	}
	if s.store.Uncertain() {
		review.Notes = append(review.Notes,
			"Local state durability is uncertain after a failed save; close and restart before saving again.")
	}
	return review, nil
}

func (s *Session) confirm(req Request) (ConfirmReceipt, error) {
	date, err := requiredDay(req)
	if err != nil {
		return ConfirmReceipt{}, err
	}
	revision, err := requiredRevision(req)
	if err != nil {
		return ConfirmReceipt{}, err
	}
	day, err := s.loadedDay(date)
	if err != nil {
		return ConfirmReceipt{}, err
	}
	if err := checkRevision(day, revision); err != nil {
		return ConfirmReceipt{}, err
	}
	if len(day.Staged) == 0 && len(day.Confirmed) == 0 {
		return ConfirmReceipt{}, newError(CodeNothingToConfirm, "there are no staged intervals to confirm")
	}
	if err := s.validateStaged(day); err != nil {
		return ConfirmReceipt{}, err
	}
	if err := s.checkSourceGate(day.Date); err != nil {
		return ConfirmReceipt{}, err
	}
	view, err := s.viewFor(day, day.Staged)
	if err != nil {
		return ConfirmReceipt{}, err
	}
	day.Confirmed = cloneIntervals(day.Staged)
	day.Revision++
	day.Staged = cloneIntervals(day.Confirmed)
	day.History = append(day.History, HistoryEntry{
		Revision:     day.Revision,
		ConfirmedAt:  time.Now().UTC().Format(time.RFC3339),
		Intervals:    len(day.Confirmed),
		TotalMinutes: view.TotalMinutes,
		Snapshot:     cloneIntervals(day.Confirmed),
	})
	return ConfirmReceipt{
		Date:          day.Date,
		Revision:      day.Revision,
		SavedRevision: day.SavedRevision,
		Dirty:         false,
		Durable:       false,
		Intervals:     len(day.Confirmed),
		TotalMinutes:  view.TotalMinutes,
		Confirmed:     view,
	}, nil
}

func (s *Session) save(req Request) (SaveReceipt, error) {
	date, err := requiredDay(req)
	if err != nil {
		return SaveReceipt{}, err
	}
	revision, err := requiredRevision(req)
	if err != nil {
		return SaveReceipt{}, err
	}
	day, err := s.loadedDay(date)
	if err != nil {
		return SaveReceipt{}, err
	}
	if err := checkRevision(day, revision); err != nil {
		return SaveReceipt{}, err
	}
	if err := s.validateConfirmed(day); err != nil {
		return SaveReceipt{}, err
	}
	if err := s.checkSourceGate(day.Date); err != nil {
		return SaveReceipt{}, err
	}
	// The receipt must describe what a restart will load after this save. Other
	// open days' unsaved confirmations are intentionally excluded.
	view, err := s.durableView(day, day.Confirmed)
	if err != nil {
		return SaveReceipt{}, err
	}
	// The persisted history entry for the revision being saved must agree with
	// the durable view, not with an earlier session-only spill calculation.
	if n := len(day.History); n > 0 && day.History[n-1].Revision == day.Revision {
		day.History[n-1].TotalMinutes = view.TotalMinutes
	}
	if err := s.store.Commit(day.SavedRevision, day); err != nil {
		return SaveReceipt{}, err
	}
	day.SavedRevision = day.Revision
	return SaveReceipt{
		Date:          day.Date,
		Revision:      day.Revision,
		SavedRevision: day.SavedRevision,
		SavedAt:       s.store.State().SavedAt,
		Intervals:     len(day.Confirmed),
		TotalMinutes:  view.TotalMinutes,
		Confirmed:     view,
	}, nil
}

func (s *Session) status() (StatusResult, error) {
	dates := make([]string, 0, len(s.days))
	for date := range s.days {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	days := make([]DayStatus, 0, len(dates))
	for _, date := range dates {
		day := s.days[date]
		confirmed, err := s.viewFor(day, day.Confirmed)
		if err != nil {
			return StatusResult{}, err
		}
		staged, err := s.viewFor(day, day.Staged)
		if err != nil {
			return StatusResult{}, err
		}
		days = append(days, DayStatus{
			Date:               day.Date,
			Timezone:           day.Timezone,
			Revision:           day.Revision,
			SavedRevision:      day.SavedRevision,
			Dirty:              !intervalsEqual(day.Staged, day.Confirmed),
			ConfirmedIntervals: len(day.Confirmed),
			StagedIntervals:    len(day.Staged),
			ConfirmedMinutes:   confirmed.TotalMinutes,
			StagedMinutes:      staged.TotalMinutes,
			Sources:            s.daySourceStatuses(day.Date),
		})
	}
	migrationReceipts, remoteBindings, unresolvedWrites, approvals, nativeUnresolved := 0, 0, 0, 0, 0
	st := s.store.State()
	if st.Migration != nil {
		migrationReceipts = len(st.Migration.Receipts)
	}
	if st.Remote != nil {
		remoteBindings = len(st.Remote.Bindings)
		for _, o := range st.Remote.Outcomes {
			if outcomeBlocks(o) {
				unresolvedWrites++
			}
		}
	}
	approvals = len(st.Approvals)
	for i := range st.Intents {
		if st.Intents[i].unresolved() {
			nativeUnresolved++
		}
	}
	return StatusResult{
		AppVersion:          AppVersion,
		StoreSchema:         st.Schema,
		DurableSchema:       s.store.DurableSchema(),
		DurabilityUncertain: s.store.Uncertain(),
		MigrationReceipts:   migrationReceipts,
		RemoteBindings:      remoteBindings,
		UnresolvedWrites:    unresolvedWrites,
		Plans:               len(st.Plans),
		Approvals:           approvals,
		NativeUnresolved:    nativeUnresolved,
		Days:                days,
	}, nil
}

// daySourceStatuses reports each enabled source's latest collection attempt
// for the date (issue #162): the persisted SourceAttempt's status and
// collection_id, or "unattempted" when no attempt is recorded. Disabled
// sources are omitted — they can never collect and never need acknowledgment.
func (s *Session) daySourceStatuses(date string) []DaySourceStatus {
	githubOn, calendarOn := s.cfg.EnabledSources()
	if !githubOn && !calendarOn {
		return nil
	}
	ev := s.store.Evidence(date)
	sources := []DaySourceStatus{}
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
		if a := latestAttempt(ev, src.name); a != nil {
			sources = append(sources, DaySourceStatus{
				Source:       src.name,
				Status:       a.Status,
				CollectionID: a.CollectionID,
			})
		} else {
			sources = append(sources, DaySourceStatus{Source: src.name, Status: "unattempted"})
		}
	}
	return sources
}

func (s *Session) checkEnvironment() EnvCheckResult {
	missing, present := MissingRequiredEnv(s.cfg, s.env)
	if missing == nil {
		missing = []string{}
	}
	if present == nil {
		present = []string{}
	}
	return EnvCheckResult{
		Required: append([]string(nil), s.cfg.RequiredEnv...),
		Missing:  missing,
		Present:  present,
	}
}

func (s *Session) summary(day *Day) (DaySummary, error) {
	confirmed, err := s.viewFor(day, day.Confirmed)
	if err != nil {
		return DaySummary{}, err
	}
	staged, err := s.viewFor(day, day.Staged)
	if err != nil {
		return DaySummary{}, err
	}
	return DaySummary{
		Date:          day.Date,
		Timezone:      day.Timezone,
		Revision:      day.Revision,
		SavedRevision: day.SavedRevision,
		Dirty:         !intervalsEqual(day.Staged, day.Confirmed),
		Confirmed:     confirmed,
		Staged:        staged,
	}, nil
}

// originDay is the authoritative interval set for one origin date. Rendering
// and validation use the durable store for every date except the viewed day,
// whose unsaved working set is supplied by the caller.
type originDay struct {
	Date      string
	Timezone  string
	Intervals []Interval
}

// origins returns the working session view: durable saved state plus the
// latest confirmed state of every open day, with the working day's chosen
// interval set overriding that date. It is used for staging, confirmation, and
// review so the user can plan dependent edits before either day is saved.
func (s *Session) origins(day *Day, source []Interval) map[string]originDay {
	origins := s.durableOrigins(day, source)
	for date, loaded := range s.days {
		origins[date] = originDay{Date: date, Timezone: loaded.Timezone, Intervals: loaded.Confirmed}
	}
	origins[day.Date] = originDay{Date: day.Date, Timezone: day.Timezone, Intervals: source}
	return origins
}

// durableOrigins is the candidate durable store: persisted state for every
// date except the working day, whose confirmed set replaces that date. Unsaved
// confirmations on other open days are deliberately absent. Save validation and
// receipts use this view because it is what a restart will load after this one
// day is committed.
func (s *Session) durableOrigins(day *Day, source []Interval) map[string]originDay {
	origins := make(map[string]originDay, len(s.store.State().Days)+1)
	for date, stored := range s.store.State().Days {
		origins[date] = originDay{Date: date, Timezone: stored.Timezone, Intervals: stored.Confirmed}
	}
	origins[day.Date] = originDay{Date: day.Date, Timezone: day.Timezone, Intervals: source}
	return origins
}

func (s *Session) viewFor(day *Day, source []Interval) (View, error) {
	return s.viewFromOrigins(day.Date, s.origins(day, source))
}

func (s *Session) durableView(day *Day, source []Interval) (View, error) {
	return s.viewFromOrigins(day.Date, s.durableOrigins(day, source))
}

// renderSegments renders every origin day's intervals onto date and returns
// them in the view's order. It is the one rendering path: viewFromOrigins wraps
// it for review, and record's lock-free recompute calls it directly so a
// previous day's spill appears in the recomputed partition exactly as review
// rendered it.
func renderSegments(date string, origins map[string]originDay) ([]Segment, error) {
	segments := make([]Segment, 0, len(origins))
	for _, origin := range origins {
		loc, err := dayLocation(origin.Timezone)
		if err != nil {
			return nil, err
		}
		for _, iv := range origin.Intervals {
			segment, ok, err := segmentOn(iv, origin.Date, date, loc)
			if err != nil {
				return nil, err
			}
			if ok {
				segments = append(segments, segment)
			}
		}
	}
	sortSegments(segments)
	return segments, nil
}

func (s *Session) viewFromOrigins(date string, origins map[string]originDay) (View, error) {
	segments, err := renderSegments(date, origins)
	if err != nil {
		return View{}, err
	}
	total := 0
	for _, segment := range segments {
		total += segment.Minutes
	}
	return View{Segments: segments, TotalMinutes: total}, nil
}

type resolvedSpan struct {
	ID    string
	Date  string
	Start time.Time
	End   time.Time
}

func (s *Session) validateStaged(day *Day) error {
	return s.validatePieces(s.origins(day, day.Staged))
}

// validateConfirmed checks the actual candidate durable state for saving this
// one day. Unsaved confirmations on other open days are not included.
func (s *Session) validateConfirmed(day *Day) error {
	return s.validatePieces(s.durableOrigins(day, day.Confirmed))
}

func (s *Session) validatePieces(origins map[string]originDay) error {
	spans := make([]resolvedSpan, 0, len(origins))
	for _, origin := range origins {
		loc, err := dayLocation(origin.Timezone)
		if err != nil {
			return err
		}
		for _, iv := range origin.Intervals {
			start, end, err := s.validateInterval(iv, origin.Date, loc)
			if err != nil {
				return err
			}
			spans = append(spans, resolvedSpan{
				ID:    iv.ID,
				Date:  origin.Date,
				Start: start,
				End:   end,
			})
		}
	}
	sort.SliceStable(spans, func(i, j int) bool {
		if !spans[i].Start.Equal(spans[j].Start) {
			return spans[i].Start.Before(spans[j].Start)
		}
		return spans[i].ID < spans[j].ID
	})
	if len(spans) < 2 {
		return nil
	}
	maxEnd := spans[0].End
	maxID := spans[0].ID
	for _, span := range spans[1:] {
		if span.Start.Before(maxEnd) {
			return overlapError(span.Date, maxID, span.ID)
		}
		if span.End.After(maxEnd) {
			maxEnd = span.End
			maxID = span.ID
		}
	}
	return nil
}

func (s *Session) validateInterval(iv Interval, origin string, loc *time.Location) (time.Time, time.Time, error) {
	if _, err := parseDate(iv.Date); err != nil {
		return time.Time{}, time.Time{}, err
	}
	if iv.Date != origin {
		return time.Time{}, time.Time{}, newError(CodeStateInvalid, "interval date does not match its day record")
	}
	start, end, err := iv.resolveEndpoints(loc)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if iv.Classification != "" && !identifierRE.MatchString(iv.Classification) {
		return time.Time{}, time.Time{}, newError(CodeInvalidInterval, "classification is not a valid classification identifier")
	}
	if strings.TrimSpace(iv.Description) == "" {
		return time.Time{}, time.Time{}, newError(CodeInvalidInterval, "description must not be empty")
	}
	if len([]rune(iv.Description)) > maxDescriptionRunes {
		return time.Time{}, time.Time{}, newError(CodeInvalidInterval, "description is too long")
	}
	if len(iv.EvidenceRefs) > maxEvidenceRefs {
		return time.Time{}, time.Time{}, newError(CodeInvalidInterval, "too many evidence references")
	}
	for _, ref := range iv.EvidenceRefs {
		if ref == "" || len([]rune(ref)) > maxEvidenceRefRunes || controlRune.MatchString(ref) {
			return time.Time{}, time.Time{}, newError(CodeInvalidInterval, "evidence reference is empty or invalid")
		}
	}
	return start, end, nil
}

func indexOf(intervals []Interval, id string) int {
	for i := range intervals {
		if intervals[i].ID == id {
			return i
		}
	}
	return -1
}

func (d *Day) nextIntervalID() string {
	// Prefix with the origin day so IDs stay unambiguous across cross-midnight
	// segments and multi-day overlap diagnostics.
	prefix := "iv-" + strings.ReplaceAll(d.Date, "-", "") + "-"
	for {
		id := fmt.Sprintf("%s%d", prefix, d.NextInterval)
		d.NextInterval++
		if indexOf(d.Staged, id) < 0 && indexOf(d.Confirmed, id) < 0 {
			return id
		}
	}
}

// --- R2 evidence collection operations ----------------------------------------

// CollectResult is the compact collect receipt: per-source status, detail, and
// counts. Structured topics and proposed meetings stay in the persisted
// evidence record and the day's review surface, not in stream summaries.
type CollectResult struct {
	Date         string                `json:"date"`
	CollectionID string                `json:"collection_id,omitempty"`
	AttemptedAt  string                `json:"attempted_at,omitempty"`
	NoSources    bool                  `json:"no_sources"`
	Sources      []SourceResultSummary `json:"sources"`
}

// SourceResultSummary is one source's visible result state.
type SourceResultSummary struct {
	Source       string `json:"source"`
	Status       string `json:"status"`
	Detail       string `json:"detail,omitempty"`
	Topics       int    `json:"topics,omitempty"`
	Meetings     int    `json:"meetings,omitempty"`
	Acknowledged bool   `json:"acknowledged,omitempty"`
}

// collectDay performs one strict collection run for an opened day. The day
// must already be open; the window is that day's calendrical day in its
// recorded timezone (including skipped-midnight boundaries). Collection never
// touches intervals, revisions, or history.
func (s *Session) collectDay(req Request) (CollectResult, error) {
	date, err := requiredDay(req)
	if err != nil {
		return CollectResult{}, err
	}
	day, err := s.loadedDay(date)
	if err != nil {
		return CollectResult{}, err
	}
	_, err = parseDate(day.Date)
	if err != nil {
		return CollectResult{}, err
	}
	loc, err := dayLocation(day.Timezone)
	if err != nil {
		return CollectResult{}, err
	}
	windowStart, err := localDayStart(day.Date, loc)
	if err != nil {
		return CollectResult{}, err
	}
	windowEnd, err := localDayStart(nextDate(day.Date), loc)
	if err != nil {
		return CollectResult{}, err
	}
	if !windowEnd.After(windowStart) {
		return CollectResult{}, newError(CodeInternal, "the day window is empty")
	}

	githubOn, calendarOn := s.cfg.EnabledSources()
	if !githubOn && !calendarOn {
		return CollectResult{
			Date: day.Date,
			Sources: []SourceResultSummary{
				{Source: SourceGithub, Status: StatusDisabled, Detail: "github collection is not enabled in configuration"},
				{Source: SourceCalendar, Status: StatusDisabled, Detail: "calendar collection is not enabled in configuration"},
			},
			NoSources: true,
		}, nil
	}

	collectionID := "col-" + strconv.FormatInt(time.Now().UTC().UnixNano(), 36)
	attemptedAt := time.Now().UTC().Format(time.RFC3339)
	evidence := &DayEvidence{}
	summaries := []SourceResultSummary{}

	if githubOn {
		scope := &githubScope{
			user:  strings.TrimSpace(s.cfg.Sources.Github.User),
			repos: s.cfg.Sources.Github.Repositories,
			token: credentialValue(s.env, "GITHUB_TOKEN"),
		}
		if base, err := normalizeGithubBase(s.cfg.Sources.Github.APIBase); err == nil {
			scope.apiBase = base
		}
		got := collectGithub(scope, windowStart, windowEnd)
		attempt := SourceAttempt{
			Source:       SourceGithub,
			CollectionID: collectionID,
			AttemptedAt:  attemptedAt,
			Status:       got.Status,
			Detail:       truncateRunes(got.Detail, maxDetailRunes),
			Families:     append([]FamilyReport(nil), got.Families...),
		}
		evidence.Attempts = append(evidence.Attempts, attempt)
		evidence.Topics = got.Topics
		if len(evidence.Topics) > maxEvidenceTopics {
			evidence.Topics = evidence.Topics[:maxEvidenceTopics]
			evidence.Attempts[len(evidence.Attempts)-1].Status = StatusIncomplete
			evidence.Attempts[len(evidence.Attempts)-1].Detail = niceJoin(evidence.Attempts[len(evidence.Attempts)-1].Detail, fmt.Sprintf("more topics than the %d-topic evidence cap", maxEvidenceTopics))
		}
		summaries = append(summaries, SourceResultSummary{
			Source: SourceGithub,
			Status: attempt.Status,
			Detail: attempt.Detail,
			Topics: len(evidence.Topics),
		})
	}

	if calendarOn {
		got := collectCalendar(calendarUser(s.cfg), s.env, windowStart, windowEnd, loc)
		attempt := SourceAttempt{
			Source:       SourceCalendar,
			CollectionID: collectionID,
			AttemptedAt:  attemptedAt,
			Status:       got.Status,
			Detail:       truncateRunes(got.Detail, maxDetailRunes),
		}
		gotMeetings := got.Meetings
		if perv := s.store.Evidence(day.Date); perv != nil {
			gotMeetings = carryMeetingDecisions(gotMeetings, perv.Meetings)
		}
		evidence.Meetings = gotMeetings
		evidence.Attempts = append(evidence.Attempts, attempt)
		summaries = append(summaries, SourceResultSummary{
			Source:   SourceCalendar,
			Status:   attempt.Status,
			Detail:   attempt.Detail,
			Meetings: len(evidence.Meetings),
		})
	}

	if len(evidence.Attempts) == 0 {
		return CollectResult{}, newError(CodeInternal, "no enabled source attempted collection")
	}
	// A GitHub attempt with families beyond ceilings keeps its status; caps
	// were applied above. Everything lands through one atomic evidence write.
	if err := s.store.CommitEvidence(day.Date, evidence); err != nil {
		return CollectResult{}, err
	}
	for i := range summaries {
		if s.store.Evidence(day.Date) != nil {
			summaries[i].Acknowledged = attemptAcknowledged(s.store.Evidence(day.Date), summaries[i].Source)
		}
	}
	return CollectResult{
		Date:         day.Date,
		CollectionID: collectionID,
		AttemptedAt:  attemptedAt,
		Sources:      summaries,
	}, nil
}

// attemptAcknowledged reports whether the source's latest recorded attempt is
// acknowledged.
func attemptAcknowledged(ev *DayEvidence, source string) bool {
	if ev == nil {
		return false
	}
	for _, a := range ev.Attempts {
		if a.Source == source {
			return a.Acknowledged
		}
	}
	return false
}

// carryMeetingDecisions restores explicit attendance decisions for meetings
// whose identity (UID) and start survive recollection. A rescheduled or
// replaced instance no longer inherits the old decision.
func carryMeetingDecisions(fresh, prior []ProposedMeeting) []ProposedMeeting {
	if len(prior) == 0 {
		return fresh
	}
	byIdentity := map[string]string{}
	for _, m := range prior {
		byIdentity[m.UID+"|"+m.Start] = m.Decision
	}
	for i := range fresh {
		if d, ok := byIdentity[fresh[i].UID+"|"+fresh[i].Start]; ok && fresh[i].Decision == "" {
			fresh[i].Decision = d
		}
	}
	return fresh
}

func calendarUser(cfg *Config) string {
	if cfg.Sources == nil || cfg.Sources.Calendar == nil {
		return ""
	}
	return strings.TrimSpace(cfg.Sources.Calendar.User)
}

// credentialValue resolves one credential name with R1 precedence (inherited
// environment overrides the file, including empty overrides). The value is
// returned only to the binary's own HTTP layer, never to the protocol.
func credentialValue(env map[string]string, name string) string {
	v, ok := resolveEnv(env, name)
	if !ok {
		return ""
	}
	return v
}

// AcknowledgedSource is the acknowledge_source receipt. An acknowledgment is
// always bound to the reviewed collection identity the caller supplies; a
// later collection attempt requires a fresh acknowledgment of that new
// attempt, and an action built from an older review is rejected.
type AcknowledgedSource struct {
	Date         string `json:"date"`
	Source       string `json:"source"`
	CollectionID string `json:"collection_id"`
	Acknowledged bool   `json:"acknowledged"`
	At           string `json:"at,omitempty"`
}

// acknowledgeSource marks this day's recorded attempt for the named enabled
// source as reviewed by the trusted local user. The request must carry the
// exact collection id of the reviewed attempt: a missing id and a stale id
// are both rejected, because collection may have run since the caller's
// review and silently acknowledging the latest attempt would acknowledge
// coverage the user never saw. The decision is copy-on-write: a failed write
// may not change review state, unblock a gate, or move durability. It is
// never settable through a model tool and never happens automatically.
func (s *Session) acknowledgeSource(req Request) (AcknowledgedSource, error) {
	date, err := requiredDay(req)
	if err != nil {
		return AcknowledgedSource{}, err
	}
	if _, err := s.loadedDay(date); err != nil {
		return AcknowledgedSource{}, err
	}
	source := strings.TrimSpace(req.Source)
	if source != SourceGithub && source != SourceCalendar {
		return AcknowledgedSource{}, newError(CodeBadRequest, "source must be github or calendar")
	}
	reviewed := strings.TrimSpace(req.CollectionID)
	if reviewed == "" {
		return AcknowledgedSource{}, newError(CodeBadRequest,
			"collection_id is required: acknowledgment must verify the exact reviewed collection attempt")
	}
	ev := s.store.Evidence(date)
	if ev == nil {
		return AcknowledgedSource{}, newError(CodeBadRequest, "source has no collection attempt to review; run collect first")
	}
	target := -1
	for i, a := range ev.Attempts {
		if a.Source == source {
			target = i
		}
	}
	if target < 0 {
		return AcknowledgedSource{}, newError(CodeBadRequest, "source has no collection attempt to review; run collect first")
	}
	if ev.Attempts[target].CollectionID != reviewed {
		return AcknowledgedSource{}, newError(CodeStaleCollection,
			"collection_id does not match this day's recorded attempt for the source; collection has changed since the review, review the current attempt before acknowledging")
	}
	if !statusesAckable(ev.Attempts[target].Status) {
		return AcknowledgedSource{}, newError(CodeBadRequest, fmt.Sprintf("the latest %s attempt does not need acknowledgment (%s)", source, ev.Attempts[target].Status))
	}
	// Copy-on-write: mutate a clone and hand it to the store. The store-owned
	// record the caller read from is untouched until the write succeeds, so a
	// pre-activation write failure leaves review state, disk, and gate state
	// exactly as they were (F2).
	candidate := ev.cloneEvidence()
	candidate.Attempts[target].Acknowledged = true
	candidate.Attempts[target].AcknowledgedAt = time.Now().UTC().Format(time.RFC3339)
	if err := s.store.CommitEvidence(date, candidate); err != nil {
		return AcknowledgedSource{}, err
	}
	return AcknowledgedSource{
		Date:         date,
		Source:       source,
		CollectionID: candidate.Attempts[target].CollectionID,
		Acknowledged: true,
		At:           candidate.Attempts[target].AcknowledgedAt,
	}, nil
}

func statusesAckable(status string) bool {
	switch status {
	case StatusIncomplete, StatusFailed, StatusMissingCredentials:
		return true
	}
	return false
}

// MeetingDecision is the decide_meeting receipt: the explicit local attendance
// decision for one proposed meeting. It is a review decision only — it never
// creates working minutes or confirms work.
type MeetingDecision struct {
	Date         string `json:"date"`
	MeetingID    string `json:"meeting_id"`
	Decision     string `json:"decision"`
	MeetingUID   string `json:"meeting_uid"`
	CollectionID string `json:"collection_id,omitempty"`
}

// decideMeeting records the user's explicit attendance decision for one
// proposed meeting. Every decision must carry the exact reviewed calendar
// collection id; it is verified before any mutation, and a missing or stale id
// is rejected rather than silently landing on whatever the latest attempt
// proposes (F3). The meeting ref additionally fingerprints the current target
// (UID, original occurrence identity and rescheduled start), so an action built
// from an earlier review cannot address a rescheduled instance even when its
// original RECURRENCE-ID is unchanged. Decisions survive restart and are
// carried across recollection only when the meeting identity and start are
// unchanged; that deliberate persistence is separate from new-action
// authorization. The decision is copy-on-write: a failed write leaves review
// state, disk, and gate state unchanged (F2).
func (s *Session) decideMeeting(req Request) (MeetingDecision, error) {
	date, err := requiredDay(req)
	if err != nil {
		return MeetingDecision{}, err
	}
	if _, err := s.loadedDay(date); err != nil {
		return MeetingDecision{}, err
	}
	if strings.TrimSpace(req.MeetingID) == "" {
		return MeetingDecision{}, newError(CodeBadRequest, "meeting_id is required")
	}
	decision := strings.TrimSpace(req.Decision)
	if !validDecision(decision) {
		return MeetingDecision{}, newError(CodeBadRequest, "decision must be pending, attended, or skipped")
	}
	reviewed := strings.TrimSpace(req.CollectionID)
	if reviewed == "" {
		return MeetingDecision{}, newError(CodeBadRequest,
			"collection_id is required: an attendance decision must verify the exact reviewed calendar collection attempt")
	}
	ev := s.store.Evidence(date)
	if ev == nil {
		return MeetingDecision{}, newError(CodeUnknownMeeting, "no collected evidence exists for this day; run collect first")
	}
	attempt := latestAttempt(ev, SourceCalendar)
	if attempt == nil {
		return MeetingDecision{}, newError(CodeBadRequest,
			"collection_id cannot be verified: no calendar collection attempt is recorded for this day; run collect first")
	}
	if attempt.CollectionID != reviewed {
		return MeetingDecision{}, newError(CodeStaleCollection,
			"collection_id does not match this day's recorded calendar attempt; collection has changed since the review, review the current attempt before deciding")
	}
	idx := -1
	for i, m := range ev.Meetings {
		if m.Ref == strings.TrimSpace(req.MeetingID) {
			idx = i
		}
	}
	if idx < 0 {
		return MeetingDecision{}, newError(CodeUnknownMeeting,
			"meeting_id does not name a proposed meeting in this day's evidence; if collection ran since the review, review the current attempt")
	}
	// Copy-on-write (F2): the store's record is never mutated in place by a
	// caller; the candidate clone fails or commits as one record.
	candidate := ev.cloneEvidence()
	candidate.Meetings[idx].Decision = decision
	if err := s.store.CommitEvidence(date, candidate); err != nil {
		return MeetingDecision{}, err
	}
	at := latestAttempt(candidate, SourceCalendar)
	return MeetingDecision{
		Date:         date,
		MeetingID:    candidate.Meetings[idx].Ref,
		Decision:     decision,
		MeetingUID:   candidate.Meetings[idx].UID,
		CollectionID: attemptCollectionID(at),
	}, nil
}

// attemptCollectionID exposes the attempt's collection id for receipts being
// built from a result the caller already owns (nil-safe, receipt-only).
func attemptCollectionID(a *SourceAttempt) string {
	if a == nil {
		return ""
	}
	return a.CollectionID
}

// --- composed source view and the incomplete-source review gate -----------------

// composedEvidenceReview merges stored attempts with configuration-disabled
// sources into one review surface.
func composedEvidenceReview(cfg *Config, ev *DayEvidence) *EvidenceReview {
	if ev == nil {
		ev = &DayEvidence{}
	}
	githubOn, calendarOn := cfg.EnabledSources()
	sources := []ComposedSourceStatus{}
	if githubOn {
		if a := latestAttempt(ev, SourceGithub); a != nil {
			sources = append(sources, composedStatus(*a, true))
		} else {
			sources = append(sources, ComposedSourceStatus{
				Source: SourceGithub, Enabled: true,
				Status: "unattempted", Detail: "no collection attempt is recorded for this date",
			})
		}
	} else {
		sources = append(sources, ComposedSourceStatus{
			Source: SourceGithub, Enabled: false,
			Status: StatusDisabled, Detail: "github collection is not enabled in configuration",
		})
	}
	if calendarOn {
		if a := latestAttempt(ev, SourceCalendar); a != nil {
			sources = append(sources, composedStatus(*a, true))
		} else {
			sources = append(sources, ComposedSourceStatus{
				Source: SourceCalendar, Enabled: true,
				Status: "unattempted", Detail: "no collection attempt is recorded for this date",
			})
		}
	} else {
		sources = append(sources, ComposedSourceStatus{
			Source: SourceCalendar, Enabled: false,
			Status: StatusDisabled, Detail: "calendar collection is not enabled in configuration",
		})
	}
	return &EvidenceReview{
		Sources:  sources,
		Topics:   ev.Topics,
		Meetings: ev.Meetings,
	}
}

func latestAttempt(ev *DayEvidence, source string) *SourceAttempt {
	if ev == nil {
		// An enabled source with no collected evidence for this date is a
		// normal, structured gate state — never a crash: callers treat a nil
		// result as the unattempted-source branch of the source gate (F1).
		return nil
	}
	var found *SourceAttempt
	for i := range ev.Attempts {
		if ev.Attempts[i].Source == source {
			found = &ev.Attempts[i]
		}
	}
	return found
}

func composedStatus(a SourceAttempt, enabled bool) ComposedSourceStatus {
	return ComposedSourceStatus{
		Source:       a.Source,
		Enabled:      enabled,
		Status:       a.Status,
		Detail:       a.Detail,
		CollectionID: a.CollectionID,
		AttemptedAt:  a.AttemptedAt,
		Acknowledged: a.Acknowledged,
	}
}

// checkSourceGate enforces the incomplete-source review gate: when any
// collector is enabled for the recorded configuration and its latest attempt
// for the date is unresolved and unacknowledged, confirm and save stay
// blocked. Manual-only R1 operation with no enabled sources is unchanged,
// and an explicit acknowledgment of the named attempt unblocks the flow.
func (s *Session) checkSourceGate(date string) error {
	githubOn, calendarOn := s.cfg.EnabledSources()
	if !githubOn && !calendarOn {
		return nil
	}
	ev := s.store.Evidence(date)
	var unresolved []string
	var unattempted []string
	for source, on := range map[string]bool{SourceGithub: githubOn, SourceCalendar: calendarOn} {
		if !on {
			continue
		}
		a := latestAttempt(ev, source)
		if a == nil {
			unattempted = append(unattempted, source)
			continue
		}
		if a.Status == StatusComplete {
			continue
		}
		if a.Acknowledged {
			continue
		}
		unresolved = append(unresolved, source)
	}
	if len(unattempted) > 0 {
		return newError(CodeSourceGate,
			"collection has not run for the enabled source(s) on this date ("+strings.Join(unattempted, ", ")+"): run collect for the day, or disable the source in configuration")
	}
	if len(unresolved) > 0 {
		return newError(CodeSourceGate,
			"the source attempt(s) ("+strings.Join(unresolved, ", ")+") must be reviewed and explicitly acknowledged before confirmation; acknowledgments never apply automatically and a new attempt requires a fresh acknowledgment")
	}
	return nil
}

// --- R3 explicit migration and read-only remote operations -------------------

// migrationPreview validates a complete explicit date selection without
// activating anything. It returns bounded counts and conflicts; a conflicted or
// already-imported candidate is never applied. It stores nothing: the
// migration_id it returns is a content binding, not a grant.
func (s *Session) migrationPreview(req Request) (MigrationPreview, error) {
	dates, err := normalizeMigrationDates(req.Dates)
	if err != nil {
		return MigrationPreview{}, err
	}
	cand, err := buildMigrationCandidate(s.cfg, s.store.State(), dates)
	if err != nil {
		return MigrationPreview{}, err
	}
	return MigrationPreview{
		MigrationID:        cand.MigrationID,
		Dates:              cand.Dates,
		Timezone:           cand.Timezone,
		Ready:              cand.Ready,
		NoChange:           cand.NoChange,
		ExistingReceipt:    cand.ExistingReceipt,
		SourceIdentity:     cand.SourceIdentity,
		Counts:             cand.Counts,
		Conflicts:          cand.Conflicts,
		ConflictsTruncated: cand.ConflictsDropped,
		SourceHashes:       cand.SourceHashes,
	}, nil
}

// migrationApply rebuilds the candidate for the same explicit date selection
// from the store and the legacy source, and activates it only when the rebuilt
// migration_id equals the one the caller reviewed (issue #272). The id hashes
// the selected dates, the legacy timezone, every source file's bytes and the
// durable target fingerprint, so it is a content binding like submit --chat's
// plan and review ids (ADR 0009), not a stored grant: a preview grants no
// standing authority, and any change to the selection, the source or the
// target since the preview is migration_stale with nothing written. It
// requires no opened day, so it can never discard a draft, and it never
// writes into the legacy root.
func (s *Session) migrationApply(req Request) (MigrationApplyReceipt, error) {
	id := strings.TrimSpace(req.MigrationID)
	if !validMigrationID(id) {
		return MigrationApplyReceipt{}, newError(CodeBadRequest, "migration_id is required")
	}
	dates, err := normalizeMigrationDates(req.Dates)
	if err != nil {
		return MigrationApplyReceipt{}, err
	}
	if len(s.days) > 0 {
		return MigrationApplyReceipt{}, newError(CodeSessionOpen,
			"no day may be open during migration activation; run migration before open_day in a fresh process")
	}
	// Revalidation always precedes a successful no-change return: the fresh
	// candidate must reproduce the reviewed id exactly and clear the conflict
	// check before a no-change receipt may restate the previously imported
	// result.
	fresh, err := buildMigrationCandidate(s.cfg, s.store.State(), dates)
	if err != nil {
		return MigrationApplyReceipt{}, err
	}
	if fresh.MigrationID != id {
		return MigrationApplyReceipt{}, newError(CodeMigrationStale,
			"migration_id does not match a fresh preview of this date selection: the selection, the legacy source or the durable target changed since the preview; preview again (nothing was activated)")
	}
	if len(fresh.Conflicts) > 0 {
		return MigrationApplyReceipt{}, newError(CodeMigrationConflict,
			"the candidate has conflicts and was not activated: "+fresh.Conflicts[0].Code)
	}
	if fresh.NoChange {
		return MigrationApplyReceipt{
			MigrationID: fresh.MigrationID, Status: MigrationNoChange, ReceiptID: fresh.ExistingReceipt,
			Dates: fresh.Dates, Counts: fresh.Counts, Unresolved: fresh.Counts.Unresolved,
		}, nil
	}
	next := mergeMigrationCandidate(s.store.State(), fresh)
	if err := s.store.CommitState(next); err != nil {
		return MigrationApplyReceipt{}, err
	}
	return MigrationApplyReceipt{
		MigrationID: id, Status: MigrationApplied, ReceiptID: id,
		Dates: fresh.Dates, Counts: fresh.Counts, Unresolved: fresh.Counts.Unresolved,
	}, nil
}

// classifications performs one bounded authoritative classification read and
// persists the bounded catalog so a later reviewed selection can reference its
// identity.
func (s *Session) classifications(req Request) (ClassificationsReceipt, error) {
	reader, err := NewGradionReader(s.cfg, s.env)
	if err != nil {
		return ClassificationsReceipt{}, err
	}
	defer func() { _ = reader.Close() }()
	ctx, cancel := contextWithTimeout()
	defer cancel()
	cat, err := reader.Classifications(ctx, req.Query)
	if err != nil {
		return ClassificationsReceipt{}, err
	}
	next := s.store.State().cloneState()
	if next.Remote == nil {
		next.Remote = &RemoteState{}
	}
	if next.Remote.Catalogs == nil {
		next.Remote.Catalogs = map[string]*CatalogRecord{}
	}
	next.Remote.Catalogs[cat.CatalogID] = cat
	pruneCatalogs(next.Remote.Catalogs, configuredDefaultCatalogID(s.cfg))
	if err := s.store.CommitState(next); err != nil {
		return ClassificationsReceipt{}, err
	}
	receipt := ClassificationsReceipt{
		CatalogID: cat.CatalogID, Query: cat.Query, Complete: cat.Complete,
		Options: cat.Options, ObservedAt: cat.ObservedAt,
	}
	if !cat.Complete {
		receipt.Note = "this catalog is filtered or truncated; it cannot prove an option absent, so selection may need a narrower query"
	}
	return receipt, nil
}

func pruneCatalogs(catalogs map[string]*CatalogRecord, pinnedID string) {
	if len(catalogs) <= maxStoredCatalogs {
		return
	}
	type aged struct {
		id string
		at string
	}
	all := make([]aged, 0, len(catalogs))
	for id, c := range catalogs {
		if id == pinnedID {
			continue
		}
		all = append(all, aged{id: id, at: c.ObservedAt})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at < all[j].at })
	remove := len(catalogs) - maxStoredCatalogs
	for i := 0; i < remove; i++ {
		delete(catalogs, all[i].id)
	}
}

// ClassificationSelection is the staged result of one explicit reviewed remote
// classification choice.
type ClassificationSelection struct {
	Date             string `json:"date"`
	IntervalID       string `json:"interval_id"`
	CatalogID        string `json:"catalog_id"`
	ClassificationID string `json:"classification_id"`
	Classification   string `json:"classification,omitempty"`
	Code             string `json:"code,omitempty"`
	CatalogComplete  bool   `json:"catalog_complete"`
	Revision         int    `json:"revision"`
	SavedRevision    int    `json:"saved_revision"`
	Dirty            bool   `json:"dirty"`
	Staged           View   `json:"staged"`
}

// selectClassification records a reviewed choice for an option a bounded catalog
// actually returned. It follows the existing staged mutation semantics: nothing
// becomes durable until confirm and save. Defaults and migrated hints are never
// selected silently.
func (s *Session) selectClassification(req Request) (ClassificationSelection, error) {
	date, err := requiredDay(req)
	if err != nil {
		return ClassificationSelection{}, err
	}
	revision, err := requiredRevision(req)
	if err != nil {
		return ClassificationSelection{}, err
	}
	if strings.TrimSpace(req.IntervalID) == "" {
		return ClassificationSelection{}, newError(CodeBadRequest, "interval_id is required")
	}
	catalogID := strings.TrimSpace(req.CatalogID)
	if catalogID == "" {
		return ClassificationSelection{}, newError(CodeBadRequest, "catalog_id is required: a selection must name the catalog it reviewed")
	}
	classificationID := strings.TrimSpace(req.ClassificationID)
	if classificationID == "" {
		return ClassificationSelection{}, newError(CodeBadRequest, "classification_id is required")
	}
	day, err := s.loadedDay(date)
	if err != nil {
		return ClassificationSelection{}, err
	}
	if err := checkRevision(day, revision); err != nil {
		return ClassificationSelection{}, err
	}
	st := s.store.State()
	if st.Remote == nil || st.Remote.Catalogs == nil {
		return ClassificationSelection{}, newError(CodeStaleCatalog, "no catalog is stored; run classifications first")
	}
	cat := st.Remote.Catalogs[catalogID]
	if cat == nil {
		return ClassificationSelection{}, newError(CodeStaleCatalog, "catalog_id is not a stored catalog; fetch the catalog again")
	}
	var chosen *ClassificationOption
	for i := range cat.Options {
		if cat.Options[i].ID == classificationID {
			chosen = &cat.Options[i]
			break
		}
	}
	if chosen == nil {
		return ClassificationSelection{}, newError(CodeUnknownClassify,
			"classification_id was not returned by this catalog; a partial or filtered catalog cannot prove it absent, so refine the query or reselect")
	}
	index := indexOf(day.Staged, req.IntervalID)
	if index < 0 {
		return ClassificationSelection{}, newError(CodeUnknownInterval, "interval is not in the staged day; review again")
	}
	previous := cloneIntervals(day.Staged)
	day.Staged[index].RemoteClassificationID = classificationID
	day.Staged[index].RemoteCatalogID = catalogID
	staged, err := s.viewFor(day, day.Staged)
	if err != nil {
		day.Staged = previous
		return ClassificationSelection{}, err
	}
	return ClassificationSelection{
		Date: day.Date, IntervalID: req.IntervalID, CatalogID: catalogID,
		ClassificationID: classificationID, Classification: chosen.Name, Code: chosen.Code,
		CatalogComplete: cat.Complete, Revision: day.Revision, SavedRevision: day.SavedRevision,
		Dirty: !intervalsEqual(day.Staged, day.Confirmed), Staged: staged,
	}, nil
}

// resolveRemoteRequest validates request shape and dispatches one explicit
// resolution. A day need not be open, but an open day's revision must match.
func (s *Session) resolveRemoteRequest(req Request) (ResolutionReceipt, error) {
	date, err := requiredDay(req)
	if err != nil {
		return ResolutionReceipt{}, err
	}
	revision, err := requiredRevision(req)
	if err != nil {
		return ResolutionReceipt{}, err
	}
	if _, err := parseDate(date); err != nil {
		return ResolutionReceipt{}, err
	}
	var day *Day
	if open, ok := s.days[date]; ok {
		day = open
	}
	return s.resolveRemote(day, date, revision, req.ObservationID, req.RemoteID, req.LocalID, req.Decision)
}

// historyWithoutLegacyLabels copies the confirmed history for review output
// with each snapshot interval's legacy project and local classification label
// cleared (ADR 0012). State keeps them; review never shows them.
func historyWithoutLegacyLabels(history []HistoryEntry) []HistoryEntry {
	out := append([]HistoryEntry(nil), history...)
	for i := range out {
		snapshot := cloneIntervals(out[i].Snapshot)
		for j := range snapshot {
			snapshot[j].Project, snapshot[j].Classification = "", ""
		}
		out[i].Snapshot = snapshot
	}
	return out
}
