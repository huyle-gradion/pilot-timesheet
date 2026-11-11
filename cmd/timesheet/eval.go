package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// eval is the golden-day evaluation verb (issue #236):
//
//	timesheet eval export [--config <path>] [--out <file>]
//	timesheet eval run --golden <file> [--out <file>] [--config <path>] [--env-file <path>]
//	timesheet eval score --golden <file> [--candidate <file>]
//
// export reads the store and writes the owner's saved days as judgment input
// plus accepted answer. The file holds personal evidence, so it is refused
// inside any git work tree; it never carries configuration, credentials or
// env-file contents. run replays the dataset through the configured judge
// (eval_run.go). score rates a candidate (default: the dataset's own
// accepted answers, the 100% self-check) with timesheet.ScoreGolden.
func runEval(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("eval requires a subcommand: export, run or score")
	}
	switch args[0] {
	case "export":
		return runEvalExport(args[1:])
	case "run":
		return runEvalRun(args[1:])
	case "score":
		return runEvalScore(args[1:])
	}
	return fmt.Errorf("eval: unknown subcommand %q; use export, run or score", args[0])
}

type evalExportReceipt struct {
	File    string                 `json:"file"`
	Days    int                    `json:"days"`
	Skipped []timesheet.GoldenSkip `json:"skipped"`
	Note    string                 `json:"note,omitempty"`
}

func runEvalExport(args []string) error {
	flags := flag.NewFlagSet("eval export", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configFlag := flags.String("config", "", "configuration path (optional; defaults to $HOME/.timesheet/config.json)")
	outFlag := flags.String("out", "", "dataset path outside any git repository (default: <state dir>/eval/golden.json)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("eval export does not accept positional arguments")
	}
	configPath, _, err := resolveStoreFlags("eval", *configFlag, "")
	if err != nil {
		return err
	}
	cfg, err := timesheet.LoadConfig(configPath)
	if err != nil {
		return err
	}
	out := *outFlag
	if out == "" {
		out = filepath.Join(filepath.Dir(cfg.StatePath), "eval", "golden.json")
	}
	out, err = filepath.Abs(out)
	if err != nil {
		return fmt.Errorf("eval export: resolve %s: %w", *outFlag, err)
	}
	if repo := enclosingGitRepo(filepath.Dir(out)); repo != "" {
		return fmt.Errorf("eval export: %s is inside the git repository %s; the dataset holds personal evidence, so write it outside any repository", out, repo)
	}
	store, err := timesheet.OpenStore(cfg.StatePath)
	if err != nil {
		return err
	}
	state := store.State()
	if err := store.Close(); err != nil {
		return fmt.Errorf("eval export: the store lock cannot be released")
	}
	g := exportGolden(cfg, state, time.Now())
	data, err := jsonBytes(g)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return fmt.Errorf("eval export: %s cannot be created: %w", filepath.Dir(out), err)
	}
	if err := writeFileAtomic(out, data); err != nil {
		return fmt.Errorf("eval export: %s cannot be written: %w", out, err)
	}
	return printJSON(evalExportReceipt{File: out, Days: len(g.Days), Skipped: g.Skipped, Note: g.Note})
}

// enclosingGitRepo returns the nearest ancestor of dir holding .git, or "".
func enclosingGitRepo(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if parent := filepath.Dir(d); parent == d {
			return ""
		}
	}
}

