package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/llm"
	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

type fakeChat struct {
	mu      sync.Mutex
	calls   int
	prompts []string
	status  int
	answer  func(prompt string) string
}

func (f *fakeChat) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	f.calls++
	prompt := req.Messages[0].Content
	f.prompts = append(f.prompts, prompt)
	f.mu.Unlock()
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	content, _ := json.Marshal(f.answer(prompt))
	_, _ = io.WriteString(w, `{"choices":[{"message":{"content":`+string(content)+`}}],"usage":{"prompt_tokens":400,"completion_tokens":40}}`)
}

var blockIDRE = regexp.MustCompile(`"block_id":"([^"]+)"`)

func describeAll(prompt string) string {
	d := map[string]string{}
	for _, m := range blockIDRE.FindAllStringSubmatch(prompt, -1) {
		d[m[1]] = "Synthetic judged work for " + m[1]
	}
	out, _ := json.Marshal(map[string]any{"notes": "synthetic", "descriptions": d})
	return string(out)
}

func addJudge(t *testing.T, cfg string, judge map[string]any) {
	t.Helper()
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	doc["judge"] = judge
	out, _ := json.Marshal(doc)
	if err := os.WriteFile(cfg, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func judgeDoc(t *testing.T, args ...string) (judgeReceipt, error) {
	t.Helper()
	out, _, err := captureOutput(t, func() error { return runJudge(args) })
	var r judgeReceipt
	if len(out) > 0 {
		if jsonErr := json.Unmarshal(out, &r); jsonErr != nil {
			t.Fatalf("judge printed %s: %v", out, jsonErr)
		}
	}
	return r, err
}

func descriptionsDayWithJudge(t *testing.T, chat *fakeChat) (cfg string, args []string, j *nextJudgment) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	cfg = writeRunConfig(t)
	srv, _ := startFeed(t)
	env := writeRunEnvFile(t, srv.URL)
	llmSrv := httptest.NewServer(chat)
	t.Cleanup(llmSrv.Close)
	addJudge(t, cfg, map[string]any{"backend": "openai", "base_url": llmSrv.URL, "model": "synthetic-model"})
	args = []string{"--config", cfg, "--env-file", env, "--date", "2026-10-20"}
	rep, err := advanceDoc(t, append(args, "--do")...)
	if err != nil || rep.Judgment == nil || rep.Judgment.Kind != judgmentBlockDescriptions {
		t.Fatalf("fixture = %+v, %v", rep.nextAction, err)
	}
	return cfg, args, rep.Judgment
}

func TestJudgeAnswersDescriptionsAndNextDoConsumesIt(t *testing.T) {
	chat := &fakeChat{answer: describeAll}
	cfg, args, j := descriptionsDayWithJudge(t, chat)
	r, err := judgeDoc(t, args...)
	if err != nil {
		t.Fatalf("judge: %v", err)
	}
	if chat.calls != 1 || r.Cached || r.Usage.InputTokens != 400 || !r.Usage.Exact || r.AnswerFile != j.AnswerFile {
		t.Fatalf("receipt = %+v after %d call(s)", r, chat.calls)
	}
	if strings.Contains(chat.prompts[0], j.DraftDigest) {
		t.Fatal("the model saw the draft digest")
	}
	var answer map[string]any
	data, _ := os.ReadFile(j.AnswerFile)
	if json.Unmarshal(data, &answer) != nil || answer["draft_digest"] != j.DraftDigest {
		t.Fatalf("answer = %s, want the binary-filled digest", data)
	}

	rep, err := advanceDoc(t, append(args, "--do")...)
	if err != nil || len(rep.Ran) == 0 || rep.Ran[0].Step != "record" || rep.Ran[0].Result != "ok" {
		t.Fatalf("next --do after judge = %+v ran %+v, %v", rep.nextAction, rep.Ran, err)
	}

	usage := filepath.Join(testArtifactDir(t, cfg, "2026-10-20"), judgeUsageFile)
	if data, err := os.ReadFile(usage); err != nil || !strings.Contains(string(data), `"input_tokens":400`) {
		t.Fatalf("usage record = %s, %v", data, err)
	}
}

func TestJudgeReusesTheStoredResponseForAnUnchangedPrompt(t *testing.T) {
	chat := &fakeChat{answer: describeAll}
	_, args, j := descriptionsDayWithJudge(t, chat)
	if _, err := judgeDoc(t, args...); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(j.AnswerFile); err != nil {
		t.Fatal(err)
	}
	r, err := judgeDoc(t, args...)
	if err != nil || !r.Cached || chat.calls != 1 {
		t.Fatalf("second judge = %+v, %v after %d call(s); want the cached reply", r, err, chat.calls)
	}
	if _, err := os.Stat(j.AnswerFile); err != nil {
		t.Fatalf("the cached reply did not rewrite the answer: %v", err)
	}
}

func TestJudgeFailureIsOneRetryThenAResumableStop(t *testing.T) {
	chat := &fakeChat{status: http.StatusBadGateway}
	_, args, j := descriptionsDayWithJudge(t, chat)
	_, err := judgeDoc(t, args...)
	if err == nil || !strings.Contains(err.Error(), "after one retry") || chat.calls != 2 {
		t.Fatalf("err = %v after %d call(s)", err, chat.calls)
	}
	if _, statErr := os.Stat(j.AnswerFile); !os.IsNotExist(statErr) {
		t.Fatalf("a failed judge wrote %s", j.AnswerFile)
	}
	rep, _ := advanceDoc(t, append(args, "--do")...)
	if rep.State != stateJudgment || rep.Judgment.Kind != judgmentBlockDescriptions {
		t.Fatalf("after failure = %+v, want the same judgment for the agent path", rep.nextAction)
	}
}

func TestJudgeStopsAtTheDailyCap(t *testing.T) {
	chat := &fakeChat{answer: describeAll}
	cfg, args, _ := descriptionsDayWithJudge(t, chat)
	data, _ := os.ReadFile(cfg)
	var doc map[string]any
	_ = json.Unmarshal(data, &doc)
	addJudge(t, cfg, map[string]any{"backend": "openai", "base_url": doc["judge"].(map[string]any)["base_url"], "model": "m", "daily_token_cap": 100})
	writeAnswerFile(t, filepath.Join(testArtifactDir(t, cfg, "2026-10-20"), judgeUsageFile), `{"kind":"block_descriptions","usage":{"input_tokens":90,"output_tokens":20}}`+"\n")
	_, err := judgeDoc(t, args...)
	if err == nil || !strings.Contains(err.Error(), "daily token cap") || chat.calls != 0 {
		t.Fatalf("err = %v after %d call(s), want the cap stop", err, chat.calls)
	}
}

func TestJudgePermanentFailureIsOneCall(t *testing.T) {
	chat := &fakeChat{status: http.StatusUnauthorized}
	cfg, args, _ := descriptionsDayWithJudge(t, chat)
	_, err := judgeDoc(t, args...)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "retry") || chat.calls != 1 {
		t.Fatalf("err = %v after %d call(s), want one call and no retry", err, chat.calls)
	}
	data, _ := os.ReadFile(filepath.Join(testArtifactDir(t, cfg, "2026-10-20"), judgeUsageFile))
	if !strings.Contains(string(data), `"calls":1`) {
		t.Fatalf("usage ledger = %s, want the one failed call", data)
	}
}

