package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func descriptionsDay(t *testing.T) (cfg string, args []string, first advanceReport) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	cfg = writeRunConfig(t)
	srv, _ := startFeed(t)
	envFile := writeRunEnvFile(t, srv.URL)
	args = []string{"--config", cfg, "--env-file", envFile, "--date", "2026-10-20", "--do"}
	first, err := advanceDoc(t, args...)
	if err != nil || first.State != stateJudgment || first.Judgment.Kind != judgmentBlockDescriptions {
		t.Fatalf("fixture = %+v, %v; want the descriptions judgment", first, err)
	}
	return cfg, args, first
}

func badDescriptions(t *testing.T, j *nextJudgment, ghost string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"draft_digest": j.DraftDigest, "notes": "n", "descriptions": map[string]string{ghost: "x"},
	})
	writeAnswerFile(t, j.AnswerFile, string(body))
}

func TestNextDoRepairsAnAnswerOnceThenStops(t *testing.T) {
	cfg, args, first := descriptionsDay(t)
	j := first.Judgment
	if j.LastError != "" {
		t.Fatalf("a fresh judgment carries last_error %q", j.LastError)
	}

	badDescriptions(t, j, "ghost-1")
	rep, err := advanceDoc(t, args...)
	if err != nil {
		t.Fatalf("a rejected answer exited nonzero: %v", err)
	}
	if rep.State != stateJudgment || rep.Judgment.DraftDigest != j.DraftDigest {
		t.Fatalf("report = %+v, want the same judgment re-asked", rep.nextAction)
	}
	if le := rep.Judgment.LastError; le == "" || len([]rune(le)) > lastErrorRuneLimit || !strings.Contains(le, "description") {
		t.Fatalf("last_error = %q, want one bounded line naming the problem", le)
	}
	if len(rep.Ran) != 1 || rep.Ran[0].Step != "record" || rep.Ran[0].Result != "failed" {
		t.Fatalf("ran = %+v, want the failed record", rep.Ran)
	}

	rep, _ = advanceDoc(t, args...)
	if rep.State != stateJudgment || rep.Judgment.LastError == "" || len(rep.Ran) != 0 {
		t.Fatalf("unrepaired answer = %+v ran %+v, want the judgment without a retry", rep.nextAction, rep.Ran)
	}

	badDescriptions(t, j, "ghost-2")
	for i := 0; i < 2; i++ {
		rep, _ = advanceDoc(t, args...)
		if rep.State != stateStopped || !strings.HasPrefix(rep.Reason, "judgment_failed: "+judgmentBlockDescriptions+": ") {
			t.Fatalf("attempt %d = %+v, want judgment_failed", i, rep.nextAction)
		}
	}
	failures := filepath.Join(testArtifactDir(t, cfg, "2026-10-20"), judgmentFailuresFile)
	if _, err := os.Stat(failures); err != nil {
		t.Fatalf("failure record missing from the day folder: %v", err)
	}

	writeNextDescriptions(t, filepath.Join(filepath.Dir(j.AnswerFile), "draft.json"), j.AnswerFile, j.DraftDigest)
	rep, _ = advanceDoc(t, args...)
	if len(rep.Ran) == 0 || rep.Ran[0].Step != "record" || rep.Ran[0].Result != "ok" {
		t.Fatalf("ran = %+v, want record to succeed after the fix", rep.Ran)
	}
	if _, err := os.Stat(failures); !os.IsNotExist(err) {
		t.Fatalf("failure record survived a successful record: %v", err)
	}
}

