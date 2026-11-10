package timesheet

import (
	"slices"
	"strings"
	"testing"
)

func TestValidateJudge(t *testing.T) {
	ok := []*JudgeConfig{
		{Command: []string{"harness", "--print"}},
		{Backend: "openai", BaseURL: "http://127.0.0.1:11434/v1/", Model: "local-model"},
		{Backend: "openai", BaseURL: "https://api.example.test/v1", Model: "m", APIKeyEnv: "JUDGE_API_KEY"},
	}
	for _, j := range ok {
		c := &Config{Judge: j}
		if err := c.validateJudge(); err != nil {
			t.Fatalf("%+v: %v", j, err)
		}
		if j.TimeoutSeconds != DefaultJudgeTimeoutSeconds || j.DailyTokenCap != DefaultJudgeDailyTokenCap || strings.HasSuffix(j.BaseURL, "/") {
			t.Fatalf("defaults not applied: %+v", j)
		}
	}
	bad := map[string]*JudgeConfig{
		"harness executable": {Backend: "command"},
		"http(s) URL":        {Backend: "openai", BaseURL: "ftp://x", Model: "m"},
		"carry credentials":  {Backend: "openai", BaseURL: "https://user:pw@x", Model: "m"},
		"judge.model":        {Backend: "openai", BaseURL: "https://x"},
		"api_key_env":        {Backend: "openai", BaseURL: "https://x", Model: "m", APIKeyEnv: "bad name"},
		"judge.backend":      {Backend: "grpc"},
		"timeout_seconds":    {Command: []string{"h"}, TimeoutSeconds: 5000},
	}
	for want, j := range bad {
		c := &Config{Judge: j}
		if err := c.validateJudge(); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%+v: err = %v, want %q", j, err, want)
		}
	}
}

func TestReservedCredentialNamesCoverTheJudgeKey(t *testing.T) {
	c := &Config{Judge: &JudgeConfig{APIKeyEnv: "JUDGE_API_KEY"}, Gradion: &GradionConfig{TokenEnv: "MY_GRADION"}}
	names := ReservedCredentialNames(c)
	for _, want := range []string{"GITHUB_TOKEN", "GCAL_ICS_URL", "MY_GRADION", "JUDGE_API_KEY"} {
		if !slices.Contains(names, want) {
			t.Fatalf("reserved names %v lack %s", names, want)
		}
	}
}
