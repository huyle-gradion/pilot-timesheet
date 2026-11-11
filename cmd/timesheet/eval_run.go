package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
// of this run; cached replies are kept across runs. A cached reply's original
// cost is written back into that day's ledger, so a rerun counts the same
// tokens per day and hits the cap where the first run did.
const evalCacheDir = "judge-cache"

// errEvalInput marks a dataset part the judge cannot be asked about.
var errEvalInput = errors.New("the dataset part cannot be replayed")

type evalRunReceipt struct {
	File     string                 `json:"file"`
	CacheDir string                 `json:"cache_dir"`
	DaysRun  int                    `json:"days_run"`
	Skipped  []timesheet.GoldenSkip `json:"skipped"`
	// Usage and FreshTokens are the backend calls this run made. Tokens and
	// MaxDayTokens also count each cached reply's original cost: they are the
	// figures to compare with the cap.
	Usage         llm.Usage    `json:"usage"`
	FreshTokens   int          `json:"fresh_tokens"`
	Tokens        int          `json:"tokens"`
	Days          []evalDayRun `json:"days"`
	MaxDayTokens  int          `json:"max_day_tokens"`
	DailyTokenCap int          `json:"daily_token_cap"`
	Stopped       string       `json:"stopped,omitempty"`
	Note          string       `json:"note,omitempty"`
}

type evalDayRun struct {
	Date        string    `json:"date"`
	Tokens      int       `json:"tokens"`
	FreshTokens int       `json:"fresh_tokens"`
	Usage       llm.Usage `json:"usage"`
	Cached      []string  `json:"cached,omitempty"`
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
	if !slices.ContainsFunc(g.Days, replayableDay) {
		// A receipt of zeros would read like a failed judge or a pass on the
		// cap (#282), so the run refuses before any judge call.
		return fmt.Errorf("eval run: the dataset %s has no replayable days (%d day(s), none with a draft block or a recorded interval and candidate set); log days with the current binary, then export again", *goldenFlag, len(g.Days))
	}
	out := *outFlag
	if out == "" {
		out = filepath.Join(filepath.Dir(*goldenFlag), "candidate.json")
	}
	if out, err = filepath.Abs(out); err != nil {
		return fmt.Errorf("eval run: resolve %s: %w", *outFlag, err)
	}
	if golden, err := filepath.Abs(*goldenFlag); err == nil && golden == out {
		return fmt.Errorf("eval run: --out %s would overwrite the dataset; choose another candidate path", out)
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
// does not validate after the retry skips that part (it then scores as
// missing). The cap or a backend failure stops the run: the answers so far
// are returned with the stop error, so a dead or rate-limited judge never
// turns days into misses blamed on the judge.
func replayGolden(ctx context.Context, cfg *timesheet.Config, g timesheet.GoldenDataset, backend llm.Backend, cacheRoot string, now time.Time) (evalRunReceipt, timesheet.GoldenCandidate, error) {
	r := evalRunReceipt{CacheDir: cacheRoot, Skipped: []timesheet.GoldenSkip{}, Days: []evalDayRun{}, DailyTokenCap: cfg.Judge.DailyTokenCap}
	r.Usage.Exact = true
	cand := timesheet.GoldenCandidate{Days: map[string]timesheet.GoldenAnswer{}}
	skip := func(date, part, reason string) {
		r.Skipped = append(r.Skipped, timesheet.GoldenSkip{Date: date, Part: part, Reason: reason})
	}
	capStop := func(day string, spent int) string {
		return fmt.Sprintf("the per-day token cap (%d) is reached replaying %s (%d tokens counted for that day, cached replies included)", cfg.Judge.DailyTokenCap, day, spent)
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
		judged := func(part, kind string, ask func(*judgeReceipt) error) {
			var jr judgeReceipt
			err := ask(&jr)
			dr.Usage = addUsage(dr.Usage, jr.Usage)
			dr.Tokens += jr.Usage.Total()
			if jr.Cached {
				dr.Cached = append(dr.Cached, part)
				cached++
				// The ledger counts the reply's original cost, so the cap
				// sees this day's real spend on the next part.
				dr.Tokens += jr.CachedUsage.Total()
				rec := judgeUsageRecord{At: now.UTC().Format(time.RFC3339), Kind: kind, OK: true, Usage: jr.CachedUsage}
				if appendErr := appendJudgeUsage(dir, rec); appendErr != nil && err == nil {
					err = appendErr
				}
			}
			var capErr judgeCapError
			switch {
			case err == nil:
				if dr.Tokens > cfg.Judge.DailyTokenCap {
					r.Stopped = capStop(day.Date, dr.Tokens)
				}
			case errors.As(err, &capErr):
				r.Stopped = capStop(day.Date, capErr.spent)
				skip(day.Date, part, "not run: "+r.Stopped)
			case errors.Is(err, llm.ErrInvalidReply), errors.Is(err, errEvalInput):
				skip(day.Date, part, err.Error())
			default:
				r.Stopped = err.Error()
				skip(day.Date, part, "not run: "+err.Error())
			}
		}
		if d := day.Descriptions; d != nil {
			judged("descriptions", judgmentBlockDescriptions, func(jr *judgeReceipt) error {
				descs, err := evalDescribe(ctx, cfg, dir, day.Date, d.Input, backend, now, jr)
				ans.Descriptions = descs
				return err
			})
		}
		if c := day.Classifications; c != nil && r.Stopped == "" {
			judged("classifications", judgmentClassifications, func(jr *judgeReceipt) error {
				picks, err := evalClassify(ctx, cfg, dir, day.Date, c, backend, now, jr)
				ans.Classifications = picks
				return err
			})
		}
		dr.FreshTokens = dr.Usage.Total()
		r.Usage = addUsage(r.Usage, dr.Usage)
		r.FreshTokens += dr.FreshTokens
		r.Tokens += dr.Tokens
		r.MaxDayTokens = max(r.MaxDayTokens, dr.Tokens)
		r.Days = append(r.Days, dr)
		if ans.Descriptions != nil || ans.Classifications != nil {
			cand.Days[day.Date] = ans
			r.DaysRun++
		}
	}
	if cached > 0 {
		r.Note = fmt.Sprintf("%d part(s) reused a cached reply: their original cost counts in tokens but not in fresh_tokens; remove %s to call the judge again", cached, cacheRoot)
	}
	if r.Stopped != "" {
		return r, cand, fmt.Errorf("eval run: stopped: %s; the partial candidate is kept", r.Stopped)
	}
	return r, cand, nil
}

// replayableDay reports whether some part of day reaches the judge: the
// inputs evalDescribe and evalClassify refuse with errEvalInput are absent.
func replayableDay(day timesheet.GoldenDay) bool {
	if d := day.Descriptions; d != nil && len(d.Input.Blocks) > 0 {
		return true
	}
	c := day.Classifications
	return c != nil && len(c.Input) > 0 && len(c.Candidates) > 0
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
