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

func evalExport(t *testing.T, cfg, out string) (evalExportReceipt, timesheet.GoldenDataset) {
	t.Helper()
	stdout, _, err := captureOutput(t, func() error {
		return runEval([]string{"export", "--config", cfg, "--out", out})
	})
	if err != nil {
		t.Fatalf("eval export: %v", err)
	}
	var r evalExportReceipt
	if err := json.Unmarshal(stdout, &r); err != nil {
		t.Fatalf("receipt: %v\n%s", err, stdout)
	}
	var g timesheet.GoldenDataset
	if err := readJSONFile(out, &g); err != nil {
		t.Fatal(err)
	}
	return r, g
}

func writeSyntheticDraft(t *testing.T, cfg string) {
	t.Helper()
	writeAnswerFile(t, filepath.Join(testArtifactDir(t, cfg, testDate), "draft.json"),
		`{"date":"`+testDate+`","blocks":[{"block_id":"work-1","kind":"work","start":"09:00","end":"10:00"}]}`)
}

// writeBoundCandidates writes the day's candidates.json bound to its staged
// ledger, as the candidates step does when next judges the day.
func writeBoundCandidates(t *testing.T, cfg string) string {
	t.Helper()
	blocks, digest, err := stagedDayBlocks(statePathFor(cfg), testDate)
	if err != nil {
		t.Fatal(err)
	}
	f := candidatesFile{LedgerDigest: digest, Intervals: map[string]string{}, Sets: []nextCandidateSet{{
		Ref: "c1", Query: "Build the bridge", CatalogID: "cat-1", Complete: true,
		Options: []nextCandidateOption{{ClassificationID: "cls-dev", Label: "Development", Code: "DEV"}},
	}}}
	for id := range blocks {
		f.Intervals[id] = "c1"
	}
	data, _ := json.Marshal(f)
	path := filepath.Join(testArtifactDir(t, cfg, testDate), "candidates.json")
	writeAnswerFile(t, path, string(data))
	return path
}

func TestEvalExportPairsInputsWithAcceptedAnswers(t *testing.T) {
	cfg, _ := savedReadyPlan(t)
	writeSyntheticDraft(t, cfg)
	writeBoundCandidates(t, cfg)
	out := filepath.Join(t.TempDir(), "golden.json")
	r, g := evalExport(t, cfg, out)
	if r.Days != 1 || len(g.Days) != 1 || r.File != out {
		t.Fatalf("receipt = %+v, want one day at %s", r, out)
	}
	day := g.Days[0]
	if day.Classifications == nil || len(day.Classifications.Input) != 1 {
		t.Fatalf("classifications = %+v", day.Classifications)
	}
	iv := day.Classifications.Input[0].IntervalID
	if c := day.Classifications; len(c.Candidates) != 1 || c.Candidates[0].CatalogID != "cat-1" || c.Input[0].Candidates != "c1" {
		t.Fatalf("candidates = %+v / %+v, want the recorded set named by the interval", c.Candidates, c.Input)
	}
	if day.Classifications.Truth[iv] != "cls-dev" || len(day.Classifications.RemoteTruth) != 0 {
		t.Fatalf("classification truth = %+v, want the saved local selection", day.Classifications)
	}
	if day.Descriptions == nil || day.Descriptions.Truth["work-1"] != "Build the bridge" {
		t.Fatalf("descriptions = %+v, want the recorded text", day.Descriptions)
	}

	score, _, err := captureOutput(t, func() error { return runEval([]string{"score", "--golden", out}) })
	if err != nil {
		t.Fatal(err)
	}
	var s timesheet.GoldenScore
	if err := json.Unmarshal(score, &s); err != nil || s.ClassificationExactRate != 1 || s.DescriptionCoverage != 1 || s.DescriptionSimilarity != 1 {
		t.Fatalf("self-check = %s, %v; want 100%%", score, err)
	}

	cand := filepath.Join(t.TempDir(), "candidate.json")
	writeAnswerFile(t, cand, `{"days":{"`+testDate+`":{"classifications":{"`+iv+`":"cls-ops"}}}}`)
	score, _, _ = captureOutput(t, func() error { return runEval([]string{"score", "--golden", out, "--candidate", cand}) })
	_ = json.Unmarshal(score, &s)
	if s.ClassificationExactRate != 0 || s.DescriptionCoverage != 0 || len(s.Misses) != 2 {
		t.Fatalf("wrong candidate = %+v, want zero rates and two misses", s)
	}
}

