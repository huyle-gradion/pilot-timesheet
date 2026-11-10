package llm

import (
	"context"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

var update = flag.Bool("update", false, "rewrite golden files")

func syntheticDraft() timesheet.Draft {
	return timesheet.Draft{Date: "2026-10-19", Blocks: []timesheet.DraftBlock{
		{BlockID: "b1", Kind: "work", Start: "09:00", End: "10:00", Title: "Synthetic bridge work",
			TopicCandidates: []string{"bridge: add the span check"}, PRs: []string{"#12 acme/api"}, PRsInline: "PRs: acme/api: #12"},
		{BlockID: "b2", Kind: "meeting", Start: "11:00", End: "11:30", Title: "Synthetic standup"},
	}}
}

type fakeBackend struct {
	replies []string
	errs    []error
	prompts []string
}

func (f *fakeBackend) Identity() string { return "fake" }

func (f *fakeBackend) Complete(_ context.Context, prompt string) (string, Usage, error) {
	i := len(f.prompts)
	f.prompts = append(f.prompts, prompt)
	var err error
	if i < len(f.errs) {
		err = f.errs[i]
	}
	reply := ""
	if i < len(f.replies) {
		reply = f.replies[i]
	}
	return reply, Usage{InputTokens: 10, OutputTokens: 5, Exact: true}, err
}

func TestOpenAIRequestBodyMatchesGolden(t *testing.T) {
	var body []byte
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"descriptions\":{\"b1\":\"Bridge\",\"b2\":\"Standup\"}}"}}],"usage":{"prompt_tokens":321,"completion_tokens":12}}`)
	}))
	defer srv.Close()
	b := OpenAIBackend{BaseURL: srv.URL + "/v1", Model: "synthetic-model", APIKey: "synthetic-key", Timeout: 5 * time.Second}
	p, err := DescribePrompt(syntheticDraft(), b)
	if err != nil {
		t.Fatal(err)
	}
	reply, usage, err := b.Complete(context.Background(), p.Text)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "Bridge") || usage != (Usage{Calls: 1, InputTokens: 321, OutputTokens: 12, Exact: true}) {
		t.Fatalf("reply/usage = %q / %+v", reply, usage)
	}
	if auth != "Bearer synthetic-key" {
		t.Fatalf("authorization = %q", auth)
	}
	golden := filepath.Join("testdata", "describe_request.golden.json")
	if *update {
		if err := os.WriteFile(golden, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("golden: %v (run with -update)", err)
	}
	if string(body) != string(want) {
		t.Fatalf("request body drifted from %s:\n%s", golden, body)
	}
	for _, leak := range []string{"draft_digest", "synthetic-key", "09:00"} {
		if strings.Contains(string(body), leak) {
			t.Fatalf("request body carries %q", leak)
		}
	}
}

func TestOpenAIErrorsNeverEchoTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"bad key synthetic-key"}`)
	}))
	defer srv.Close()
	_, _, err := OpenAIBackend{BaseURL: srv.URL, Model: "m", APIKey: "synthetic-key", Timeout: time.Second}.Complete(context.Background(), "p")
	if !errors.Is(err, ErrBackend) || strings.Contains(err.Error(), "synthetic-key") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
}

func TestCommandBackendStripsCredentialsAndBoundsTime(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "synthetic-secret")
	script := `cat >/dev/null; if [ -n "$GITHUB_TOKEN" ]; then echo LEAK; else echo '{"ok":true}'; fi`
	b := CommandBackend{Argv: []string{"sh", "-c", script}, Env: CommandEnv([]string{"GITHUB_TOKEN"}), Timeout: 5 * time.Second}
	reply, usage, err := b.Complete(context.Background(), "prompt text")
	if err != nil || strings.Contains(reply, "LEAK") || usage.Exact || usage.InputTokens == 0 {
		t.Fatalf("reply/usage/err = %q / %+v / %v", reply, usage, err)
	}
	slow := CommandBackend{Argv: []string{"sh", "-c", "sleep 5"}, Env: CommandEnv(nil), Timeout: 100 * time.Millisecond}
	if _, _, err := slow.Complete(context.Background(), ""); !errors.Is(err, ErrBackend) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("slow err = %v", err)
	}
	missing := CommandBackend{Argv: []string{"definitely-not-a-harness-241"}, Env: CommandEnv(nil), Timeout: time.Second}
	if _, _, err := missing.Complete(context.Background(), ""); !errors.Is(err, ErrBackend) {
		t.Fatalf("missing err = %v", err)
	}
}

