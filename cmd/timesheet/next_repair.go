package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// The repair-once judgment policy (issue #240) lives here, not in skill text.
// When `next --do` hands a bound answer to its consuming act (record for
// descriptions, classify for classifications) and the act rejects the
// answer's CONTENT, the failure is saved in the day folder. The next
// derivation re-asks the same judgment with a compact last_error; a second
// rejected answer for the same judged content stops with judgment_failed. A
// new answer after that stop is tried again, so the day stays resumable, and
// a change in the judged content (its digest) resets the count.
const (
	judgmentFailuresFile = "judgment-failures.json"
	lastErrorRuneLimit   = 300
	maxAnswerRepairs     = 1
)

var answerValidationCodes = []string{
	timesheet.CodeBadRequest,
	timesheet.CodeUnknownInterval,
	timesheet.CodeUnknownClassify,
	timesheet.CodeStaleCatalog,
}

type judgmentFailure struct {
	Kind      string `json:"kind"`
	Digest    string `json:"digest"`
	AnswerSHA string `json:"answer_sha"`
	Failures  int    `json:"failures"`
	LastError string `json:"last_error"`
}

func isAnswerValidation(err error) bool {
	var app *timesheet.AppError
	return errors.As(err, &app) && slices.Contains(answerValidationCodes, app.Code)
}

func compactError(err error) string {
	msg := err.Error()
	var app *timesheet.AppError
	if errors.As(err, &app) {
		msg = app.Message
	}
	if r := []rune(msg); len(r) > lastErrorRuneLimit {
		msg = string(r[:lastErrorRuneLimit-1]) + "…"
	}
	return msg
}

func answerSHA(data []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func judgedAnswer(o nextObservation, a nextAction) (kind, digest, sha string, ok bool) {
	switch stepAct(a) {
	case "record":
		return judgmentBlockDescriptions, o.DescriptionsBinding, o.DescriptionsSHA, true
	case "classify":
		return judgmentClassifications, o.ChoicesBinding, o.ChoicesSHA, true
	}
	return "", "", "", false
}

func applyRepairPolicy(o nextObservation, n nextAction) nextAction {
	f := o.JudgmentFailure
	if f == nil || n.State != stateAction {
		return n
	}
	kind, digest, sha, ok := judgedAnswer(o, n)
	if !ok || f.Kind != kind || f.Digest != digest || f.AnswerSHA != sha {
		return n
	}
	if f.Failures > maxAnswerRepairs {
		return n.withStop("judgment_failed: " + kind + ": " + f.LastError +
			"; the answer was repaired once and still fails, so next will not ask again; fix the answer and run timesheet log again")
	}
	re := o
	re.JudgmentFailure = nil
	switch kind {
	case judgmentBlockDescriptions:
		re.DescriptionsPresent, re.DescriptionsBinding = false, ""
	case judgmentClassifications:
		re.ChoicesPresent, re.ChoicesBinding = false, ""
	}
	j := deriveDayNext(re)
	if j.State != stateJudgment || j.Judgment == nil || j.Judgment.Kind != kind {
		return n
	}
	j.Judgment.LastError = f.LastError
	return j
}

func judgmentFailuresPath(cfg *timesheet.Config, day string) (string, error) {
	dir, err := dayArtifactsDir(cfg.StatePath, day)
	return filepath.Join(dir, judgmentFailuresFile), err
}

func readJudgmentFailure(path string) (*judgmentFailure, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f judgmentFailure
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, nil
	}
	return &f, nil
}

func recordJudgmentFailure(cfg *timesheet.Config, day string, o nextObservation, a nextAction, actErr error) error {
	kind, digest, _, ok := judgedAnswer(o, a)
	if !ok {
		return nil
	}
	answerPath := o.DescriptionsPath
	if kind == judgmentClassifications {
		answerPath = o.ChoicesPath
	}
	data, err := os.ReadFile(answerPath)
	if err != nil {
		return fmt.Errorf("next: the answer %s cannot be read to record its failure: %w", answerPath, err)
	}
	path, err := judgmentFailuresPath(cfg, day)
	if err != nil {
		return err
	}
	prev, err := readJudgmentFailure(path)
	if err != nil {
		return err
	}
	next := judgmentFailure{Kind: kind, Digest: digest, AnswerSHA: answerSHA(data), Failures: 1, LastError: compactError(actErr)}
	if prev != nil && prev.Kind == kind && prev.Digest == digest {
		next.Failures = prev.Failures + 1
	}
	if err := ensureDayArtifactsDir("next", filepath.Dir(path)); err != nil {
		return err
	}
	out, err := jsonBytes(next)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("next: %s cannot be written: %w", path, err)
	}
	return nil
}

func clearJudgmentFailure(cfg *timesheet.Config, day string) error {
	path, err := judgmentFailuresPath(cfg, day)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("next: %s cannot be removed: %w", path, err)
	}
	return nil
}
