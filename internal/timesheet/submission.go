package timesheet

// The writer adds exactly two tool calls over the R3 read boundary. Where the
// reader verifies the two read tools, the writer additionally requires
// timesheet__log_time and timesheet__edit_time. There is deliberately NO
// generic call method: the writer exposes createEntry, editEntry, a read-back
// observeDate and the classifications check, and nothing else. A delete tool,
// REST call or caller-supplied tool name is unreachable from this surface.
//
// Submission lifecycle per operation:
//
//	(plan is read-only) -> durable in_flight intent (commit)
//	-> exactly ONE dispatch (never retried)
//	-> acknowledged stable identity + fresh COMPLETE read-back whose content
//	   matches the intended fields
//	-> confirmed outcome + binding persisted in one protected transition
//
// Uncertainty never retries and stops the batch. Post-rename local durability
// uncertainty blocks dispatch and further persistence claims under the sticky
// store semantics. Only resolve_write can address an unresolved write; writes
// are never replayed and never deleted remotely.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/mcp"
)

const (
	SubmitAllConfirmed          = "all_confirmed"
	SubmitPartialStopped        = "partial_stopped"
	SubmitBlockedBeforeDispatch = "blocked_before_dispatch"
	SubmitDurabilityUncertain   = "durability_uncertain"
	// SubmitPersistenceFailed reports a zero-operation consumption whose local
	// persistence failed BEFORE activation. Nothing was consumed and no
	// execution is durable; the approval stays active for a later explicit
	// submission. It is never reported as all_confirmed or as uncertainty.
	SubmitPersistenceFailed = "persistence_failed"
)

// Stable attempt-level binder/recovery error strings (not top-level protocol
// codes) recorded on unresolved native intents.
const (
	selfBindingConflict = "identity_conflict"
	// editIdentityMismatch marks an edit whose acknowledgment names a
	// DIFFERENT remote identity than the approved bound one (R4F4). A different
	// acknowledged id is never proof about the intended target, even when
	// another entry carries identical content; the attempted and acknowledged
	// identities stay distinct audit and the attempt remains unresolved.
	editIdentityMismatch = "edit_identity_mismatch"
)

// Verified write-boundary field limits (docs/gradion-mcp-verification.md). They
// mirror the wire contract's local policy constants.
const (
	writeMaxDescriptionRunes = 3000
	writeMaxReasonRunes      = 200
)

// The two additional tools this boundary may call, beyond the declared read
// tools. No other tool name is reachable here.
const (
	toolLogTime  = "timesheet__log_time"
	toolEditTime = "timesheet__edit_time"
)

// GradionWriter verifies the write capabilities and dispatches exactly one
// write per attempt.
type GradionWriter struct {
	session    *mcp.Session
	verified   bool
	writeCalls int
	readCalls  int
}

// newGradionWriter resolves the configured credential NAME and builds a session
// over the operator-configured endpoint. It performs no IO by itself.
func newGradionWriter(cfg *Config, env map[string]string) (*GradionWriter, error) {
	if cfg == nil || cfg.Gradion == nil || !cfg.Gradion.Enabled {
		return nil, newError(CodeRemoteDisabled, "verified write access is not enabled in configuration")
	}
	name := cfg.Gradion.TokenVariable()
	token, ok := resolveEnv(env, name)
	if !ok || strings.TrimSpace(token) == "" {
		return nil, missingCredentialError(name)
	}
	session, err := mcp.NewSession(mcp.Config{
		Endpoint:         cfg.Gradion.Endpoint,
		Token:            token,
		Timeout:          gradionTimeout,
		MaxResponseBytes: maxGradionResponseBytes,
		ClientName:       "timesheet",
		ClientVersion:    AppVersion,
	})
	if err != nil {
		return nil, newError(CodeRemoteUnavailable, "write session cannot be created")
	}
	return &GradionWriter{session: session}, nil
}

// Close releases the session; it performs no IO.
func (w *GradionWriter) Close() error {
	if w == nil || w.session == nil {
		return nil
	}
	return w.session.Close()
}

// verifyWriteCapabilities initializes the session and requires the two read
// tools plus the two write tools. This boundary is created only for an explicit
// submit request that has already consumed its plan reconciliation.
func (w *GradionWriter) verifyWriteCapabilities(ctx context.Context) error {
	if w.verified {
		return nil
	}
	if _, err := w.session.Initialize(ctx); err != nil {
		return remoteError(err)
	}
	tools, err := w.session.ListTools(ctx)
	if err != nil {
		return remoteError(err)
	}
	have := make(map[string]bool, len(tools))
	for _, t := range tools {
		have[t.Name] = true
	}
	for _, required := range []string{toolGetMyWeek, toolGetClassifications, toolLogTime, toolEditTime} {
		if !have[required] {
			return newError(CodeRemoteUnavailable, "required write-path tool unavailable: "+required)
		}
	}
	w.verified = true
	return nil
}

// Classifications performs one bounded classification read with the same read
// semantics as the reader boundary, so fresh classification proof is identical
// on both paths.
func (w *GradionWriter) Classifications(ctx context.Context, query string) (*CatalogRecord, error) {
	if err := w.verifyWriteCapabilities(ctx); err != nil {
		return nil, err
	}
	return classificationsCall(ctx, w.session, query)
}

func (w *GradionWriter) observeDate(ctx context.Context, date string) *RemoteObservation {
	if err := w.verifyWriteCapabilities(ctx); err != nil {
		return capabilityFailureObservation(date, err)
	}
	res, err := w.session.CallTool(ctx, toolGetMyWeek, map[string]any{"weekStart": date})
	w.readCalls++
	return dateObservationFromWeekCall(date, res, err)
}

// capabilityFailureObservation builds a failed observation record from a local
// capability failure, with the reader's fail-closed semantics.
func capabilityFailureObservation(date string, err error) *RemoteObservation {
	obs := &RemoteObservation{Date: date, ObservedAt: time.Now().UTC().Format(time.RFC3339)}
	obs.Complete = false
	obs.Failed = true
	obs.Detail = err.Error()
	if app, ok := err.(*AppError); ok {
		obs.Detail = app.Code + ": " + app.Message
	}
	obs.ObservationID = observationID(date, nil, false, obs.Detail)
	return obs
}

// writeAttempt is one classified write attempt result. An acknowledge-with-id
// attempt is "identified"; identity becomes confirmed only through verifyWrite.
// Detail is the sanitized provider error text on uncertain attempts (issue
// #167): the receipt carries it so a refusal names its reason.
type writeAttempt struct {
	Status   string // "identified" | IntentUncertain | IntentUnidentified
	RemoteID string
	Code     string
	Detail   string
}

// validateWriteOp validates an approved operation against the bounded wire
// limits before any dispatch. A local failure means NOTHING was sent.
func validateWriteOp(op PlanOperation, isEdit bool) error {
	if strings.TrimSpace(op.LocalID) == "" || strings.TrimSpace(op.Date) == "" ||
		strings.TrimSpace(op.StartTime) == "" || strings.TrimSpace(op.EndTime) == "" ||
		strings.TrimSpace(op.ClassificationID) == "" || strings.TrimSpace(op.Description) == "" {
		return newError(CodeInternal, "the approved operation is missing required content; nothing was dispatched")
	}
	if len([]rune(op.Description)) > writeMaxDescriptionRunes {
		return newError(CodeInternal, "the approved operation exceeds the bounded description limit; nothing was dispatched")
	}
	if isEdit {
		if strings.TrimSpace(op.RemoteID) == "" || strings.TrimSpace(op.Reason) == "" {
			return newError(CodeInternal, "the approved edit is missing its remote identity or audit reason; nothing was dispatched")
		}
		if len([]rune(op.Reason)) > writeMaxReasonRunes {
			return newError(CodeInternal, "the approved edit exceeds the bounded reason limit; nothing was dispatched")
		}
	}
	return nil
}

// createEntry performs EXACTLY ONE log_time attempt. It never retries.
func (w *GradionWriter) createEntry(ctx context.Context, op PlanOperation) (writeAttempt, error) {
	if err := w.verifyWriteCapabilities(ctx); err != nil {
		return writeAttempt{}, remoteError(err)
	}
	if err := validateWriteOp(op, false); err != nil {
		return writeAttempt{}, err
	}
	body := map[string]any{
		"date":             op.Date,
		"startTime":        op.StartTime,
		"endTime":          op.EndTime,
		"description":      op.Description,
		"classificationId": op.ClassificationID,
	}
	res, err := w.session.CallTool(ctx, toolLogTime, body)
	w.writeCalls++
	return classifyWriteAttempt(err, res), nil
}

// editEntry performs EXACTLY ONE edit_time attempt with the audit reason and
// ONLY approved fields. Remote-only content (task, billable) is preserved by
// omission: these approved bodies never carry those arguments, so absence is
// never read as zero, false or empty.
func (w *GradionWriter) editEntry(ctx context.Context, op PlanOperation) (writeAttempt, error) {
	if err := w.verifyWriteCapabilities(ctx); err != nil {
		return writeAttempt{}, remoteError(err)
	}
	if err := validateWriteOp(op, true); err != nil {
		return writeAttempt{}, err
	}
	body := map[string]any{
		"entryId":          op.RemoteID,
		"reason":           op.Reason,
		"date":             op.Date,
		"startTime":        op.StartTime,
		"endTime":          op.EndTime,
		"description":      op.Description,
		"classificationId": op.ClassificationID,
	}
	res, err := w.session.CallTool(ctx, toolEditTime, body)
	w.writeCalls++
	return classifyWriteAttempt(err, res), nil
}