func TestParseDescriptionsValidatesAndFitsTheLedgerLimit(t *testing.T) {
	d := syntheticDraft()
	if _, err := ParseDescriptions(d, `{"descriptions":{"b1":"x"}}`); !errors.Is(err, ErrInvalidReply) || !strings.Contains(err.Error(), "b2") {
		t.Fatalf("missing block err = %v", err)
	}
	if _, err := ParseDescriptions(d, `{"descriptions":{"b1":"x","b2":"y","b9":"z"}}`); !errors.Is(err, ErrInvalidReply) || !strings.Contains(err.Error(), "b9") {
		t.Fatalf("unknown block err = %v", err)
	}
	long := strings.Repeat("word ", 200)
	f, err := ParseDescriptions(d, "```json\n{\"notes\":\"n\",\"descriptions\":{\"b1\":\""+long+"\",\"b2\":\"Standup\"}}\n```")
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(f.Descriptions["b1"])) + 1 + len([]rune(d.Blocks[0].PRsInline)); n > maxDescriptionRunes {
		t.Fatalf("prose plus PRs list = %d runes, over the ledger limit", n)
	}
}

func TestParseSelectionsOnlyAcceptsTheIntervalsOwnSet(t *testing.T) {
	intervals := []Interval{{IntervalID: "iv-1", Description: "a", Set: "c1"}, {IntervalID: "iv-2", Description: "b"}}
	sets := []CandidateSet{{Ref: "c1", CatalogID: "cat-1", Options: []Option{{ID: "cls-dev", Label: "Development"}}}}
	picks, err := ParseSelections(intervals, sets, `{"selections":{"iv-1":"cls-dev"}}`)
	if err != nil || picks["iv-1"] != (Selection{CatalogID: "cat-1", ClassificationID: "cls-dev"}) {
		t.Fatalf("picks = %+v, %v", picks, err)
	}
	for _, bad := range []string{`{"selections":{"iv-1":"cls-other"}}`, `{"selections":{"iv-2":"cls-dev"}}`, `{"selections":{"iv-9":"cls-dev"}}`, `nope`} {
		if _, err := ParseSelections(intervals, sets, bad); !errors.Is(err, ErrInvalidReply) {
			t.Fatalf("%s: err = %v, want an invalid reply", bad, err)
		}
	}
}

func TestAskRetriesOnceWithACompactNote(t *testing.T) {
	d := syntheticDraft()
	parse := func(r string) (timesheet.DescriptionsFile, error) { return ParseDescriptions(d, r) }
	f := &fakeBackend{replies: []string{`{"descriptions":{"b1":"x"}}`, `{"descriptions":{"b1":"x","b2":"y"}}`}}
	p, _ := DescribePrompt(d, f)
	if _, _, usage, err := Ask(context.Background(), f, p, parse); err != nil || len(f.prompts) != 2 || usage.InputTokens != 20 {
		t.Fatalf("retry: err %v, calls %d, usage %+v", err, len(f.prompts), usage)
	}
	if !strings.Contains(f.prompts[1], "previous reply was rejected") {
		t.Fatalf("retry prompt lacks the note:\n%s", f.prompts[1])
	}
	g := &fakeBackend{errs: []error{ErrBackend, ErrBackend}}
	if _, _, _, err := Ask(context.Background(), g, p, parse); err == nil || len(g.prompts) != 2 {
		t.Fatalf("two failures: err %v, calls %d", err, len(g.prompts))
	}
}

func TestPromptKeyBindsBackendAndContent(t *testing.T) {
	d := syntheticDraft()
	a, _ := DescribePrompt(d, &fakeBackend{})
	b, _ := DescribePrompt(d, OpenAIBackend{BaseURL: "http://x", Model: "m"})
	d.Blocks[1].Title = "Changed"
	c, _ := DescribePrompt(d, &fakeBackend{})
	if a.Key == b.Key || a.Key == c.Key || !strings.HasPrefix(a.Text, "prompt: block_descriptions v1") {
		t.Fatalf("keys %s %s %s; text %q", a.Key, b.Key, c.Key, a.Text[:40])
	}
}
