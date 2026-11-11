package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

const (
	evalDayA = "2026-10-01"
	evalDayB = "2026-10-02"
)

var intervalIDRE = regexp.MustCompile(`"interval_id":"([^"]+)"`)

// classifyOrDescribe answers a classifications prompt by picking cls-dev for
// every listed interval, and a descriptions prompt with describeAll.
func classifyOrDescribe(prompt string) string {
	if !strings.Contains(prompt, "prompt: classifications") {
		return describeAll(prompt)
	}
	sel := map[string]string{}
	for _, m := range intervalIDRE.FindAllStringSubmatch(prompt, -1) {
		sel[m[1]] = "cls-dev"
	}
	out, _ := json.Marshal(map[string]any{"selections": sel})
	return string(out)
}

// syntheticEvalGolden is a two-day dataset: day A has both judgments (one
// block with an inline PRs list), day B descriptions only.
func syntheticEvalGolden(t *testing.T) string {
	t.Helper()
	g := timesheet.GoldenDataset{
		Schema: timesheet.GoldenSchema, ExportedAt: "2026-11-11T00:00:00Z",
		Days: []timesheet.GoldenDay{
			{
				Date: evalDayA,
				Descriptions: &timesheet.GoldenDescriptions{
					Input: timesheet.Draft{Date: evalDayA, Blocks: []timesheet.DraftBlock{
						{BlockID: "work-1", Kind: "work", Start: "09:00", End: "10:00", Title: "Bridge", PRsInline: "PRs: acme/bridge: #7 Bridge"},
					}},
					Truth: map[string]string{"work-1": "Synthetic judged work for work-1 PRs: acme/bridge: #7 Bridge"},
				},
				Classifications: &timesheet.GoldenClassifications{
					Input: []timesheet.GoldenBlock{{IntervalID: "iv-a", BlockID: "work-1", Start: "09:00", End: "10:00", Description: "Build the bridge", Candidates: "c1"}},
					Candidates: []timesheet.GoldenCandidateSet{{Ref: "c1", CatalogID: "cat-1", Options: []timesheet.GoldenOption{
						{ClassificationID: "cls-dev", Label: "Development"}, {ClassificationID: "cls-ops", Label: "Operations"},
					}}},
					Truth: map[string]string{"iv-a": "cls-dev"},
				},
			},
			{
				Date: evalDayB,
				Descriptions: &timesheet.GoldenDescriptions{
					Input: timesheet.Draft{Date: evalDayB, Blocks: []timesheet.DraftBlock{{BlockID: "work-2", Kind: "work", Title: "Docs"}}},
					Truth: map[string]string{"work-2": "Synthetic judged work for work-2"},
				},
			},
		},
		Skipped: []timesheet.GoldenSkip{},
	}
	path := filepath.Join(t.TempDir(), "golden.json")
	data, _ := json.Marshal(g)
	writeAnswerFile(t, path, string(data))
	return path
}

// evalJudgeConfig is a judge-configured test configuration whose backend is
// the fake chat endpoint; HOME is isolated so no real env file is read.
func evalJudgeConfig(t *testing.T, chat *fakeChat, extra map[string]any) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	cfg := writeTestConfig(t)
	srv := httptest.NewServer(chat)
	t.Cleanup(srv.Close)
	judge := map[string]any{"backend": "openai", "base_url": srv.URL, "model": "synthetic-model"}
	for k, v := range extra {
		judge[k] = v
	}
	addJudge(t, cfg, judge)
	return cfg
}

func evalRunDoc(t *testing.T, args ...string) (evalRunReceipt, error) {
	t.Helper()
	out, _, err := captureOutput(t, func() error { return runEval(append([]string{"run"}, args...)) })
	var r evalRunReceipt
	if len(out) > 0 {
		if jsonErr := json.Unmarshal(out, &r); jsonErr != nil {
			t.Fatalf("eval run printed %s: %v", out, jsonErr)
		}
	}
	return r, err
}

func evalScoreDoc(t *testing.T, golden, candidate string) timesheet.GoldenScore {
	t.Helper()
	out, _, err := captureOutput(t, func() error {
		return runEval([]string{"score", "--golden", golden, "--candidate", candidate})
	})
	if err != nil {
		t.Fatalf("eval score: %v", err)
	}
	var s timesheet.GoldenScore
	if err := json.Unmarshal(out, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// treeSnapshot lists every file under dir with its size and mtime.
func treeSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		snap[path] = info.ModTime().String() + "/" + info.Mode().String()
		if !d.IsDir() {
			data, _ := os.ReadFile(path)
			snap[path] += "/" + string(data)
		}
		return nil
	})
	return snap
}

