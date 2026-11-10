package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func autoSteps(rep advanceReport) string {
	steps := make([]string, 0, len(rep.Ran))
	for _, r := range rep.Ran {
		steps = append(steps, r.Step+":"+r.Result)
	}
	return strings.Join(steps, ",")
}

func judgeServer(t *testing.T, chat *fakeChat) string {
	t.Helper()
	srv := httptest.NewServer(chat)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestNextDoAutoAnswersDescriptionsAndReportsUsage(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	cfg := writeRunConfig(t)
	feed, _ := startFeed(t)
	env := writeRunEnvFile(t, feed.URL)
	chat := &fakeChat{answer: describeAll}
	csv := filepath.Join(t.TempDir(), "tokens.csv")
	addJudge(t, cfg, map[string]any{"backend": "openai", "base_url": judgeServer(t, chat), "model": "m", "tokens_csv": csv})

	rep, err := advanceDoc(t, "--config", cfg, "--env-file", env, "--date", "2026-10-20", "--do", "--auto")
	if err != nil {
		t.Fatalf("next --do --auto: %v (%+v)", err, rep.nextAction)
	}
	if got := autoSteps(rep); !strings.HasPrefix(got, "run:ok,judge:block_descriptions:ok,record:ok") {
		t.Fatalf("ran = %s, want run, the judge and record in one invocation", got)
	}
	if rep.Usage == nil || rep.Usage.Calls != 1 || rep.Usage.InputTokens != 400 || rep.Usage.OutputTokens != 40 {
		t.Fatalf("usage = %+v, want one call's counts", rep.Usage)
	}
	data, err := os.ReadFile(csv)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || lines[0] != tokensCSVHeader || !strings.HasPrefix(lines[1], "2026-10-20,timesheet-auto:") || !strings.HasSuffix(lines[1], ",400,40,0,440") {
		t.Fatalf("tokens.csv = %q", data)
	}
}

func TestNextDoAutoReachesTheHumanGates(t *testing.T) {
	stub := &stubRemote{entries: map[string][]map[string]any{testDate: {}}, classifications: stubCatalogText}
	cfg := candidatesFixture(t, stub, timedBlock("work-1", "09:00", "10:00", "Build the bridge"))
	ivRE := regexp.MustCompile(`"interval_id":"([^"]+)"`)
	chat := &fakeChat{answer: func(prompt string) string {
		return `{"selections":{"` + ivRE.FindStringSubmatch(prompt)[1] + `":"cls-dev"}}`
	}}
	addJudge(t, cfg, map[string]any{"backend": "openai", "base_url": judgeServer(t, chat), "model": "m"})

	rep, err := advanceDoc(t, "--config", cfg, "--date", testDate, "--do", "--auto")
	if err != nil {
		t.Fatalf("next --do --auto: %v (%+v) ran %s", err, rep.nextAction, autoSteps(rep))
	}
	if rep.State != stateHumanGates || rep.Human == nil {
		t.Fatalf("state = %q (%s) ran %s, want human_gates", rep.State, rep.Reason, autoSteps(rep))
	}
	if !strings.Contains(autoSteps(rep), "judge:classifications:ok,classify:ok") || rep.Usage.Calls != 1 {
		t.Fatalf("ran = %s usage %+v", autoSteps(rep), rep.Usage)
	}
	for _, r := range rep.Ran {
		if r.Step == "approve" || r.Step == "submit" || strings.HasPrefix(r.Step, "resolve-write") {
			t.Fatalf("--auto ran the human gate %s", r.Step)
		}
	}
}

func TestNextDoAutoStopsResumablyWhenTheJudgeFails(t *testing.T) {
	chat := &fakeChat{status: http.StatusServiceUnavailable}
	_, args, _ := descriptionsDayWithJudge(t, chat)
	rep, err := advanceDoc(t, append(args, "--do", "--auto")...)
	if err == nil || rep.State != stateStopped || !strings.HasPrefix(rep.Reason, "judgment_failed: block_descriptions:") || chat.calls != 2 {
		t.Fatalf("report = %+v err %v after %d call(s)", rep.nextAction, err, chat.calls)
	}
	if rep.Usage == nil || rep.Usage.Calls != 2 {
		t.Fatalf("usage = %+v, want both failed calls counted", rep.Usage)
	}
	agent, _ := advanceDoc(t, append(args, "--do")...)
	if agent.State != stateJudgment || agent.Judgment.Kind != judgmentBlockDescriptions {
		t.Fatalf("agent path after failure = %+v", agent.nextAction)
	}
}

func TestNextDoAutoNeverAnswersAskHuman(t *testing.T) {
	cfg := incompleteSourceDay(t)
	chat := &fakeChat{answer: describeAll}
	addJudge(t, cfg, map[string]any{"backend": "openai", "base_url": judgeServer(t, chat), "model": "m"})
	rep, err := advanceDoc(t, "--config", cfg, "--date", testDate, "--do", "--auto")
	if err != nil || rep.State != stateAskHuman || chat.calls != 0 {
		t.Fatalf("report = %+v err %v after %d call(s), want ask_human untouched", rep.nextAction, err, chat.calls)
	}
}

func TestNextDoAutoStopsBeforeTheDailyCap(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	cfg := writeRunConfig(t)
	feed, _ := startFeed(t)
	env := writeRunEnvFile(t, feed.URL)
	chat := &fakeChat{answer: describeAll}
	addJudge(t, cfg, map[string]any{"backend": "openai", "base_url": judgeServer(t, chat), "model": "m", "daily_token_cap": 10})
	rep, err := advanceDoc(t, "--config", cfg, "--env-file", env, "--date", "2026-10-20", "--do", "--auto")
	if err == nil || !strings.Contains(rep.Reason, "daily token cap") || chat.calls != 0 {
		t.Fatalf("report = %+v err %v after %d call(s)", rep.nextAction, err, chat.calls)
	}
}

func TestNextAutoFlagsAreChecked(t *testing.T) {
	cfg := writeTestConfig(t)
	if _, err := advanceDoc(t, "--config", cfg, "--date", testDate, "--auto"); err == nil || !strings.Contains(err.Error(), "--auto requires --do") {
		t.Fatalf("--auto alone: %v", err)
	}
	if _, err := advanceDoc(t, "--config", cfg, "--date", testDate, "--do", "--auto"); err == nil || !strings.Contains(err.Error(), "judge section") {
		t.Fatalf("--auto without a judge: %v", err)
	}
}
