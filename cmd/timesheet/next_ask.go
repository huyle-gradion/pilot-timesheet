package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// ask_human (issue #239) is the typed form of a decision only the owner can
// make. It mirrors a judgment: a question, the closed set of answers when
// there is one, an answer file and a digest binding the answer to the current
// content. A valid bound answer licenses exactly one existing deterministic
// action (acknowledge, resolve) or settles the day's project; an invalid or
// stale one asks the same question again with last_error. The agent never
// answers on its own: it relays the question, and the answer records the
// surface it came from. approve, submit and resolve-write stay TTY-only human
// gates and are never asked here.
const (
	stateAskHuman = "ask_human"

	askSourceAck = "source_acknowledgement"
	askProject   = "project"
	askResolve   = "resolve"
)

// askSurfaces are where a human answer may come from.
var askSurfaces = []string{"terminal", "chat", "tui", "notification"}

// resolveDecisions is the resolve verb's closed decision set.
var resolveDecisions = []string{"adopt", "accept_observed", "foreign"}

type askHuman struct {
	Kind       string   `json:"kind"`
	Day        string   `json:"day"`
	Question   string   `json:"question"`
	Options    []string `json:"options,omitempty"`
	AnswerFile string   `json:"answer_file"`
	Digest     string   `json:"digest"`
	Protocol   string   `json:"protocol"`
	// LastError is why the present answer was not accepted.
	LastError string `json:"last_error,omitempty"`
}

// askAnswer is one parsed answer file. Answer is "answer", or the resolve
// file's own "decision".
type askAnswer struct {
	Present  bool
	Digest   string
	Answer   string
	Surface  string
	Unparsed bool
}

func (n nextAction) withAsk(a askHuman) nextAction {
	n.State, n.Step = stateAskHuman, "ask"
	n.Ask = &a
	n.Action, n.Judgment, n.Human, n.Reason = nil, nil, nil, ""
	return n
}

func askDigest(parts ...string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(parts, "\x00"))))
}

func askFileName(kind, qualifier string) string {
	if qualifier != "" {
		return "ask-" + kind + "-" + qualifier + ".json"
	}
	return "ask-" + kind + ".json"
}

// readAskAnswers parses every ask answer file in the day folder, keyed by
// file name.
func readAskAnswers(dir string) (map[string]askAnswer, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "ask-*.json"))
	if err != nil {
		return nil, err
	}
	out := map[string]askAnswer{}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var raw struct {
			Digest   string `json:"digest"`
			Answer   string `json:"answer"`
			Decision string `json:"decision"`
			Surface  string `json:"surface"`
		}
		a := askAnswer{Present: true}
		if json.Unmarshal(data, &raw) != nil {
			a.Unparsed = true
		} else {
			a.Digest, a.Answer, a.Surface = raw.Digest, raw.Answer, raw.Surface
			if a.Answer == "" {
				a.Answer = raw.Decision
			}
		}
		out[filepath.Base(p)] = a
	}
	return out, nil
}

// askProblem is why a present answer is not accepted; "" accepts it.
func askProblem(a askAnswer, digest string, options []string) string {
	switch {
	case a.Unparsed:
		return "the answer file is not valid JSON"
	case a.Digest != digest:
		return "the answer's digest does not match this question (stale or missing); copy digest verbatim"
	case !slices.Contains(askSurfaces, a.Surface):
		return "surface must be one of " + strings.Join(askSurfaces, ", ") + ", naming where the human answered"
	case len(options) > 0 && !slices.Contains(options, a.Answer):
		return "answer " + quote(a.Answer) + " is not one of: " + strings.Join(options, ", ")
	}
	return ""
}

// ask resolves one question: the accepted answer, or the ask action to
// report (asking again with last_error when an answer is present but wrong).
func (o nextObservation) ask(n nextAction, q askHuman) (askAnswer, *nextAction) {
	name := filepath.Base(q.AnswerFile)
	a := o.Asks[name]
	if a.Present {
		if problem := askProblem(a, q.Digest, q.Options); problem == "" {
			return a, nil
		} else {
			q.LastError = problem
		}
	}
	out := n.withAsk(q)
	return askAnswer{}, &out
}

func askProtocol(file, digest, answerHint string) string {
	return "relay the question to the human verbatim and write ONLY their explicit answer to " + file +
		` as {"digest":"` + digest + `","answer":` + answerHint + `,"surface":"terminal|chat|tui|notification"}` +
		"; never answer on the human's behalf; then run next again"
}