func TestEvalRunWritesACandidateThatScoreAccepts(t *testing.T) {
	chat := &fakeChat{answer: classifyOrDescribe}
	cfg := evalJudgeConfig(t, chat, nil)
	golden := syntheticEvalGolden(t)
	out := filepath.Join(t.TempDir(), "candidate.json")
	before := treeSnapshot(t, filepath.Dir(cfg))

	r, err := evalRunDoc(t, "--config", cfg, "--golden", golden, "--out", out)
	if err != nil {
		t.Fatalf("eval run: %v", err)
	}
	if chat.calls != 3 || r.DaysRun != 2 || len(r.Skipped) != 0 || r.File != out {
		t.Fatalf("receipt = %+v after %d call(s), want 2 days in 3 calls", r, chat.calls)
	}
	if r.Usage.Calls != 3 || r.Usage.Total() != 3*440 || !r.Usage.Exact || r.DailyTokenCap != 10000 {
		t.Fatalf("usage = %+v cap %d, want 3 exact calls of 440 tokens under the default cap", r.Usage, r.DailyTokenCap)
	}
	if len(r.Days) != 2 || r.Days[0].Date != evalDayA || r.Days[0].Tokens != 880 || r.Days[1].Tokens != 440 || r.MaxDayTokens != 880 {
		t.Fatalf("per-day tokens = %+v (max %d)", r.Days, r.MaxDayTokens)
	}

	s := evalScoreDoc(t, golden, out)
	if s.ClassificationExactRate != 1 || s.DescriptionCoverage != 1 || s.DescriptionSimilarity != 1 {
		t.Fatalf("score = %+v, want the judged answers (record's PRs suffix included) to match", s)
	}
	if after := treeSnapshot(t, filepath.Dir(cfg)); len(after) != len(before) {
		t.Fatalf("eval run wrote into the configuration/store folder: %d -> %d entries", len(before), len(after))
	} else {
		for k, v := range before {
			if after[k] != v {
				t.Fatalf("eval run changed %s", k)
			}
		}
	}

	// A second run reuses the cached replies: no new call, but each day still
	// counts what its replies cost, so the per-day figures do not shrink.
	r2, err := evalRunDoc(t, "--config", cfg, "--golden", golden, "--out", out)
	if err != nil || chat.calls != 3 || r2.Usage.Total() != 0 || r2.FreshTokens != 0 || len(r2.Days[0].Cached) != 2 || r2.Note == "" {
		t.Fatalf("second run = %+v, %v after %d call(s), want every part cached", r2, err, chat.calls)
	}
	if r2.Days[0].Tokens != 880 || r2.Days[0].FreshTokens != 0 || r2.MaxDayTokens != 880 || r2.Tokens != 3*440 {
		t.Fatalf("second run tokens = %+v (max %d, total %d), want the cached cost counted", r2.Days, r2.MaxDayTokens, r2.Tokens)
	}
}

func TestEvalRunRecordsAnUnparseableReplyAsAMissingPart(t *testing.T) {
	chat := &fakeChat{answer: func(prompt string) string {
		if strings.Contains(prompt, "prompt: classifications") {
			return classifyOrDescribe(prompt)
		}
		return "I could not decide."
	}}
	cfg := evalJudgeConfig(t, chat, nil)
	golden := syntheticEvalGolden(t)
	out := filepath.Join(t.TempDir(), "candidate.json")
	r, err := evalRunDoc(t, "--config", cfg, "--golden", golden, "--out", out)
	if err != nil {
		t.Fatalf("eval run: %v", err)
	}
	if len(r.Skipped) != 2 || r.Skipped[0].Part != "descriptions" || !strings.Contains(r.Skipped[0].Reason, "no JSON object") {
		t.Fatalf("skipped = %+v, want both description parts with the parse reason", r.Skipped)
	}
	if r.DaysRun != 1 || r.Usage.Calls != 5 {
		t.Fatalf("receipt = %+v, want day A run and the retried calls counted", r)
	}
	s := evalScoreDoc(t, golden, out)
	if s.DescriptionCoverage != 0 || s.DescriptionAnswered != 0 || s.ClassificationExactRate != 1 {
		t.Fatalf("score = %+v, want unanswered descriptions to score as missing", s)
	}
}