// exportGolden builds the dataset from every saved day. A day part whose
// input or truth is missing or stale is listed in Skipped, never included.
func exportGolden(cfg *timesheet.Config, state *timesheet.State, now time.Time) timesheet.GoldenDataset {
	g := timesheet.GoldenDataset{
		Schema: timesheet.GoldenSchema, ExportedAt: now.UTC().Format(time.RFC3339),
		Days: []timesheet.GoldenDay{}, Skipped: []timesheet.GoldenSkip{},
	}
	observed := map[[2]string]*timesheet.RemoteEntry{}
	for _, in := range state.Intents {
		if in.Status == timesheet.IntentConfirmed && in.Observed != nil {
			observed[[2]string{in.Date, in.LocalID}] = in.Observed
		}
	}
	dates := make([]string, 0, len(state.Days))
	for date, day := range state.Days {
		if day != nil && len(day.Confirmed) > 0 {
			dates = append(dates, date)
		}
	}
	sort.Strings(dates)
	skip := func(date, part, reason string) {
		g.Skipped = append(g.Skipped, timesheet.GoldenSkip{Date: date, Part: part, Reason: reason})
	}
	for _, date := range dates {
		artifacts, err := timesheet.ReadDayArtifacts(cfg.StatePath, date)
		if err != nil || ((artifacts.Entries == nil || len(artifacts.Entries.Entries) == 0) && len(artifacts.CarryIn) == 0) {
			skip(date, "day", "the entries ledger is missing or unreadable")
			continue
		}
		day := timesheet.GoldenDay{Date: date}
		// The day's stage input includes segments carried in from the previous
		// date (ADR 0011). Finalized days only: a stage map written under a
		// legacy overnight rule (#176 or before) is as current as today's.
		entries := artifactStageEntries(artifacts)
		tz := state.Days[date].Timezone
		var staged map[string]timesheet.EntryRecord
		for _, units := range append([][]ledgerUnit{ledgerUnits(entries, date, tz)}, legacyDerivations(entries, date, tz)...) {
			if stageMapMatchesUnits(artifacts.StageMap, units) {
				staged = stagedBlocksFrom(units, artifacts.StageMap)
				break
			}
		}

		if staged == nil {
			skip(date, "classifications", "the stage map no longer matches the entries ledger")
		} else if c, reason := goldenClassifications(cfg, date, state.Days[date].Confirmed, staged, observed); c == nil {
			skip(date, "classifications", reason)
		} else {
			day.Classifications = c
		}

		if d, reason := goldenDescriptions(cfg, date, artifacts, staged, observed); d == nil {
			skip(date, "descriptions", reason)
		} else {
			day.Descriptions = d
		}
		if day.Classifications != nil || day.Descriptions != nil {
			g.Days = append(g.Days, day)
		}
	}
	if len(dates) == 0 {
		g.Note = "the store holds no saved days yet, so the dataset is empty"
	}
	return g
}

// goldenClassifications records the classifications judgment as next built
// it: the staged blocks minus those decided from approved history, each naming
// its set from the day's candidates.json. Those sets came from live catalog
// reads at log time, so a ledger without bound candidates is skipped rather
// than re-read: the dataset replays offline (#279).
func goldenClassifications(cfg *timesheet.Config, date string, confirmed []timesheet.Interval, staged map[string]timesheet.EntryRecord, observed map[[2]string]*timesheet.RemoteEntry) (*timesheet.GoldenClassifications, string) {
	digest := timesheet.StagedLedgerDigest(date, staged)
	dir, err := dayArtifactsDir(cfg.StatePath, date)
	if err != nil {
		return nil, "the day folder cannot be resolved"
	}
	cands, err := readCandidatesFile(filepath.Join(dir, "candidates.json"))
	if err != nil || cands == nil || cands.LedgerDigest != digest {
		return nil, "no catalog candidates are recorded for this ledger (candidates.json is missing or stale), so the judgment cannot be replayed"
	}
	picked := boundHistory(cfg.StatePath, date, digest)
	judged := make(map[string]timesheet.EntryRecord, len(staged))
	for id, e := range staged {
		if _, ok := picked[id]; !ok {
			judged[id] = e
		}
	}
	if len(judged) == 0 {
		return nil, "every staged interval was classified from approved history, so the judge had nothing to pick"
	}
	blocks := judgmentBlocks(judged)
	c := &timesheet.GoldenClassifications{Candidates: []timesheet.GoldenCandidateSet{}, Truth: map[string]string{}}
	for _, s := range judgmentCandidates(cands, blocks) {
		set := timesheet.GoldenCandidateSet{Ref: s.Ref, CatalogID: s.CatalogID, Options: []timesheet.GoldenOption{}}
		for _, o := range s.Options {
			set.Options = append(set.Options, timesheet.GoldenOption{ClassificationID: o.ClassificationID, Label: o.Label, Code: o.Code})
		}
		c.Candidates = append(c.Candidates, set)
	}
	for _, b := range blocks {
		c.Input = append(c.Input, timesheet.GoldenBlock{
			IntervalID: b.IntervalID, BlockID: b.BlockID, Start: b.Start, End: b.End, Description: b.Description, Candidates: b.Candidates,
		})
	}
	for _, iv := range confirmed {
		if _, ok := judged[iv.ID]; !ok {
			continue
		}
		if obs := observed[[2]string{date, iv.ID}]; obs != nil && obs.ClassificationID != "" {
			c.Truth[iv.ID] = obs.ClassificationID
			c.RemoteTruth = append(c.RemoteTruth, iv.ID)
		} else if iv.RemoteClassificationID != "" {
			c.Truth[iv.ID] = iv.RemoteClassificationID
		}
	}
	if len(c.Truth) == 0 {
		return nil, "no saved interval carries an accepted classification"
	}
	sort.Strings(c.RemoteTruth)
	return c, ""
}

