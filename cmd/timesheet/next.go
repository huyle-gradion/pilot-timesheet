package main

import (
	"strings"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

const (
	stateAction     = "action"      // executable argv; the caller runs it verbatim
	stateJudgment   = "judgment"    // model-owned; never executed by next
	stateStopped    = "stopped"     // blocker / underivable; Reason is required
	stateHumanGates = "human_gates" // terminal; approve/submit are human-only
	// stateAskHuman (next_ask.go) is a decision only the human makes; the
	// agent relays it and never answers it.
)

const (
	judgmentBlockDescriptions = "block_descriptions"
	judgmentClassifications   = "classifications"
)

type nextAction struct {
	Day      string          `json:"day"`
	State    string          `json:"state"`
	Step     string          `json:"step,omitempty"`
	Action   *nextArgv       `json:"action,omitempty"`
	Judgment *nextJudgment   `json:"judgment,omitempty"`
	Reason   string          `json:"reason,omitempty"`
	Human    *nextHumanGates `json:"human,omitempty"`
	Ask      *askHuman       `json:"ask,omitempty"`
}

type nextArgv struct {
	Cmd []string `json:"cmd"`
	Why string   `json:"why"`
}

type nextJudgment struct {
	Kind           string             `json:"kind"`
	Day            string             `json:"day"`
	AnswerFile     string             `json:"answer_file"`
	Protocol       string             `json:"protocol"`
	DraftDigest    string             `json:"draft_digest,omitempty"`
	LedgerDigest   string             `json:"ledger_digest,omitempty"`
	Blocks         []nextBlock        `json:"blocks,omitempty"`
	Candidates     []nextCandidateSet `json:"candidates,omitempty"`
	CandidatesNote string             `json:"candidates_note,omitempty"`
	Draft          *timesheet.Draft   `json:"draft,omitempty"`
	LastError      string             `json:"last_error,omitempty"`
}

type nextBlock struct {
	IntervalID  string `json:"interval_id"`
	BlockID     string `json:"block_id"`
	Start       string `json:"start"`
	End         string `json:"end"`
	Description string `json:"description"`
	// Candidates is the ref of this interval's set in judgment.candidates;
	// empty when the interval has no candidates.
	Candidates  string `json:"candidates"`
	CarriedFrom string `json:"carried_from,omitempty"`
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
	Unresolved       int                               `json:"unresolved_writes"`
	NativeUnresolved int                               `json:"native_unresolved_writes"`
	Status           *timesheet.SubmissionStatusResult `json:"status,omitempty"`
	Carried          []nextCarried                     `json:"carried,omitempty"`
}

type nextCarried struct {
	From        string `json:"from"`
	To          string `json:"to"`
	BlockID     string `json:"block_id"`
	Start       string `json:"start"`
	End         string `json:"end"`
	Minutes     int    `json:"minutes"`
	Description string `json:"description"`
}

func carriedView(carries ...[]timesheet.CarrySegment) []nextCarried {
	var out []nextCarried
	for _, list := range carries {
		for _, c := range list {
			out = append(out, nextCarried{
				From: c.SourceDate, To: c.Date, BlockID: c.BlockID(), Start: c.Start, End: c.End,
				Minutes: c.Minutes, Description: c.Description,
			})
		}
	}
	return out
}

type nextObservation struct {
	Day string

	Exe        string
	ConfigPath string
	EnvFile    string

	DraftPath           string
	DescriptionsPath    string
	ChoicesPath         string
	CandidatesPath      string
	SidecarPath         string
	DraftPresent        bool
	DraftValid          bool
	DraftBlocks         int
	DraftDigest         string
	Draft               *timesheet.Draft
	DescriptionsPresent bool
	DescriptionsBinding string
	ChoicesPresent      bool
	ChoicesBinding      string
	Choices             map[string]timesheet.ClassificationChoiceEntry
	ChoicesError        error
	DescriptionsSHA     string
	ChoicesSHA          string
	JudgmentFailure     *judgmentFailure
	Candidates          *candidatesFile
	// History is the parsed history.json (nil when absent); used only when
	// its ledger_digest binds to the current staged ledger (issue #235).
	History *historyFile

	Artifacts    timesheet.DayArtifacts
	Units        []ledgerUnit
	LegacyUnits  [][]ledgerUnit
	CarryDerived []timesheet.CarrySegment
	Workflow     timesheet.WorkflowSnapshot

	Asks map[string]askAnswer

	GradionEnabled bool
}

func (n nextAction) withAction(step string, cmd []string, why string) nextAction {
	n.State, n.Step = stateAction, step
	n.Action = &nextArgv{Cmd: cmd, Why: why}
	n.Judgment, n.Human, n.Reason, n.Ask = nil, nil, "", nil
	return n
}

func (n nextAction) withJudgment(step string, j nextJudgment) nextAction {
	n.State, n.Step = stateJudgment, step
	n.Judgment = &j
	n.Action, n.Human, n.Reason, n.Ask = nil, nil, "", nil
	return n
}

func (n nextAction) withStop(reason string) nextAction {
	n.State = stateStopped
	n.Reason = reason
	n.Action, n.Judgment, n.Human, n.Step, n.Ask = nil, nil, nil, "", nil
	return n
}

func (n nextAction) withHuman(h nextHumanGates) nextAction {
	n.State, n.Step = stateHumanGates, "approve"
	n.Human = &h
	n.Action, n.Judgment, n.Reason, n.Ask = nil, nil, "", nil
	return n
}

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

func quote(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

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

func sameInstant(a, b string) bool {
	at, err := time.Parse(time.RFC3339, a)
	if err != nil {
		return false
	}
	bt, err := time.Parse(time.RFC3339, b)
	return err == nil && at.Equal(bt)
}

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