func TestEvalRunStopsAtTheDailyCapAndKeepsThePartialCandidate(t *testing.T) {
	chat := &fakeChat{answer: classifyOrDescribe}
	cfg := evalJudgeConfig(t, chat, map[string]any{"daily_token_cap": 500})
	golden := syntheticEvalGolden(t)
	out := filepath.Join(t.TempDir(), "candidate.json")
	r, err := evalRunDoc(t, "--config", cfg, "--golden", golden, "--out", out)
	if err == nil || !strings.Contains(err.Error(), "per-day token cap (500)") || strings.Contains(err.Error(), "agent path") {
		t.Fatalf("err = %v, want the eval cap stop", err)
	}
	if chat.calls != 1 || r.Stopped == "" || r.DailyTokenCap != 500 {
		t.Fatalf("receipt = %+v after %d call(s), want a stop after the first part", r, chat.calls)
	}
	var c timesheet.GoldenCandidate
	if err := readJSONFile(out, &c); err != nil {
		t.Fatalf("the partial candidate is missing: %v", err)
	}
	if got := c.Days[evalDayA].Descriptions["work-1"]; got == "" || len(c.Days[evalDayA].Classifications) != 0 || len(c.Days) != 1 {
		t.Fatalf("partial candidate = %+v, want day A descriptions only", c)
	}
	parts := map[string]bool{}
	for _, s := range r.Skipped {
		parts[s.Date+"/"+s.Part] = true
	}
	if !parts[evalDayA+"/classifications"] || !parts[evalDayB+"/day"] {
		t.Fatalf("skipped = %+v, want the capped part and the day not reached", r.Skipped)
	}

	// A rerun takes descriptions from the cache but still counts their 440
	// tokens, so the day hits the cap again instead of passing.
	r2, err := evalRunDoc(t, "--config", cfg, "--golden", golden, "--out", out)
	if err == nil || chat.calls != 1 || r2.Days[0].Tokens != 440 || r2.Days[0].FreshTokens != 0 || r2.Stopped == "" {
		t.Fatalf("rerun = %+v, %v after %d call(s), want the cached cost to hit the cap again", r2, err, chat.calls)
	}
}

func TestEvalRunStopsOnAFailingBackend(t *testing.T) {
	chat := &fakeChat{status: 401}
	cfg := evalJudgeConfig(t, chat, nil)
	golden := syntheticEvalGolden(t)
	out := filepath.Join(t.TempDir(), "candidate.json")
	r, err := evalRunDoc(t, "--config", cfg, "--golden", golden, "--out", out)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("err = %v, want the backend stop", err)
	}
	if chat.calls != 2 || r.Stopped == "" || len(r.Skipped) != 2 || r.Skipped[1].Part != "day" {
		t.Fatalf("receipt = %+v after %d call(s), want a stop at the first part", r, chat.calls)
	}
	var c timesheet.GoldenCandidate
	if err := readJSONFile(out, &c); err != nil || len(c.Days) != 0 {
		t.Fatalf("candidate = %+v, %v; want the empty partial candidate written", c, err)
	}
}