func TestJudgeChecksTheCapBeforeTheRetry(t *testing.T) {
	chat := &fakeChat{answer: func(string) string { return `{"descriptions":{}}` }}
	cfg, args, _ := descriptionsDayWithJudge(t, chat)
	data, _ := os.ReadFile(cfg)
	var doc map[string]any
	_ = json.Unmarshal(data, &doc)
	addJudge(t, cfg, map[string]any{"backend": "openai", "base_url": doc["judge"].(map[string]any)["base_url"], "model": "m", "daily_token_cap": 600})
	_, err := judgeDoc(t, args...)
	if err == nil || !strings.HasPrefix(err.Error(), "judge: the daily token cap (600)") || !strings.Contains(err.Error(), "(440 spent)") || chat.calls != 1 {
		t.Fatalf("err = %v after %d call(s), want the cap to stop the retry", err, chat.calls)
	}
	if !strings.Contains(err.Error(), "no description for block(s)") {
		t.Fatalf("err = %v, want the first reply's rejection kept", err)
	}
	usage, _ := os.ReadFile(filepath.Join(testArtifactDir(t, cfg, "2026-10-20"), judgeUsageFile))
	if !strings.Contains(string(usage), `"input_tokens":400`) {
		t.Fatalf("usage ledger = %s, want the first call counted", usage)
	}
}

