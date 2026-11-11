package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/llm"
	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

const (
	defaultMaxActions = 12
	// defaultAutoMaxActions leaves room for the judge steps of an --auto run.
	defaultAutoMaxActions = 24
	noticeRuneLimit       = 200
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
	held       *headlessSession
	// cmd is the derived argv the act runs, for acts whose inputs are
	// carried by their argv (acknowledge, resolve).
	cmd []string
	// judge answers a judgment in-process (next --do --auto); nil leaves
	// every judgment to the caller.
	judge func(*nextJudgment) (judgeReceipt, error)
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
	case "acknowledge":
		return d.acknowledgeStep(h)
	case "resolve":
		return d.resolveStep(h)
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
	// Usage is the judge usage of an --auto run (issue #242).
	Usage *llm.Usage `json:"usage,omitempty"`
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
	var usage *llm.Usage
	if d.judge != nil {
		usage = &llm.Usage{Exact: true}
	}
	report := func(a nextAction) advanceReport { return advanceReport{nextAction: a, Ran: ran, Usage: usage} }
	seen := map[string]bool{}
	for i := 0; i < limits.MaxActions; i++ {
		obs, stop, err := derive()
		if err != nil {
			return advanceReport{}, err
		}
		if stop != nil {
			return report(*stop), nil
		}
		a := deriveDayNext(obs)
		if a.State == stateJudgment && a.Judgment != nil && d.judge != nil {
			// --auto: the binary answers the judgment and the loop re-derives.
			// The same question asked twice without progress stops; a rejected
			// answer comes back with last_error under the repair-once policy.
			j := a.Judgment
			key := "judge\x00" + j.Kind + "\x00" + j.DraftDigest + j.LedgerDigest + "\x00" + j.LastError
			step := "judge:" + j.Kind
			if seen[key] {
				return report(nextAction{Day: a.Day}.withStop("judgment_failed: " + j.Kind +
					": the judge answered and the same judgment was asked again; answer it through the agent path")), nil
			}
			seen[key] = true
			r, err := d.judge(j)
			if usage != nil {
				*usage = addUsage(*usage, r.Usage)
			}
			if err != nil {
				ran = append(ran, advanceStep{Step: step, Result: "failed", Error: boundedNotice(err.Error())})
				return report(nextAction{Day: a.Day}.withStop("judgment_failed: " + j.Kind + ": " + err.Error())), err
			}
			ran = append(ran, advanceStep{Step: step, Result: "ok", Notice: boundedNotice(r.Notice)})
			continue
		}
		if a.State != stateAction {
			return report(a), nil
		}
		step := stepAct(a)
		if step == "" {
			return report(a), nil
		}
		key := step + "\x00" + strings.Join(a.Action.Cmd, "\x00")
		if seen[key] {
			stop := nextAction{Day: a.Day}.withStop("action_stalled: " + step +
				" repeated with the same command, so the derivation is not progressing")
			return report(stop), nil
		}
		seen[key] = true
		d.cmd = a.Action.Cmd
		notice, actErr := d.run(step)
		_, _, _, judged := judgedAnswer(obs, a)
		if actErr != nil {
			ran = append(ran, advanceStep{Step: step, Result: "failed", Error: boundedNotice(actErr.Error())})
			if judged && isAnswerValidation(actErr) {
				// The answer itself was rejected: save it and re-derive, which
				// re-asks the judgment once with last_error or stops with
				// judgment_failed (issue #240).
				if err := recordJudgmentFailure(d.cfg, d.day, obs, a, actErr); err != nil {
					return report(nextAction{Day: a.Day}.withStop("action_failed: " + step + ": " + err.Error())), err
				}
				continue
			}
			return report(nextAction{Day: a.Day}.withStop("action_failed: " + step + ": " + actErr.Error())), actErr
		}
		if judged {
			if err := clearJudgmentFailure(d.cfg, d.day); err != nil {
				return report(nextAction{Day: a.Day}.withStop(err.Error())), err
			}
		}
		ran = append(ran, advanceStep{Step: step, Result: "ok", Notice: boundedNotice(notice)})
	}
	stop := nextAction{Day: d.day}.withStop("action_cap: " + strconv.Itoa(limits.MaxActions) +
		" actions ran without reaching a judgment, human gate or stop")
	return report(stop), nil
}

// runNextDo is the --do loop: every derivation opens and closes the store, then
// the winning act opens its own; the terminal report is the only stdout.
func runNextDo(f *nextFlags, cfg *timesheet.Config, configPath, envFile, day string) error {
	report, runErr := advanceDayReport(f, cfg, configPath, envFile, day)
	if report.Day == "" {
		return runErr
	}
	if err := printJSON(report); err != nil {
		return err
	}
	return runErr
}

// advanceDayReport is the --do run minus its stdout: the scheduled daily run
// (issue #243) reads the same report to build its notification.
func advanceDayReport(f *nextFlags, cfg *timesheet.Config, configPath, envFile, day string) (advanceReport, error) {
	d := actDispatcher{cfg: cfg, configPath: configPath, envFile: envFile, day: day}
	if f.auto {
		if cfg.Judge == nil {
			return advanceReport{}, fmt.Errorf("next: --auto needs a judge section in the configuration (ADR 0010); without it, run next --do and answer judgments through the agent path")
		}
		backend, err := newJudgeBackend(cfg, envFile)
		if err != nil {
			return advanceReport{}, err
		}
		d.judge = func(j *nextJudgment) (judgeReceipt, error) {
			return answerJudgment(context.Background(), cfg, day, j, backend, time.Now())
		}
	}
	derive := func() (nextObservation, *nextAction, error) {
		store, err := timesheet.OpenStore(cfg.StatePath)
		if err != nil {
			return nextObservation{}, nil, err
		}
		obs, stop, err := buildNextObservation(cfg, configPath, envFile, day, store)
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
		return report, runErr
	}
	if report.State == stateHumanGates && report.Human != nil {
		report.Human.Status = readSubmissionStatus(configPath, envFile, day)
	}
	if f.auto && report.Usage != nil {
		if err := appendAutoTokenRow(cfg, day, *report.Usage, time.Now()); err != nil {
			fmt.Fprintf(os.Stderr, "next: token usage row not written: %v\n", err)
		}
	}
	return report, runErr
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
