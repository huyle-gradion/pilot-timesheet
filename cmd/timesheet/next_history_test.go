package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

type pastBlock struct {
	block    timesheet.ReviewedBlock
	classID  string
	name     string
	recorded string
}

func seedSubmitted(t *testing.T, cfg, day string, blocks ...pastBlock) {
	t.Helper()
	reviewed := make([]timesheet.ReviewedBlock, 0, len(blocks))
	for _, b := range blocks {
		reviewed = append(reviewed, b.block)
	}
	seedEntries(t, cfg, day, reviewed)
	receipt := runStageCapture(t, []string{"--config", cfg, "--date", day})
	ivByBlock := map[string]string{}
	for _, ref := range receipt.Staged {
		ivByBlock[ref.BlockID] = ref.IntervalID
	}
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	st := store.State()
	for _, b := range blocks {
		st.Intents = append(st.Intents, timesheet.WriteIntent{
			WriteRef: day + "-" + b.block.BlockID, Date: day, LocalID: ivByBlock[b.block.BlockID],
			ExecutionID: "ex-" + day, ApprovalID: "ap-" + day, PlanID: "pl-" + day, Action: "create",
			OpDigest: "op", Preconditions: "pre",
			Status: timesheet.IntentConfirmed, RecordedAt: b.recorded, AttemptedAt: b.recorded,
			Observed: &timesheet.RemoteEntry{RemoteID: "r-" + day + "-" + b.block.BlockID, Date: day, ClassificationID: b.classID},
			Operation: timesheet.PlanOperation{
				LocalID: ivByBlock[b.block.BlockID], Date: day, ClassificationID: b.classID,
				SelectedChoice: &timesheet.ClassificationChoice{ID: b.classID, Name: b.name},
			},
		})
	}
	if err := store.CommitState(st); err != nil {
		t.Fatal(err)
	}
}

func historyFixture(t *testing.T) (*stubRemote, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	stub := &stubRemote{entries: map[string][]map[string]any{testDate: {}}, classifications: stubCatalogText}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	t.Setenv("SYNTHETIC_GRADION_TOKEN", "synthetic-token")
	return stub, writeTestConfigWithGradion(t, server.URL)
}

func past(id, start, end, desc, classID, name string) pastBlock {
	return pastBlock{block: timedBlock(id, start, end, desc), classID: classID, name: name, recorded: "2026-10-20T10:00:00Z"}
}

func stageToday(t *testing.T, cfg string, blocks ...timesheet.ReviewedBlock) {
	t.Helper()
	seedEntries(t, cfg, testDate, blocks)
	runStageCapture(t, []string{"--config", cfg, "--date", testDate})
}

func ranSteps(rep advanceReport) string {
	steps := make([]string, 0, len(rep.Ran))
	for _, r := range rep.Ran {
		steps = append(steps, r.Step+":"+r.Result)
	}
	return strings.Join(steps, ",")
}

func readSidecar(t *testing.T, cfg string) map[string]timesheet.ClassificationChoiceEntry {
	t.Helper()
	choices, err := timesheet.ReadClassificationChoices(statePathFor(cfg), testDate)
	if err != nil {
		t.Fatal(err)
	}
	return choices
}