func TestEvalRunRefusesAnOutPathThatIsTheDataset(t *testing.T) {
	chat := &fakeChat{answer: classifyOrDescribe}
	cfg := evalJudgeConfig(t, chat, nil)
	golden := syntheticEvalGolden(t)
	before, _ := os.ReadFile(golden)
	rel, err := filepath.Rel(mustGetwd(t), golden)
	if err != nil {
		rel = golden
	}
	_, err = evalRunDoc(t, "--config", cfg, "--golden", golden, "--out", rel)
	if err == nil || !strings.Contains(err.Error(), "would overwrite the dataset") || chat.calls != 0 {
		t.Fatalf("err = %v after %d call(s), want the overwrite refusal", err, chat.calls)
	}
	if after, _ := os.ReadFile(golden); string(after) != string(before) {
		t.Fatal("the dataset was changed")
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func TestEvalRunRefusesWithoutAJudgeSection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := writeTestConfig(t)
	out := filepath.Join(t.TempDir(), "candidate.json")
	_, err := evalRunDoc(t, "--config", cfg, "--golden", syntheticEvalGolden(t), "--out", out)
	if err == nil || !strings.Contains(err.Error(), "needs a judge section in the configuration (ADR 0010)") {
		t.Fatalf("err = %v, want the judge-section refusal", err)
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatal("a refused run wrote a candidate")
	}
}

func TestEvalRunRefusesAnOutPathInsideARepository(t *testing.T) {
	chat := &fakeChat{answer: classifyOrDescribe}
	cfg := evalJudgeConfig(t, chat, nil)
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(repo, "evals", "candidate.json")
	_, err := evalRunDoc(t, "--config", cfg, "--golden", syntheticEvalGolden(t), "--out", out)
	if err == nil || !strings.Contains(err.Error(), "outside any repository") || chat.calls != 0 {
		t.Fatalf("err = %v after %d call(s), want the repository refusal before any call", err, chat.calls)
	}
	if _, statErr := os.Stat(filepath.Join(repo, "evals")); !os.IsNotExist(statErr) {
		t.Fatal("a refused run wrote into the repository")
	}
}

func TestEvalExportRunScoreRoundTrip(t *testing.T) {
	cfg, _ := savedReadyPlan(t)
	writeSyntheticDraft(t, cfg)
	writeBoundCandidates(t, cfg)
	chat := &fakeChat{answer: classifyOrDescribe}
	srv := httptest.NewServer(chat)
	t.Cleanup(srv.Close)
	addJudge(t, cfg, map[string]any{"backend": "openai", "base_url": srv.URL, "model": "synthetic-model"})
	dir := t.TempDir()
	golden, out := filepath.Join(dir, "golden.json"), filepath.Join(dir, "candidate.json")
	evalExport(t, cfg, golden)
	r, err := evalRunDoc(t, "--config", cfg, "--golden", golden)
	if err != nil || r.File != out || r.DaysRun != 1 || chat.calls != 2 {
		t.Fatalf("receipt = %+v, %v after %d call(s)", r, err, chat.calls)
	}
	if !strings.Contains(chat.prompts[1], `"set":"c1"`) || !strings.Contains(chat.prompts[1], "cls-dev") {
		t.Fatalf("classifications prompt lacks the recorded candidates:\n%s", chat.prompts[1])
	}
	if s := evalScoreDoc(t, golden, out); s.ClassificationExactRate != 1 || s.DescriptionCoverage != 1 {
		t.Fatalf("score = %+v", s)
	}
}

func TestEvalRunRefusesAnOlderDatasetSchema(t *testing.T) {
	chat := &fakeChat{answer: classifyOrDescribe}
	cfg := evalJudgeConfig(t, chat, nil)
	golden := filepath.Join(t.TempDir(), "golden.json")
	writeAnswerFile(t, golden, `{"schema":1,"days":[]}`)
	_, err := evalRunDoc(t, "--config", cfg, "--golden", golden, "--out", filepath.Join(t.TempDir(), "c.json"))
	if err == nil || !strings.Contains(err.Error(), "export it again") {
		t.Fatalf("err = %v, want the schema refusal", err)
	}
}

func TestEvalRunRefusesADatasetWithNothingToReplay(t *testing.T) {
	for name, days := range map[string]string{
		"no days": `[]`,
		// Every part would be skipped before a judge call.
		"empty parts": `[{"date":"` + evalDayA + `","descriptions":{"input":{"date":"` + evalDayA + `","blocks":[]},"truth":{}}},` +
			`{"date":"` + evalDayB + `","classifications":{"input":[{"interval_id":"iv-b","candidates":"c1"}],"candidates":[],"truth":{"iv-b":"cls-dev"}}}]`,
	} {
		t.Run(name, func(t *testing.T) {
			chat := &fakeChat{answer: classifyOrDescribe}
			cfg := evalJudgeConfig(t, chat, nil)
			golden := filepath.Join(t.TempDir(), "golden.json")
			writeAnswerFile(t, golden, fmt.Sprintf(`{"schema":%d,"days":%s}`, timesheet.GoldenSchema, days))
			out := filepath.Join(t.TempDir(), "candidate.json")
			stdout, _, err := captureOutput(t, func() error {
				return runEval([]string{"run", "--config", cfg, "--golden", golden, "--out", out})
			})
			if err == nil || !strings.Contains(err.Error(), "no replayable days") || strings.Contains(err.Error(), "\n") {
				t.Fatalf("err = %v, want one line naming the empty dataset", err)
			}
			if chat.calls != 0 || len(stdout) != 0 {
				t.Fatalf("%d call(s), stdout %s; want no judge call and no receipt", chat.calls, stdout)
			}
			if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
				t.Fatal("a refused run wrote a candidate")
			}
		})
	}
}