func TestEvalExportPrefersTheSubmittedRemoteVersion(t *testing.T) {
	cfg, _ := savedReadyPlan(t)
	writeSyntheticDraft(t, cfg)
	writeBoundCandidates(t, cfg)
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatal(err)
	}
	st := store.State()
	iv := st.Days[testDate].Confirmed[0].ID
	st.Intents = append(st.Intents, timesheet.WriteIntent{
		WriteRef: "w1", ExecutionID: "ex", ApprovalID: "ap", PlanID: "pl", Date: testDate, LocalID: iv,
		Action: "create", OpDigest: "op", Preconditions: "pre", Status: timesheet.IntentConfirmed,
		AttemptedAt: "2026-10-26T12:00:00Z", RecordedAt: "2026-10-26T12:00:00Z",
		Observed: &timesheet.RemoteEntry{RemoteID: "r1", Date: testDate, ClassificationID: "cls-ops", Description: "Edited remotely"},
	})
	if err := store.CommitState(st); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	_, g := evalExport(t, cfg, filepath.Join(t.TempDir(), "golden.json"))
	day := g.Days[0]
	if day.Classifications.Truth[iv] != "cls-ops" || day.Descriptions.Truth["work-1"] != "Edited remotely" {
		t.Fatalf("truth = %+v / %+v, want the remote version", day.Classifications.Truth, day.Descriptions.Truth)
	}
	if len(day.Classifications.RemoteTruth) != 1 || len(day.Descriptions.RemoteTruth) != 1 {
		t.Fatalf("remote markers = %+v / %+v", day.Classifications.RemoteTruth, day.Descriptions.RemoteTruth)
	}
}

func TestEvalExportSkipsMissingArtifactsWithAReason(t *testing.T) {
	cfg, _ := savedReadyPlan(t)
	writeBoundCandidates(t, cfg)
	r, g := evalExport(t, cfg, filepath.Join(t.TempDir(), "golden.json"))
	if len(g.Days) != 1 || g.Days[0].Descriptions != nil || g.Days[0].Classifications == nil {
		t.Fatalf("days = %+v, want classifications only", g.Days)
	}
	if len(r.Skipped) != 1 || r.Skipped[0].Part != "descriptions" || !strings.Contains(r.Skipped[0].Reason, "missing") {
		t.Fatalf("skipped = %+v, want the missing draft listed", r.Skipped)
	}
}

func TestEvalExportSkipsClassificationsWithoutBoundCandidates(t *testing.T) {
	cfg, _ := savedReadyPlan(t)
	writeSyntheticDraft(t, cfg)
	r, g := evalExport(t, cfg, filepath.Join(t.TempDir(), "golden.json"))
	if len(g.Days) != 1 || g.Days[0].Classifications != nil || g.Days[0].Descriptions == nil {
		t.Fatalf("days = %+v, want descriptions only", g.Days)
	}
	if len(r.Skipped) != 1 || r.Skipped[0].Part != "classifications" || !strings.Contains(r.Skipped[0].Reason, "candidates.json") {
		t.Fatalf("skipped = %+v, want the missing candidates named", r.Skipped)
	}

	// Candidates bound to other content are stale, not replayable.
	path := writeBoundCandidates(t, cfg)
	writeAnswerFile(t, path, `{"ledger_digest":"sha256:other","sets":[],"intervals":{}}`)
	if r, _ = evalExport(t, cfg, filepath.Join(t.TempDir(), "golden.json")); len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Reason, "stale") {
		t.Fatalf("skipped = %+v, want the stale candidates named", r.Skipped)
	}
}

