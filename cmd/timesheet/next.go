package main

// next is the deterministic regular-Pi progression planner (issue #195). It
// derives the day's next action from observations that already exist — the
// store's durable day, evidence and remote records, plus the artifact files the
// binary already owns — and prints ONE typed action. Without --do it writes
// nothing, carries no state between invocations and never executes anything;
// with --do it executes each deterministic action the derivation licenses and
// stops at the first judgment, stop or human gate.
//
// The predicate order below IS the pipeline. It is grounded in what the binary
// actually persists, which differs from the design sketch in two places:
//
//   - A finalized day is derived FIRST. The draft is a working file, so its
//     absence says nothing about a saved day; re-running the collect flow after
//     save would be refused by the verbs anyway.
//   - A recorded day is monotone: once entries exist, the draft/descriptions
//     judgment is over even if the day folder no longer holds the artifacts.
//     Re-judging a recorded day would re-run a paid judgment for no progress.
//
// Day.Staged and Day.SavedRevision are session-only fields (model.go), so the
// planner never reads them.
//
// Freshness is proven from CONTENT, not from published timestamps (which are
// second resolution and cannot distinguish a same-second ledger edit):
//
//   - staging is current when the stored stage map matches the ledger's own
//     stage-time projection (unit ids and per-unit span counts);
//   - classification is current when every persisted selection's
//     BlockContentDigest binds to the block that projection derives — the same
//     check save performs before it replays;
//   - a judgment answer is only consumed when it carries the digest this
//     report published (draft_digest for descriptions, ledger_digest for
//     classifications), so a stale answer file is never handed to a verb.
//
// Durables timestamps are used only where ordering genuinely is the fact: the
// plan tail's plan/observation/resolution relation.