func goldenDescriptions(cfg *timesheet.Config, date string, artifacts timesheet.DayArtifacts, staged map[string]timesheet.EntryRecord, observed map[[2]string]*timesheet.RemoteEntry) (*timesheet.GoldenDescriptions, string) {
	dir, err := dayArtifactsDir(cfg.StatePath, date)
	if err != nil {
		return nil, "the day folder cannot be resolved"
	}
	data, err := os.ReadFile(filepath.Join(dir, "draft.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, "the draft artifact is missing"
	}
	var draft timesheet.Draft
	if err != nil || json.Unmarshal(data, &draft) != nil || len(draft.Blocks) == 0 {
		return nil, "the draft artifact is unreadable or empty"
	}
	if artifacts.Entries == nil {
		return nil, "the day has no entries ledger of its own (carried segments only)"
	}
	ledger := map[string]string{}
	for _, e := range artifacts.Entries.Entries {
		ledger[e.BlockID] = e.Description
	}
	// A block whose own text is one staged interval (not merged) can take the
	// submitted remote text as truth.
	single := map[string]string{}
	for blockID, ivs := range artifacts.StageMap {
		if len(ivs) == 1 && staged != nil && staged[ivs[0]].Description == ledger[blockID] {
			single[blockID] = ivs[0]
		}
	}
	d := &timesheet.GoldenDescriptions{Input: draft, Truth: map[string]string{}}
	for _, b := range draft.Blocks {
		text, ok := ledger[b.BlockID]
		if !ok {
			return nil, "the draft no longer matches the recorded entries (block " + b.BlockID + " is not in the ledger)"
		}
		if iv, ok := single[b.BlockID]; ok {
			if obs := observed[[2]string{date, iv}]; obs != nil && obs.Description != "" {
				text = obs.Description
				d.RemoteTruth = append(d.RemoteTruth, b.BlockID)
			}
		}
		d.Truth[b.BlockID] = text
	}
	sort.Strings(d.RemoteTruth)
	return d, ""
}

func runEvalScore(args []string) error {
	flags := flag.NewFlagSet("eval score", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	goldenFlag := flags.String("golden", "", "dataset written by eval export (required)")
	candidateFlag := flags.String("candidate", "", `answers to score: {"days":{"<date>":{"descriptions":{...},"classifications":{...}}}} (default: the accepted answers, a self-check)`)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *goldenFlag == "" {
		return fmt.Errorf("eval score requires --golden")
	}
	g, err := readGolden("eval score", *goldenFlag)
	if err != nil {
		return err
	}
	candidate := timesheet.AcceptedCandidate(g)
	if *candidateFlag != "" {
		candidate = timesheet.GoldenCandidate{}
		if err := readJSONFile(*candidateFlag, &candidate); err != nil {
			return fmt.Errorf("eval score: the candidate %s: %w", *candidateFlag, err)
		}
	}
	return printJSON(timesheet.ScoreGolden(g, candidate))
}

// readGolden reads a dataset and refuses one written under another schema.
func readGolden(verb, path string) (timesheet.GoldenDataset, error) {
	var g timesheet.GoldenDataset
	if err := readJSONFile(path, &g); err != nil {
		return g, fmt.Errorf("%s: the dataset %s: %w", verb, path, err)
	}
	if g.Schema != timesheet.GoldenSchema {
		return g, fmt.Errorf("%s: the dataset %s has schema %d, want %d; export it again", verb, path, g.Schema, timesheet.GoldenSchema)
	}
	return g, nil
}

func readJSONFile(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