func TestJudgeIsOffWithoutConfiguration(t *testing.T) {
	cfg := writeTestConfig(t)
	_, err := judgeDoc(t, "--config", cfg, "--date", testDate)
	if err == nil || !strings.Contains(err.Error(), "no judge is configured") {
		t.Fatalf("err = %v", err)
	}
}

func TestJudgeClassifiesOnlyFromTheCandidates(t *testing.T) {
	stub := &stubRemote{entries: map[string][]map[string]any{testDate: {}}, classifications: stubCatalogText}
	cfg := candidatesFixture(t, stub, timedBlock("work-1", "09:00", "10:00", "Build the bridge"))
	ivRE := regexp.MustCompile(`"interval_id":"([^"]+)"`)
	chat := &fakeChat{answer: func(prompt string) string {
		m := ivRE.FindStringSubmatch(prompt)
		return `{"selections":{"` + m[1] + `":"cls-dev"}}`
	}}
	llmSrv := httptest.NewServer(chat)
	t.Cleanup(llmSrv.Close)
	addJudge(t, cfg, map[string]any{"backend": "openai", "base_url": llmSrv.URL, "model": "synthetic-model"})
	args := []string{"--config", cfg, "--date", testDate}
	rep, err := advanceDoc(t, append(args, "--do")...)
	if err != nil || rep.Judgment == nil || rep.Judgment.Kind != judgmentClassifications {
		t.Fatalf("fixture = %+v, %v", rep.nextAction, err)
	}
	r, err := judgeDoc(t, args...)
	if err != nil || !strings.Contains(r.Notice, "classified 1 of 1") {
		t.Fatalf("judge = %+v, %v", r, err)
	}
	if strings.Contains(chat.prompts[0], rep.Judgment.LedgerDigest) || strings.Contains(chat.prompts[0], rep.Judgment.Candidates[0].CatalogID) {
		t.Fatal("the model saw the ledger digest or a catalog id")
	}
	rep, err = advanceDoc(t, append(args, "--do")...)
	if err != nil || len(rep.Ran) == 0 || rep.Ran[0].Step != "classify" || rep.Ran[0].Result != "ok" {
		t.Fatalf("next --do after judge = %+v ran %+v, %v", rep.nextAction, rep.Ran, err)
	}
}

func TestJudgeCommandBackendNeverSeesPipelineSecrets(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "synthetic-secret")
	cfg := writeTestConfig(t)
	addJudge(t, cfg, map[string]any{"backend": "command", "command": []string{"sh", "-c", "true"}})
	c, err := timesheet.LoadConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := newJudgeBackend(c, "")
	if err != nil {
		t.Fatal(err)
	}
	cb, ok := b.(llm.CommandBackend)
	if !ok {
		t.Fatalf("backend = %T, want the command backend", b)
	}
	for _, kv := range cb.Env {
		if strings.HasPrefix(kv, "GITHUB_TOKEN=") || strings.Contains(kv, "synthetic-secret") {
			t.Fatalf("the harness environment carries %q", kv)
		}
	}
}

func TestJudgeReceiptNeverPrintsCachedUsage(t *testing.T) {
	out, err := json.Marshal(judgeReceipt{Cached: true, CachedUsage: llm.Usage{Calls: 1, InputTokens: 4321}})
	if err != nil || strings.Contains(string(out), "4321") {
		t.Fatalf("receipt = %s, %v; want the cached usage left out", out, err)
	}
}

func TestJudgeTokensSpentSkipsCachedRecords(t *testing.T) {
	dir := t.TempDir()
	writeAnswerFile(t, filepath.Join(dir, judgeUsageFile),
		`{"cached":true,"usage":{"input_tokens":500}}`+"\n"+`{"usage":{"input_tokens":30,"output_tokens":5}}`+"\n")
	if spent, err := judgeTokensSpent(dir); err != nil || spent != 35 {
		t.Fatalf("spent = %d, %v; want 35 from the non-cached record only", spent, err)
	}
}