func TestNextDoRepairsAClassificationAnswerNamingTheInterval(t *testing.T) {
	stub := &stubRemote{entries: map[string][]map[string]any{testDate: {}}, classifications: stubCatalogText}
	cfg := candidatesFixture(t, stub, timedBlock("work-1", "09:00", "10:00", "Build the bridge"))
	args := []string{"--config", cfg, "--date", testDate, "--do"}
	rep, err := advanceDoc(t, args...)
	if err != nil || rep.Judgment == nil || rep.Judgment.Kind != judgmentClassifications {
		t.Fatalf("fixture = %+v, %v", rep.nextAction, err)
	}
	j := rep.Judgment
	iv := j.Blocks[0].IntervalID
	writeAnswerFile(t, j.AnswerFile, `{"ledger_digest":"`+j.LedgerDigest+`","selections":{"`+iv+
		`":{"catalog_id":"`+j.Candidates[0].CatalogID+`","classification_id":"cls-invented"}}}`)
	rep, err = advanceDoc(t, args...)
	if err != nil {
		t.Fatalf("a rejected answer exited nonzero: %v", err)
	}
	if rep.State != stateJudgment || rep.Judgment.LedgerDigest != j.LedgerDigest ||
		!strings.Contains(rep.Judgment.LastError, "interval "+iv) {
		t.Fatalf("report = %+v, want the judgment re-asked naming interval %s", rep.Judgment, iv)
	}
}

func TestRepairPolicyResetsWhenTheJudgedContentChanges(t *testing.T) {
	cfg := writeRunConfig(t)
	c, err := timesheet.LoadConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir := testArtifactDir(t, cfg, nextDay)
	o := nextObservation{Day: nextDay, DescriptionsPath: filepath.Join(dir, "descriptions.json"), DescriptionsBinding: "d1"}
	writeAnswerFile(t, o.DescriptionsPath, `{"draft_digest":"d1"}`)
	act := nextAction{Day: nextDay}.withAction("record", []string{"/exe", "record"}, "why")
	bad := &timesheet.AppError{Code: timesheet.CodeBadRequest, Message: "record: missing"}
	for i := 0; i < 2; i++ {
		if err := recordJudgmentFailure(c, nextDay, o, act, bad); err != nil {
			t.Fatal(err)
		}
	}
	f, _ := readJudgmentFailure(filepath.Join(dir, judgmentFailuresFile))
	if f == nil || f.Failures != 2 {
		t.Fatalf("record = %+v, want two failures for one digest", f)
	}
	o.DescriptionsBinding = "d2"
	if err := recordJudgmentFailure(c, nextDay, o, act, bad); err != nil {
		t.Fatal(err)
	}
	if f, _ = readJudgmentFailure(filepath.Join(dir, judgmentFailuresFile)); f.Failures != 1 || f.Digest != "d2" {
		t.Fatalf("record = %+v, want a reset count for the new digest", f)
	}

	o.DescriptionsSHA = f.AnswerSHA
	o.JudgmentFailure = &judgmentFailure{Kind: judgmentBlockDescriptions, Digest: "d1", AnswerSHA: f.AnswerSHA, Failures: 2}
	if got := applyRepairPolicy(o, act); got.State != stateAction {
		t.Fatalf("stale-digest record rewrote the act: %+v", got)
	}
}

func TestOnlyAnswerValidationErrorsAreRepaired(t *testing.T) {
	cases := map[error]bool{
		&timesheet.AppError{Code: timesheet.CodeBadRequest}:                            true,
		&timesheet.AppError{Code: timesheet.CodeUnknownInterval}:                       true,
		&timesheet.AppError{Code: timesheet.CodeUnknownClassify}:                       true,
		&timesheet.AppError{Code: "store_locked"}:                                      false,
		fmt.Errorf("record: the draft file cannot be read"):                            false,
		fmt.Errorf("wrapped: %w", &timesheet.AppError{Code: timesheet.CodeBadRequest}): true,
	}
	for err, want := range cases {
		if got := isAnswerValidation(err); got != want {
			t.Errorf("isAnswerValidation(%v) = %v, want %v", err, got, want)
		}
	}
	long := &timesheet.AppError{Code: timesheet.CodeBadRequest, Message: strings.Repeat("x", 1000)}
	if n := len([]rune(compactError(long))); n != lastErrorRuneLimit {
		t.Fatalf("compact error length = %d, want %d", n, lastErrorRuneLimit)
	}
}
