// Package llm answers the binary's two judgments, block descriptions and
// classifications, through an owner-configured model backend (ADR 0010, issue
// #241). It builds a short owned prompt from versioned templates, sends it
// through one Backend, and parses the reply into the existing answer shapes.
// The model never sees a digest, a credential, an env value or a store path:
// the caller fills digests in after validation.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

const (
	MaxReplyBytes   = 1 << 20
	maxOutputTokens = 4096
)

// Usage is one call's token counts. Exact is false when the backend cannot
// report usage and the counts are estimated from the text sizes.
type Usage struct {
	Calls        int  `json:"calls"`
	InputTokens  int  `json:"input_tokens"`
	OutputTokens int  `json:"output_tokens"`
	Exact        bool `json:"exact"`
}

func (u Usage) add(v Usage) Usage {
	return Usage{Calls: u.Calls + v.Calls, InputTokens: u.InputTokens + v.InputTokens, OutputTokens: u.OutputTokens + v.OutputTokens, Exact: u.Exact && v.Exact}
}

func (u Usage) Total() int { return u.InputTokens + u.OutputTokens }

func EstimateTokens(s string) int { return (len(s) + 3) / 4 }

// Backend sends one prompt and returns the model's reply text.
type Backend interface {
	Complete(ctx context.Context, prompt string) (string, Usage, error)
	Identity() string
}

// ErrBackend marks a transport or provider failure (as opposed to a reply
// that does not validate). Every backend failure is a *BackendError, which
// matches it with errors.Is.
var ErrBackend = errors.New("judge backend failed")

// BackendError is one backend failure. A transient one (a timeout, HTTP 408,
// 429 or 5xx, an unreachable endpoint, a harness that exits non-zero) may pass
// when sent again; any other (HTTP 401, 403, 404, a missing executable, a 200
// that is not a chat completion) never will. RetryAfter is the provider's
// Retry-After hint, zero when it gave none.
type BackendError struct {
	Reason     string
	Transient  bool
	RetryAfter time.Duration
}

func (e *BackendError) Error() string { return ErrBackend.Error() + ": " + e.Reason }

func (e *BackendError) Is(target error) bool { return target == ErrBackend }

func permanent(format string, args ...any) error {
	return &BackendError{Reason: fmt.Sprintf(format, args...)}
}

func transient(format string, args ...any) error {
	return &BackendError{Reason: fmt.Sprintf(format, args...), Transient: true}
}

// UsageFormatClaudeJSON names the output envelope that
// `claude -p --output-format json` prints: one JSON object of type "result"
// whose "result" is the reply text and whose "usage" carries token counts.
const UsageFormatClaudeJSON = timesheet.JudgeUsageFormatClaudeJSON

// CommandBackend runs a headless agent CLI with the prompt on stdin and reads
// the reply from stdout. Stderr is discarded: a third-party harness may print
// anything there. UsageFormat is empty (stdout is the reply, usage estimated)
// or UsageFormatClaudeJSON (stdout is that envelope, usage as reported).
type CommandBackend struct {
	Argv        []string
	Env         []string
	Timeout     time.Duration
	UsageFormat string
}

func (b CommandBackend) Identity() string { return "command:" + strings.Join(b.Argv, "\x1f") }

func (b CommandBackend) Complete(ctx context.Context, prompt string) (string, Usage, error) {
	if b.Env == nil {
		return "", Usage{}, permanent("the harness command has no filtered environment; build it with CommandEnv so pipeline credentials are never inherited")
	}
	if b.UsageFormat != "" && b.UsageFormat != UsageFormatClaudeJSON {
		return "", Usage{}, permanent("the harness usage format %q is not supported", b.UsageFormat)
	}
	ctx, cancel := context.WithTimeout(ctx, b.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.Argv[0], b.Argv[1:]...)
	cmd.Env = b.Env
	cmd.Stdin = strings.NewReader(prompt)
	var out limitedBuffer
	out.limit = MaxReplyBytes
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	err := cmd.Run()
	// A harness that ran was sent the prompt and may have spent it, so its
	// failures carry the estimated input; one that never started spent nothing.
	sent := Usage{Calls: 1, InputTokens: EstimateTokens(prompt)}
	var exit *exec.ExitError
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return "", sent, transient("the harness command timed out after %s", b.Timeout)
	case errors.Is(err, exec.ErrNotFound):
		return "", Usage{}, permanent("the harness command %q was not found", b.Argv[0])
	case errors.As(err, &exit):
		return "", sent, transient("the harness command failed (exit status %d)", exit.ExitCode())
	case err != nil:
		return "", Usage{}, permanent("the harness command failed (could not start)")
	case out.over:
		return "", sent, transient("the harness reply exceeds %d bytes", MaxReplyBytes)
	}
	if b.UsageFormat == UsageFormatClaudeJSON {
		return claudeJSONReply(out.buf.Bytes(), prompt)
	}
	reply := out.buf.String()
	return reply, Usage{Calls: 1, InputTokens: EstimateTokens(prompt), OutputTokens: EstimateTokens(reply)}, nil
}