// writeAckID decodes one acknowledged stable id from a write response,
// mirroring the wire contract's fail-closed semantics: a complete structured
// entry, an id-only entry object, a single entry-kind resource reference or a
// bare object carrying an id acknowledges identity; anything else does not.
func writeAckID(res mcp.CallResult) string {
	if res.IsError {
		return ""
	}
	if res.Structured == nil {
		return ""
	}
	trimmed := strings.TrimSpace(string(res.Structured))
	if trimmed == "" || trimmed == "null" || trimmed[0] != '{' {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
		return ""
	}
	if raw, ok := obj["entry"]; ok && strings.TrimSpace(string(raw)) != "" && string(raw) != "null" {
		var entry struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &entry) == nil && strings.TrimSpace(entry.ID) != "" {
			return strings.TrimSpace(entry.ID)
		}
	}
	if raw, ok := obj["resources"]; ok {
		var refs []struct {
			Kind string `json:"kind"`
			ID   string `json:"id"`
		}
		if json.Unmarshal(raw, &refs) == nil && len(refs) == 1 &&
			strings.EqualFold(strings.TrimSpace(refs[0].Kind), "entry") &&
			strings.TrimSpace(refs[0].ID) != "" {
			return strings.TrimSpace(refs[0].ID)
		}
	}
	if raw, ok := obj["id"]; ok {
		var id string
		if json.Unmarshal(raw, &id) == nil && strings.TrimSpace(id) != "" {
			return strings.TrimSpace(id)
		}
	}
	return ""
}

// sanitizeProviderDetail renders a bounded, sanitized provider error text for
// write receipts: control characters replaced, URLs removed, rune-bounded. It
// strips transport detail — a provider refusal names its reason without echoing
// the request URL that carried the endpoint and query string (issue #167) — and
// nothing more: credential-shaped text outside a URL is NOT redacted here, so a
// caller must not treat the detail as credential-free. The effective bound is
// 240 content runes plus an ellipsis: sanitizerText caps the text at
// maxTitleRunes first, which the maxDetailRunes truncation below can only ever
// re-apply unchanged.
func sanitizeProviderDetail(s string) string {
	snippet := strings.TrimSpace(providerURLRE.ReplaceAllString(sanitizerText(s), ""))
	snippet = strings.TrimSpace(strings.Trim(snippet, "\",:' "))
	return truncateRunes(snippet, maxDetailRunes)
}

func classifyWriteAttempt(err error, res mcp.CallResult) writeAttempt {
	if err != nil {
		code := ""
		detail := ""
		var mcpErr *mcp.Error
		if errors.As(err, &mcpErr) {
			code = mcpErr.StableCode
			detail = sanitizeProviderDetail(err.Error())
		}
		return writeAttempt{Status: IntentUncertain, Code: code, Detail: detail}
	}
	if res.IsError {
		return writeAttempt{Status: IntentUncertain, Code: CodeWriteUncertain, Detail: sanitizeProviderDetail(res.Content)}
	}
	id := writeAckID(res)
	if id == "" {
		return writeAttempt{Status: IntentUnidentified, Code: ErrCodeWriteUnidentified}
	}
	return writeAttempt{Status: "identified", RemoteID: id}
}

