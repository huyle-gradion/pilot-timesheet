package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/llm"
	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// judge answers the day's current judgment with the configured judge backend
// (ADR 0010, issue #241):
//
//	timesheet judge [--config <path>] [--env-file <path>] [--date <day>] [--project <id>]
//
// It derives the day exactly as next does. At a block_descriptions or
// classifications judgment it renders the owned prompt, reuses a stored
// response when the prompt is unchanged, otherwise makes one backend call
// (one retry), and writes the judgment's answer file with the digest the
// report published. next --do then consumes that file exactly as it consumes
// an agent-written one. Without a judge configuration it refuses and changes
// nothing; the agent-driven path is unaffected.
const (
	judgeResponsesDir = "judge"
	judgeUsageFile    = "judge-usage.jsonl"
)

type judgeReceipt struct {
	Day        string    `json:"day"`
	State      string    `json:"state"`
	Kind       string    `json:"kind,omitempty"`
	AnswerFile string    `json:"answer_file,omitempty"`
	Cached     bool      `json:"cached,omitempty"`
	Usage      llm.Usage `json:"usage"`
	Notice     string    `json:"notice"`
}

// judgeUsageRecord is one line of judge-usage.jsonl in the day folder.
type judgeUsageRecord struct {
	At     string    `json:"at"`
	Kind   string    `json:"kind"`
	Cached bool      `json:"cached"`
	OK     bool      `json:"ok"`
	Usage  llm.Usage `json:"usage"`
}

type judgeResponse struct {
	Key   string    `json:"key"`
	Kind  string    `json:"kind"`
	Reply string    `json:"reply"`
	Usage llm.Usage `json:"usage"`
	At    string    `json:"at"`
}

func runJudge(args []string) error {
	f, err := parseNextFlags("judge", args)
	if err != nil {
		return err
	}
	if f.do || f.maxActionsSet {
		return fmt.Errorf("judge does not accept --do or --max-actions")
	}
	configPath, err := filepath.Abs(f.configPath)
	if err != nil {
		return fmt.Errorf("judge: the configuration path cannot be made absolute")
	}
	envFile := ""
	if f.envFile != "" {
		if envFile, err = filepath.Abs(f.envFile); err != nil {
			return fmt.Errorf("judge: the environment-file path cannot be made absolute")
		}
	}
	cfg, err := timesheet.LoadConfig(configPath)
	if err != nil {
		return err
	}
	if cfg.Judge == nil {
		return fmt.Errorf("judge: no judge is configured (the judge section of the configuration); the agent-driven judgment is unchanged")
	}
	day, err := resolveDay(configPath, f.date)
	if err != nil {
		return err
	}
	store, err := timesheet.OpenStore(cfg.StatePath)
	if err != nil {
		return err
	}
	obs, stop, err := buildNextObservation(cfg, configPath, envFile, day, f.project, store)
	if closeErr := store.Close(); closeErr != nil {
		return fmt.Errorf("judge: the store lock cannot be released")
	}
	if err != nil {
		return err
	}
	if stop != nil {
		return printJSON(judgeReceipt{Day: day, State: stop.State, Notice: "nothing to judge: " + stop.Reason})
	}
	a := deriveDayNext(obs)
	if a.State != stateJudgment || a.Judgment == nil {
		return printJSON(judgeReceipt{Day: day, State: a.State, Notice: "nothing to judge: the day is not at a judgment"})
	}
	backend, err := newJudgeBackend(cfg, envFile)
	if err != nil {
		return err
	}
	receipt, err := answerJudgment(context.Background(), cfg, day, a.Judgment, backend, time.Now())
	if err != nil {
		return err
	}
	return printJSON(receipt)
}

// newJudgeBackend builds the configured backend. The HTTP key is read from
// the env file by NAME and never echoed.
func newJudgeBackend(cfg *timesheet.Config, envFile string) (llm.Backend, error) {
	j := cfg.Judge
	timeout := time.Duration(j.TimeoutSeconds) * time.Second
	switch j.Backend {
	case timesheet.JudgeBackendOpenAI:
		key := ""
		if j.APIKeyEnv != "" {
			env := map[string]string{}
			if envFile != "" {
				fileEnv, err := timesheet.LoadEnvFile(envFile)
				if err != nil {
					return nil, err
				}
				env = timesheet.MergeEnv(fileEnv)
			}
			v, ok := timesheet.ResolveEnv(env, j.APIKeyEnv)
			if !ok || v == "" {
				return nil, fmt.Errorf("judge: the key variable %s is not set; add it to the env file or clear judge.api_key_env for a local endpoint", j.APIKeyEnv)
			}
			key = v
		}
		return llm.OpenAIBackend{BaseURL: j.BaseURL, Model: j.Model, APIKey: key, Timeout: timeout}, nil
	default:
		return llm.CommandBackend{Argv: j.Command, Env: llm.CommandEnv(timesheet.ReservedCredentialNames(cfg)), Timeout: timeout}, nil
	}
}

