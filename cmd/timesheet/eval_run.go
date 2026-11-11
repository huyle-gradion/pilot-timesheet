package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/llm"
	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// eval run (issue #279) replays every golden day through the configured judge
// and writes a candidate file for eval score:
//
//	timesheet eval run --golden <file> [--out <file>] [--config <path>] [--env-file <path>]
//
// It uses the production judge path unchanged: the same owned prompts
// (llm.DescribePrompt, llm.ClassifyPrompt), the same parsers, one retry,
// the response cache and the per-day token cap (askCached). Only the folder
// those two live in differs: an eval cache next to --out, never the store's
// day folders, so a replay neither spends a real day's cap nor writes state.
// The usage ledger is reset per replayed day, so the cap applies to each day
// of this run; cached replies are kept across runs.
const evalCacheDir = "judge-cache"

// errEvalInput marks a dataset part the judge cannot be asked about.
var errEvalInput = errors.New("the dataset part cannot be replayed")

type evalRunReceipt struct {
	File     string                 `json:"file"`
	CacheDir string                 `json:"cache_dir"`
	DaysRun  int                    `json:"days_run"`
	Skipped  []timesheet.GoldenSkip `json:"skipped"`
	// Usage is every backend call this run made; cached parts add nothing.
	Usage         llm.Usage    `json:"usage"`
	Days          []evalDayRun `json:"days"`
	MaxDayTokens  int          `json:"max_day_tokens"`
	DailyTokenCap int          `json:"daily_token_cap"`
	Stopped       string       `json:"stopped,omitempty"`
	Note          string       `json:"note,omitempty"`
}

type evalDayRun struct {
	Date   string    `json:"date"`
	Tokens int       `json:"tokens"`
	Usage  llm.Usage `json:"usage"`
	Cached []string  `json:"cached,omitempty"`
}

func runEvalRun(args []string) error {
	flags := flag.NewFlagSet("eval run", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	goldenFlag := flags.String("golden", "", "dataset written by eval export (required)")
	outFlag := flags.String("out", "", "candidate path outside any git repository (default: candidate.json next to the dataset)")
	configFlag := flags.String("config", "", "configuration path (optional; defaults to $HOME/.timesheet/config.json)")
	envFlag := flags.String("env-file", "", "environment file holding the judge key variable (optional)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("eval run does not accept positional arguments")
	}
	if *goldenFlag == "" {
		return fmt.Errorf("eval run requires --golden")
	}
	g, err := readGolden("eval run", *goldenFlag)
	if err != nil {
		return err
	}
	out := *outFlag
	if out == "" {
		out = filepath.Join(filepath.Dir(*goldenFlag), "candidate.json")
	}
	if out, err = filepath.Abs(out); err != nil {
		return fmt.Errorf("eval run: resolve %s: %w", *outFlag, err)
	}
	if repo := enclosingGitRepo(filepath.Dir(out)); repo != "" {
		return fmt.Errorf("eval run: %s is inside the git repository %s; the judge's answers hold personal evidence, so write them outside any repository", out, repo)
	}
	configPath, envFile, err := resolveStoreFlags("eval", *configFlag, *envFlag)
	if err != nil {
		return err
	}
	cfg, err := timesheet.LoadConfig(configPath)
	if err != nil {
		return err
	}
	if cfg.Judge == nil {
		return fmt.Errorf("eval run: the run needs a judge section in the configuration (ADR 0010)")
	}
	backend, err := newJudgeBackend(cfg, envFile)
	if err != nil {
		return err
	}
	r, cand, runErr := replayGolden(context.Background(), cfg, g, backend, filepath.Join(filepath.Dir(out), evalCacheDir), time.Now())
	r.File = out
	data, err := jsonBytes(cand)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return fmt.Errorf("eval run: %s cannot be created: %w", filepath.Dir(out), err)
	}
	if err := writeFileAtomic(out, data); err != nil {
		return fmt.Errorf("eval run: %s cannot be written: %w", out, err)
	}
	if err := printJSON(r); err != nil {
		return err
	}
	return runErr
}