import (
	"strings"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// The exhaustive state set. The switch in deriveDayNext is total over it.
const (
	stateAction     = "action"      // executable argv; the caller runs it verbatim
	stateJudgment   = "judgment"    // model-owned; never executed by next
	stateStopped    = "stopped"     // blocker / underivable; Reason is required
	stateHumanGates = "human_gates" // terminal; approve/submit are human-only
)

// The judgment kinds, anchored to the two existing file contracts.
const (
	judgmentBlockDescriptions = "block_descriptions"
	judgmentClassifications   = "classifications"
)

// nextAction is the one JSON document `next` prints.
type nextAction struct {
	Day      string          `json:"day"`
	State    string          `json:"state"`
	Step     string          `json:"step,omitempty"`
	Action   *nextArgv       `json:"action,omitempty"`
	Judgment *nextJudgment   `json:"judgment,omitempty"`
	Reason   string          `json:"reason,omitempty"`
	Human    *nextHumanGates `json:"human,omitempty"`
}

// nextArgv is one exact executable argv plus the observation that selected it.
type nextArgv struct {
	Cmd []string `json:"cmd"`
	Why string   `json:"why"`
}

// nextJudgment names the model-owned decision and the file it writes. The
// answer file is bound to the content it was judged from (draft_digest or
// ledger_digest), so next only advances when the file matches the current
// content; the consuming verb still re-validates fail-closed as today.
type nextJudgment struct {
	Kind         string      `json:"kind"`
	Day          string      `json:"day"`
	AnswerFile   string      `json:"answer_file"`
	Protocol     string      `json:"protocol"`
	DraftDigest  string      `json:"draft_digest,omitempty"`
	LedgerDigest string      `json:"ledger_digest,omitempty"`
	Blocks       []nextBlock `json:"blocks,omitempty"`
	// Draft is the exact document a descriptions answer must judge: the same
	// parse record consumes, embedded instead of re-rendered, so the model
	// never reads draft.json and no second read can disagree with the digest.
	// Absent for the classifications judgment.
	Draft *timesheet.Draft `json:"draft,omitempty"`
}

// nextBlock is one staged interval's current recorded content, so the
// classification judgment can be made from any cwd even when the day's draft
// and descriptions artifacts are gone.
type nextBlock struct {
	IntervalID  string `json:"interval_id"`
	BlockID     string `json:"block_id"`
	Start       string `json:"start"`
	End         string `json:"end"`
	Description string `json:"description"`
}

// nextHumanGates is the terminal state: the two human-only commands, the
// read-only report command, and the store's unresolved-write counts. The counts
// are STORE-WIDE, exactly like StatusResult's: an unresolved write from any
// date blocks every new submission, so a day-scoped count would understate the
// gate.
type nextHumanGates struct {
	ApproveCommand string `json:"approve_command"`
	SubmitCommand  string `json:"submit_command"`
	StatusCommand  string `json:"status_command"`
	// Unresolved counts legacy write outcomes; NativeUnresolved counts native
	// write intents. Both are global, never day-scoped.
	Unresolved       int `json:"unresolved_writes"`
	NativeUnresolved int `json:"native_unresolved_writes"`
	// Status is the read-only submission_status result, attached only when
	// `next --do` reaches the gates so the caller needs no extra turn.
	Status *timesheet.SubmissionStatusResult `json:"status,omitempty"`
}

// nextObservation is every fact the derivation may read. Each field is
// measured from an existing artifact, sidecar or durable store field; nothing
// here is inferred, and no decision uses a second-resolution timestamp.
type nextObservation struct {
	Day string

	// Absolute addressing so any cwd works for every derived command.
	Exe        string
	ConfigPath string
	EnvFile    string

	DraftPath        string
	DescriptionsPath string
	ChoicesPath      string
	// SidecarPath is the durable classification sidecar the verbs replay.
	SidecarPath string
	// DraftPresent/DraftValid report the draft artifact and whether it parses
	// as the exact timesheet.Draft type record consumes. A present but invalid
	// draft is re-collected, never judged or recorded against. DraftBlocks is
	// the parsed block count: record refuses a day with no blocks, so zero is
	// an unrecordable day, not a judgment to re-ask.
	DraftPresent bool
	DraftValid   bool
	DraftBlocks  int
	// DraftDigest is the SHA-256 of the current draft bytes; the descriptions
	// answer must carry it to be accepted.
	DraftDigest string
	// Draft is the parsed draft artifact, kept so the descriptions judgment can
	// embed the exact document record will consume.
	Draft *timesheet.Draft
	// DescriptionsPresent/ChoicesPresent report the answer artifacts; the
	// bindings are the digests those files carry ("" when absent/unbound).
	DescriptionsPresent bool
	DescriptionsBinding string
	ChoicesPresent      bool
	ChoicesBinding      string
	Choices             map[string]timesheet.ClassificationChoiceEntry
	ChoicesError        error

	// Artifacts is the ledger and its sidecars, one validated parse each.
	Artifacts timesheet.DayArtifacts
	// Units is the current ledger's stage-time content projection.
	Units    []ledgerUnit
	Workflow timesheet.WorkflowSnapshot

	// Project is the exact project id the staged/classified/saved commands
	// carry; ProjectBlocked means the configuration has several projects and
	// none was chosen, so the planner will not guess one.
	Project        string
	ProjectBlocked bool
	ProjectIDs     []string

	// GradionEnabled is the configuration's live remote boundary: classification
	// and planning both read the remote, and their verbs fail closed without it.
	GradionEnabled bool
}

// --- typed constructors ------------------------------------------------------

func (n nextAction) withAction(step string, cmd []string, why string) nextAction {
	n.State, n.Step = stateAction, step
	n.Action = &nextArgv{Cmd: cmd, Why: why}
	n.Judgment, n.Human, n.Reason = nil, nil, ""
	return n
}

func (n nextAction) withJudgment(step string, j nextJudgment) nextAction {
	n.State, n.Step = stateJudgment, step
	n.Judgment = &j
	n.Action, n.Human, n.Reason = nil, nil, ""
	return n
}

func (n nextAction) withStop(reason string) nextAction {
	n.State = stateStopped
	n.Reason = reason
	n.Action, n.Judgment, n.Human, n.Step = nil, nil, nil, ""
	return n
}

func (n nextAction) withHuman(h nextHumanGates) nextAction {
	n.State, n.Step = stateHumanGates, "approve"
	n.Human = &h
	n.Action, n.Judgment, n.Reason = nil, nil, ""
	return n
}

// --- shared formatting helpers ----------------------------------------------

func blockerCodes(blockers []timesheet.Blocker) string {
	codes := make([]string, 0, len(blockers))
	for _, b := range blockers {
		code := b.Code
		if b.LocalID != "" {
			code += "/" + b.LocalID
		}
		if b.RemoteID != "" {
			code += "/" + b.RemoteID
		}
		codes = append(codes, code)
	}
	return strings.Join(codes, ", ")
}

// quote renders one value for a human-readable reason without inventing shell
// syntax; the executable form of every irreversible step stays an argv.
func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// safeToken is the shell-safe alphabet for shellJoin: a value made only of
// these runes can be pasted into a POSIX shell without quoting.
func safeToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("_@%+=:,./-", r):
		default:
			return false
		}
	}
	return true
}

// shellJoin renders one argv as a copy-pasteable POSIX shell command. The
// derived Action.Cmd stays the exact argv; this string form exists only for the
// human-gate and stopped reasons a person types.
func shellJoin(cmd []string) string {
	parts := make([]string, 0, len(cmd))
	for _, arg := range cmd {
		if safeToken(arg) {
			parts = append(parts, arg)
			continue
		}
		parts = append(parts, "'"+strings.ReplaceAll(arg, "'", `'\''`)+"'")
	}
	return strings.Join(parts, " ")
}

// sameInstant reports whether two RFC3339 stamps name the same instant.
func sameInstant(a, b string) bool {
	at, err := time.Parse(time.RFC3339, a)
	if err != nil {
		return false
	}
	bt, err := time.Parse(time.RFC3339, b)
	return err == nil && at.Equal(bt)
}

// timestampAfter parses two RFC3339 stamps and reports whether a is strictly
// later than b. It never compares timestamp strings lexicographically.
func timestampAfter(a, b string) (bool, error) {
	at, err := time.Parse(time.RFC3339, a)
	if err != nil {
		return false, err
	}
	bt, err := time.Parse(time.RFC3339, b)
	if err != nil {
		return false, err
	}
	return at.After(bt), nil
}