// answerJudgment renders, asks (or reuses) and writes the answer file.
func answerJudgment(ctx context.Context, cfg *timesheet.Config, day string, j *nextJudgment, backend llm.Backend, now time.Time) (judgeReceipt, error) {
	r := judgeReceipt{Day: day, State: stateJudgment, Kind: j.Kind, AnswerFile: j.AnswerFile}
	dir, err := dayArtifactsDir(cfg.StatePath, day)
	if err != nil {
		return r, err
	}
	if err := ensureDayArtifactsDir("judge", filepath.Join(dir, judgeResponsesDir)); err != nil {
		return r, err
	}
	switch j.Kind {
	case judgmentBlockDescriptions:
		if j.Draft == nil || len(j.Draft.Blocks) == 0 {
			return r, fmt.Errorf("judge: the descriptions judgment carries no draft blocks")
		}
		p, err := llm.DescribePrompt(*j.Draft, backend)
		if err != nil {
			return r, err
		}
		parse := func(reply string) (timesheet.DescriptionsFile, error) { return llm.ParseDescriptions(*j.Draft, reply) }
		answer, err := askCached(ctx, cfg, dir, day, j.Kind, p, j.LastError, backend, parse, now, &r)
		if err != nil {
			return r, err
		}
		body := map[string]any{"draft_digest": j.DraftDigest, "descriptions": answer.Descriptions}
		if answer.Notes != "" {
			body["notes"] = answer.Notes
		}
		r.Notice = fmt.Sprintf("described %d block(s)", len(answer.Descriptions))
		return r, writeJSONFile(j.AnswerFile, body)
	case judgmentClassifications:
		intervals, sets := judgeIntervals(j)
		if len(intervals) == 0 {
			r.Notice = "no interval to classify; wrote the documented no-op"
			return r, writeJSONFile(j.AnswerFile, map[string]any{"ledger_digest": j.LedgerDigest, "selections": map[string]any{}})
		}
		if len(sets) == 0 {
			return r, fmt.Errorf("judge: this classifications judgment carries no catalog candidates (saved-day recovery); answer it through the agent path")
		}
		p, err := llm.ClassifyPrompt(intervals, sets, backend)
		if err != nil {
			return r, err
		}
		parse := func(reply string) (map[string]llm.Selection, error) {
			return llm.ParseSelections(intervals, sets, reply)
		}
		picks, err := askCached(ctx, cfg, dir, day, j.Kind, p, j.LastError, backend, parse, now, &r)
		if err != nil {
			return r, err
		}
		selections := map[string]any{}
		for id, s := range picks {
			selections[id] = map[string]string{"catalog_id": s.CatalogID, "classification_id": s.ClassificationID}
		}
		r.Notice = fmt.Sprintf("classified %d of %d interval(s)", len(picks), len(intervals))
		return r, writeJSONFile(j.AnswerFile, map[string]any{"ledger_digest": j.LedgerDigest, "selections": selections})
	}
	return r, fmt.Errorf("judge: the judgment kind %q is not judged by the binary", j.Kind)
}

func judgeIntervals(j *nextJudgment) ([]llm.Interval, []llm.CandidateSet) {
	intervals := make([]llm.Interval, 0, len(j.Blocks))
	for _, b := range j.Blocks {
		intervals = append(intervals, llm.Interval{IntervalID: b.IntervalID, Start: b.Start, End: b.End, Description: b.Description, Set: b.Candidates})
	}
	sets := make([]llm.CandidateSet, 0, len(j.Candidates))
	for _, s := range j.Candidates {
		cs := llm.CandidateSet{Ref: s.Ref, CatalogID: s.CatalogID}
		for _, o := range s.Options {
			cs.Options = append(cs.Options, llm.Option{ID: o.ClassificationID, Label: o.Label, Code: o.Code})
		}
		sets = append(sets, cs)
	}
	return intervals, sets
}