type WriteReceiptRow struct {
	WriteRef  string `json:"write_ref"`
	OpIndex   int    `json:"op_index"`
	Action    string `json:"action"`
	Date      string `json:"date"`
	LocalID   string `json:"local_id"`
	Status    string `json:"status"`
	RemoteID  string `json:"remote_id,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
	// ErrorDetail is the sanitized provider refusal text captured at dispatch
	// (empty when none arrived): a write_uncertain receipt names the provider's
	// reason instead of swallowing it (issue #167).
	ErrorDetail string `json:"error_detail,omitempty"`
}

type SubmissionCallCounts struct {
	WriteCalls int `json:"write_calls"`
	ReadCalls  int `json:"read_calls"`
}

type SubmissionReceipt struct {
	Date        string               `json:"date"`
	ApprovalID  string               `json:"approval_id"`
	PlanID      string               `json:"plan_id,omitempty"`
	ExecutionID string               `json:"execution_id,omitempty"`
	Status      string               `json:"status"`
	Operations  int                  `json:"operations"`
	Attempts    int                  `json:"attempts"`
	Confirmed   int                  `json:"confirmed"`
	Unresolved  int                  `json:"unresolved"`
	Unattempted int                  `json:"unattempted"`
	Writes      []WriteReceiptRow    `json:"writes,omitempty"`
	Blockers    []Blocker            `json:"blockers,omitempty"`
	NextAction  string               `json:"next_action"`
	Calls       SubmissionCallCounts `json:"calls"`
}

type UnresolvedRow struct {
	Origin      string `json:"origin"` // native | migrated
	WriteRef    string `json:"write_ref"`
	Status      string `json:"status,omitempty"`
	ErrorCode   string `json:"error_code,omitempty"`
	Action      string `json:"action,omitempty"`
	Date        string `json:"date,omitempty"`
	LocalID     string `json:"local_id,omitempty"`
	RemoteID    string `json:"remote_id,omitempty"`
	Detail      string `json:"detail,omitempty"`
	ExecutionID string `json:"execution_id,omitempty"`
	ApprovalID  string `json:"approval_id,omitempty"`
}

type SubmissionStatusResult struct {
	Date       string            `json:"date"`
	Approvals  []ApprovalRow     `json:"approvals"`
	Executions []ExecutionRecord `json:"executions,omitempty"`
	Unresolved []UnresolvedRow   `json:"unresolved"`
	Blockers   []Blocker         `json:"blockers,omitempty"`
	NextAction string            `json:"next_action"`
}

type WriteResolutionReceipt struct {
	WriteRef      string `json:"write_ref"`
	Origin        string `json:"origin"`
	Decision      string `json:"decision"`
	Date          string `json:"date"`
	LocalID       string `json:"local_id,omitempty"`
	RemoteID      string `json:"remote_id,omitempty"`
	ObservationID string `json:"observation_id"`
	Bound         bool   `json:"bound"`
	Resolved      bool   `json:"resolved"`
	RecordedAt    string `json:"recorded_at"`
}

// migratedWriteRef is a stable opaque write reference for a migrated outcome
// row. The legacy operation key is opaque; rows without one get a stable
// content-derived reference. resolve_write addresses migrated rows by the same
// construction, so references never change between calls.
func migratedWriteRef(o WriteOutcome) string {
	if strings.TrimSpace(o.OperationKey) != "" {
		return "mw-" + o.OperationKey
	}
	payload := struct {
		LocalID string `json:"local_id,omitempty"`
		ID      string `json:"id,omitempty"`
		Date    string `json:"date,omitempty"`
		Start   string `json:"start,omitempty"`
		End     string `json:"end,omitempty"`
		Intent  string `json:"intent,omitempty"`
	}{o.LocalID, o.RemoteID, o.Date, o.StartTime, o.EndTime, o.IntentAt}
	data, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return "mw-" + sha256Hex(data)
}

// submission_status reports bounded approvals, execution history and every
// unresolved native intent or migrated outcome relevant to the date, with the
// safe next action. It performs no remote call and no durable write.
func (s *Session) submissionStatus(req Request) (SubmissionStatusResult, error) {
	date, err := requiredDay(req)
	if err != nil {
		return SubmissionStatusResult{}, err
	}
	if _, err := parseDate(date); err != nil {
		return SubmissionStatusResult{}, err
	}
	st := s.store.State()
	out := SubmissionStatusResult{
		Date: date, Approvals: []ApprovalRow{}, Unresolved: []UnresolvedRow{}, Blockers: []Blocker{},
	}
	for _, a := range st.Approvals {
		if a.Date != date {
			continue
		}
		out.Approvals = append(out.Approvals, ApprovalRow{
			ApprovalID: a.ApprovalID, PlanID: a.PlanID, State: a.State,
			ApprovedAt: a.ApprovedAt, ExecutionID: a.ExecutionID,
			ConsumedAt: a.ConsumedAt, RevokedAt: a.RevokedAt,
		})
	}
	for _, e := range st.Executions {
		if e.Date != date {
			continue
		}
		out.Executions = append(out.Executions, e)
	}
	// Native unresolved intents relevant to this date: their allocation date or
	// any contributing origin date.
	for i := range st.Intents {
		it := &st.Intents[i]
		if !it.unresolved() {
			continue
		}
		if it.Date != date && !intentRelevant(*it, map[string]bool{date: true}) {
			continue
		}
		remote := it.RemoteID
		ackDetail := ""
		if it.AcknowledgedID != "" {
			if remote == "" {
				remote = it.AcknowledgedID
			}
			ackDetail = "; the provider acknowledged an id that was never content-verified"
		}
		out.Unresolved = append(out.Unresolved, UnresolvedRow{
			Origin: "native", WriteRef: it.WriteRef, Status: it.Status, ErrorCode: it.ErrorCode,
			Action: it.Action, Date: it.Date, LocalID: it.LocalID, RemoteID: remote,
			Detail: unresolvedIntentDetail(it) + ackDetail, ExecutionID: it.ExecutionID, ApprovalID: it.ApprovalID,
		})
		out.Blockers = append(out.Blockers, Blocker{
			Code: "unresolved_write", Date: it.Date, LocalID: it.LocalID, RemoteID: it.RemoteID,
			Detail: "write reference " + it.WriteRef + " is unresolved and blocks new submissions",
		})
	}
	// Migrated unresolved outcomes, addressed by the same stable references
	// resolve_write uses. Ordinary confirmed/failed/skipped history is not
	// listed as pending. Relevance is by the row's recorded date AND its actual
	// allocation dates resolved from the local interval (R4F5): a pending
	// spill-day outcome whose recorded date is its origin must surface for the
	// spill date too.
	if st.Remote != nil {
		for _, o := range st.Remote.Outcomes {
			if !outcomeBlocks(o) {
				continue
			}
			if !outcomeRelevant(st, o, map[string]bool{date: true}) {
				continue
			}
			detail := unresolvedOutcomeDetail(o)
			out.Unresolved = append(out.Unresolved, UnresolvedRow{
				Origin: "migrated", WriteRef: migratedWriteRef(o), Status: o.Status,
				ErrorCode: o.ErrorCode, Action: o.Action, Date: o.Date, LocalID: o.LocalID,
				RemoteID: o.RemoteID, Detail: detail,
			})
			out.Blockers = append(out.Blockers, Blocker{
				Code: "unresolved_write", Date: o.Date, LocalID: o.LocalID, RemoteID: o.RemoteID,
				Detail: detail,
			})
		}
	}
	out.NextAction = nextSafeAction(out.Unresolved)
	return out, nil
}

func unresolvedIntentDetail(it *WriteIntent) string {
	switch it.Status {
	case IntentInFlight:
		if it.ErrorCode != "" {
			return "the dispatch attempt never established an outcome (" + it.ErrorCode + ")"
		}
		return "the intent was persisted but its outcome was never established"
	case IntentUnidentified:
		if it.ErrorCode == editIdentityMismatch {
			return "the acknowledged remote id never matched the edit's approved bound identity; the acknowledged entry was never bound or confirmed"
		}
		return "the attempt acknowledged no stable entry identity (" + it.ErrorCode + ")"
	default:
		return "the attempt ended without verified content (" + it.Status + ")"
	}
}

func nextSafeAction(unresolved []UnresolvedRow) string {
	if len(unresolved) > 0 {
		return "resolve_write each unresolved write reference above (adopt against a fresh complete observation, or not_applied with explicit operator attestation); writes are never replayed and an unresolved write blocks every new submission"
	}
	return "preview a fresh plan, approve it and submit; no unresolved native or migrated write exists for this date"
}

// writeContentMatches compares the verified observed entry against an
// operation's INTENDED content: date/spans/classification/description. Task and
// billable are preserved by omission and are not part of what was sent.
func writeContentMatches(op PlanOperation, entry RemoteEntry) bool {
	eq := func(a, b string) bool { return strings.TrimSpace(a) == strings.TrimSpace(b) }
	return eq(entry.Date, op.Date) && eq(entry.StartTime, op.StartTime) &&
		eq(entry.EndTime, op.EndTime) && eq(entry.ClassificationID, op.ClassificationID) &&
		eq(entry.Description, op.Description)
}

// verifyWrite requires a stable acknowledged identity and a fresh COMPLETE
// read-back that still carries that identity with matching intended content.
// An identity-only or malformed acknowledgment with missing, mismatched or
// incomplete content never falsely confirms. The read-back observation record
// is returned even when incomplete, so the durable store keeps fail-closed
// evidence instead of leaving a stale good observation unchallenged.
func (w *GradionWriter) verifyWrite(ctx context.Context, op PlanOperation, attempt writeAttempt) (ackID string, entry *RemoteEntry, obs *RemoteObservation, errCode string) {
	if attempt.Status != "identified" {
		return "", nil, nil, ""
	}
	ackID = strings.TrimSpace(attempt.RemoteID)
	if ackID == "" {
		return "", nil, nil, ErrCodeWriteContentUnverified
	}
	obs = w.observeDate(ctx, op.Date)
	if obs == nil {
		return ackID, nil, nil, ErrCodeWriteContentUnverified
	}
	if obs.Failed || !obs.Complete {
		return ackID, nil, obs, ErrCodeWriteContentUnverified
	}
	if op.Kind == "edit" && ackID != strings.TrimSpace(op.RemoteID) {
		return ackID, nil, obs, editIdentityMismatch
	}
	found, ok := entryByID(obs.Entries, ackID)
	if !ok {
		return ackID, nil, obs, ErrCodeWriteContentUnverified
	}
	if !writeContentMatches(op, found) {
		return ackID, &found, obs, ErrCodeWriteContentMismatch
	}
	return ackID, &found, obs, ""
}

// submitPlan consumes one persisted, unconsumed native approval. It
// re-validates every precondition immediately before dispatch (fresh plan
// render, fresh classification proof, bound context), then executes the
// complete approved operation list in deterministic order with a durable
// pre-write intent before each single dispatch. Approval, startup, restart and
// preview never write; a submission writes only what this approval bound.
func (s *Session) submitPlan(req Request) (SubmissionReceipt, error) {
	date, err := requiredDay(req)
	if err != nil {
		return SubmissionReceipt{}, err
	}
	if _, err := parseDate(date); err != nil {
		return SubmissionReceipt{}, err
	}
	approvalID := strings.TrimSpace(req.ApprovalID)
	if approvalID == "" {
		return SubmissionReceipt{}, newError(CodeBadRequest,
			"approval_id is required: submission consumes one exact persisted native approval, never a guess or a bare date")
	}

	receiptFor := func(status string, b Blocker) SubmissionReceipt {
		return SubmissionReceipt{
			Date: date, ApprovalID: approvalID, Status: status,
			Operations: 0, Attempts: 0, Confirmed: 0, Unresolved: 0, Unattempted: 0,
			Blockers:   []Blocker{b},
			NextAction: "no remote write was attempted; resolve the blocker, then re-verify preconditions or approve a fresh preview",
			Calls:      SubmissionCallCounts{},
		}
	}

	if s.store.Uncertain() {
		return receiptFor(SubmitDurabilityUncertain, Blocker{
			Code:   CodeWriteUncertain,
			Detail: "local state durability is uncertain; nothing is dispatched and no persistence is claimed",
		}), nil
	}

	st := s.store.State()
	approval := approvalByDateAndID(st, date, approvalID)
	if approval == nil {
		return receiptFor(SubmitBlockedBeforeDispatch, Blocker{
			Code:   CodeUnknownApproval,
			Detail: "approval_id does not name a native approval for this date; zero remote calls were made",
		}), nil
	}
	if approval.State == ApprovalRevoked {
		return receiptFor(SubmitBlockedBeforeDispatch, Blocker{
			Code:   CodeStaleApproval,
			Detail: "this approval is revoked and can never be consumed",
		}), nil
	}
	if approval.State == ApprovalExecuting {
		return receiptFor(SubmitBlockedBeforeDispatch, Blocker{
			Code:   CodeStaleApproval,
			Detail: "this approval is already executing or spent for execution " + approval.ExecutionID,
		}), nil
	}

	preview, err := s.previewPlan(date)
	if err != nil {
		code, detail := CodeInternal, err.Error()
		if app, ok := err.(*AppError); ok {
			code, detail = app.Code, app.Message
		}
		return receiptFor(SubmitBlockedBeforeDispatch, Blocker{
			Code:   code,
			Detail: "plan revalidation failed before any dispatch: " + detail,
		}), nil
	}
	if preview.PlanID != approval.PlanID {
		return receiptFor(SubmitBlockedBeforeDispatch, Blocker{
			Code:   CodeStalePlan,
			Detail: "the current plan for this date differs from the plan the approval binds; a new preview and a new approval are required",
		}), nil
	}

	writer, err := newGradionWriter(s.cfg, s.env)
	if err != nil {
		code, detail := CodeInternal, err.Error()
		if app, ok := err.(*AppError); ok {
			code, detail = app.Code, app.Message
		}
		return receiptFor(SubmitBlockedBeforeDispatch, Blocker{
			Code:   code,
			Detail: "the write boundary could not start: " + detail,
		}), nil
	}
	defer func() { _ = writer.Close() }()
	ctx, cancel := contextWithTimeout()
	defer cancel()

	st = s.store.State()
	proof, err := proofClassifications(ctx, writer, st.Remote, preview.Operations)
	if err != nil {
		code, detail := CodeInternal, err.Error()
		if app, ok := err.(*AppError); ok {
			code, detail = app.Code, app.Message
		}
		return receiptFor(SubmitBlockedBeforeDispatch, Blocker{
			Code:   code,
			Detail: "current classification validation failed before any dispatch: " + detail,
		}), nil
	}
	if approval.Summary.Choices == nil {
		return receiptFor(SubmitBlockedBeforeDispatch, Blocker{
			Code:   CodeStaleApproval,
			Detail: "this approval carries no bound classification choice proof; a new preview, explicit reselection and a new approval are required",
		}), nil
	}
	if len(approval.Summary.Choices) != len(proof.Choices) {
		return receiptFor(SubmitBlockedBeforeDispatch, Blocker{
			Code:   CodeStaleApproval,
			Detail: "the set of selected classifications proved by the current catalog no longer matches the approved choice set",
		}), nil
	}
	for id, bound := range approval.Summary.Choices {
		c, ok := proof.Choices[id]
		if !ok || c == nil {
			return receiptFor(SubmitBlockedBeforeDispatch, Blocker{
				Code:   CodeStaleApproval,
				Detail: "the selected classification " + id + " cannot be established from the current catalog results; a new preview, explicit reselection and a new approval are required",
			}), nil
		}
		if !choiceEqual(*bound, *c) {
			return receiptFor(SubmitBlockedBeforeDispatch, Blocker{
				Code:   CodeClassificationChanged,
				Detail: "the selected classification " + id + " changed its relevant content since approval; explicit reselection and a new approval are required",
			}), nil
		}
	}

	digest, err := approvalContextDigest(s.cfg, s.env)
	if err != nil {
		code, detail := CodeInternal, err.Error()
		if app, ok := err.(*AppError); ok {
			code, detail = app.Code, app.Message
		}
		return receiptFor(SubmitBlockedBeforeDispatch, Blocker{
			Code:   code,
			Detail: "the approval context could not be re-verified: " + detail,
		}), nil
	}
	if digest != approval.ContextDigest {
		return receiptFor(SubmitBlockedBeforeDispatch, Blocker{
			Code:   CodeStaleApproval,
			Detail: "the effective endpoint/credential context changed since approval; this approval cannot be consumed",
		}), nil
	}

	run := newSubmissionRun(s.store, approval, preview, proof.ReadCalls)
	return run.execute(ctx, writer), nil
}

type submissionRun struct {
	store     *Store
	approval  *ApprovalRecord
	preview   PlanPreview
	execution ExecutionRecord
	baseline  map[string]map[string]RemoteEntry // consumption-time entries per date
	// expected per date is the consumption baseline ADVANCED ONLY by this
	// run's verified self-effects (R4F3). All later drift checks compare fresh
	// read-back content against it; previously verified self entries are never
	// an exemption for outside changes to them.
	expected   map[string]map[string]RemoteEntry
	fresh      map[string]map[string]RemoteEntry // read-back entries per date
	rows       []WriteReceiptRow
	blockers   []Blocker
	proofReads int
	confirmed  int
	attempts   int
}

func newSubmissionRun(store *Store, approval *ApprovalRecord, preview PlanPreview, proofReads int) *submissionRun {
	run := &submissionRun{
		store: store, approval: approval, preview: preview,
		baseline:   map[string]map[string]RemoteEntry{},
		expected:   map[string]map[string]RemoteEntry{},
		fresh:      map[string]map[string]RemoteEntry{},
		proofReads: proofReads,
	}
	run.execution = ExecutionRecord{
		ExecutionID: "run-" + randHex(8), ApprovalID: approval.ApprovalID,
		PlanID: approval.PlanID, Date: approval.Date, State: ExecutionStarted,
		StartedAt: nowUTC(),
	}
	st := store.State()
	if st.Remote != nil {
		for d := range preview.RemoteObservations {
			obs := st.Remote.Observations[d]
			if obs == nil {
				continue
			}
			m := make(map[string]RemoteEntry, len(obs.Entries))
			for _, e := range obs.Entries {
				m[e.RemoteID] = e
			}
			run.baseline[d] = m
		}
	}
	// The expected baseline starts as an exact deep copy of the consumption
	// baseline and is never replaced by a later snapshot (never reapproved,
	// never silently narrowed).
	for d, m := range run.baseline {
		c := make(map[string]RemoteEntry, len(m))
		for id, e := range m {
			c[id] = cloneEntryVal(e)
		}
		run.expected[d] = c
	}
	return run
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

func cloneEntryVal(e RemoteEntry) RemoteEntry {
	if e.Billable != nil {
		b := *e.Billable
		e.Billable = &b
	}
	if e.DurationMinutes != nil {
		d := *e.DurationMinutes
		e.DurationMinutes = &d
	}
	return e
}

func (run *submissionRun) knownEntries(date string) map[string]RemoteEntry {
	if m, ok := run.fresh[date]; ok {
		return m
	}
	return run.baseline[date]
}

// checkDrift enforces the batch precondition semantics: the only allowed
// remote change is this run's verified self-effects, never a widened window.
// For a date never re-read since consumption, the precondition is inherited
// from the consumed render (its plan identity just matched). For a re-read
// date, the ENTIRE fresh content is compared against the consumption baseline
// advanced only by verified self-effects (R4F3): changed existing entries,
// unexpected followers and vanished entries all stop later operations instead
// of being absorbed; a previously verified self entry that someone else
// changed is an outside change, not an exemption.
func (run *submissionRun) checkDrift(op PlanOperation) *Blocker {
	expected, hasExpected := run.expected[op.Date]
	fresh, hasFresh := run.fresh[op.Date]
	if hasFresh {
		if !hasExpected {
			hasExpected, expected = true, map[string]RemoteEntry{}
		}
		for id, now := range fresh {
			had, ok := expected[id]
			if !ok {
				return &Blocker{Code: "external_remote_drift", Date: op.Date, LocalID: op.LocalID, RemoteID: id,
					Detail: "an unexpected remote entry appeared during the batch; later operations are stopped and confirmed effects are durable"}
			}
			if !entryContentEqual(had, now, nil) {
				return &Blocker{Code: "external_remote_drift", Date: op.Date, LocalID: op.LocalID, RemoteID: id,
					Detail: "an existing remote entry's content changed during the batch; later operations are stopped and confirmed effects are durable"}
			}
		}
		for id := range expected {
			if _, still := fresh[id]; !still {
				return &Blocker{Code: "external_remote_drift", Date: op.Date, LocalID: op.LocalID, RemoteID: id,
					Detail: "a previously expected remote entry disappeared during the batch; later operations are stopped and confirmed effects are durable"}
			}
		}
	}
	if op.Kind == "edit" {
		current, ok := run.knownEntries(op.Date)[op.RemoteID]
		if !ok {
			return &Blocker{Code: "external_remote_drift", Date: op.Date, LocalID: op.LocalID, RemoteID: op.RemoteID,
				Detail: "the edit's intended remote entry is not present in the current view; later operations are stopped"}
		}
		if op.ExpectedRemote == nil || !entryContentEqual(current, *op.ExpectedRemote, nil) {
			return &Blocker{Code: "remote_changed", Date: op.Date, LocalID: op.LocalID, RemoteID: op.RemoteID,
				Detail: "the edit's target content no longer matches the approval's expected preconditions; later operations are stopped"}
		}
	}
	// Defensive one-to-one guards: two approved operations may never target the
	// same binding or the same remote id.
	if op.Kind == "create" {
		if _, b := bindingByLocalAndDate(run.store.State().Remote, op.LocalID, op.Date); b != nil {
			return &Blocker{Code: selfBindingConflict, Date: op.Date, LocalID: op.LocalID,
				Detail: "the allocation is already bound; it cannot be created again"}
		}
	}
	if op.Kind == "edit" {
		if _, b := bindingByLocalAndDate(run.store.State().Remote, op.LocalID, op.Date); b == nil || b.RemoteID != op.RemoteID {
			return &Blocker{Code: selfBindingConflict, Date: op.Date, LocalID: op.LocalID, RemoteID: op.RemoteID,
				Detail: "the edit's intended identity does not match the allocation's binding"}
		}
	}
	return nil
}

func bindingByLocalAndDate(r *RemoteState, localID, date string) (int, *RemoteBinding) {
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

func (run *submissionRun) buildIntent(st *State, op PlanOperation, index int) *WriteIntent {
	ref := "wr-" + randHex(12)
	if ref == "" {
		return nil
	}
	for i := range st.Intents {
		if st.Intents[i].WriteRef == ref {
			return nil
		}
	}
	return &WriteIntent{
		WriteRef: ref, ExecutionID: run.execution.ExecutionID,
		ApprovalID: run.approval.ApprovalID, PlanID: run.approval.PlanID,
		Date: op.Date, OpIndex: index, LocalID: op.LocalID, Action: op.Kind,
		RemoteID: op.RemoteID, OpDigest: planOperationDigest(op),
		Operation: clonePlanOperation(op), Preconditions: run.approval.Summary.RemoteFingerprint,
		Status: IntentInFlight, AttemptedAt: nowUTC(),
	}
}

// commitIntent durably records the intent. On the first operation of the run it
// consumes the approval into executing in the SAME protected transition, so a
// crash cannot leave reusable authorization for an already dispatched
// operation. Returns "ok", or "" for a pre-activation failure (nothing
// changed), or "uncertain" for post-rename activation (no dispatch, no further
// persistence claims).
func (run *submissionRun) commitIntent(intent *WriteIntent, first bool) string {
	st := run.store.State()
	next := st.cloneState()
	if first {
		for i := range next.Approvals {
			if next.Approvals[i].ApprovalID == run.approval.ApprovalID {
				next.Approvals[i].State = ApprovalExecuting
				next.Approvals[i].ExecutionID = run.execution.ExecutionID
				next.Approvals[i].ConsumedAt = run.execution.StartedAt
			}
		}
		next.Executions = append(next.Executions, run.execution)
	} else {
		for i := range next.Executions {
			if next.Executions[i].ExecutionID == run.execution.ExecutionID {
				next.Executions[i].Attempts = intent.OpIndex + 1
			}
		}
	}
	next.Intents = append(next.Intents, *intent)
	err := run.store.CommitState(next)
	if err == nil {
		return "ok"
	}
	if run.store.Uncertain() {
		return "uncertain"
	}
	return ""
}

func (run *submissionRun) prepareExecutionUpdate(next *State, state, outcome, nextAction, finishedAt string) {
	for i := range next.Executions {
		if next.Executions[i].ExecutionID != run.execution.ExecutionID {
			continue
		}
		e := &next.Executions[i]
		e.Attempts = run.attempts
		e.Confirmed = run.confirmed
		e.Unresolved = run.unresolvedCount()
		e.Unattempted = len(run.preview.Operations) - run.attempts
		e.State = state
		e.Outcome = outcome
		e.NextAction = nextAction
		e.FinishedAt = finishedAt
	}
}

func (run *submissionRun) unresolvedCount() int {
	n := 0
	for _, r := range run.rows {
		switch r.Status {
		case IntentInFlight, IntentUncertain, IntentUnidentified:
			n++
		}
	}
	return n
}

// recordOutcome persists one intent's terminal or interim state together with
// its binding and read-back observation updates in one protected transition.
// The execution row always carries the run's CURRENT interim summary; the
// lifecycle/finish fields are finalized by persistExecutionSummary.
func (run *submissionRun) recordOutcome(ref, status, errCode, ackID string, observed *RemoteEntry, readBack *RemoteObservation, binding *RemoteBinding, editUpdate bool) (*State, error) {
	st := run.store.State()
	next := st.cloneState()
	idx := -1
	if ref != "" {
		for i := range next.Intents {
			if next.Intents[i].WriteRef == ref {
				idx = i
			}
		}
		if idx < 0 {
			return nil, newError(CodeInternal, "the durable intent could not be located for its outcome record")
		}
	}
	var intent *WriteIntent
	if idx >= 0 {
		intent = &next.Intents[idx]
		intent.Status = status
		if errCode != "" {
			intent.ErrorCode = errCode
		}
		if ackID != "" {
			intent.AcknowledgedID = ackID
		}
		if observed != nil {
			intent.Observed = cloneEntry(observed)
		}
		intent.RecordedAt = nowUTC()
	}

	if next.Remote == nil {
		next.Remote = &RemoteState{}
	}
	if binding != nil {
		if editUpdate {
			found := false
			for i := range next.Remote.Bindings {
				if next.Remote.Bindings[i].LocalID == binding.LocalID && next.Remote.Bindings[i].Date == binding.Date {
					b := &next.Remote.Bindings[i]
					b.Date = binding.Date
					b.StartTime = binding.StartTime
					b.EndTime = binding.EndTime
					b.ClassificationID = binding.ClassificationID
					b.Description = binding.Description
					b.LastObserved = cloneEntry(binding.LastObserved)
					b.BaselineFields = append([]string(nil), binding.BaselineFields...)
					b.UpdatedAt = binding.UpdatedAt
					found = true
				}
			}
			if !found {
				return nil, newError(CodeInternal, "the edit's binding disappeared before its verified outcome could be recorded")
			}
		} else {
			next.Remote.Bindings = append(next.Remote.Bindings, *binding)
		}
	}
	if readBack != nil {
		if next.Remote.Observations == nil {
			next.Remote.Observations = map[string]*RemoteObservation{}
		}
		next.Remote.Observations[readBack.Date] = readBack
	}
	for i := range next.Executions {
		if next.Executions[i].ExecutionID != run.execution.ExecutionID {
			continue
		}
		e := &next.Executions[i]
		e.Attempts = run.attempts
		e.Confirmed = run.confirmed
		e.Unresolved = run.unresolvedCount()
		e.Unattempted = len(run.preview.Operations) - run.attempts
	}
	return next, nil
}

// persistExecutionSummary commits the run's final execution summary exactly
// once, after the loop.
func (run *submissionRun) persistExecutionSummary() string {
	next := run.store.State().cloneState()
	run.prepareExecutionUpdate(next, run.execution.State, run.execution.Outcome, run.execution.NextAction, run.execution.FinishedAt)
	return run.persist(next)
}

// persist commits a candidate; it reports whether the commit was clean,
// uncertain (post-rename; sticky), or failed (pre-activation).
func (run *submissionRun) persist(next *State) string {
	if err := run.store.CommitState(next); err != nil {
		if run.store.Uncertain() {
			return "uncertain"
		}
		return ""
	}
	return "ok"
}

// execute runs the exact operation list: one durable intent, one dispatch, one
// verification and one protected outcome persisted per operation. Any
// uncertainty STOPS the batch; remaining operations are reported unattempted.
func (run *submissionRun) execute(ctx context.Context, writer *GradionWriter) SubmissionReceipt {
	date := run.execution.Date
	total := len(run.preview.Operations)
	receipt := SubmissionReceipt{
		Date: date, ApprovalID: run.approval.ApprovalID, PlanID: run.approval.PlanID,
		ExecutionID: run.execution.ExecutionID, Operations: total,
		Writes: []WriteReceiptRow{},
	}

	// Defensive one-to-one check across the whole plan before anything is
	// dispatched: two approved operations must never share a binding or a
	// target.
	if b := run.checkPlanConsistency(); b != nil {
		return run.stopWithoutDispatch(ctx, writer, receipt, b)
	}

	// A plan with zero operations needs nothing remote; the approval is still
	// spent by this explicit consumption in one protected transition.
	if total == 0 {
		return run.consumeWithoutWrites(receipt, writer)
	}

	stopNow := false
	for i := 0; i < total && !stopNow; i++ {
		op := run.preview.Operations[i]

		if b := run.checkDrift(op); b != nil {
			run.blockers = append(run.blockers, *b)
			stopNow = true
			break
		}

		st := run.store.State()
		intent := run.buildIntent(st, op, i)
		if intent == nil {
			run.blockers = append(run.blockers, Blocker{Code: CodeInternal, Date: op.Date, LocalID: op.LocalID,
				Detail: "the approved operation could not be prepared as a durable intent; nothing was dispatched"})
			stopNow = true
			break
		}
		switch result := run.commitIntent(intent, i == 0); result {
		case "":
			run.blockers = append(run.blockers, Blocker{Code: CodeWriteFailed, Date: op.Date, LocalID: op.LocalID,
				Detail: "the durable intent could not be persisted; nothing further is dispatched and this operation was never remotely attempted"})
			stopNow = true
		case "uncertain":
			receipt.Status = SubmitDurabilityUncertain
			run.blockers = append(run.blockers, Blocker{Code: CodeWriteUncertain, Date: op.Date, LocalID: op.LocalID,
				Detail: "the durable intent's persistence is uncertain after the protected transition; dispatch is not performed and no further persistence is claimed"})
			stopNow = true
		default:
			run.attempts = i + 1
			run.rows = append(run.rows, WriteReceiptRow{
				WriteRef: intent.WriteRef, OpIndex: i, Action: op.Kind,
				Date: op.Date, LocalID: op.LocalID, Status: IntentInFlight,
			})
			rows := &run.rows[len(run.rows)-1]
			run.dispatch(ctx, writer, op, intent, rows, &stopNow)
		}
	}

	run.execution.State = ExecutionCompleted
	run.execution.Outcome = SubmitAllConfirmed
	run.execution.NextAction = "the exact approved operations completed and verified; no further action is required for this approval"
	run.execution.FinishedAt = nowUTC()
	if stopNow {
		run.execution.State = ExecutionStopped
		if receipt.Status == "" {
			receipt.Status = SubmitPartialStopped
		}
		if run.execution.Outcome == SubmitAllConfirmed {
			if run.unresolvedCount() > 0 {
				run.execution.Outcome = "stopped_unresolved"
			} else {
				run.execution.Outcome = "stopped_unattempted"
			}
		}
		run.execution.NextAction = "resolve_write each unresolved write reference via submission_status; remaining approved operations require a fresh preview and a new approval"
	}
	if receipt.Status == "" {
		receipt.Status = SubmitAllConfirmed
	}
	if result := run.persistExecutionSummary(); result == "uncertain" && receipt.Status == SubmitAllConfirmed {
		receipt.Status = SubmitDurabilityUncertain
	}
	return run.assemble(receipt, writer)
}

// stopWithoutDispatch handles pre-loop consistency failures: zero calls, the
// execution summary is persisted as stopped, and the approval stays unconsumed.
func (run *submissionRun) stopWithoutDispatch(_ context.Context, writer *GradionWriter, receipt SubmissionReceipt, b *Blocker) SubmissionReceipt {
	run.execution.State = ExecutionStopped
	run.execution.Outcome = "stopped_consistency"
	run.execution.NextAction = "resolve the consistency blocker before any submission; zero remote calls were made for this attempt"
	run.execution.FinishedAt = nowUTC()
	if run.persistExecutionSummary() == "uncertain" {
		receipt.Status = SubmitDurabilityUncertain
	} else {
		receipt.Status = SubmitBlockedBeforeDispatch
	}
	receipt.Blockers = []Blocker{*b}
	receipt.NextAction = run.execution.NextAction
	receipt.Calls = run.callCounts(writer)
	return receipt
}

// consumeWithoutWrites consumes a zero-operation approval and records its
// completed execution. No write can exist here.
//
// The single persistence transition is reported honestly (R4F7):
//   - clean success: all_confirmed, the execution row is durable;
//   - post-rename uncertainty: durability_uncertain, the existing sticky
//     semantics are unchanged;
//   - failure before activation: persistence_failed, no consumption and no
//     execution are claimed durable, and the same approval remains active for a
//     later explicit submit. Nothing is replayed automatically.
func (run *submissionRun) consumeWithoutWrites(receipt SubmissionReceipt, writer *GradionWriter) SubmissionReceipt {
	st := run.store.State()
	next := st.cloneState()
	for i := range next.Approvals {
		if next.Approvals[i].ApprovalID == run.approval.ApprovalID {
			next.Approvals[i].State = ApprovalExecuting
			next.Approvals[i].ExecutionID = run.execution.ExecutionID
			next.Approvals[i].ConsumedAt = run.execution.StartedAt
		}
	}
	run.execution.State = ExecutionCompleted
	run.execution.Outcome = SubmitAllConfirmed
	run.execution.NextAction = "the exact approved operations completed and verified; no further action is required for this approval"
	run.execution.FinishedAt = run.execution.StartedAt
	next.Executions = append(next.Executions, run.execution)
	receipt.Confirmed = 0
	receipt.Calls = run.callCounts(writer)
	switch run.persist(next) {
	case "ok":
		receipt.Status = SubmitAllConfirmed
		receipt.Blockers = []Blocker{}
	case "uncertain":
		receipt.Status = SubmitDurabilityUncertain
		receipt.Blockers = []Blocker{}
	default:
		receipt.Status = SubmitPersistenceFailed
		receipt.ExecutionID = ""
		receipt.Blockers = []Blocker{{
			Code:   CodeWriteFailed,
			Detail: "the zero-operation consumption could not be persisted before activation; no consumption and no execution are durable, no remote call was made for this attempt, and the approval remains active",
		}}
		receipt.NextAction = "restore local state availability, then explicitly submit the same active approval again; nothing was consumed and nothing is replayed automatically"
	}
	return receipt
}

func (run *submissionRun) lastIntentRef() string {
	for i := len(run.rows) - 1; i >= 0; i-- {
		if run.rows[i].WriteRef != "" {
			return run.rows[i].WriteRef
		}
	}
	return ""
}

// checkPlanConsistency guards the deterministic batch: create operations must
// not repeat an allocation, and edit operations must not repeat a remote id.
func (run *submissionRun) checkPlanConsistency() *Blocker {
	seenLocal := map[string]bool{}
	seenRemote := map[string]bool{}
	for _, op := range run.preview.Operations {
		switch op.Kind {
		case "create":
			key := op.LocalID + "|" + op.Date
			if seenLocal[key] {
				return &Blocker{Code: selfBindingConflict, Date: op.Date,
					LocalID: op.LocalID,
					Detail:  "the approved plan repeats a create for one allocation; nothing was dispatched"}
			}
			seenLocal[key] = true
		case "edit":
			if seenRemote[op.RemoteID] {
				return &Blocker{Code: selfBindingConflict, Date: op.Date,
					RemoteID: op.RemoteID, LocalID: op.LocalID,
					Detail: "the approved plan repeats an edit for one remote identity; nothing was dispatched"}
			}
			seenRemote[op.RemoteID] = true
		default:
			return &Blocker{Code: CodeInternal, Date: op.Date, LocalID: op.LocalID,
				Detail: "an approved operation has an unknown kind"}
		}
	}
	return nil
}

// dispatch performs exactly one write for one operation and persists its
// verified (or unresolved) outcome.
func (run *submissionRun) dispatch(ctx context.Context, writer *GradionWriter, op PlanOperation, intent *WriteIntent, row *WriteReceiptRow, stopNow *bool) {
	var attempt writeAttempt
	var err error
	switch op.Kind {
	case "create":
		attempt, err = writer.createEntry(ctx, op)
	case "edit":
		attempt, err = writer.editEntry(ctx, op)
	default:
		err = newError(CodeInternal, "an approved operation has an unknown kind")
	}
	// A dispatch-side local failure means nothing was sent: the intent is
	// durable and blocks replay until an explicit resolution clears it.
	if err != nil {
		row.ErrorCode = errorCode(err)
		run.blockers = append(run.blockers, Blocker{Code: errorCode(err), Date: op.Date, LocalID: op.LocalID,
			Detail: "the approved operation could not be dispatched; nothing was sent and the durable write reference " + row.WriteRef + " blocks replay until resolved"})
		run.execution.State = ExecutionStopped
		run.execution.Outcome = "stopped_unresolved"
		run.execution.NextAction = "resolve_write write reference " + row.WriteRef + " (not_applied attests it never applied); then a fresh preview and approval"
		run.execution.FinishedAt = nowUTC()
		if state, err := run.recordOutcome(row.WriteRef, IntentInFlight, row.ErrorCode, "", nil, nil, nil, false); err == nil {
			_ = run.persist(state)
		}
		*stopNow = true
		return
	}

	ackID, verified, readBack, errCode := writer.verifyWrite(ctx, op, attempt)

	// Uncertain or unidentified: never retried, never continued. Any identity
	// the provider acknowledged is preserved as receipt data only.
	if errCode != "" || attempt.Status != "identified" {
		status := attempt.Status
		code := attempt.Code
		if attempt.Status == "identified" {
			status = IntentUncertain
			code = errCode
			row.RemoteID = ackID
		}
		if code == editIdentityMismatch {
			status = IntentUnidentified
			run.blockers = append(run.blockers, Blocker{Code: editIdentityMismatch, Date: op.Date,
				LocalID: op.LocalID, RemoteID: op.RemoteID,
				Detail: "the provider acknowledged " + ackID + " instead of the approved edit identity " + op.RemoteID + "; the acknowledged entry is never bound or confirmed"})
		}
		row.Status = status
		row.ErrorCode = code
		row.ErrorDetail = attempt.Detail
		if attempt.Status == "identified" && row.RemoteID == "" {
			row.RemoteID = ackID
		}
		run.execution.State = ExecutionStopped
		run.execution.Outcome = "stopped_unresolved"
		run.execution.NextAction = "resolve_write write reference " + row.WriteRef + " (adopt with matching verified content, or not_applied with explicit operator attestation); remaining approved operations require a fresh preview and a new approval"
		run.execution.FinishedAt = nowUTC()
		if readBack != nil {
			if state, err := run.recordOutcome(row.WriteRef, status, code, ackID, nil, readBack, nil, false); err == nil {
				_ = run.persist(state)
			}
		} else if state, err := run.recordOutcome(row.WriteRef, status, code, ackID, nil, nil, nil, false); err == nil {
			_ = run.persist(state)
		}
		*stopNow = true
		return
	}

	binding, bindingErr := run.bindingFor(op, ackID, verified, intent)
	if bindingErr != nil {
		row.Status = IntentUncertain
		row.ErrorCode = selfBindingConflict
		row.RemoteID = ackID
		run.blockers = append(run.blockers, Blocker{Code: selfBindingConflict, Date: op.Date, LocalID: op.LocalID, RemoteID: ackID,
			Detail: "the write's effect was verified but its identity binding was refused (" + bindingErr.Error() + "); the write reference blocks until explicitly resolved"})
		run.execution.State = ExecutionStopped
		run.execution.Outcome = "stopped_unresolved"
		run.execution.NextAction = "resolve_write write reference " + row.WriteRef + " with the observed stable identity; remaining approved operations require a fresh preview and a new approval"
		run.execution.FinishedAt = nowUTC()
		if readBack != nil {
			if state, err := run.recordOutcome(row.WriteRef, IntentUncertain, selfBindingConflict, ackID, nil, readBack, nil, false); err == nil {
				_ = run.persist(state)
			}
		}
		*stopNow = true
		return
	}
	editUpdate := op.Kind == "edit"
	var candidate *State
	var persistErr error
	if readBack != nil {
		candidate, persistErr = run.recordOutcome(row.WriteRef, IntentConfirmed, "", ackID, verified, readBack, binding, editUpdate)
	} else {
		candidate, persistErr = run.recordOutcome(row.WriteRef, IntentConfirmed, "", ackID, verified, nil, binding, editUpdate)
	}
	if persistErr != nil {
		row.ErrorCode = selfBindingConflict
		run.blockers = append(run.blockers, Blocker{Code: selfBindingConflict, Date: op.Date, LocalID: op.LocalID, RemoteID: ackID,
			Detail: "the verified outcome could not be framed for persistence (" + persistErr.Error() + "); the write reference blocks until explicitly resolved"})
		run.execution.State = ExecutionStopped
		run.execution.Outcome = "stopped_unresolved"
		run.execution.FinishedAt = nowUTC()
		*stopNow = true
		return
	}
	persistResult := run.persist(candidate)
	switch persistResult {
	case "ok":
		row.Status = IntentConfirmed
		row.RemoteID = ackID
		run.confirmed++
		if verified != nil {
			// R4F3: advance the expected state ONLY by this verified self
			// effect. The consumption-time baseline itself stays untouched so
			// later drift checks can still distinguish this run's effects
			// from unexpected outside changes.
			if _, ok := run.expected[op.Date]; !ok {
				run.expected[op.Date] = map[string]RemoteEntry{}
			}
			run.expected[op.Date][ackID] = cloneEntryVal(*verified)
		}
		if readBack != nil {
			m := make(map[string]RemoteEntry, len(readBack.Entries))
			for _, e := range readBack.Entries {
				m[e.RemoteID] = e
			}
			// Only the FRESH view advances.
			run.fresh[op.Date] = m
		}
	case "uncertain":
		row.Status = IntentUncertain
		row.ErrorCode = CodeWriteUncertain
		row.RemoteID = ackID
		run.blockers = append(run.blockers, Blocker{Code: CodeWriteUncertain, Date: op.Date, LocalID: op.LocalID,
			Detail: "the write's effect was verified but its confirmed outcome could not be durably recorded; no further persistence is claimed and the write reference blocks until a restart and explicit resolution"})
		run.execution.State = ExecutionStopped
		run.execution.Outcome = "stopped_unresolved"
		run.execution.FinishedAt = nowUTC()
		*stopNow = true
	default:
		// Pre-activation persistence failure: the write happened remotely but
		// the confirmed outcome is not durable. The intent stays in_flight and
		// blocks replay until an explicit resolution; nothing is retried.
		row.Status = IntentInFlight
		row.ErrorCode = CodeWriteFailed
		row.RemoteID = ackID
		run.blockers = append(run.blockers, Blocker{Code: CodeWriteFailed, Date: op.Date, LocalID: op.LocalID, RemoteID: ackID,
			Detail: "the write's effect was verified but its confirmed outcome could not be durably recorded; the write reference blocks replay until explicitly resolved"})
		run.execution.State = ExecutionStopped
		run.execution.Outcome = "stopped_unresolved"
		run.execution.FinishedAt = nowUTC()
		*stopNow = true
	}
}

// bindingFor builds the binding update for a verified write, refusing any
// violation of one-to-one allocation identity.
func (run *submissionRun) bindingFor(op PlanOperation, ackID string, verified *RemoteEntry, intent *WriteIntent) (*RemoteBinding, error) {
	now := nowUTC()
	st := run.store.State()
	remote := st.Remote
	if remote == nil {
		remote = &RemoteState{}
	}
	if op.Kind == "create" {
		if _, b := bindingByRemote(remote, ackID); b != nil {
			return nil, newError(selfBindingConflict, "the acknowledged remote id is already bound to another allocation")
		}
		if _, b := bindingByLocalAndDate(remote, op.LocalID, op.Date); b != nil {
			return nil, newError(selfBindingConflict, "the allocation is already bound; it cannot be bound twice")
		}
		return &RemoteBinding{
			LocalID: op.LocalID, RemoteID: ackID, Date: op.Date,
			StartTime: op.StartTime, EndTime: op.EndTime,
			ClassificationID: op.ClassificationID, Description: op.Description,
			LastObserved:   cloneEntry(verified),
			BaselineFields: []string{},
			CreatedAt:      now, UpdatedAt: now, Origin: "submission",
		}, nil
	}
	idx, b := bindingByLocalAndDate(remote, op.LocalID, op.Date)
	_ = idx
	if b == nil || b.RemoteID != op.RemoteID {
		return nil, newError(selfBindingConflict, "the edit's binding no longer matches its intended remote identity")
	}
	if _, other := bindingByRemote(remote, op.RemoteID); other == nil {
		return nil, newError(selfBindingConflict, "the edit's target binding has disappeared from the durable ledger")
	}
	return &RemoteBinding{
		LocalID: op.LocalID, RemoteID: op.RemoteID, Date: op.Date,
		StartTime: op.StartTime, EndTime: op.EndTime,
		ClassificationID: op.ClassificationID, Description: op.Description,
		LastObserved: cloneEntry(verified), BaselineFields: []string{},
		UpdatedAt: now, Origin: b.Origin,
	}, nil
}

func (run *submissionRun) callCounts(writer *GradionWriter) SubmissionCallCounts {
	return SubmissionCallCounts{
		WriteCalls: writer.writeCalls,
		ReadCalls:  writer.readCalls + run.proofReads,
	}
}

func (run *submissionRun) assemble(receipt SubmissionReceipt, writer *GradionWriter) SubmissionReceipt {
	receipt.Attempts = run.attempts
	receipt.Confirmed = run.confirmed
	receipt.Unresolved = run.unresolvedCount()
	receipt.Unattempted = len(run.preview.Operations) - run.attempts
	receipt.Blockers = run.blockers
	if receipt.Blockers == nil {
		receipt.Blockers = []Blocker{}
	}
	receipt.Writes = run.rows
	receipt.Calls = run.callCounts(writer)
	if len(run.blockers) > 0 && receipt.Status == SubmitAllConfirmed {
		receipt.Status = SubmitPartialStopped
	}
	return receipt
}

// resolveWrite addresses ONE unresolved write (a native interrupted intent or a
// migrated unresolved outcome) with an explicit human decision bound to a
// current COMPLETE observation. adopt requires the explicitly selected observed
// stable id and matching intended content with one-to-one allocation identity;
// not_applied records an owner/operator attestation and rejects a contradictory
// observed matching effect. Original uncertainty/reason stays; only the
// addressed blocker clears. Nothing here dispatches, approves, revives a spent
// approval, skips other unresolved rows or deletes anything remotely.
func (s *Session) resolveWrite(req Request) (WriteResolutionReceipt, error) {
	ref := strings.TrimSpace(req.WriteRef)
	observationID := strings.TrimSpace(req.ObservationID)
	decision := strings.TrimSpace(req.Decision)
	remoteID := strings.TrimSpace(req.RemoteID)
	if ref == "" {
		return WriteResolutionReceipt{}, newError(CodeBadRequest, "write_ref is required")
	}
	if observationID == "" {
		return WriteResolutionReceipt{}, newError(CodeBadRequest, "observation_id is required: every resolution is bound to one current complete observation")
	}
	if decision != DecisionWriteAdopt && decision != DecisionWriteNotApplied {
		return WriteResolutionReceipt{}, newError(CodeBadRequest,
			"decision must be adopt or not_applied; nothing else can carry an explicit human resolution")
	}
	if decision == DecisionWriteAdopt && remoteID == "" {
		return WriteResolutionReceipt{}, newError(CodeBadRequest, "remote_id is required for adopt: the observed stable id must be explicitly selected")
	}
	if s.store.Uncertain() {
		return WriteResolutionReceipt{}, newError(CodeWriteUncertain,
			"local state durability is uncertain; close and restart before resolving")
	}

	st := s.store.State()
	// Resolve against the CURRENT observation for the intended date. A stale or
	// failed observation can never authorize a decision.
	receipt := WriteResolutionReceipt{WriteRef: ref, Decision: decision, ObservationID: observationID, RecordedAt: nowUTC()}
	if native, err := s.resolveNativeIntent(st, ref, observationID, decision, remoteID, &receipt); native || err != nil {
		return receipt, err
	}
	return s.resolveMigratedOutcome(st, ref, observationID, decision, remoteID, &receipt)
}

func observationForResolution(remote *RemoteState, date, observationID string) (*RemoteObservation, error) {
	if remote == nil {
		return nil, newError(CodeStaleObservation, "no current observation exists for this date; reconcile first")
	}
	obs := remote.Observations[date]
	if obs == nil {
		return nil, newError(CodeStaleObservation, "observation_id is not the current observation for this date; reconcile first")
	}
	if obs.Failed || obs.ObservationID != observationID {
		return nil, newError(CodeStaleObservation, "observation_id is not the current successful observation for this date; reconcile first")
	}
	if !obs.Complete {
		return nil, newError(CodeStaleObservation, "the current observation for this date is incomplete; only a current complete read-only observation can carry a resolution")
	}
	return obs, nil
}

// resolveNativeIntent applies resolution rules to one native intent. It reports
// handled=true when the request indeed names a native write reference.
func (s *Session) resolveNativeIntent(st *State, ref, observationID, decision, remoteID string, receipt *WriteResolutionReceipt) (bool, error) {
	idx := -1
	for i := range st.Intents {
		if st.Intents[i].WriteRef == ref {
			idx = i
			break
		}
	}
	if idx < 0 {
		return false, nil
	}
	intent := &st.Intents[idx]
	receipt.Origin = "native"
	receipt.Date = intent.Date
	receipt.LocalID = intent.LocalID
	receipt.RemoteID = intent.RemoteID
	if intent.unresolved() == false {
		if intent.Status == IntentConfirmed {
			return true, newError(CodeBadRequest, "write_ref names a confirmed write; confirmed history is not pending and is never reopened")
		}
		return true, newError(CodeBadRequest, "write_ref is already resolved ("+intent.Resolution+") and never re-resolved")
	}
	obs, err := observationForResolution(st.Remote, intent.Date, observationID)
	if err != nil {
		return true, err
	}
	if intent.Preconditions == "" {
		return true, newError(CodeStateInvalid, "the write reference lacks its preconditions; start a new submission after a fresh preview and approval instead")
	}

	next := st.cloneState()
	target := &next.Intents[idx]
	now := nowUTC()
	switch decision {
	case DecisionWriteAdopt:
		entry, ok := entryByID(obs.Entries, remoteID)
		if !ok {
			return true, newError(CodeUnknownRemote, "remote_id does not name an entry in the current observation; adopt binds one explicitly selected observed entry")
		}
		adoptEntry := entry
		// Content must match the intended operation: the adopt decision can
		// bind a real effect; it cannot invent one.
		if !writeContentMatches(intent.Operation, adoptEntry) {
			return true, newError(CodeIdentityConflict,
				"the selected observed entry's content does not match the intended operation; adopt requires the intended identity content")
		}
		if err := bindResolvedWrite(next, intent, remoteID, &adoptEntry, now); err != nil {
			return true, err
		}
		target.Resolved = true
		target.Resolution = decision
		target.ResolvedAt = now
		target.ResolutionObservationID = observationID
		receipt.Bound = true
		receipt.RemoteID = remoteID
	case DecisionWriteNotApplied:
		if err := notAppliedConsistent(intent, obs); err != nil {
			return true, err
		}
		target.Resolved = true
		target.Resolution = decision
		target.ResolvedAt = now
		target.ResolutionObservationID = observationID
	}
	// The observation record backing this decision is already durable; the
	// operator is asked to reconcile first when it is missing.
	if err := s.store.CommitState(next); err != nil {
		return true, err
	}
	receipt.Resolved = true
	return true, nil
}

// bindResolvedWrite persists a verified one-to-one mapping for an adopted
// write without dispatching anything.
func bindResolvedWrite(next *State, intent *WriteIntent, remoteID string, observed *RemoteEntry, now string) error {
	if next.Remote == nil {
		next.Remote = &RemoteState{}
	}
	if next.Remote.Bindings == nil {
		next.Remote.Bindings = []RemoteBinding{}
	}
	edit := intent.Action == "edit"
	idx, existing := bindingByLocalAndDate(next.Remote, intent.LocalID, intent.Date)
	_, byRemote := bindingByRemote(next.Remote, remoteID)
	if edit {
		if _, taken := bindingByRemote(next.Remote, remoteID); taken == nil {
			return newError(CodeIdentityConflict, "the intended remote identity is not bound; never rebind an edit by adoption")
		}
		if byRemote == nil || (idx < 0) || existing.RemoteID != remoteID {
			return newError(CodeIdentityConflict, "the observed id does not match the edit's intended one-to-one allocation identity")
		}
		b := &next.Remote.Bindings[idx]
		b.Date = intent.Date
		b.StartTime = intent.Operation.StartTime
		b.EndTime = intent.Operation.EndTime
		b.ClassificationID = intent.Operation.ClassificationID
		b.Description = intent.Operation.Description
		b.LastObserved = cloneEntry(observed)
		b.BaselineFields = []string{}
		b.UpdatedAt = now
		return nil
	}
	if _, b := bindingByLocalAndDate(next.Remote, intent.LocalID, intent.Date); b != nil {
		return newError(CodeIdentityConflict, "the allocation is already bound; adoption never rebinding an active allocation")
	}
	if _, b := bindingByRemote(next.Remote, remoteID); b != nil {
		return newError(CodeIdentityConflict, "the observed remote id is already bound to another allocation; adoption never steals bindings")
	}
	// One verified entry maps to one live allocation: refuse a mapping that
	// would leave two active bindings for the same remote id.
	if idx >= 0 && existing != nil {
		return newError(CodeIdentityConflict, "the local allocation identity is ambiguous")
	}
	next.Remote.Bindings = append(next.Remote.Bindings, RemoteBinding{
		LocalID: intent.LocalID, RemoteID: remoteID, Date: intent.Date,
		StartTime: intent.Operation.StartTime, EndTime: intent.Operation.EndTime,
		ClassificationID: intent.Operation.ClassificationID, Description: intent.Operation.Description,
		LastObserved: cloneEntry(observed), BaselineFields: []string{},
		CreatedAt: now, UpdatedAt: now, Origin: "resolution",
	})
	return nil
}

// migrationOutcomeCrosswalkMatches verifies that an addressed legacy outcome's
// local identity actually belongs to the migration the row claims (R4F6).
// Rows recorded without their migration key keep the existing local-identity
// verification path; a row claiming an unknown or unrelated migration is
// insufficient legacy provenance and fails closed.
func migrationOutcomeCrosswalkMatches(st *State, o WriteOutcome) bool {
	mid := strings.TrimSpace(o.MigrationID)
	if mid == "" {
		return true
	}
	if st.Migration == nil {
		return false
	}
	for i := range st.Migration.Receipts {
		if strings.TrimSpace(st.Migration.Receipts[i].MigrationID) != mid {
			continue
		}
		r := &st.Migration.Receipts[i]
		for _, a := range r.LegacyIntervals {
			if a.NewID == o.LocalID {
				return true
			}
		}
		for _, c := range r.IDCrosswalk {
			if c.NewID == o.LocalID {
				return true
			}
		}
		for _, v := range r.IDMapping {
			if v == o.LocalID {
				return true
			}
		}
		return false
	}
	return false
}

// notAppliedConsistent rejects a not_applied attestation contradicted by an
// observed matching effect in the current complete observation.
func notAppliedConsistent(intent *WriteIntent, obs *RemoteObservation) error {
	for _, e := range obs.Entries {
		if writeContentMatches(intent.Operation, e) {
			return newError(CodeIdentityConflict,
				"the current observation shows a matching effect for the intended write; not_applied contradicts the reviewed observation and is refused")
		}
	}
	return nil
}

// resolveMigratedOutcome applies resolution rules to one migrated unresolved
// outcome. Insufficient legacy intent/identity information remains a visible
// blocker, never guessed proof.
func (s *Session) resolveMigratedOutcome(st *State, ref, observationID, decision, remoteID string, receipt *WriteResolutionReceipt) (WriteResolutionReceipt, error) {
	if st.Remote == nil {
		return *receipt, newError(CodeUnknownWrite, "no migrated unresolved writes exist for this reference")
	}
	idx := -1
	for i := range st.Remote.Outcomes {
		if migratedWriteRef(st.Remote.Outcomes[i]) == ref {
			if idx >= 0 {
				return *receipt, newError(CodeIdentityConflict, "the write reference is not unique among migrated outcomes; the exact row cannot be addressed")
			}
			idx = i
		}
	}
	if idx < 0 {
		return *receipt, newError(CodeUnknownWrite, "write_ref does not name an unresolved native intent or migrated outcome")
	}
	outcome := &st.Remote.Outcomes[idx]
	receipt.Origin = "migrated"
	receipt.Date = outcome.Date
	receipt.LocalID = outcome.LocalID
	receipt.RemoteID = outcome.RemoteID
	if !outcomeBlocks(*outcome) {
		if outcome.Status == "confirmed" {
			return *receipt, newError(CodeBadRequest, "write_ref names confirmed migration history; it is not pending and is never reopened")
		}
		return *receipt, newError(CodeBadRequest, "write_ref is already resolved and never re-resolved")
	}

	// The intended content: legacy rows carry their own content, and an absent
	// Action is interpreted only from the row's rich record.
	action := outcome.Action
	if action == "" && outcome.RemoteID != "" {
		action = "edit"
	}
	if action != "create" && action != "edit" {
		return *receipt, newError(CodeUnknownLocal, "the migrated outcome lacks usable intent information; this blocker remains visible and requires a documented legacy resolution")
	}
	if action == "edit" && strings.TrimSpace(outcome.RemoteID) == "" {
		return *receipt, newError(CodeUnknownLocal, "the migrated edit lacks its intended remote identity; this blocker remains visible and requires a documented legacy resolution")
	}

	date := outcome.Date
	if date == "" {
		return *receipt, newError(CodeUnknownLocal, "the migrated outcome lacks a usable date; this blocker cannot be addressed by this decision")
	}
	obs, err := observationForResolution(st.Remote, date, observationID)
	if err != nil {
		return *receipt, err
	}

	next := st.cloneState()
	target := &next.Remote.Outcomes[idx]
	now := nowUTC()
	switch decision {
	case DecisionWriteAdopt:
		entry, ok := entryByID(obs.Entries, remoteID)
		if !ok {
			return *receipt, newError(CodeUnknownRemote, "remote_id does not name an entry in the current observation; adopt binds one explicitly selected observed entry")
		}
		adoptEntry := entry
		matched := writeContentMatches(PlanOperation{
			Date: outcome.Date, StartTime: outcome.StartTime, EndTime: outcome.EndTime,
			ClassificationID: outcome.ClassificationID, Description: outcome.Description,
		}, adoptEntry)
		if !matched {
			return *receipt, newError(CodeIdentityConflict, "the selected observed entry's content does not match the migrated outcome's intended content")
		}
		if action == "edit" && remoteID != outcome.RemoteID {
			return *receipt, newError(CodeIdentityConflict, "an edit resolution must retain the row's intended remote identity")
		}
		if outcome.LocalID == "" {
			return *receipt, newError(CodeUnknownLocal, "the migrated outcome lacks a usable local identity; this blocker remains visible")
		}
		// The actual (origin interval ID, allocation date) contribution this row
		// addresses (R4F6): a spill allocation belongs to an interval stored on an
		// earlier origin day, so the resolution validates the migrated interval
		// identity and its timezone-resolved allocation on the row's recorded
		// write date instead of requiring a spill-day record that never existed.
		iv, origin, _, ok := confirmedIntervalByID(s.store.State(), outcome.LocalID)
		if !ok {
			return *receipt, newError(CodeUnknownLocal, "the migrated outcome's local identity does not name confirmed durable work; the allocation identity cannot be verified")
		}
		originDay, hasOrigin := s.store.State().Days[origin]
		if !hasOrigin {
			return *receipt, newError(CodeUnknownLocal, "no confirmed work day exists for the migrated outcome's origin; the allocation identity cannot be verified")
		}
		originLoc, err := dayLocation(originDay.Timezone)
		if err != nil {
			return *receipt, err
		}
		// Crosswalk identity: the addressed row must belong to the migration the
		// origin interval came from, preserving the legacy→new identity audit.
		// A row without usable migration linkage is insufficient legacy
		// information and remains a visible blocker.
		if !migrationOutcomeCrosswalkMatches(s.store.State(), *outcome) {
			return *receipt, newError(CodeUnknownLocal, "no migration crosswalk backs this outcome's local identity and recorded write date; the allocation identity cannot be verified")
		}
		var alloc *Allocation
		allocs, err := iv.ResolvedAllocations(originLoc)
		if err != nil {
			return *receipt, err
		}
		for i := range allocs {
			if allocs[i].Date == date {
				a := allocs[i]
				alloc = &a
			}
		}
		if alloc == nil {
			return *receipt, newError(CodeUnknownLocal, "the migrated outcome's local identity does not allocate onto the recorded write date "+date+"; the allocation identity cannot be verified")
		}
		startText, endText, representable := allocationRemoteSpan(*alloc)
		if !representable {
			return *receipt, newError(CodeUnknownLocal, "the migrated outcome's allocation has no representable same-day remote span; the allocation identity cannot be verified")
		}
		if outcome.StartTime != "" || outcome.EndTime != "" {
			eq := func(a, b string) bool {
				if b == "00:00" && a == "24:00" {
					return true
				}
				return strings.TrimSpace(a) == strings.TrimSpace(b)
			}
			if !eq(startText, outcome.StartTime) || !eq(endText, outcome.EndTime) {
				return *receipt, newError(CodeUnknownLocal, "the migrated outcome's intended span does not match the timezone-resolved allocation it claims; the allocation identity cannot be verified")
			}
		}
		if err := bindResolvedWrite(next, &WriteIntent{
			LocalID: outcome.LocalID, Date: outcome.Date, Action: action, RemoteID: outcome.RemoteID,
			Operation: PlanOperation{
				LocalID: outcome.LocalID, Kind: action, Date: outcome.Date, RemoteID: outcome.RemoteID,
				StartTime: outcome.StartTime, EndTime: outcome.EndTime,
				ClassificationID: outcome.ClassificationID, Description: outcome.Description,
			},
		}, remoteID, &adoptEntry, now); err != nil {
			return *receipt, err
		}
		target.Resolved = true
		target.Resolution = decision
		target.ResolvedAt = now
		target.ResolutionObservationID = observationID
		receipt.Bound = true
		receipt.RemoteID = remoteID
	case DecisionWriteNotApplied:
		intended := PlanOperation{
			Date: outcome.Date, StartTime: outcome.StartTime, EndTime: outcome.EndTime,
			ClassificationID: outcome.ClassificationID, Description: outcome.Description,
		}
		for _, e := range obs.Entries {
			if writeContentMatches(intended, e) {
				return *receipt, newError(CodeIdentityConflict,
					"the current observation shows a matching effect for the intended migrated write; not_applied contradicts the reviewed observation and is refused")
			}
		}
		target.Resolved = true
		target.Resolution = decision
		target.ResolvedAt = now
		target.ResolutionObservationID = observationID
	}
	if err := s.store.CommitState(next); err != nil {
		return *receipt, err
	}
	receipt.Resolved = true
	return *receipt, nil
}
