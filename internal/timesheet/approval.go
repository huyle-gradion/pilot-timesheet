package timesheet

// The ledger is the only authority: a successful preview is never approval, a
// plan record carries no authorization flag, and historical imported approvals
// stay inert audit forever.
//
// These ops run in-process for the local operator's commands. They are
// deliberately NOT authenticated presence: a scripted request is not proof a
// human is physically present, so no security flag such as human:true exists
// here. Nothing in this file performs AI inference, source
// acknowledgment, work confirmation or a submission: a persisted, unconsumed
// approval can only ever be used by a later explicit submit request.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

const (
	ApprovalActive    = "active"
	ApprovalExecuting = "executing"
	ApprovalRevoked   = "revoked"
)

const (
	IntentInFlight     = "in_flight"
	IntentConfirmed    = "confirmed"
	IntentUncertain    = "uncertain"
	IntentUnidentified = "unidentified"
)

const (
	ExecutionStarted   = "started"
	ExecutionCompleted = "completed"
	ExecutionStopped   = "stopped"
)

// Resolution decisions accepted by resolve_write. adopt binds an unresolved
// native intent or migrated outcome to verified observed content; not_applied
// records an explicit owner/operator attestation that a specific write did not
// apply. They never authorize a remote write and never erase original audit.
const (
	DecisionWriteAdopt      = "adopt"
	DecisionWriteNotApplied = "not_applied"
)

// Stable attempt-level error-code strings recorded on unresolved write intents.
// They are attempt receipts, not top-level protocol error codes.
const (
	ErrCodeWriteContentUnverified = "write_content_unverified"
	ErrCodeWriteContentMismatch   = "write_content_mismatch"
	ErrCodeWriteUnidentified      = "write_unidentified"
)

// ApprovalSummary is the bound plan/revision summary recorded inside one
// approval. It is audit data about what was approved, not authorization on its
// own.
type ApprovalSummary struct {
	SourceRevisions    map[string]int    `json:"source_revisions,omitempty"`
	RemoteObservations map[string]string `json:"remote_observations,omitempty"`
	RemoteFingerprint  string            `json:"remote_fingerprint,omitempty"`
	OperationDigests   []string          `json:"operation_digests,omitempty"`
	// Classifications maps each operation's classification id to the identity of
	// the fresh catalog result that proved it exists at approval time.
	Classifications map[string]string `json:"classifications,omitempty"`
	// Choices maps each operation's classification id to the relevant semantic
	// identity (id/code/name/activity type) the fresh authoritative proof
	// established at approval time (R4F1). Consumption revalidates this bound
	// choice against the current catalog: a relevant change requires explicit
	// reselection and a new approval, never a silently mutated approved body.
	// Dev.10 approvals predate this field; they never authorize a write and
	// consumption fails closed for them.
	Choices map[string]*ClassificationChoice `json:"choices"`
}

type ApprovalRecord struct {
	ApprovalID    string          `json:"approval_id"`
	PlanID        string          `json:"plan_id"`
	Date          string          `json:"date"`
	State         string          `json:"state"`
	ApprovedAt    string          `json:"approved_at"`
	ContextDigest string          `json:"context_digest"`
	Summary       ApprovalSummary `json:"summary"`
	ExecutionID   string          `json:"execution_id,omitempty"`
	ConsumedAt    string          `json:"consumed_at,omitempty"`
	RevokedAt     string          `json:"revoked_at,omitempty"`
}

type ApprovalReceipt struct {
	ApprovalID string          `json:"approval_id"`
	PlanID     string          `json:"plan_id"`
	Date       string          `json:"date"`
	State      string          `json:"state"`
	ApprovedAt string          `json:"approved_at"`
	Operations int             `json:"operations"`
	Summary    ApprovalSummary `json:"summary"`
}

type ApprovalRevocationReceipt struct {
	ApprovalID string `json:"approval_id"`
	PlanID     string `json:"plan_id"`
	Date       string `json:"date"`
	State      string `json:"state"`
	RevokedAt  string `json:"revoked_at"`
}

type ApprovalRow struct {
	ApprovalID  string `json:"approval_id"`
	PlanID      string `json:"plan_id"`
	State       string `json:"state"`
	ApprovedAt  string `json:"approved_at"`
	ExecutionID string `json:"execution_id,omitempty"`
	ConsumedAt  string `json:"consumed_at,omitempty"`
	RevokedAt   string `json:"revoked_at,omitempty"`
}

// randHex returns n cryptographically random bytes as lowercase hex.
func randHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	return hex.EncodeToString(buf)
}