func TestHistoryKey(t *testing.T) {
	cases := map[string]string{
		"Fix the bug. PRs: acme/api: #7":           "repo:acme/api",
		"Ship. PRs: acme/api: #1–#3; acme/web: #2": "",
		"  Daily   Standup. ":                      "title:daily standup",
		"daily standup":                            "title:daily standup",
		"PRs: #9 weird-label":                      "",
		"":                                         "",
	}
	for in, want := range cases {
		if got := historyKey(in); got != want {
			t.Errorf("historyKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHistoryClassifiesKnownIntervalsAndJudgesOnlyTheRest(t *testing.T) {
	_, cfg := historyFixture(t)
	seedSubmitted(t, cfg, "2026-10-19",
		past("w1", "09:00", "10:00", "Build the API. PRs: acme/api: #1", "cls-dev", "Development"),
		past("w2", "11:00", "12:00", "Daily standup", "cls-ops", "Operations"),
	)
	stageToday(t, cfg,
		timedBlock("t1", "09:00", "10:00", "Fix the API bug. PRs: acme/api: #7"),
		timedBlock("t2", "11:00", "12:00", "daily  STANDUP"),
		timedBlock("t3", "13:00", "14:00", "Something new"),
	)
	args := []string{"--config", cfg, "--date", testDate, "--do"}
	rep, err := advanceDoc(t, args...)
	if err != nil {
		t.Fatalf("next --do: %v", err)
	}
	if got := ranSteps(rep); got != "classify-history:ok,candidates:ok" {
		t.Fatalf("ran = %s, want history then candidates", got)
	}
	j := rep.Judgment
	if rep.State != stateJudgment || j == nil || len(j.Blocks) != 1 || j.Blocks[0].BlockID != "t3" {
		t.Fatalf("judgment = %+v, want only the unknown interval", j)
	}
	if !strings.Contains(j.Protocol, "2 interval(s) were classified from history") {
		t.Fatalf("protocol = %q, want the kept-from-history note", j.Protocol)
	}

	writeAnswerFile(t, j.AnswerFile, `{"ledger_digest":"`+j.LedgerDigest+`","selections":{"`+j.Blocks[0].IntervalID+
		`":{"catalog_id":"`+j.Candidates[0].CatalogID+`","classification_id":"cls-dev"}}}`)
	rep, err = advanceDoc(t, args...)
	if err != nil {
		t.Fatalf("next --do after the answer: %v (%+v)", err, rep.nextAction)
	}
	got := map[string]int{}
	for _, c := range readSidecar(t, cfg) {
		got[c.Source]++
	}
	if got[timesheet.SelectionSourceHistory] != 2 || got[timesheet.SelectionSourceJudgment] != 1 {
		t.Fatalf("sidecar sources = %v, want 2 history + 1 judgment", got)
	}

	out, _, err := captureOutput(t, func() error { return runStatus([]string{"--config", cfg, "--date", testDate}) })
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		Classifications []statusClassification `json:"classifications"`
	}
	if err := json.Unmarshal(out, &status); err != nil || len(status.Classifications) != 3 {
		t.Fatalf("status classifications = %s, %v", out, err)
	}
}

func TestHistoryCoveringEveryIntervalSkipsTheJudgment(t *testing.T) {
	stub, cfg := historyFixture(t)
	seedSubmitted(t, cfg, "2026-10-19", past("w1", "09:00", "10:00", "Daily standup", "cls-ops", "Operations"))
	stageToday(t, cfg, timedBlock("t1", "09:00", "10:00", "Daily standup"))
	rep, err := advanceDoc(t, "--config", cfg, "--date", testDate, "--do")
	if err != nil {
		t.Fatalf("next --do: %v (%+v)", err, rep.nextAction)
	}
	if got := ranSteps(rep); !strings.HasPrefix(got, "classify-history:ok,classify:ok,save:ok") {
		t.Fatalf("ran = %s, want history, classify, save with no judgment", got)
	}
	if classificationCalls(stub) != 1 {
		t.Fatalf("catalog reads = %d, want one live check and no candidates read", classificationCalls(stub))
	}
	for _, c := range readSidecar(t, cfg) {
		if c.Source != timesheet.SelectionSourceHistory || c.ClassificationID != "cls-ops" {
			t.Fatalf("sidecar = %+v, want the history selection", c)
		}
	}
}

func TestHistoryNeverPicksAmbiguousOrRetiredClassifications(t *testing.T) {
	_, cfg := historyFixture(t)
	seedSubmitted(t, cfg, "2026-10-19",
		past("w1", "09:00", "10:00", "Work. PRs: acme/api: #1", "cls-dev", "Development"),
		past("w2", "11:00", "12:00", "Old ritual", "cls-gone", "Retired"),
	)
	seedSubmitted(t, cfg, "2026-10-20", past("w1", "09:00", "10:00", "More. PRs: acme/api: #2", "cls-ops", "Operations"))
	stageToday(t, cfg,
		timedBlock("t1", "09:00", "10:00", "Today. PRs: acme/api: #3"),
		timedBlock("t2", "11:00", "12:00", "Old ritual"),
	)
	out, _, err := captureOutput(t, func() error {
		return runClassify([]string{"--config", cfg, "--date", testDate, "--from-history"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var receipt historyReceipt
	if err := json.Unmarshal(out, &receipt); err != nil {
		t.Fatal(err)
	}
	if len(receipt.Selections) != 0 || len(receipt.Skipped) != 2 {
		t.Fatalf("receipt = %+v, want nothing picked and two skips", receipt)
	}
	var all []string
	for _, r := range receipt.Skipped {
		all = append(all, r)
	}
	reasons := strings.Join(all, " | ")
	if !strings.Contains(reasons, "ambiguous") || !strings.Contains(reasons, "not in the current live catalog") {
		t.Fatalf("skip reasons = %v", receipt.Skipped)
	}
	if _, err := os.Stat(filepath.Join(testArtifactDir(t, cfg, testDate), "choices.json")); !os.IsNotExist(err) {
		t.Fatalf("an empty history wrote an answer: %v", err)
	}
}

func TestHistoryStepIsDerivedOnlyWithHistoryAndNoDefault(t *testing.T) {
	o := unclassifiedDay()
	if n := deriveDayNext(o); n.State != stateJudgment {
		t.Fatalf("fresh store = %+v, want today's judgment unchanged", n)
	}
	o.Workflow.SubmittedHistory = 3
	n := deriveDayNext(o)
	if n.Step != "classify" || stepAct(n) != "classify-history" {
		t.Fatalf("with history = %+v, want classify --from-history", n)
	}
	o.Workflow.ConfiguredDefaultPresent = true
	if n := deriveDayNext(o); stepAct(n) != "classify-default" {
		t.Fatalf("with a configured default = %+v, want the default to keep priority", n)
	}
}