// askCached reuses a stored reply for an unchanged prompt; otherwise it checks
// the daily cap, asks the backend and stores the reply. Every call made is
// recorded in judge-usage.jsonl.
func askCached[T any](ctx context.Context, cfg *timesheet.Config, dir, day, kind string, p llm.Prompt, lastError string, backend llm.Backend, parse func(string) (T, error), now time.Time, r *judgeReceipt) (T, error) {
	var zero T
	cachePath := filepath.Join(dir, judgeResponsesDir, kind+"-"+p.Key[:16]+".json")
	if lastError != "" {
		// The stored answer was rejected downstream (repair-once, #240): do
		// not reuse it, and tell the model what was wrong.
		p.Text += "\n\nThe previous answer for this same content was rejected: " + lastError + "\nFix exactly that."
	} else if data, err := os.ReadFile(cachePath); err == nil {
		var stored judgeResponse
		if json.Unmarshal(data, &stored) == nil && stored.Key == p.Key {
			if v, err := parse(stored.Reply); err == nil {
				r.Cached = true
				_ = appendJudgeUsage(dir, judgeUsageRecord{At: now.UTC().Format(time.RFC3339), Kind: kind, Cached: true, OK: true})
				return v, nil
			}
		}
	}
	spent, err := judgeTokensSpent(dir)
	if err != nil {
		return zero, err
	}
	if limit := cfg.Judge.DailyTokenCap; spent+llm.EstimateTokens(p.Text) > limit {
		return zero, fmt.Errorf("judge: the daily token cap (%d) is reached for %s (%d spent); the judgment stays open for the agent path", limit, day, spent)
	}
	v, reply, usage, askErr := llm.Ask(ctx, backend, p, parse)
	r.Usage = usage
	if err := appendJudgeUsage(dir, judgeUsageRecord{At: now.UTC().Format(time.RFC3339), Kind: kind, OK: askErr == nil, Usage: usage}); err != nil {
		return zero, err
	}
	if askErr != nil {
		return zero, fmt.Errorf("judge: %s failed after one retry: %s; the judgment stays open (run judge again or answer it through the agent path)", kind, askErr.Error())
	}
	if err := writeJSONFile(cachePath, judgeResponse{Key: p.Key, Kind: kind, Reply: reply, Usage: usage, At: now.UTC().Format(time.RFC3339)}); err != nil {
		return zero, err
	}
	return v, nil
}

func appendJudgeUsage(dir string, rec judgeUsageRecord) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, judgeUsageFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("judge: %s cannot be opened: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("judge: %s cannot be written: %w", path, err)
	}
	return nil
}

// judgeTokensSpent sums the day's non-cached judge usage.
func judgeTokensSpent(dir string) (int, error) {
	f, err := os.Open(filepath.Join(dir, judgeUsageFile))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	total := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec judgeUsageRecord
		if json.Unmarshal([]byte(strings.TrimSpace(sc.Text())), &rec) == nil && !rec.Cached {
			total += rec.Usage.Total()
		}
	}
	return total, sc.Err()
}

func addUsage(a, b llm.Usage) llm.Usage {
	return llm.Usage{
		Calls: a.Calls + b.Calls, InputTokens: a.InputTokens + b.InputTokens,
		OutputTokens: a.OutputTokens + b.OutputTokens, Exact: a.Exact && (b.Calls == 0 || b.Exact),
	}
}

// tokensCSVHeader is benchmarks/tokens.csv's header (scripts/tokens.py FIELDS).
const tokensCSVHeader = "date,session_id,input_tokens,output_tokens,cache_tokens,total_tokens"

// appendAutoTokenRow appends one row for an --auto run to judge.tokens_csv,
// creating the file with its header when needed. scripts/tokens.py keeps
// existing rows when it refreshes the file.
func appendAutoTokenRow(cfg *timesheet.Config, day string, u llm.Usage, now time.Time) error {
	path := strings.TrimSpace(cfg.Judge.TokensCSV)
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	_, statErr := os.Stat(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if errors.Is(statErr, os.ErrNotExist) {
		if _, err := f.WriteString(tokensCSVHeader + "\n"); err != nil {
			return err
		}
	}
	session := "timesheet-auto:" + now.UTC().Format("20060102T150405Z")
	_, err = fmt.Fprintf(f, "%s,%s,%d,%d,0,%d\n", day, session, u.InputTokens, u.OutputTokens, u.InputTokens+u.OutputTokens)
	return err
}