// approvalContextDigest binds the effective Gradion endpoint and credential so
// an approval cannot be consumed against a different endpoint/credential
// context even when two providers return identical entry data. The digest never
// stores or prints the raw values and does not prove a user's real-world
// identity: it only detects that the configured context changed.
func approvalContextDigest(cfg *Config, env map[string]string) (string, error) {
	if cfg == nil || cfg.Gradion == nil || !cfg.Gradion.Enabled {
		return "", newError(CodeRemoteDisabled, "remote access is not enabled in configuration")
	}
	name := cfg.Gradion.TokenVariable()
	token, ok := resolveEnv(env, name)
	if !ok || strings.TrimSpace(token) == "" {
		return "", missingCredentialError(name)
	}
	return sha256Hex([]byte(cfg.Gradion.Endpoint + "\x00" + name + "\x00" + token)), nil
}

// planOperationDigest is the canonical immutable content identity of one
// operation, including the audit reason that is part of an edit's exact body.
// Audit-only timestamps stay absent so an identical fresh observation does not
// change the digest.
func planOperationDigest(op PlanOperation) string {
	var expected *RemoteEntry
	if op.ExpectedRemote != nil {
		e := *op.ExpectedRemote
		expected = &e
	}
	canon := canonicalOperation{
		LocalID: op.LocalID, Kind: op.Kind, RemoteID: op.RemoteID, Date: op.Date,
		StartTime: op.StartTime, EndTime: op.EndTime,
		ClassificationID: op.ClassificationID, Description: op.Description,
		CrossesMidnight: op.CrossesMidnight, PreserveRemoteOnly: op.PreserveRemoteOnly,
		ExpectedRemote: expected, Reason: op.Reason,
	}
	data, err := json.Marshal(canon)
	if err != nil {
		return ""
	}
	return sha256Hex(data)
}

func clonePlanOperation(op PlanOperation) PlanOperation {
	out := op
	out.OriginDates = append([]string(nil), op.OriginDates...)
	out.PreserveRemoteOnly = append([]string(nil), op.PreserveRemoteOnly...)
	out.ExpectedRemote = cloneEntry(op.ExpectedRemote)
	if op.OriginRevisions != nil {
		out.OriginRevisions = make(map[string]int, len(op.OriginRevisions))
		for k, v := range op.OriginRevisions {
			out.OriginRevisions[k] = v
		}
	}
	return out
}

func approvalByDateAndID(st *State, date, approvalID string) *ApprovalRecord {
	for i := range st.Approvals {
		a := &st.Approvals[i]
		if a.ApprovalID == approvalID {
			if a.Date != date {
				return nil
			}
			return a
		}
	}
	return nil
}

func (s *Session) approvalBoundFor(planID string) bool {
	st := s.store.State()
	for _, a := range st.Approvals {
		if a.PlanID == planID && (a.State == ApprovalActive || a.State == ApprovalExecuting) {
			return true
		}
	}
	return false
}

