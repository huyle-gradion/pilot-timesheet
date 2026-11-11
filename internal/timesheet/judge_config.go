package timesheet

import (
	"net/url"
	"strings"
)

// JudgeConfig is the optional binary-owned judge (ADR 0010, issue #241). Absent
// means off: the judgments stay agent-driven and nothing else changes. It
// holds no secret: the HTTP backend names its key variable, never its value.
type JudgeConfig struct {
	// Backend is "command" (a headless agent CLI run with the prompt on stdin,
	// the default) or "openai" (an OpenAI-compatible chat-completions endpoint).
	Backend string `json:"backend,omitempty"`
	// Command is the headless harness argv; the prompt goes to its stdin.
	Command []string `json:"command,omitempty"`
	BaseURL string   `json:"base_url,omitempty"`
	Model   string   `json:"model,omitempty"`
	// APIKeyEnv names the key variable in the env file; empty means no key
	// (a local endpoint).
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// TimeoutSeconds bounds one judge call (default 120).
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// DailyTokenCap stops further judge calls for a day once reached
	// (default 10000).
	DailyTokenCap int `json:"daily_token_cap,omitempty"`
	// TokensCSV is where next --do --auto appends one usage row per run, in
	// the benchmarks/tokens.csv columns; empty writes no row.
	TokensCSV string `json:"tokens_csv,omitempty"`
}

const (
	JudgeBackendCommand = "command"
	JudgeBackendOpenAI  = "openai"

	DefaultJudgeTimeoutSeconds = 120
	DefaultJudgeDailyTokenCap  = 10000
	maxJudgeTimeoutSeconds     = 900
)

func (c *Config) validateJudge() error {
	j := c.Judge
	if j == nil {
		return nil
	}
	j.Backend = strings.TrimSpace(j.Backend)
	if j.Backend == "" {
		j.Backend = JudgeBackendCommand
	}
	switch j.Backend {
	case JudgeBackendCommand:
		if len(j.Command) == 0 || strings.TrimSpace(j.Command[0]) == "" {
			return newError(CodeConfigInvalid, "config: judge.command must name the harness executable")
		}
	case JudgeBackendOpenAI:
		u, err := url.Parse(strings.TrimSpace(j.BaseURL))
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return newError(CodeConfigInvalid, "config: judge.base_url must be an http(s) URL")
		}
		if u.User != nil {
			return newError(CodeConfigInvalid, "config: judge.base_url must not carry credentials; name the key in judge.api_key_env")
		}
		j.BaseURL = strings.TrimRight(u.String(), "/")
		if strings.TrimSpace(j.Model) == "" {
			return newError(CodeConfigInvalid, "config: judge.model is required for the openai backend")
		}
		if j.APIKeyEnv != "" && !envNameRE.MatchString(j.APIKeyEnv) {
			return newError(CodeConfigInvalid, "config: judge.api_key_env is not a valid variable name")
		}
	default:
		return newError(CodeConfigInvalid, `config: judge.backend must be "command" or "openai"`)
	}
	if j.TimeoutSeconds == 0 {
		j.TimeoutSeconds = DefaultJudgeTimeoutSeconds
	}
	if j.TimeoutSeconds < 1 || j.TimeoutSeconds > maxJudgeTimeoutSeconds {
		return newError(CodeConfigInvalid, "config: judge.timeout_seconds must be between 1 and 900")
	}
	if j.DailyTokenCap == 0 {
		j.DailyTokenCap = DefaultJudgeDailyTokenCap
	}
	if j.DailyTokenCap < 0 {
		return newError(CodeConfigInvalid, "config: judge.daily_token_cap must not be negative")
	}
	return nil
}

// ResolveEnv resolves one variable NAME with the binary's precedence:
// inherited process values over env-file values. Callers never log the value.
func ResolveEnv(merged map[string]string, name string) (string, bool) {
	return resolveEnv(merged, name)
}

// ReservedCredentialNames are every credential variable name the configuration
// knows. They are removed from a judge harness's environment so pipeline
// secrets never reach a third-party tool.
func ReservedCredentialNames(c *Config) []string {
	names := []string{"GITHUB_TOKEN", "GCAL_ICS_URL", DefaultGradionTokenEnv}
	if c.Gradion != nil && c.Gradion.TokenEnv != "" {
		names = append(names, c.Gradion.TokenEnv)
	}
	if c.Judge != nil && c.Judge.APIKeyEnv != "" {
		names = append(names, c.Judge.APIKeyEnv)
	}
	names = append(names, c.RequiredEnv...)
	return names
}