// sourceAckAsk is the source gate's question for one unacknowledged source.
func (o nextObservation) sourceAckAsk(src timesheet.SourceSnapshot) askHuman {
	digest := askDigest(o.Day, src.Source, src.CollectionID)
	file := filepath.Join(o.artifactDir(), askFileName(askSourceAck, src.Source))
	return askHuman{
		Kind: askSourceAck, Day: o.Day, AnswerFile: file, Digest: digest, Options: []string{"acknowledge"},
		Question: "the " + src.Source + " collection for " + o.Day + " is " + src.Status +
			" (collection " + src.CollectionID + "); acknowledge it and continue with the evidence collected so far?",
		Protocol: askProtocol(file, digest, `"acknowledge"`) +
			"; to refuse, leave the file unwritten and re-collect instead",
	}
}

// projectAsk is the question for a configuration with several projects and
// none chosen.
func projectAsk(day, dir string, ids []string) askHuman {
	sorted := slices.Clone(ids)
	sort.Strings(sorted)
	digest := askDigest(append([]string{day}, sorted...)...)
	file := filepath.Join(dir, askFileName(askProject, ""))
	return askHuman{
		Kind: askProject, Day: day, AnswerFile: file, Digest: digest, Options: sorted,
		Question: "the configuration defines the projects " + strings.Join(sorted, ", ") +
			"; which project are " + day + "'s staged intervals for?",
		Protocol: askProtocol(file, digest, `"<project id>"`) + "; the answer applies to this day only (--project still overrides it)",
	}
}

// settleProjectAnswer applies a valid project answer to the observation, or
// records why it was not accepted for the ask's last_error.
func settleProjectAnswer(obs *nextObservation, dir string) {
	if !obs.ProjectBlocked {
		return
	}
	q := projectAsk(obs.Day, dir, obs.ProjectIDs)
	a := obs.Asks[filepath.Base(q.AnswerFile)]
	if !a.Present {
		return
	}
	if problem := askProblem(a, q.Digest, q.Options); problem != "" {
		obs.ProjectAskError = problem
		return
	}
	obs.Project, obs.ProjectBlocked = a.Answer, false
}

// resolveAsk is the blocked-plan question. The answer file is the resolve
// decision file itself, plus digest and surface, so the derived resolve argv
// consumes it unchanged.
func (o nextObservation) resolveAsk(plan *timesheet.PlanSnapshot, context string) askHuman {
	obsID := ""
	if o.Workflow.Observation != nil {
		obsID = o.Workflow.Observation.ObservationID
	}
	digest := askDigest(o.Day, plan.PlanID, obsID)
	file := filepath.Join(o.artifactDir(), askFileName(askResolve, ""))
	return askHuman{
		Kind: askResolve, Day: o.Day, AnswerFile: file, Digest: digest, Options: resolveDecisions,
		Question: context + "; which identity decision does the human make?",
		Protocol: "relay the question and the reconcile receipt to the human and write ONLY their explicit decision to " + file +
			` as {"digest":"` + digest + `","surface":"terminal|chat|tui|notification","decision":"adopt|accept_observed|foreign",` +
			`"revision":<n>,"observation_id":"...","remote_id":"...","local_id":"..."} (revision, observation_id, remote_id and local_id ` +
			"copied verbatim from the reconcile receipt; local_id omitted for foreign); never decide on the human's behalf; then run next again",
	}
}

// --- acts -------------------------------------------------------------------

func (d actDispatcher) acknowledgeStep(h *headlessSession) (string, error) {
	if len(d.cmd) < 2 {
		return "", fmt.Errorf("next: the acknowledge act needs its derived argv")
	}
	f, err := parseAcknowledgeFlags(d.cmd[2:])
	if err != nil {
		return "", err
	}
	if _, err := h.call("open_day", timesheet.Request{Day: f.date}); err != nil {
		return "", err
	}
	if _, err := h.call("acknowledge_source", timesheet.Request{
		Day: f.date, Source: f.source, CollectionID: f.collectionID,
	}); err != nil {
		return "", err
	}
	return "acknowledged " + f.source + " collection " + f.collectionID, nil
}

func (d actDispatcher) resolveStep(h *headlessSession) (string, error) {
	if len(d.cmd) < 2 {
		return "", fmt.Errorf("next: the resolve act needs its derived argv")
	}
	f, err := parseResolveFlags(d.cmd[2:])
	if err != nil {
		return "", err
	}
	receipt, err := resolveFileDay(h, f.date, f.file)
	if err != nil {
		return "", err
	}
	return "resolved " + receipt.Decision + " for " + f.date, nil
}