// claudeJSONReply unwraps one `claude -p --output-format json` result object.
// Its "result" string is the reply, which the caller validates exactly as a
// plain reply. Input tokens are usage.input_tokens plus the cache creation and
// cache read counts (the harness's cached setup is still sent to the model).
// A count the envelope lacks is estimated and marks the usage inexact, so
// missing usage never fails a call. Output that is not one result object
// carrying a reply is a permanent failure: the configured format does not
// match the harness, and sending the prompt again would not change that. An
// object with is_error set is transient, like a harness that exits non-zero.
// No reason echoes the harness output.
func claudeJSONReply(stdout []byte, prompt string) (string, Usage, error) {
	var env struct {
		Type    string          `json:"type"`
		IsError bool            `json:"is_error"`
		Result  json.RawMessage `json:"result"`
		Usage   *struct {
			InputTokens   *int `json:"input_tokens"`
			OutputTokens  *int `json:"output_tokens"`
			CacheCreation *int `json:"cache_creation_input_tokens"`
			CacheRead     *int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	sent := Usage{Calls: 1, InputTokens: EstimateTokens(prompt)}
	dec := json.NewDecoder(bytes.NewReader(stdout))
	if dec.Decode(&env) != nil || dec.Decode(&struct{}{}) != io.EOF || env.Type != "result" {
		return "", sent, permanent("the harness output is not one %s result object", UsageFormatClaudeJSON)
	}
	var reply string
	if !env.IsError && json.Unmarshal(env.Result, &reply) != nil {
		return "", sent, permanent("the harness result object carries no reply text")
	}
	usage := Usage{Calls: 1, InputTokens: EstimateTokens(prompt), OutputTokens: EstimateTokens(reply)}
	if u := env.Usage; u != nil {
		in, inOK := tokenCount(u.InputTokens)
		out, outOK := tokenCount(u.OutputTokens)
		if inOK {
			cacheCreation, _ := tokenCount(u.CacheCreation)
			cacheRead, _ := tokenCount(u.CacheRead)
			usage.InputTokens = in + cacheCreation + cacheRead
		}
		if outOK {
			usage.OutputTokens = out
		}
		usage.Exact = inOK && outOK
	}
	if env.IsError {
		return "", usage, transient("the harness reported an error result")
	}
	return reply, usage, nil
}

func tokenCount(n *int) (int, bool) {
	if n == nil || *n < 0 {
		return 0, false
	}
	return *n, true
}

// limitedBuffer keeps its bytes.Buffer unexported so io.Copy cannot reach
// the buffer's ReadFrom and skip the limit.
type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.over || l.buf.Len()+len(p) > l.limit {
		l.over = true
		return len(p), nil
	}
	return l.buf.Write(p)
}

func FilterEnv(environ []string, reserved []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(reserved, name) {
			out = append(out, kv)
		}
	}
	return out
}

func CommandEnv(reserved []string) []string { return FilterEnv(os.Environ(), reserved) }

// OpenAIBackend sends one chat-completions request to an OpenAI-compatible
// endpoint: a hosted provider, a gateway or a local runner.
type OpenAIBackend struct {
	BaseURL string
	Model   string
	APIKey  string
	Timeout time.Duration
	Client  *http.Client
}

func (b OpenAIBackend) Identity() string { return "openai:" + b.BaseURL + "\x1f" + b.Model }

type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	MaxTokens int           `json:"max_tokens"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func ChatRequestBody(model, prompt string) ([]byte, error) {
	return json.Marshal(chatRequest{
		Model: model, MaxTokens: maxOutputTokens,
		Messages: []chatMessage{{Role: "user", Content: prompt}},
	})
}

func (b OpenAIBackend) Complete(ctx context.Context, prompt string) (string, Usage, error) {
	ctx, cancel := context.WithTimeout(ctx, b.Timeout)
	defer cancel()
	body, err := ChatRequestBody(b.Model, prompt)
	if err != nil {
		return "", Usage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, permanent("the endpoint URL is not usable")
	}
	req.Header.Set("Content-Type", "application/json")
	if b.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.APIKey)
	}
	client := b.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", Usage{}, transient("the endpoint timed out after %s", b.Timeout)
		}
		return "", Usage{}, transient("the endpoint is unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", Usage{}, statusError(resp.StatusCode, resp.Header.Get("Retry-After"), time.Now())
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxReplyBytes+1))
	if err != nil {
		return "", Usage{}, transient("the endpoint reply could not be read")
	}
	if len(data) > MaxReplyBytes {
		return "", Usage{}, transient("the endpoint reply exceeds %d bytes", MaxReplyBytes)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil || len(parsed.Choices) == 0 {
		return "", Usage{}, permanent("the endpoint reply is not a chat completion")
	}
	reply := parsed.Choices[0].Message.Content
	if parsed.Usage == nil {
		return reply, Usage{Calls: 1, InputTokens: EstimateTokens(prompt), OutputTokens: EstimateTokens(reply)}, nil
	}
	return reply, Usage{Calls: 1, InputTokens: parsed.Usage.PromptTokens, OutputTokens: parsed.Usage.CompletionTokens, Exact: true}, nil
}

// statusError classifies a non-200 status: 408, 429 and 5xx may pass when
// sent again, any other will not.
func statusError(code int, retryAfterHeader string, now time.Time) error {
	if code != http.StatusRequestTimeout && code != http.StatusTooManyRequests && code < 500 {
		return permanent("the endpoint returned HTTP %d", code)
	}
	return &BackendError{
		Reason:     fmt.Sprintf("the endpoint returned HTTP %d", code),
		Transient:  true,
		RetryAfter: retryAfter(retryAfterHeader, now),
	}
}

// retryAfter reads a Retry-After value in seconds or as an HTTP date. An
// absent, past or unreadable value is zero; a huge one is held to a day.
func retryAfter(h string, now time.Time) time.Duration {
	h = strings.TrimSpace(h)
	const most = 24 * time.Hour
	secs, err := strconv.Atoi(h)
	switch {
	case err == nil:
		return time.Duration(min(max(secs, 0), int(most/time.Second))) * time.Second
	case errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(h, "-"):
		return most
	}
	if at, err := http.ParseTime(h); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}