func TestEvalExportLeavesHistoryDecidedIntervalsOutOfTheJudgment(t *testing.T) {
	cfg, _ := savedReadyPlan(t)
	writeSyntheticDraft(t, cfg)
	writeBoundCandidates(t, cfg)
	blocks, digest, err := stagedDayBlocks(statePathFor(cfg), testDate)
	if err != nil {
		t.Fatal(err)
	}
	h := historyFile{LedgerDigest: digest, Selections: map[string]historyPick{}}
	for id := range blocks {
		h.Selections[id] = historyPick{CatalogID: "cat-1", ClassificationID: "cls-dev", Key: "title:build the bridge"}
	}
	data, _ := json.Marshal(h)
	writeAnswerFile(t, filepath.Join(testArtifactDir(t, cfg, testDate), historyFileName), string(data))
	r, g := evalExport(t, cfg, filepath.Join(t.TempDir(), "golden.json"))
	if g.Days[0].Classifications != nil || len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Reason, "approved history") {
		t.Fatalf("days = %+v, skipped = %+v; want the history-decided day left out of the judgment", g.Days, r.Skipped)
	}
}

func TestEvalExportOfAnEmptyStoreIsAnEmptyDataset(t *testing.T) {
	cfg := writeTestConfig(t)
	r, g := evalExport(t, cfg, filepath.Join(t.TempDir(), "golden.json"))
	if r.Days != 0 || len(g.Days) != 0 || r.Note == "" {
		t.Fatalf("receipt = %+v, want an empty dataset with a note", r)
	}
}

func TestEvalExportRefusesAPathInsideARepository(t *testing.T) {
	cfg := writeTestConfig(t)
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(repo, "fixtures", "golden.json")
	err := runEval([]string{"export", "--config", cfg, "--out", out})
	if err == nil || !strings.Contains(err.Error(), "outside any repository") {
		t.Fatalf("err = %v, want the repository refusal", err)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatalf("a refused export wrote %s", out)
	}
}

func TestEvalScoreRefusesADatasetWithNothingToScore(t *testing.T) {
	for name, days := range map[string]string{
		"no days": `[]`,
		"no truth": `[{"date":"2026-10-01","descriptions":{"input":{"date":"2026-10-01","blocks":[]},"truth":{}},` +
			`"classifications":{"input":[],"candidates":[],"truth":{}}}]`,
	} {
		t.Run(name, func(t *testing.T) {
			golden := filepath.Join(t.TempDir(), "golden.json")
			writeAnswerFile(t, golden, fmt.Sprintf(`{"schema":%d,"days":%s}`, timesheet.GoldenSchema, days))
			stdout, _, err := captureOutput(t, func() error { return runEval([]string{"score", "--golden", golden}) })
			if err == nil || !strings.Contains(err.Error(), "nothing to score") || strings.Contains(err.Error(), "\n") {
				t.Fatalf("err = %v, want one line naming the empty dataset", err)
			}
			if len(stdout) != 0 {
				t.Fatalf("stdout = %s, want no rates printed", stdout)
			}
		})
	}
}

func TestEvalExportOfNoUsableDaysWarnsButKeepsTheFile(t *testing.T) {
	cfg, _ := savedReadyPlan(t) // a saved day, but no draft and no candidates
	out := filepath.Join(t.TempDir(), "golden.json")
	_, stderr, err := captureOutput(t, func() error {
		return runEval([]string{"export", "--config", cfg, "--out", out})
	})
	if err != nil {
		t.Fatalf("eval export: %v", err)
	}
	var g timesheet.GoldenDataset
	if err := readJSONFile(out, &g); err != nil || len(g.Days) != 0 || len(g.Skipped) == 0 {
		t.Fatalf("dataset = %+v, %v; want the file written with its skip reasons", g, err)
	}
	warnings := 0
	for _, line := range strings.Split(string(stderr), "\n") {
		if strings.Contains(line, "exported 0 days") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatalf("stderr = %q, want one warning line", stderr)
	}
}
