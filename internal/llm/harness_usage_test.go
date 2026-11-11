package llm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeHarness is a harness script that drains the prompt from stdin and
// prints stdout verbatim, so a test can emit any output envelope.
func fakeHarness(t *testing.T, stdout string) []string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "stdout")
	if err := os.WriteFile(out, []byte(stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"sh", "-c", `cat >/dev/null; if [ -n "$GITHUB_TOKEN" ]; then echo LEAK; exit 0; fi; cat "$0"`, out}
}

func claudeJSON(argv []string) CommandBackend {
	return CommandBackend{Argv: argv, Env: CommandEnv([]string{"GITHUB_TOKEN"}), Timeout: 5 * time.Second, UsageFormat: UsageFormatClaudeJSON}
}

const answerDoc = `{"descriptions":{"b1":"x"}}`

func TestCommandBackendReadsTheHarnessReportedUsage(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "synthetic-secret")
	const prompt = "prompt text that is forty bytes long...."
	cases := map[string]struct {
		stdout string
		want   Usage
	}{
		"usage without modelUsage": {
			`{"type":"result","subtype":"success","is_error":false,"result":"{\"descriptions\":{\"b1\":\"x\"}}","total_cost_usd":0.01,` +
				`"usage":{"input_tokens":12,"cache_creation_input_tokens":100,"cache_read_input_tokens":1000,"output_tokens":7}}` + "\n",
			Usage{Calls: 1, InputTokens: 12, OutputTokens: 7, CacheTokens: 1100, Exact: true},
		},
		"modelUsage preferred and summed": {
			`{"type":"result","is_error":false,"result":"{\"descriptions\":{\"b1\":\"x\"}}",` +
				`"usage":{"input_tokens":5,"cache_read_input_tokens":50,"output_tokens":1},` +
				`"modelUsage":{"model-a":{"inputTokens":10,"outputTokens":4,"cacheReadInputTokens":100,"cacheCreationInputTokens":20,"costUSD":0.01},` +
				`"model-b":{"inputTokens":3,"outputTokens":2,"cacheReadInputTokens":0,"cacheCreationInputTokens":0}}}`,
			Usage{Calls: 1, InputTokens: 13, OutputTokens: 6, CacheTokens: 120, Exact: true},
		},
		"modelUsage missing a count": {
			`{"type":"result","is_error":false,"result":"{\"descriptions\":{\"b1\":\"x\"}}","usage":{"input_tokens":5,"output_tokens":1},` +
				`"modelUsage":{"model-a":{"inputTokens":10,"outputTokens":4},"model-b":{"inputTokens":3}}}`,
			Usage{Calls: 1, InputTokens: 13, OutputTokens: EstimateTokens(answerDoc)},
		},
		"empty modelUsage falls back to usage": {
			`{"type":"result","is_error":false,"result":"{\"descriptions\":{\"b1\":\"x\"}}","usage":{"input_tokens":12,"output_tokens":7},"modelUsage":{}}`,
			Usage{Calls: 1, InputTokens: 12, OutputTokens: 7, Exact: true},
		},
		"null cache counts": {
			`{"type":"result","is_error":false,"result":"{\"descriptions\":{\"b1\":\"x\"}}","usage":{"input_tokens":12,"cache_creation_input_tokens":null,"output_tokens":7}}`,
			Usage{Calls: 1, InputTokens: 12, OutputTokens: 7, Exact: true},
		},
		"negative count": {
			`{"type":"result","is_error":false,"result":"{\"descriptions\":{\"b1\":\"x\"}}","usage":{"input_tokens":-5,"output_tokens":7,"cache_read_input_tokens":-9}}`,
			Usage{Calls: 1, InputTokens: EstimateTokens(prompt), OutputTokens: 7},
		},
		"no usage": {
			`{"type":"result","is_error":false,"result":"{\"descriptions\":{\"b1\":\"x\"}}"}`,
			Usage{Calls: 1, InputTokens: EstimateTokens(prompt), OutputTokens: EstimateTokens(answerDoc)},
		},
		"input only": {
			`{"type":"result","is_error":false,"result":"{\"descriptions\":{\"b1\":\"x\"}}","usage":{"input_tokens":12}}`,
			Usage{Calls: 1, InputTokens: 12, OutputTokens: EstimateTokens(answerDoc)},
		},
		"output only": {
			`{"type":"result","is_error":false,"result":"{\"descriptions\":{\"b1\":\"x\"}}","usage":{"output_tokens":7}}`,
			Usage{Calls: 1, InputTokens: EstimateTokens(prompt), OutputTokens: 7},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			reply, usage, err := claudeJSON(fakeHarness(t, tc.stdout)).Complete(context.Background(), prompt)
			if err != nil || reply != answerDoc || usage != tc.want {
				t.Fatalf("reply/usage/err = %q / %+v / %v; want %q / %+v", reply, usage, err, answerDoc, tc.want)
			}
		})
	}
}

