package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

const validDescriptions = `{"descriptions":{"b1":"x","b2":"y"}}`

// statusServer answers each request with the next scripted status (and
// Retry-After header); a 200 carries a valid descriptions reply.
type statusServer struct {
	mu      sync.Mutex
	steps   []statusStep
	prompts []string
}

type statusStep struct {
	status     int
	retryAfter string
}

func (s *statusServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	i := len(s.prompts)
	s.prompts = append(s.prompts, string(body))
	s.mu.Unlock()
	step := statusStep{status: http.StatusOK}
	if i < len(s.steps) {
		step = s.steps[i]
	}
	if step.retryAfter != "" {
		w.Header().Set("Retry-After", step.retryAfter)
	}
	if step.status != http.StatusOK {
		w.WriteHeader(step.status)
		return
	}
	_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"descriptions\":{\"b1\":\"x\",\"b2\":\"y\"}}"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
}

type askFixture struct {
	backend Backend
	prompt  Prompt
	parse   func(string) (timesheet.DescriptionsFile, error)
	waits   []time.Duration
}

func newAskFixture(t *testing.T, b Backend) *askFixture {
	t.Helper()
	d := syntheticDraft()
	p, err := DescribePrompt(d, b)
	if err != nil {
		t.Fatal(err)
	}
	return &askFixture{backend: b, prompt: p, parse: func(r string) (timesheet.DescriptionsFile, error) { return ParseDescriptions(d, r) }}
}

func (f *askFixture) policy() RetryPolicy {
	return RetryPolicy{Wait: func(_ context.Context, d time.Duration) error {
		f.waits = append(f.waits, d)
		return nil
	}}
}

func (f *askFixture) ask(policy RetryPolicy) (Usage, error) {
	_, _, usage, err := Ask(context.Background(), f.backend, f.prompt, f.parse, policy)
	return usage, err
}

func openAIAt(t *testing.T, s *statusServer) OpenAIBackend {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return OpenAIBackend{BaseURL: srv.URL, Model: "m", Timeout: 2 * time.Second}
}

func TestAskNotesOnlyAnInvalidReply(t *testing.T) {
	invalid := &fakeBackend{replies: []string{`{"descriptions":{"b1":"x"}}`, validDescriptions}}
	f := newAskFixture(t, invalid)
	if _, err := f.ask(f.policy()); err != nil || len(invalid.prompts) != 2 {
		t.Fatalf("invalid reply: err %v after %d call(s)", err, len(invalid.prompts))
	}
	if !strings.Contains(invalid.prompts[1], "Your previous reply was rejected: the judge reply is not a valid answer") || len(f.waits) != 0 {
		t.Fatalf("retry after an invalid reply: waits %v, prompt:\n%s", f.waits, invalid.prompts[1])
	}

	failing := &fakeBackend{errs: []error{transient("the endpoint is unreachable")}, replies: []string{"", validDescriptions}}
	g := newAskFixture(t, failing)
	if _, err := g.ask(g.policy()); err != nil || len(failing.prompts) != 2 {
		t.Fatalf("transient failure: err %v after %d call(s)", err, len(failing.prompts))
	}
	if failing.prompts[1] != g.prompt.Text {
		t.Fatalf("retry after a backend failure carries a note:\n%s", failing.prompts[1])
	}
}

func TestAskNeverRetriesAPermanentBackendFailure(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		s := &statusServer{steps: []statusStep{{status: code}}}
		f := newAskFixture(t, openAIAt(t, s))
		usage, err := f.ask(f.policy())
		if !errors.Is(err, ErrBackend) || len(s.prompts) != 1 || usage.Calls != 1 || len(f.waits) != 0 {
			t.Fatalf("HTTP %d: err %v after %d call(s), usage %+v, waits %v; want one call", code, err, len(s.prompts), usage, f.waits)
		}
	}

	notExecutable := filepath.Join(t.TempDir(), "harness")
	if err := os.WriteFile(notExecutable, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for argv0, want := range map[string]string{"definitely-not-a-harness-285": "was not found", notExecutable: "could not start"} {
		h := &countingBackend{Backend: CommandBackend{Argv: []string{argv0}, Env: CommandEnv(nil), Timeout: time.Second}}
		f := newAskFixture(t, h)
		if usage, err := f.ask(f.policy()); !errors.Is(err, ErrBackend) || !strings.Contains(err.Error(), want) || h.calls != 1 || usage.Calls != 1 {
			t.Fatalf("%s: err %v after %d call(s), usage %+v", argv0, err, h.calls, usage)
		}
	}
}

func TestAskRetriesATransientFailureOnceAfterABackoff(t *testing.T) {
	cases := []struct {
		name  string
		steps []statusStep
		calls int
		wait  time.Duration
		ok    bool
	}{
		{"429 with Retry-After", []statusStep{{status: 429, retryAfter: "3"}}, 2, 3 * time.Second, true},
		{"503 without Retry-After", []statusStep{{status: 503}}, 2, RetryWait, true},
		{"Retry-After shorter than the floor", []statusStep{{status: 429, retryAfter: "0"}}, 2, RetryWait, true},
		{"500 twice", []statusStep{{status: 500}, {status: 502}}, 2, RetryWait, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &statusServer{steps: tc.steps}
			f := newAskFixture(t, openAIAt(t, s))
			usage, err := f.ask(f.policy())
			if (err == nil) != tc.ok || len(s.prompts) != tc.calls || usage.Calls != tc.calls {
				t.Fatalf("err %v after %d call(s), usage %+v", err, len(s.prompts), usage)
			}
			if len(f.waits) != 1 || f.waits[0] != tc.wait {
				t.Fatalf("waits = %v, want [%s]", f.waits, tc.wait)
			}
			if s.prompts[0] != s.prompts[1] {
				t.Fatal("the retried request differs from the first")
			}
		})
	}
}

