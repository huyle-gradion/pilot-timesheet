package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func testArtifactDir(t *testing.T, cfg, day string) string {
	t.Helper()
	dir, err := dayArtifactsDir(statePathFor(cfg), day)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDayArtifactsLiveInTheStore(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	root := t.TempDir()
	t.Chdir(root)
	want := filepath.Join(root, "days", testDate)
	for _, path := range []string{"state.json", "./nested/../state.json", filepath.Join(root, "state.json")} {
		got, err := dayArtifactsDir(path, testDate)
		if err != nil || got != want {
			t.Fatalf("day folder for %q = %q, %v; want %q", path, got, err, want)
		}
	}
}

func TestNextResumesAfterTempDirIsWiped(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	cfg := writeRunConfig(t)
	srv, _ := startFeed(t)
	env := writeRunEnvFile(t, srv.URL)
	args := []string{"--config", cfg, "--env-file", env, "--date", "2026-10-20"}
	executable(t, nextDoc(t, args...))
	before := nextDoc(t, args...)
	if before.State != stateJudgment {
		t.Fatalf("after run = %+v, want judgment", before)
	}
	if !strings.HasPrefix(before.Judgment.AnswerFile, filepath.Dir(statePathFor(cfg))) {
		t.Fatalf("answer file %q is not under the store", before.Judgment.AnswerFile)
	}
	t.Setenv("TMPDIR", t.TempDir())
	if after := nextDoc(t, args...); after.State != stateJudgment || after.Judgment.DraftDigest != before.Judgment.DraftDigest {
		t.Fatalf("after temp wipe = %+v, want the same judgment %+v", after, before)
	}
}

func TestNextReportsLegacyTempDayFolder(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	cfg := writeRunConfig(t)
	legacy, err := legacyDayArtifactsDir(statePathFor(cfg), "2026-10-20")
	if err != nil {
		t.Fatal(err)
	}
	writeAnswerFile(t, filepath.Join(legacy, "draft.json"), `{"date":"2026-10-20","blocks":[]}`)
	got := nextDoc(t, "--config", cfg, "--date", "2026-10-20")
	if got.State != stateStopped || !strings.Contains(got.Reason, legacy) || !strings.Contains(got.Reason, "mv ") {
		t.Fatalf("legacy day = %+v, want a stop naming %s and the move", got, legacy)
	}
	dir := testArtifactDir(t, cfg, "2026-10-20")
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(legacy, dir); err != nil {
		t.Fatal(err)
	}
	if got := nextDoc(t, "--config", cfg, "--date", "2026-10-20"); got.State == stateStopped && strings.Contains(got.Reason, "old temp location") {
		t.Fatalf("moved day still reported as legacy: %+v", got)
	}
}

func TestRunNamesUnwritableDayFolder(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	cfg := writeRunConfig(t)
	days := filepath.Join(filepath.Dir(statePathFor(cfg)), "days")
	if err := os.WriteFile(days, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := ensureDayArtifactsDir("run", filepath.Join(days, "2026-10-20"))
	if err == nil || !strings.Contains(err.Error(), filepath.Join(days, "2026-10-20")) || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("err = %v, want the path and the cause", err)
	}
}

func TestNextArtifactsAreIsolatedByStore(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	first, second := writeRunConfig(t), writeRunConfig(t)
	srv, _ := startFeed(t)
	env := writeRunEnvFile(t, srv.URL)
	args := func(cfg string) []string {
		return []string{"--config", cfg, "--env-file", env, "--date", "2026-10-20"}
	}
	writeAnswerFile(t, filepath.Join(os.TempDir(), "timesheet", "2026-10-20", "draft.json"),
		`{"date":"2026-10-20","blocks":[{"block_id":"legacy"}]}`)
	executable(t, nextDoc(t, args(first)...))
	judgment := nextDoc(t, args(first)...)
	if judgment.State != stateJudgment {
		t.Fatalf("first store = %+v, want judgment", judgment)
	}
	other := nextDoc(t, args(second)...)
	if other.State != stateAction || other.Step != "run" {
		t.Fatalf("second store imported first store's draft: %+v", other)
	}
	executable(t, other)
	other = nextDoc(t, args(second)...)
	if other.State != stateJudgment || other.Judgment.AnswerFile == judgment.Judgment.AnswerFile {
		t.Fatalf("stores share an answer file: %+v / %+v", judgment, other)
	}
	writeNextDescriptions(t, filepath.Join(filepath.Dir(judgment.Judgment.AnswerFile), "draft.json"),
		judgment.Judgment.AnswerFile, judgment.Judgment.DraftDigest)
	if got := nextDoc(t, args(second)...); got.State != stateJudgment {
		t.Fatalf("second store consumed first store's descriptions: %+v", got)
	}
}

func TestNextClassificationAnswersAreIsolatedByStore(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	first := writeTestConfigWithGradion(t, "http://127.0.0.1:1/mcp")
	second := writeTestConfigWithGradion(t, "http://127.0.0.1:1/mcp")
	for _, cfg := range []string{first, second} {
		seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
			timedBlock("work-1", "09:00", "10:00", "Build the bridge"),
		})
		runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	}
	judgment := nextJudgmentDoc(t, first, "cat-1", "--config", first, "--date", testDate)
	if judgment.State != stateJudgment || judgment.Judgment.Kind != judgmentClassifications {
		t.Fatalf("first store = %+v, want classification judgment", judgment)
	}
	writeAnswerFile(t, judgment.Judgment.AnswerFile,
		`{"ledger_digest":"`+judgment.Judgment.LedgerDigest+`","selections":{}}`)
	if got := nextDoc(t, "--config", first, "--date", testDate); got.Step != "classify" || got.State != stateAction {
		t.Fatalf("first store did not consume its own bound answer: %+v", got)
	}
	got := nextJudgmentDoc(t, second, "cat-1", "--config", second, "--date", testDate)
	if got.State != stateJudgment || got.Judgment.AnswerFile == judgment.Judgment.AnswerFile {
		t.Fatalf("second store consumed first store's choices: %+v", got)
	}
}