func TestCommandBackendDefaultFormatKeepsTheRawReply(t *testing.T) {
	envelope := `{"type":"result","result":"x","usage":{"input_tokens":12,"output_tokens":7}}`
	b := claudeJSON(fakeHarness(t, envelope))
	b.UsageFormat = ""
	reply, usage, err := b.Complete(context.Background(), "prompt")
	if err != nil || reply != envelope || usage.Exact || usage.OutputTokens != EstimateTokens(envelope) {
		t.Fatalf("reply/usage/err = %q / %+v / %v; want the raw stdout and an estimate", reply, usage, err)
	}
}

func TestCommandBackendRejectsAMalformedEnvelope(t *testing.T) {
	const prompt = "prompt"
	sent := Usage{Calls: 1, InputTokens: EstimateTokens(prompt)}
	// Every stdout carries the marker; no error may repeat it.
	const marker = "SYNTHETIC-HARNESS-OUTPUT-286"
	limitUsage := Usage{Calls: 1, InputTokens: 30, OutputTokens: 2, CacheTokens: 400, Exact: true}
	const limitTokens = `"usage":{"input_tokens":30,"output_tokens":2,"cache_read_input_tokens":400}`
	cases := map[string]struct {
		stdout    string
		transient bool
		usage     Usage
	}{
		"plain text":      {`{"descriptions":{"b1":"` + marker + `"}}`, false, sent},
		"not json":        {"Error: " + marker, false, sent},
		"no result":       {`{"type":"result","subtype":"success","is_error":false,"session_id":"` + marker + `"}`, false, sent},
		"result not text": {`{"type":"result","is_error":false,"result":{"descriptions":{"b1":"` + marker + `"}}}`, false, sent},
		"another message": {`{"type":"assistant","result":"` + marker + `"}`, false, sent},
		"two documents":   {`{"type":"result","result":"x"}{"type":"result","result":"` + marker + `"}`, false, sent},
		"harness error": {
			`{"type":"result","subtype":"error_during_execution","is_error":true,"errors":["` + marker + `"],` + limitTokens + `}`,
			true, limitUsage,
		},
		"max turns": {
			`{"type":"result","subtype":"error_max_turns","is_error":true,"errors":["` + marker + `"],` + limitTokens + `}`,
			false, limitUsage,
		},
		"max budget": {
			`{"type":"result","subtype":"error_max_budget_usd","is_error":true,"errors":["` + marker + `"],` + limitTokens + `}`,
			false, limitUsage,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			reply, usage, err := claudeJSON(fakeHarness(t, tc.stdout)).Complete(context.Background(), prompt)
			var be *BackendError
			if !errors.As(err, &be) || be.Transient != tc.transient || reply != "" || usage != tc.usage {
				t.Fatalf("reply/usage/err = %q / %+v / %v; want a backend failure (transient %v) with usage %+v", reply, usage, err, tc.transient, tc.usage)
			}
			if strings.Contains(err.Error(), marker) {
				t.Fatalf("err %q echoes harness output", err)
			}
		})
	}
}

func TestCommandBackendBoundsTheEnvelope(t *testing.T) {
	big := `{"type":"result","is_error":false,"result":"` + strings.Repeat("x", MaxReplyBytes) + `"}`
	_, usage, err := claudeJSON(fakeHarness(t, big)).Complete(context.Background(), "prompt")
	var be *BackendError
	if !errors.As(err, &be) || !be.Transient || !strings.Contains(err.Error(), "exceeds") || usage.Exact {
		t.Fatalf("usage/err = %+v / %v; want the reply size limit", usage, err)
	}
}

func TestCommandBackendRefusesAnUnknownUsageFormat(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	b := CommandBackend{Argv: []string{"sh", "-c", "touch " + marker}, Env: CommandEnv(nil), Timeout: 5 * time.Second, UsageFormat: "yaml"}
	if _, usage, err := b.Complete(context.Background(), "prompt"); !errors.Is(err, ErrBackend) || usage.Calls != 0 {
		t.Fatalf("usage/err = %+v / %v; want a permanent failure before the harness runs", usage, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the harness ran with an unknown usage format")
	}
}

func TestAskDoesNotResendAMalformedEnvelope(t *testing.T) {
	h := &countingBackend{Backend: claudeJSON(fakeHarness(t, "plain text, no envelope"))}
	f := newAskFixture(t, h)
	usage, err := f.ask(f.policy())
	if !errors.Is(err, ErrBackend) || errors.Is(err, ErrInvalidReply) || h.calls != 1 || usage.Calls != 1 || len(f.waits) != 0 {
		t.Fatalf("err %v after %d call(s), usage %+v, waits %v; want one call and no retry", err, h.calls, usage, f.waits)
	}
}

func TestAskValidatesTheUnwrappedReplyAsBefore(t *testing.T) {
	h := &countingBackend{Backend: claudeJSON(fakeHarness(t,
		`{"type":"result","is_error":false,"result":"{\"descriptions\":{\"b1\":\"x\"}}","usage":{"input_tokens":12,"output_tokens":7}}`))}
	f := newAskFixture(t, h)
	usage, err := f.ask(f.policy())
	if !errors.Is(err, ErrInvalidReply) || h.calls != 2 || usage != (Usage{Calls: 2, InputTokens: 24, OutputTokens: 14, Exact: true}) {
		t.Fatalf("err %v after %d call(s), usage %+v; want the repair-once retry and exact usage for both calls", err, h.calls, usage)
	}
}