// approve_plan validates all current preconditions, re-renders the plan fresh
// and binds the new approval occurrence to it. It performs zero remote
// mutations and never submits anything.
func (s *Session) approvePlan(req Request) (ApprovalReceipt, error) {
	date, err := requiredDay(req)
	if err != nil {
		return ApprovalReceipt{}, err
	}
	if _, err := parseDate(date); err != nil {
		return ApprovalReceipt{}, err
	}
	planID := strings.TrimSpace(req.PlanID)
	if planID == "" {
		return ApprovalReceipt{}, newError(CodeBadRequest,
			"plan_id is required: approval binds one exact rendered plan, never a bare date or current plan guess")
	}
	if s.store.Uncertain() {
		return ApprovalReceipt{}, newError(CodeWriteUncertain,
			"local state durability is uncertain; close and restart before approving")
	}

	preview, err := s.previewPlan(date)
	if err != nil {
		return ApprovalReceipt{}, err
	}
	if preview.PlanID != planID {
		return ApprovalReceipt{}, newError(CodeStalePlan,
			"the rendered current plan for this date is different from the requested plan_id; preview again")
	}
	if preview.State != PlanReady {
		return ApprovalReceipt{}, newError(CodePlanNotReady,
			"the requested plan is not ready; the current preview for this date is blocked")
	}
	st := s.store.State()
	if st.Plans == nil || st.Plans[planID] == nil {
		return ApprovalReceipt{}, newError(CodeStalePlan,
			"the requested plan is not a stored immutable preview; preview again")
	}

	reader, err := NewGradionReader(s.cfg, s.env)
	if err != nil {
		return ApprovalReceipt{}, err
	}
	defer func() { _ = reader.Close() }()
	ctx, cancel := contextWithTimeout()
	defer cancel()
	proof, err := proofClassifications(ctx, reader, st.Remote, preview.Operations)
	if err != nil {
		return ApprovalReceipt{}, err
	}

	digest, err := approvalContextDigest(s.cfg, s.env)
	if err != nil {
		return ApprovalReceipt{}, err
	}

	approvalID := ""
	for seen := true; seen; {
		approvalID = "apr-" + randHex(16)
		seen = approvalByDateAndID(st, date, approvalID) != nil
	}
	digests := make([]string, 0, len(preview.Operations))
	for _, op := range preview.Operations {
		digests = append(digests, planOperationDigest(op))
	}
	summary := ApprovalSummary{
		SourceRevisions:    deepCopyIntMap(preview.SourceRevisions),
		RemoteObservations: cloneStringMap(preview.RemoteObservations),
		RemoteFingerprint:  preview.RemoteFingerprint,
		OperationDigests:   digests,
		Classifications:    proof.Catalogs,
		Choices:            proof.Choices,
	}
	now := time.Now().UTC().Format(time.RFC3339)
	record := ApprovalRecord{
		ApprovalID: approvalID, PlanID: planID, Date: date, State: ApprovalActive,
		ApprovedAt: now, ContextDigest: digest, Summary: summary,
	}
	next := st.cloneState()
	next.Approvals = append(next.Approvals, record)
	if err := s.store.CommitState(next); err != nil {
		return ApprovalReceipt{}, err
	}
	return ApprovalReceipt{
		ApprovalID: approvalID, PlanID: planID, Date: date, State: ApprovalActive,
		ApprovedAt: now, Operations: len(preview.Operations), Summary: summary,
	}, nil
}