// replayGolden asks the judge for every day part in the dataset. A reply that
// does not validate after the retry, or a backend failure, skips that part
// (it then scores as missing). The cap stops the run; the answers so far are
// returned with the stop error.
func replayGolden(ctx context.Context, cfg *timesheet.Config, g timesheet.GoldenDataset, backend llm.Backend, cacheRoot string, now time.Time) (evalRunReceipt, timesheet.GoldenCandidate, error) {
	r := evalRunReceipt{CacheDir: cacheRoot, Skipped: []timesheet.GoldenSkip{}, Days: []evalDayRun{}, DailyTokenCap: cfg.Judge.DailyTokenCap}
	r.Usage.Exact = true
	cand := timesheet.GoldenCandidate{Days: map[string]timesheet.GoldenAnswer{}}
	skip := func(date, part, reason string) {
		r.Skipped = append(r.Skipped, timesheet.GoldenSkip{Date: date, Part: part, Reason: reason})
	}
	cached := 0
	for _, day := range g.Days {
		if r.Stopped != "" {
			skip(day.Date, "day", "not run: the run stopped before this day")
			continue
		}
		dir := filepath.Join(cacheRoot, day.Date)
		if err := os.MkdirAll(filepath.Join(dir, judgeResponsesDir), 0o700); err != nil {
			return r, cand, fmt.Errorf("eval run: %s cannot be created: %w", dir, err)
		}
		if err := os.Remove(filepath.Join(dir, judgeUsageFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return r, cand, fmt.Errorf("eval run: the usage ledger in %s cannot be reset: %w", dir, err)
		}
		dr := evalDayRun{Date: day.Date}
		dr.Usage.Exact = true
		var ans timesheet.GoldenAnswer
		judged := func(part string, ask func(*judgeReceipt) error) {
			var jr judgeReceipt
			err := ask(&jr)
			dr.Usage = addUsage(dr.Usage, jr.Usage)
			if jr.Cached {
				dr.Cached = append(dr.Cached, part)
				cached++
			}
			var capErr judgeCapError
			switch {
			case err == nil:
			case errors.As(err, &capErr):
				r.Stopped = capErr.Error()
				skip(day.Date, part, "not run: "+capErr.Error())
			case errors.Is(err, llm.ErrInvalidReply), errors.Is(err, llm.ErrBackend), errors.Is(err, errEvalInput):
				skip(day.Date, part, err.Error())
			default:
				r.Stopped = err.Error()
				skip(day.Date, part, "not run: "+err.Error())
			}
		}
		if d := day.Descriptions; d != nil {
			judged("descriptions", func(jr *judgeReceipt) error {
				descs, err := evalDescribe(ctx, cfg, dir, day.Date, d.Input, backend, now, jr)
				ans.Descriptions = descs
				return err
			})
		}
		if c := day.Classifications; c != nil && r.Stopped == "" {
			judged("classifications", func(jr *judgeReceipt) error {
				picks, err := evalClassify(ctx, cfg, dir, day.Date, c, backend, now, jr)
				ans.Classifications = picks
				return err
			})
		}
		r.Usage = addUsage(r.Usage, dr.Usage)
		dr.Tokens = dr.Usage.Total()
		r.MaxDayTokens = max(r.MaxDayTokens, dr.Tokens)
		r.Days = append(r.Days, dr)
		if ans.Descriptions != nil || ans.Classifications != nil {
			cand.Days[day.Date] = ans
			r.DaysRun++
		}
	}
	if cached > 0 {
		r.Note = fmt.Sprintf("%d part(s) reused a cached reply and add no tokens; remove %s to measure them again", cached, cacheRoot)
	}
	if r.Stopped != "" {
		return r, cand, fmt.Errorf("eval run: stopped: %s; the partial candidate is kept", r.Stopped)
	}
	return r, cand, nil
}

// evalDescribe answers one draft and returns each block's text as record
// would store it (the inline PRs list appended), which is what the dataset's
// truth holds.
func evalDescribe(ctx context.Context, cfg *timesheet.Config, dir, day string, draft timesheet.Draft, backend llm.Backend, now time.Time, jr *judgeReceipt) (map[string]string, error) {
	if len(draft.Blocks) == 0 {
		return nil, fmt.Errorf("%w: the draft has no blocks", errEvalInput)
	}
	p, err := llm.DescribePrompt(draft, backend)
	if err != nil {
		return nil, err
	}
	parse := func(reply string) (timesheet.DescriptionsFile, error) { return llm.ParseDescriptions(draft, reply) }
	answer, err := askCached(ctx, cfg, dir, day, judgmentBlockDescriptions, p, "", backend, parse, now, jr)
	if err != nil {
		return nil, err
	}
	reviewed, err := timesheet.ComposeReviewed(draft, answer)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", llm.ErrInvalidReply, err.Error())
	}
	out := make(map[string]string, len(reviewed.Blocks))
	for _, b := range reviewed.Blocks {
		out[b.BlockID] = b.Description
	}
	return out, nil
}

// evalClassify answers the recorded intervals against their recorded
// candidate sets, as judgeIntervals builds them from a live judgment.
func evalClassify(ctx context.Context, cfg *timesheet.Config, dir, day string, c *timesheet.GoldenClassifications, backend llm.Backend, now time.Time, jr *judgeReceipt) (map[string]string, error) {
	intervals := make([]llm.Interval, 0, len(c.Input))
	for _, b := range c.Input {
		intervals = append(intervals, llm.Interval{IntervalID: b.IntervalID, Start: b.Start, End: b.End, Description: b.Description, Set: b.Candidates})
	}
	sets := make([]llm.CandidateSet, 0, len(c.Candidates))
	for _, s := range c.Candidates {
		cs := llm.CandidateSet{Ref: s.Ref, CatalogID: s.CatalogID}
		for _, o := range s.Options {
			cs.Options = append(cs.Options, llm.Option{ID: o.ClassificationID, Label: o.Label, Code: o.Code})
		}
		sets = append(sets, cs)
	}
	if len(intervals) == 0 || len(sets) == 0 {
		return nil, fmt.Errorf("%w: no intervals or no candidate sets are recorded for this day", errEvalInput)
	}
	p, err := llm.ClassifyPrompt(intervals, sets, backend)
	if err != nil {
		return nil, err
	}
	parse := func(reply string) (map[string]llm.Selection, error) {
		return llm.ParseSelections(intervals, sets, reply)
	}
	picks, err := askCached(ctx, cfg, dir, day, judgmentClassifications, p, "", backend, parse, now, jr)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(picks))
	for id, s := range picks {
		out[id] = s.ClassificationID
	}
	return out, nil
}