func savedReadyPlan(t *testing.T) (string, timesheet.PlanPreview) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	stub := &stubRemote{entries: map[string][]map[string]any{testDate: {}}}
	server := httptest.NewServer(stub)
	t.Cleanup(server.Close)
	t.Setenv("SYNTHETIC_GRADION_TOKEN", "synthetic-token")
	cfg := writeTestConfigWithGradion(t, server.URL)
	seedEntries(t, cfg, testDate, []timesheet.ReviewedBlock{
		timedBlock("work-1", "09:00", "10:00", "Build the bridge"),
	})
	seedCatalog(t, cfg, "cat-1", timesheet.ClassificationOption{
		ID: "cls-dev", Name: "Development", Code: "DEV", ActivityType: "engineering",
	})
	stage := runStageCapture(t, []string{"--config", cfg, "--date", testDate})
	choices := writeChoicesFile(t, cfg, `{"selections":{"`+stage.Staged[0].IntervalID+
		`":{"catalog_id":"cat-1","classification_id":"cls-dev"}}}`)
	runClassifyCapture(t, []string{"--config", cfg, "--date", testDate, "--file", choices})
	if _, _, err := captureOutput(t, func() error {
		return runSave([]string{"--config", cfg, "--date", testDate})
	}); err != nil {
		t.Fatal(err)
	}
	plan := runPlanCapture(t, []string{"--config", cfg, "--date", testDate})
	if plan.State != timesheet.PlanReady {
		t.Fatalf("fixture plan = %+v, want ready", plan)
	}
	return cfg, plan
}