// revoke_approval cancels one approval that has not yet been dispatched. It
// never undoes remote effects and never touches imported legacy approvals.
func (s *Session) revokeApproval(req Request) (ApprovalRevocationReceipt, error) {
	date, err := requiredDay(req)
	if err != nil {
		return ApprovalRevocationReceipt{}, err
	}
	if _, err := parseDate(date); err != nil {
		return ApprovalRevocationReceipt{}, err
	}
	approvalID := strings.TrimSpace(req.ApprovalID)
	if approvalID == "" {
		return ApprovalRevocationReceipt{}, newError(CodeBadRequest, "approval_id is required")
	}
	if s.store.Uncertain() {
		return ApprovalRevocationReceipt{}, newError(CodeWriteUncertain,
			"local state durability is uncertain; close and restart before revoking")
	}
	st := s.store.State()
	approval := approvalByDateAndID(st, date, approvalID)
	if approval == nil {
		return ApprovalRevocationReceipt{}, newError(CodeUnknownApproval,
			"approval_id does not name a native approval for this date")
	}
	if approval.State != ApprovalActive {
		return ApprovalRevocationReceipt{}, newError(CodeStaleApproval,
			"approval is already "+approval.State+" and cannot be revoked")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	next := st.cloneState()
	for i := range next.Approvals {
		if next.Approvals[i].ApprovalID == approvalID {
			next.Approvals[i].State = ApprovalRevoked
			next.Approvals[i].RevokedAt = now
		}
	}
	if err := s.store.CommitState(next); err != nil {
		return ApprovalRevocationReceipt{}, err
	}
	return ApprovalRevocationReceipt{
		ApprovalID: approvalID, PlanID: approval.PlanID, Date: date,
		State: ApprovalRevoked, RevokedAt: now,
	}, nil
}

func classificationReferences(ops []PlanOperation) (map[string]*ClassificationChoice, error) {
	refs := map[string]*ClassificationChoice{}
	for _, op := range ops {
		if op.SelectedChoice == nil {
			return nil, newError(CodeSelectionUnverifiable,
				"the plan operation "+op.LocalID+" carries no bound selected-choice proof; preview again")
		}
		if got, ok := refs[op.ClassificationID]; ok {
			if got == nil || *got != *op.SelectedChoice {
				return nil, newError(CodeInternal,
					"the plan's classification proof is inconsistent between its operations")
			}
			continue
		}
		refs[op.ClassificationID] = op.SelectedChoice
	}
	return refs, nil
}

type classificationReader interface {
	Classifications(ctx context.Context, query string) (*CatalogRecord, error)
}

type classificationsProof struct {
	Catalogs  map[string]string
	Choices   map[string]*ClassificationChoice
	ReadCalls int
}

// proofClassifications freshly validates every operation's selected
// classification against authoritative CURRENT catalog results, comparing the
// RELEVANT content (id/code/name/activity type) of the matched option against
// the bound selected reference. Stored option/name/code metadata is used only
// as a retrieval hint; a fresh partial result can prove a returned option but
// can never prove a missing one absent, so any non-proof or a relevant change
// fail closed before anything is written (R4F1). Unrelated options, query
// hints and audit timestamps never enter the comparison, so unchanged choices
// remain stable. Identical read queries inside one request are deduplicated.
func proofClassifications(ctx context.Context, reader classificationReader, remote *RemoteState, ops []PlanOperation) (*classificationsProof, error) {
	refs, err := classificationReferences(ops)
	if err != nil {
		return nil, err
	}
	hintFor := func(op PlanOperation) string {
		if remote == nil {
			return ""
		}
		cat := remote.Catalogs[op.CatalogID]
		if cat == nil {
			return ""
		}
		for _, opt := range cat.Options {
			if opt.ID == op.ClassificationID {
				if strings.TrimSpace(opt.Code) != "" {
					return opt.Code
				}
				if strings.TrimSpace(opt.Name) != "" {
					return opt.Name
				}
			}
		}
		return ""
	}
	type target struct {
		id     string
		hint   string
		found  string
		choice *ClassificationChoice
	}
	seen := map[string]bool{}
	var targets []target
	queries := map[string]bool{"": true}
	for _, op := range ops {
		id := op.ClassificationID
		if seen[id] {
			continue
		}
		seen[id] = true
		t := target{id: id}
		if hint := hintFor(op); hint != "" {
			t.hint = hint
			queries[hint] = true
		}
		targets = append(targets, t)
	}
	cache := make(map[string]*CatalogRecord, len(queries))
	readCalls := 0
	fetch := func(query string) (*CatalogRecord, error) {
		if cat, ok := cache[query]; ok {
			return cat, nil
		}
		cat, err := reader.Classifications(ctx, query)
		readCalls++
		if err != nil {
			return nil, err
		}
		cache[query] = cat
		return cat, nil
	}
	// Fetch each distinct query exactly once so the request performs one bounded
	// set of authoritative reads.
	for q := range queries {
		if _, err := fetch(q); err != nil {
			return nil, remoteError(err)
		}
	}
	proof := &classificationsProof{Catalogs: map[string]string{}, Choices: map[string]*ClassificationChoice{}}
	for _, t := range targets {
		order := []string{""}
		if t.hint != "" {
			order = []string{t.hint, ""}
		}
		for _, q := range order {
			cat := cache[q]
			if cat == nil {
				continue
			}
			for _, opt := range cat.Options {
				if opt.ID == t.id {
					c := choiceFromOption(opt)
					t.found = cat.CatalogID
					t.choice = &c
				}
			}
			if t.found != "" {
				break
			}
		}
		if t.found == "" {
			return nil, newError(CodeClassificationUnverified,
				"the fresh catalog results do not establish that the selected classification "+t.id+" still exists; explicit reselection and a new preview are required")
		}
		ref := refs[t.id]
		if ref == nil {
			return nil, newError(CodeSelectionUnverifiable,
				"the selected classification "+t.id+" carries no established choice binding; explicit reselection and a new preview are required")
		}
		if !choiceEqual(*ref, *t.choice) {
			return nil, newError(CodeClassificationChanged,
				"the authoritative catalog now shows different relevant content for the selected classification "+t.id+
					" (selected as "+ref.Name+" ["+ref.Code+"] "+ref.ActivityType+", current option is "+
					t.choice.Name+" ["+t.choice.Code+"] "+t.choice.ActivityType+"); explicit reselection and a new preview are required")
		}
		proof.Catalogs[t.id] = t.found
		choice := *t.choice
		proof.Choices[t.id] = &choice
	}
	proof.ReadCalls = readCalls
	return proof, nil
}

func deepCopyIntMap(in map[string]int) map[string]int {
	if in == nil {
		return nil
	}
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneClassChoiceMap(in map[string]*ClassificationChoice) map[string]*ClassificationChoice {
	if in == nil {
		return nil
	}
	out := make(map[string]*ClassificationChoice, len(in))
	for k, v := range in {
		if v == nil {
			out[k] = nil
			continue
		}
		c := *v
		out[k] = &c
	}
	return out
}
