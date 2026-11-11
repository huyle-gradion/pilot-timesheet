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
	"strings"
	"time"
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
// that does not validate).
var ErrBackend = errors.New("judge backend failed")

// CommandBackend runs a headless agent CLI with the prompt on stdin and reads
// the reply from stdout. Stderr is discarded: a third-party harness may print
// anything there.
type CommandBackend struct {
	Argv    []string
	Env     []string
	Timeout time.Duration
}

func (b CommandBackend) Identity() string { return "command:" + strings.Join(b.Argv, "\x1f") }

func (b CommandBackend) Complete(ctx context.Context, prompt string) (string, Usage, error) {
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
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return "", Usage{}, fmt.Errorf("%w: the harness command timed out after %s", ErrBackend, b.Timeout)
	case errors.Is(err, exec.ErrNotFound):
		return "", Usage{}, fmt.Errorf("%w: the harness command %q was not found", ErrBackend, b.Argv[0])
	case err != nil:
		return "", Usage{}, fmt.Errorf("%w: the harness command failed (%v)", ErrBackend, exitSummary(err))
	case out.over:
		return "", Usage{}, fmt.Errorf("%w: the harness reply exceeds %d bytes", ErrBackend, MaxReplyBytes)
	}
	reply := out.String()
	return reply, Usage{Calls: 1, InputTokens: EstimateTokens(prompt), OutputTokens: EstimateTokens(reply)}, nil
}

func exitSummary(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Sprintf("exit status %d", exit.ExitCode())
	}
	return "could not start"
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
	over  bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if l.Len()+len(p) > l.limit {
		l.over = true
		return len(p), nil
	}
	return l.Buffer.Write(p)
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
		return "", Usage{}, fmt.Errorf("%w: the endpoint URL is not usable", ErrBackend)
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
			return "", Usage{}, fmt.Errorf("%w: the endpoint timed out after %s", ErrBackend, b.Timeout)
		}
		return "", Usage{}, fmt.Errorf("%w: the endpoint is unreachable", ErrBackend)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxReplyBytes+1))
	if err != nil {
		return "", Usage{}, fmt.Errorf("%w: the endpoint reply could not be read", ErrBackend)
	}
	if len(data) > MaxReplyBytes {
		return "", Usage{}, fmt.Errorf("%w: the endpoint reply exceeds %d bytes", ErrBackend, MaxReplyBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return "", Usage{}, fmt.Errorf("%w: the endpoint returned HTTP %d", ErrBackend, resp.StatusCode)
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
		return "", Usage{}, fmt.Errorf("%w: the endpoint reply is not a chat completion", ErrBackend)
	}
	reply := parsed.Choices[0].Message.Content
	if parsed.Usage == nil {
		return reply, Usage{Calls: 1, InputTokens: EstimateTokens(prompt), OutputTokens: EstimateTokens(reply)}, nil
	}
	return reply, Usage{Calls: 1, InputTokens: parsed.Usage.PromptTokens, OutputTokens: parsed.Usage.CompletionTokens, Exact: true}, nil
}