func TestAskRetriesAFailingHarnessOnce(t *testing.T) {
	for name, tc := range map[string]struct {
		argv    []string
		timeout time.Duration
		want    string
	}{
		"timeout":  {[]string{"sh", "-c", "sleep 5"}, 50 * time.Millisecond, "timed out"},
		"non-zero": {[]string{"sh", "-c", "cat >/dev/null; exit 3"}, 5 * time.Second, "exit status 3"},
	} {
		h := &countingBackend{Backend: CommandBackend{Argv: tc.argv, Env: CommandEnv(nil), Timeout: tc.timeout}}
		f := newAskFixture(t, h)
		usage, err := f.ask(f.policy())
		if err == nil || !strings.Contains(err.Error(), tc.want) || h.calls != 2 || usage.Calls != 2 || len(f.waits) != 1 {
			t.Fatalf("%s: err %v after %d call(s), usage %+v, waits %v", name, err, h.calls, usage, f.waits)
		}
	}
}

func TestAskDoesNotRetryBeforeAFarRetryAfter(t *testing.T) {
	s := &statusServer{steps: []statusStep{{status: 429, retryAfter: "120"}}}
	f := newAskFixture(t, openAIAt(t, s))
	if _, err := f.ask(f.policy()); err == nil || !strings.Contains(err.Error(), "HTTP 429") || len(s.prompts) != 1 || len(f.waits) != 0 {
		t.Fatalf("err %v after %d call(s), waits %v; want no retry sooner than the provider allows", err, len(s.prompts), f.waits)
	}
}

func TestAskStopsWhenTheBackoffIsCancelled(t *testing.T) {
	failing := &fakeBackend{errs: []error{transient("the endpoint is unreachable")}, replies: []string{"", validDescriptions}}
	f := newAskFixture(t, failing)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, _, err := Ask(ctx, failing, f.prompt, f.parse, RetryPolicy{})
	if !errors.Is(err, ErrBackend) || len(failing.prompts) != 1 {
		t.Fatalf("err %v after %d call(s), want the backend failure and no retry", err, len(failing.prompts))
	}
}

func TestAskChecksTheBudgetBeforeEveryCall(t *testing.T) {
	stop := errors.New("over budget")
	invalid := &fakeBackend{replies: []string{`{"descriptions":{"b1":"x"}}`, validDescriptions}}
	f := newAskFixture(t, invalid)
	var seen []string
	var spent []Usage
	policy := f.policy()
	policy.BeforeCall = func(prompt string, used Usage) error {
		seen, spent = append(seen, prompt), append(spent, used)
		if len(seen) == 2 {
			return stop
		}
		return nil
	}
	usage, err := f.ask(policy)
	if !errors.Is(err, stop) || len(invalid.prompts) != 1 || usage.Calls != 1 || usage.Total() != 15 {
		t.Fatalf("err %v after %d call(s), usage %+v; want the first call counted and no retry", err, len(invalid.prompts), usage)
	}
	if len(seen) != 2 || seen[0] != f.prompt.Text || !strings.Contains(seen[1], "previous reply was rejected") || spent[0].Total() != 0 || spent[1].Total() != 15 {
		t.Fatalf("budget saw prompts %q with spent %+v", seen, spent)
	}
}

func TestAskReturnsAnUnclassifiedErrorWithoutRetrying(t *testing.T) {
	plain := &fakeBackend{errs: []error{errors.New("something else")}}
	f := newAskFixture(t, plain)
	if _, err := f.ask(f.policy()); err == nil || len(plain.prompts) != 1 {
		t.Fatalf("err %v after %d call(s)", err, len(plain.prompts))
	}
}

func TestBackendErrorKeepsItsTextAndSentinel(t *testing.T) {
	err := statusError(http.StatusUnauthorized, "", time.Now())
	if err.Error() != "judge backend failed: the endpoint returned HTTP 401" || !errors.Is(err, ErrBackend) {
		t.Fatalf("err = %q", err)
	}
	var be *BackendError
	if !errors.As(statusError(http.StatusRequestTimeout, "", time.Now()), &be) || !be.Transient {
		t.Fatal("HTTP 408 is not transient")
	}
}

func TestRetryAfterReadsSecondsAndDates(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	for h, want := range map[string]time.Duration{
		"":                              0,
		"7":                             7 * time.Second,
		"-3":                            0,
		"soon":                          0,
		"99999999999":                   24 * time.Hour,
		"Wed, 11 Nov 2026 09:00:05 GMT": 5 * time.Second,
		"Wed, 11 Nov 2026 08:59:00 GMT": 0,
	} {
		if got := retryAfter(h, now); got != want {
			t.Errorf("retryAfter(%q) = %s, want %s", h, got, want)
		}
	}
}

type countingBackend struct {
	Backend
	calls int
}

func (c *countingBackend) Complete(ctx context.Context, prompt string) (string, Usage, error) {
	c.calls++
	return c.Backend.Complete(ctx, prompt)
}
