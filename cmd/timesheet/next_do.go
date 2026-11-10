package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

const (
	defaultMaxActions = 12
	noticeRuneLimit   = 200
)

// actDispatcher runs one derived step's act for one day. `next --do` and the
// tui both drive it, so each act body exists once. A caller that already holds
// the store (the tui) passes held; a caller that does not opens and closes one
// per act, because the store's flock is per open file descriptor.
type actDispatcher struct {
	cfg        *timesheet.Config
	configPath string
	envFile    string
	day        string
	project    string
	held       *headlessSession
}

// run executes one act and returns ONE bounded notice line. It never writes a
// verb's own JSON to stdout, so a --do run cannot stream a multi-KB receipt
// into the caller's context.
func (d actDispatcher) run(step string) (string, error) {
	if d.held != nil {
		return d.exec(d.held, step)
	}
	h, err := openHeadless(d.configPath, d.envFile)
	if err != nil {
		return "", err
	}
	defer h.done()
	return d.exec(h, step)
}

func (d actDispatcher) exec(h *headlessSession, step string) (string, error) {
	switch step {
	case "run":
		return d.runStep(h)
	case "record":
		return d.recordStep(h)
	case "stage":
		return d.stageStep(h)
	case "classify":
		return d.classifyStep(h)
	case "candidates":
		return d.candidatesStep(h)
	case "classify-history":
		return d.classifyHistoryStep(h)
	case "classify-default":
		return d.classifyDefaultStep(h)
	case "save":
		return d.saveStep(h)
	case "plan":
		return d.planStep(h)
	}
	return "", fmt.Errorf("next: the derived step %q offers no act", step)
}

// boundedNotice keeps one receipt line inside the report's size contract.
func boundedNotice(s string) string {
	runes := []rune(s)
	if len(runes) <= noticeRuneLimit {
		return s
	}
	return string(runes[:noticeRuneLimit-1]) + "…"
}

// advanceStep is one executed action's bounded receipt.
type advanceStep struct {
	Step   string `json:"step"`
	Result string `json:"result"`
	Notice string `json:"notice,omitempty"`
	Error  string `json:"error,omitempty"`
}

// advanceReport is `next --do`'s one document: the terminal derivation plus
// every deterministic action the run executed before reaching it.
type advanceReport struct {
	nextAction
	Ran []advanceStep `json:"ran,omitempty"`
}

type advanceLimits struct{ MaxActions int }

type deriveFn func() (nextObservation, *nextAction, error)

// advanceDay drives every deterministic action the derivation licenses, in
// order, until it reaches a state a model or a human owns. It never opens the
// store itself: derive closes it before returning and the dispatcher opens its
// own handle per act.
func advanceDay(derive deriveFn, d actDispatcher, limits advanceLimits) (advanceReport, error) {
	if limits.MaxActions < 1 {
		limits.MaxActions = defaultMaxActions
	}
	var ran []advanceStep
	seen := map[string]bool{}
	for i := 0; i < limits.MaxActions; i++ {
		obs, stop, err := derive()
		if err != nil {
			return advanceReport{}, err
		}
		if stop != nil {
			return advanceReport{nextAction: *stop, Ran: ran}, nil
		}
		// The derivation resolves the project (a single configured project is
		// implicit); the act must carry the same one, never the raw flag.
		d.project = obs.Project
		a := deriveDayNext(obs)
		if a.State != stateAction {
			return advanceReport{nextAction: a, Ran: ran}, nil
		}
		step := stepAct(a)
		if step == "" {
			return advanceReport{nextAction: a, Ran: ran}, nil
		}
		key := step + "\x00" + strings.Join(a.Action.Cmd, "\x00")
		if seen[key] {
			stop := nextAction{Day: a.Day}.withStop("action_stalled: " + step +
				" repeated with the same command, so the derivation is not progressing")
			return advanceReport{nextAction: stop, Ran: ran}, nil
		}
		seen[key] = true
		notice, actErr := d.run(step)
		_, _, _, judged := judgedAnswer(obs, a)
		if actErr != nil {
			ran = append(ran, advanceStep{Step: step, Result: "failed", Error: boundedNotice(actErr.Error())})
			if judged && isAnswerValidation(actErr) {
				// The answer itself was rejected: save it and re-derive, which
				// re-asks the judgment once with last_error or stops with
				// judgment_failed (issue #240).
				if err := recordJudgmentFailure(d.cfg, d.day, obs, a, actErr); err != nil {
					failed := nextAction{Day: a.Day}.withStop("action_failed: " + step + ": " + err.Error())
					return advanceReport{nextAction: failed, Ran: ran}, err
				}
				continue
			}
			failed := nextAction{Day: a.Day}.withStop("action_failed: " + step + ": " + actErr.Error())
			return advanceReport{nextAction: failed, Ran: ran}, actErr
		}
		if judged {
			if err := clearJudgmentFailure(d.cfg, d.day); err != nil {
				return advanceReport{nextAction: nextAction{Day: a.Day}.withStop(err.Error()), Ran: ran}, err
			}
		}
		ran = append(ran, advanceStep{Step: step, Result: "ok", Notice: boundedNotice(notice)})
	}
	stop := nextAction{Day: d.day}.withStop("action_cap: " + strconv.Itoa(limits.MaxActions) +
		" actions ran without reaching a judgment, human gate or stop")
	return advanceReport{nextAction: stop, Ran: ran}, nil
}

// runNextDo is the --do loop: every derivation opens and closes the store, then
// the winning act opens its own; the terminal report is the only stdout.
func runNextDo(f *nextFlags, cfg *timesheet.Config, configPath, envFile, day string) error {
	d := actDispatcher{cfg: cfg, configPath: configPath, envFile: envFile, day: day, project: f.project}
	derive := func() (nextObservation, *nextAction, error) {
		store, err := timesheet.OpenStore(cfg.StatePath)
		if err != nil {
			return nextObservation{}, nil, err
		}
		obs, stop, err := buildNextObservation(cfg, configPath, envFile, day, f.project, store)
		closeErr := store.Close()
		if err != nil {
			return obs, stop, err
		}
		if closeErr != nil {
			return nextObservation{}, nil, fmt.Errorf("next: the store lock cannot be released")
		}
		return obs, stop, nil
	}
	report, runErr := advanceDay(derive, d, advanceLimits{MaxActions: f.maxActions})
	if report.Day == "" {
		return runErr
	}
	if report.State == stateHumanGates && report.Human != nil {
		report.Human.Status = readSubmissionStatus(configPath, envFile, day)
	}
	if err := printJSON(report); err != nil {
		return err
	}
	return runErr
}

// readSubmissionStatus is the one extra read-only call the gates need, so the
// caller never spends a turn on submit-status. A failed read leaves it absent.
func readSubmissionStatus(configPath, envFile, day string) *timesheet.SubmissionStatusResult {
	h, err := openHeadless(configPath, envFile)
	if err != nil {
		return nil
	}
	defer h.done()
	res, err := h.call("submission_status", timesheet.Request{Day: day})
	if err != nil {
		return nil
	}
	status, ok := res.(timesheet.SubmissionStatusResult)
	if !ok {
		return nil
	}
	return &status
}