type promptRecorder struct{ prompts []string }

func (b *promptRecorder) Identity() string { return "recorder" }

func (b *promptRecorder) Complete(_ context.Context, prompt string) (string, llm.Usage, error) {
	b.prompts = append(b.prompts, prompt)
	return "fresh", llm.Usage{Calls: 1}, nil
}

func TestAskCachedSkipsTheStoredReplyAfterARejection(t *testing.T) {
	dir := t.TempDir()
	p := llm.Prompt{Text: "describe", Key: strings.Repeat("a", 64)}
	if err := os.MkdirAll(filepath.Join(dir, judgeResponsesDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSONFile(filepath.Join(dir, judgeResponsesDir, "kind-"+p.Key[:16]+".json"), judgeResponse{Key: p.Key, Reply: "stale"}); err != nil {
		t.Fatal(err)
	}
	cfg := &timesheet.Config{Judge: &timesheet.JudgeConfig{DailyTokenCap: 10000}}
	backend := &promptRecorder{}
	identity := func(s string) (string, error) { return s, nil }
	got, err := askCached(context.Background(), cfg, dir, testDate, "kind", p, "synthetic rejection", backend, identity, time.Now(), &judgeReceipt{})
	if err != nil || got != "fresh" || len(backend.prompts) != 1 || !strings.Contains(backend.prompts[0], "synthetic rejection") {
		t.Fatalf("askCached = %q, %v after prompts %q; want a fresh call naming the rejection", got, err, backend.prompts)
	}
}

func TestNewJudgeBackendCommandEnvIsNeverNil(t *testing.T) {
	secrets := []string{"GITHUB_TOKEN", "GCAL_ICS_URL", timesheet.DefaultGradionTokenEnv,
		"SYNTHETIC_GRADION_CRED", "SYNTHETIC_JUDGE_CRED", "SYNTHETIC_REQUIRED_CRED"}
	for _, name := range secrets {
		t.Setenv(name, "synthetic-secret")
	}
	for _, backend := range []string{timesheet.JudgeBackendCommand, ""} {
		c := &timesheet.Config{
			Gradion:     &timesheet.GradionConfig{TokenEnv: "SYNTHETIC_GRADION_CRED"},
			RequiredEnv: []string{"SYNTHETIC_REQUIRED_CRED"},
			Judge: &timesheet.JudgeConfig{Backend: backend, Command: []string{"sh", "-c", "true"}, TimeoutSeconds: 1,
				APIKeyEnv: "SYNTHETIC_JUDGE_CRED"},
		}
		b, err := newJudgeBackend(c, "")
		if err != nil {
			t.Fatal(err)
		}
		cb, ok := b.(llm.CommandBackend)
		if !ok {
			t.Fatalf("backend %q = %T, want the command backend", backend, b)
		}
		if cb.Env == nil {
			t.Fatalf("backend %q has a nil Env and would inherit the parent environment", backend)
		}
		for _, kv := range cb.Env {
			for _, name := range secrets {
				if strings.HasPrefix(kv, name+"=") {
					t.Fatalf("backend %q environment carries %q", backend, kv)
				}
			}
		}
	}
}

func TestJudgeUsageFormatReachesTheCommandBackend(t *testing.T) {
	cfg := writeTestConfig(t)
	addJudge(t, cfg, map[string]any{"command": []string{"harness", "-p", "--output-format", "json"}, "usage_format": "claude-json"})
	c, err := timesheet.LoadConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := newJudgeBackend(c, "")
	if err != nil {
		t.Fatal(err)
	}
	if cb, ok := b.(llm.CommandBackend); !ok || cb.UsageFormat != llm.UsageFormatClaudeJSON {
		t.Fatalf("backend = %#v, want the command backend reading the claude-json envelope", b)
	}
	addJudge(t, cfg, map[string]any{"command": []string{"harness"}, "usage_format": "stream-json"})
	if _, err := timesheet.LoadConfig(cfg); err == nil || !strings.Contains(err.Error(), "judge.usage_format") {
		t.Fatalf("unknown usage_format: err = %v, want a config error at load", err)
	}
}
