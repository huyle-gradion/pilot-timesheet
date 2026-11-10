package main

import (
	"encoding/json"
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

func TestEvalExportPairsInputsWithAcceptedAnswers(t *testing.T) {
	cfg, _ := savedReadyPlan(t)
	writeSyntheticDraft(t, cfg)
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
	r, g := evalExport(t, cfg, filepath.Join(t.TempDir(), "golden.json"))
	if len(g.Days) != 1 || g.Days[0].Descriptions != nil || g.Days[0].Classifications == nil {
		t.Fatalf("days = %+v, want classifications only", g.Days)
	}
	if len(r.Skipped) != 1 || r.Skipped[0].Part != "descriptions" || !strings.Contains(r.Skipped[0].Reason, "missing") {
		t.Fatalf("skipped = %+v, want the missing draft listed", r.Skipped)
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