func TestNextFinalizedLedgerMustRemainPresentAndNonempty(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "missing"}[missing], func(t *testing.T) {
			cfg, _ := savedReadyPlan(t)
			if missing {
				if err := os.Remove(timesheet.EntriesPath(statePathFor(cfg), testDate)); err != nil {
					t.Fatal(err)
				}
			} else {
				writeEntriesFile(t, cfg, testDate, nil)
			}
			got := nextDoc(t, "--config", cfg, "--date", testDate)
			if got.State != stateStopped || got.Human != nil || got.Action != nil {
				t.Fatalf("finalized day with unavailable ledger reached %+v, want stopped", got)
			}
		})
	}
}

func TestNextReplansAfterRealRevisionEvenWhenContentMatches(t *testing.T) {
	cfg, plan := savedReadyPlan(t)
	args := []string{"--config", cfg, "--date", testDate}
	if got := nextDoc(t, args...); got.State != stateHumanGates {
		t.Fatalf("current ready plan = %+v, want human gates", got)
	}
	before := readDurableDay(t, cfg, testDate).Revision
	runStageCapture(t, append(args, "--revise"))
	after := readDurableDay(t, cfg, testDate).Revision
	if after <= before || plan.SourceRevisions[testDate] != before {
		t.Fatalf("revision before/after/plan = %d/%d/%v", before, after, plan.SourceRevisions)
	}
	got := nextDoc(t, args...)
	if got.State != stateAction || got.Step != "plan" || got.Human != nil {
		t.Fatalf("stale ready plan after revision %d -> %d reached %+v, want fresh plan", before, after, got)
	}
	fresh := runPlanCapture(t, args)
	if fresh.PlanID == plan.PlanID || fresh.SourceRevisions[testDate] != after {
		t.Fatalf("fresh plan did not bind revision %d: %+v", after, fresh)
	}
}

func TestNextReplansAfterOvernightContributorRevision(t *testing.T) {
	cfg, _ := savedReadyPlan(t)
	origin := "2026-10-25"
	seedEntries(t, cfg, origin, []timesheet.ReviewedBlock{
		timedBlock("overnight", "23:00", "01:00", "Overnight work"),
	})
	args := []string{"--config", cfg, "--date", origin}
	stage := runStageCapture(t, args)
	choices := writeChoicesFile(t, cfg, `{"selections":{"`+stage.Staged[0].IntervalID+
		`":{"catalog_id":"cat-1","classification_id":"cls-dev"}}}`)
	runClassifyCapture(t, append(args, "--file", choices))
	if _, _, err := captureOutput(t, func() error { return runSave(args) }); err != nil {
		t.Fatal(err)
	}
	targetArgs := []string{"--config", cfg, "--date", testDate}
	plan := runPlanCapture(t, targetArgs)
	before := readDurableDay(t, cfg, origin).Revision
	if plan.State != timesheet.PlanReady || plan.SourceRevisions[origin] != before || len(plan.SourceRevisions) != 2 {
		t.Fatalf("plan must bind both origins: %+v", plan)
	}
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatal(err)
	}
	state := store.State()
	if len(state.Plans) < 2 {
		t.Fatalf("fixture must retain both plan generations: %+v", state.Plans)
	}
	for id, record := range state.Plans {
		record.CreatedAt = "2026-10-26T10:00:00Z"
		if id == plan.PlanID {
			record.CreatedAt = "2026-10-26T11:00:00Z"
		}
	}
	if err := store.CommitState(state); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if got := nextDoc(t, targetArgs...); got.State != stateHumanGates {
		t.Fatalf("current overnight plan = %+v, want human gates", got)
	}
	runStageCapture(t, append(args, "--revise"))
	after := readDurableDay(t, cfg, origin).Revision
	if after <= before {
		t.Fatalf("origin revision did not change: %d -> %d", before, after)
	}
	if got := nextDoc(t, targetArgs...); got.State != stateAction || got.Step != "plan" {
		t.Fatalf("stale overnight plan after origin revision %d -> %d reached %+v", before, after, got)
	}
}
