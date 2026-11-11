package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// overflowDay is the 2026-11-09 shape from issue #309: one repository whose
// events feed carries more attributable events than one topic lists in the
// draft. Every family finishes; the events family is windowed.
const overflowDay = "2026-11-09"

// startOverflowGithub serves a fake GitHub where every family is empty except
// /repos/o/r/events, which returns the given attributable events oldest
// first, 100 per page.
func startOverflowGithub(t *testing.T, events []string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/search/issues":
			fmt.Fprint(w, `{"total_count":0,"incomplete_results":false,"items":[]}`)
		case "/repos/o/r/events":
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if page < 1 {
				page = 1
			}
			lo, hi := (page-1)*100, page*100
			docs := []map[string]any{}
			for i := lo; i < hi && i < len(events); i++ {
				docs = append(docs, map[string]any{
					"id": "evt-" + strconv.Itoa(i), "type": "WatchEvent",
					"actor": map[string]any{"login": "user"}, "created_at": events[i],
				})
			}
			_ = json.NewEncoder(w).Encode(docs)
		default:
			fmt.Fprint(w, `[]`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// overflowEvents returns n event times on 2026-11-09 Asia/Ho_Chi_Minh: the
// first listed fifty at 00:10–00:59 local, then the rest at local times
// starting 02:07 and ending 09:40 when there are exactly six.
func overflowEvents(n int) []string {
	start := time.Date(2026, 11, 8, 17, 10, 0, 0, time.UTC) // 00:10 local
	tail := []string{"19:07", "20:00", "21:00", "23:00", "01:00", "02:40"}
	var out []string
	for i := 0; i < n; i++ {
		if i < 50 {
			out = append(out, start.Add(time.Duration(i)*time.Minute).Format(time.RFC3339))
			continue
		}
		k := i - 50
		if n-50 == len(tail) {
			day := "2026-11-08"
			if tail[k] < "17:00" {
				day = "2026-11-09"
			}
			out = append(out, day+"T"+tail[k]+":00Z")
			continue
		}
		out = append(out, time.Date(2026, 11, 8, 19, 7, 0, 0, time.UTC).Add(time.Duration(k)*time.Minute).Format(time.RFC3339))
	}
	return out
}

func writeOverflowGithubConfig(t *testing.T, apiBase string) (cfg, env string) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("GITHUB_TOKEN", "synthetic-token")
	dir := t.TempDir()
	cfg = filepath.Join(dir, "config.json")
	body := `{
  "timezone": "Asia/Ho_Chi_Minh",
  "state_path": "` + filepath.Join(dir, "state.json") + `",
  "sources": {"enabled": true, "github": {"enabled": true, "user": "user", "repositories": ["o/r"], "api_base": "` + apiBase + `"}}
}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	env = filepath.Join(dir, "env.local")
	if err := os.WriteFile(env, []byte("GITHUB_TOKEN=synthetic-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg, env
}

// overflowAttempt reads the day's only GitHub attempt and releases the store
// lock before the next invocation.
func overflowAttempt(t *testing.T, cfg string) timesheet.SourceAttempt {
	t.Helper()
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	ev := store.Evidence(overflowDay)
	if ev == nil || len(ev.Attempts) != 1 {
		t.Fatalf("evidence = %+v, want one attempt", ev)
	}
	return ev.Attempts[0]
}

func overflowTopicLines(t *testing.T, d *timesheet.Draft) []string {
	t.Helper()
	if d == nil {
		t.Fatal("no inline draft")
	}
	var lines []string
	for _, b := range d.Blocks {
		lines = append(lines, b.TopicCandidates...)
	}
	return lines
}

func TestNextSummarizesRetainedTopicOverflowWithoutAsking(t *testing.T) {
	srv := startOverflowGithub(t, overflowEvents(56))
	cfg, env := writeOverflowGithubConfig(t, srv.URL)

	rep, err := advanceDoc(t, "--config", cfg, "--env-file", env, "--date", overflowDay, "--do")
	if err != nil {
		t.Fatalf("next --do: %v (%+v)", err, rep.nextAction)
	}
	if rep.State == stateAskHuman || rep.Ask != nil {
		t.Fatalf("a fully retained overflow must not ask the human: %+v", rep.Ask)
	}
	if rep.State != stateJudgment || rep.Judgment == nil {
		t.Fatalf("report = %+v, want the descriptions judgment", rep.nextAction)
	}
	d := rep.Judgment.Draft
	if slices.Contains(d.Flags, "source_incomplete:github") {
		t.Fatalf("draft flags = %v, want the source complete", d.Flags)
	}
	want := "Other attributable activity (o/r) — overflow: +6 more items not listed (event 6), 02:07–09:40"
	if got := overflowTopicLines(t, d); !slices.Contains(got, want) {
		t.Fatalf("topic_candidates = %q, want the summary line %q", got, want)
	}
	for _, b := range d.Blocks {
		if b.Completeness["github"] != timesheet.StatusComplete {
			t.Fatalf("block completeness = %v, want github complete", b.Completeness)
		}
	}

	at := overflowAttempt(t, cfg)
	if at.Status != timesheet.StatusComplete || at.Acknowledged {
		t.Fatalf("attempt = %+v, want complete and unacknowledged", at)
	}
	if !strings.Contains(at.Detail, "6 attributable item(s) beyond the 50-item topic cap are summarized in the draft") {
		t.Fatalf("attempt detail %q must note the summarized overflow", at.Detail)
	}

	again, err := advanceDoc(t, "--config", cfg, "--env-file", env, "--date", overflowDay)
	if err != nil || again.Judgment == nil {
		t.Fatalf("second next: %v (%+v)", err, again.nextAction)
	}
	a, _ := json.Marshal(d)
	b, _ := json.Marshal(again.Judgment.Draft)
	if string(a) != string(b) {
		t.Fatalf("draft is not deterministic:\nfirst:  %s\nsecond: %s", a, b)
	}

	chat := &fakeChat{answer: describeAll}
	addJudge(t, cfg, map[string]any{"backend": "openai", "base_url": judgeServer(t, chat), "model": "m"})
	rep, err = advanceDoc(t, "--config", cfg, "--env-file", env, "--date", overflowDay, "--do", "--auto")
	if err != nil {
		t.Fatalf("next --do --auto: %v (%+v) ran %s", err, rep.nextAction, autoSteps(rep))
	}
	if rep.State == stateAskHuman || rep.Ask != nil {
		t.Fatalf("a fully retained overflow must not ask the human past the descriptions: %+v ran %s", rep.Ask, autoSteps(rep))
	}
	if !strings.Contains(autoSteps(rep), "judge:block_descriptions:ok,record:ok") {
		t.Fatalf("ran = %s, want the descriptions judged and recorded", autoSteps(rep))
	}
	if len(chat.prompts) == 0 || !strings.Contains(chat.prompts[0], "+6 more items not listed (event 6), 02:07–09:40") {
		t.Fatalf("the judge prompt must carry the overflow summary: %q", chat.prompts)
	}
}

func TestNextStillAsksWhenTopicOverflowIsLost(t *testing.T) {
	// 50 listed + 150 retained overflow + 10 beyond the per-topic ceiling.
	srv := startOverflowGithub(t, overflowEvents(210))
	cfg, env := writeOverflowGithubConfig(t, srv.URL)

	rep, err := advanceDoc(t, "--config", cfg, "--env-file", env, "--date", overflowDay, "--do")
	if err != nil || rep.Judgment == nil {
		t.Fatalf("next --do: %v (%+v)", err, rep.nextAction)
	}
	if d := rep.Judgment.Draft; !slices.Contains(d.Flags, "source_incomplete:github") {
		t.Fatalf("draft flags = %v, want github incomplete", d.Flags)
	}
	want := "Other attributable activity (o/r) — overflow: +150 more items not listed (event 150), 10 lost, 02:07–04:36"
	if got := overflowTopicLines(t, rep.Judgment.Draft); !slices.Contains(got, want) {
		t.Fatalf("topic_candidates = %q, want the summary line %q naming the lost count", got, want)
	}
	chat := &fakeChat{answer: describeAll}
	addJudge(t, cfg, map[string]any{"backend": "openai", "base_url": judgeServer(t, chat), "model": "m"})
	rep, err = advanceDoc(t, "--config", cfg, "--env-file", env, "--date", overflowDay, "--do", "--auto")
	if err != nil {
		t.Fatalf("next --do --auto: %v (%+v)", err, rep.nextAction)
	}
	if rep.State != stateAskHuman || rep.Ask == nil || rep.Ask.Kind != askSourceAck {
		t.Fatalf("report = %+v ran %s, want the source acknowledgement question", rep.nextAction, autoSteps(rep))
	}
	at := overflowAttempt(t, cfg)
	if at.Status != timesheet.StatusIncomplete {
		t.Fatalf("attempt = %+v, want incomplete", at)
	}
	if !strings.Contains(at.Detail, "10 beyond the overflow caps are not retained") {
		t.Fatalf("attempt detail %q must name the lost count", at.Detail)
	}
}

func TestNextExactlyFiftyItemsCarriesNoSummary(t *testing.T) {
	srv := startOverflowGithub(t, overflowEvents(50))
	cfg, env := writeOverflowGithubConfig(t, srv.URL)

	rep, err := advanceDoc(t, "--config", cfg, "--env-file", env, "--date", overflowDay, "--do")
	if err != nil || rep.Judgment == nil {
		t.Fatalf("next --do: %v (%+v)", err, rep.nextAction)
	}
	raw, _ := json.Marshal(rep.Judgment.Draft)
	if strings.Contains(string(raw), "overflow") || strings.Contains(string(raw), "source_incomplete") {
		t.Fatalf("a 50-item topic draft must carry no overflow marker: %s", raw)
	}
	if got := overflowTopicLines(t, rep.Judgment.Draft); !slices.Equal(got, []string{"Other attributable activity (o/r)"}) {
		t.Fatalf("topic_candidates = %q, want the bare title", got)
	}
}
