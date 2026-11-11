package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

var dateOptionalCommands = map[string]bool{
	"collect": true,
	"draft":   true,
	"run":     true,
	"record":  true,
	"status":  true,
	"next":    true,
}

func dateFlagUsage(name string) string {
	if dateOptionalCommands[name] {
		return "day to operate on, YYYY-MM-DD (optional; omitted means today in the configured timezone)"
	}
	return "day to operate on, YYYY-MM-DD (required)"
}

// dateSupplied reports whether the caller gave --date at all. The flag package
// stores the same "" for an omitted flag and an explicit --date "", so the only
// honest signal is which flags it recorded as set during Parse.
func dateSupplied(flags *flag.FlagSet) bool {
	supplied := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "date" {
			supplied = true
		}
	})
	return supplied
}

func validateDate(name, date string, supplied bool) error {
	if !supplied {
		if !dateOptionalCommands[name] {
			return fmt.Errorf("%s requires --date", name)
		}
		return nil
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return fmt.Errorf("%s: --date must be YYYY-MM-DD", name)
	}
	return nil
}

type headlessFlags struct {
	configPath string
	envFile    string
	date       string
}

func parseHeadlessFlags(name string, args []string) (*headlessFlags, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	f := &headlessFlags{}
	flags.StringVar(&f.configPath, "config", "", "configuration path (optional; defaults to $HOME/.timesheet/config.json)")
	flags.StringVar(&f.envFile, "env-file", "", "environment-file path (optional; defaults to $HOME/.timesheet/.env.local when it exists)")
	flags.StringVar(&f.date, "date", "", dateFlagUsage(name))
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("%s does not accept positional arguments", name)
	}
	configPath, envFile, err := resolveStoreFlags(name, f.configPath, f.envFile)
	if err != nil {
		return nil, err
	}
	f.configPath, f.envFile = configPath, envFile
	if err := validateDate(name, f.date, dateSupplied(flags)); err != nil {
		return nil, err
	}
	return f, nil
}

type recordFlags struct {
	configPath   string
	envFile      string
	date         string
	file         string
	draft        string
	descriptions string
	replace      bool
}

func parseRecordFlags(args []string) (*recordFlags, error) {
	flags := flag.NewFlagSet("record", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	f := &recordFlags{}
	flags.StringVar(&f.configPath, "config", "", "configuration path (optional; defaults to $HOME/.timesheet/config.json)")
	flags.StringVar(&f.envFile, "env-file", "", "environment-file path (optional; defaults to $HOME/.timesheet/.env.local when it exists)")
	flags.StringVar(&f.date, "date", "", dateFlagUsage("record"))
	flags.StringVar(&f.file, "file", "", "reviewed draft JSON path (recovery input mode)")
	flags.StringVar(&f.draft, "draft", "", "draft JSON path from `timesheet draft` (descriptions-only input mode, with --descriptions)")
	flags.StringVar(&f.descriptions, "descriptions", "", "descriptions-only JSON path (descriptions-only input mode, with --draft)")
	flags.BoolVar(&f.replace, "replace", false, "the reviewed result is the day's complete content: replace all blocks for the resolved day instead of merging (re-collected day; the new generation supersedes the old)")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("record does not accept positional arguments")
	}
	configPath, envFile, err := resolveStoreFlags("record", f.configPath, f.envFile)
	if err != nil {
		return nil, err
	}
	f.configPath, f.envFile = configPath, envFile
	if err := validateDate("record", f.date, dateSupplied(flags)); err != nil {
		return nil, err
	}
	if f.file != "" && (f.draft != "" || f.descriptions != "") {
		return nil, fmt.Errorf("record takes either --file or --draft with --descriptions, not both input modes")
	}
	switch {
	case f.file != "":
	case f.draft != "" && f.descriptions != "":
	default:
		return nil, fmt.Errorf("record requires either --file or both --draft and --descriptions")
	}
	return f, nil
}

type headlessSession struct {
	session *timesheet.Session
	store   *timesheet.Store
	cfg     *timesheet.Config
	seq     int
	close   func()
}

func openHeadless(configPath, envFile string) (*headlessSession, error) {
	cfg, err := timesheet.LoadConfig(configPath)
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	if envFile != "" {
		fileEnv, err := timesheet.LoadEnvFile(envFile)
		if err != nil {
			return nil, err
		}
		env = timesheet.MergeEnv(fileEnv)
	}
	store, err := timesheet.OpenStore(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	return &headlessSession{
		session: timesheet.NewSession(cfg, env, store),
		store:   store,
		cfg:     cfg,
		close:   func() { _ = store.Close() },
	}, nil
}

func (h *headlessSession) done() { h.close() }

func (h *headlessSession) call(op string, req timesheet.Request) (any, error) {
	h.seq++
	req.ID = fmt.Sprintf("headless-%d", h.seq)
	req.Op = op
	line, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	resp, _ := h.session.HandleLine(line)
	if resp.Error != nil {
		return nil, &timesheet.AppError{Code: resp.Error.Code, Message: resp.Error.Message}
	}
	return resp.Result, nil
}

func jsonBytes(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func printJSON(v any) error {
	data, err := jsonBytes(v)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(data)
	return err
}

func runCollect(args []string) error {
	f, err := parseHeadlessFlags("collect", args)
	if err != nil {
		return err
	}
	day, err := resolveDay(f.configPath, f.date)
	if err != nil {
		return err
	}
	h, err := openHeadless(f.configPath, f.envFile)
	if err != nil {
		return err
	}
	defer h.done()
	if _, err := h.call("open_day", timesheet.Request{Day: day}); err != nil {
		return err
	}
	res, err := h.call("collect", timesheet.Request{Day: day})
	if err != nil {
		return err
	}
	if result, ok := res.(timesheet.CollectResult); ok && result.CollectionID != "" {
		fmt.Fprintf(os.Stderr, "collect: collection_id %s\n", result.CollectionID)
	}
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(res)
}

func buildDayDraft(configPath, envFile, date string) (timesheet.Draft, string, error) {
	h, err := openHeadless(configPath, envFile)
	if err != nil {
		return timesheet.Draft{}, "", err
	}
	defer h.done()
	draft, collectionID, err := collectDraft(h, date)
	if err != nil {
		return timesheet.Draft{}, "", err
	}
	_, _ = h.call("close", timesheet.Request{})
	return draft, collectionID, nil
}

func collectDraft(h *headlessSession, day string) (timesheet.Draft, string, error) {
	if _, err := h.call("open_day", timesheet.Request{Day: day}); err != nil {
		return timesheet.Draft{}, "", err
	}
	res, err := h.call("collect", timesheet.Request{Day: day})
	if err != nil {
		return timesheet.Draft{}, "", err
	}
	collectionID := ""
	if result, ok := res.(timesheet.CollectResult); ok {
		collectionID = result.CollectionID
	}
	res, err = h.call("review", timesheet.Request{Day: day})
	if err != nil {
		return timesheet.Draft{}, "", err
	}
	review, ok := res.(timesheet.Review)
	if !ok {
		return timesheet.Draft{}, "", fmt.Errorf("draft: unexpected review payload")
	}
	draft := timesheet.BuildDraft(review, timesheet.AppVersion)
	// Work-hours annotation is presentation-only: it reads the loaded config's
	// optional policy and the day's recorded timezone, and leaves every block
	// untouched when no policy is configured. A day whose timezone cannot be
	// loaded is left unannotated rather than annotated in a guessed zone; the
	// review already fails earlier for such a day, so this is defence in depth.
	if h.cfg != nil && h.cfg.WorkHours != nil {
		if loc := timesheet.DayLocationOrNil(review.Timezone); loc != nil {
			timesheet.AnnotateWorkHours(&draft, h.cfg.WorkHours, loc)
		}
	}
	return draft, collectionID, nil
}

var clockNow = time.Now

func resolveDay(configPath, date string) (string, error) {
	if date != "" {
		return date, nil
	}
	cfg, err := timesheet.LoadConfig(configPath)
	if err != nil {
		return "", err
	}
	return timesheet.TodayIn(cfg.Timezone, clockNow())
}

func runDraft(args []string) error {
	f, err := parseHeadlessFlags("draft", args)
	if err != nil {
		return err
	}
	day, err := resolveDay(f.configPath, f.date)
	if err != nil {
		return err
	}
	draft, _, err := buildDayDraft(f.configPath, f.envFile, day)
	if err != nil {
		return err
	}
	return printJSON(draft)
}

func runRun(args []string) error {
	f, err := parseHeadlessFlags("run", args)
	if err != nil {
		return err
	}
	day, err := resolveDay(f.configPath, f.date)
	if err != nil {
		return err
	}
	draft, collectionID, err := buildDayDraft(f.configPath, f.envFile, day)
	if err != nil {
		return err
	}
	data, err := jsonBytes(draft)
	if err != nil {
		return err
	}
	cfg, err := timesheet.LoadConfig(f.configPath)
	if err != nil {
		return err
	}
	dir, err := dayArtifactsDir(cfg.StatePath, day)
	if err != nil {
		return err
	}
	if err := ensureDayArtifactsDir("run", dir); err != nil {
		return err
	}
	artifacts := filepath.Join(dir, "draft.json")
	if err := os.WriteFile(artifacts, data, 0o600); err != nil {
		return fmt.Errorf("run: the draft file %s cannot be written: %w", artifacts, err)
	}
	if collectionID != "" {
		fmt.Fprintf(os.Stderr, "run: collection_id %s\n", collectionID)
	}
	fmt.Fprintf(os.Stderr, "run: artifacts: %s\n", artifacts)
	_, err = os.Stdout.Write(data)
	return err
}

func runRecord(args []string) error {
	f, err := parseRecordFlags(args)
	if err != nil {
		return err
	}
	var reviewed timesheet.ReviewedDraft
	if f.draft != "" {
		if reviewed, err = composeDescriptionsOnly(f.draft, f.descriptions); err != nil {
			return err
		}
	} else {
		data, err := os.ReadFile(f.file)
		if err != nil {
			return fmt.Errorf("record: the reviewed draft file cannot be read")
		}
		if err := json.Unmarshal(data, &reviewed); err != nil {
			return fmt.Errorf("record: the reviewed draft is not valid JSON for the expected schema")
		}
	}
	cfg, err := timesheet.LoadConfig(f.configPath)
	if err != nil {
		return err
	}
	day, err := resolveDay(f.configPath, f.date)
	if err != nil {
		return err
	}
	if reviewed.Date != day {
		return fmt.Errorf("record: the reviewed draft's date %s does not match the resolved day %s", reviewed.Date, day)
	}
	receipt, err := timesheet.RecordEntries(cfg.StatePath, timesheet.AppVersion, reviewed, f.replace, time.Now())
	if err != nil {
		return err
	}
	return printJSON(receipt)
}

func composeDescriptionsOnly(draftPath, descriptionsPath string) (timesheet.ReviewedDraft, error) {
	draftData, err := os.ReadFile(draftPath)
	if err != nil {
		return timesheet.ReviewedDraft{}, fmt.Errorf("record: the draft file cannot be read")
	}
	var draft timesheet.Draft
	if err := json.Unmarshal(draftData, &draft); err != nil {
		return timesheet.ReviewedDraft{}, fmt.Errorf("record: the draft file is not valid JSON for the expected draft schema")
	}
	descData, err := os.ReadFile(descriptionsPath)
	if err != nil {
		return timesheet.ReviewedDraft{}, fmt.Errorf("record: the descriptions file cannot be read")
	}
	desc, err := timesheet.ParseDescriptions(descData)
	if err != nil {
		return timesheet.ReviewedDraft{}, err
	}
	return timesheet.ComposeReviewed(draft, desc)
}

func runStatus(args []string) error {
	f, err := parseHeadlessFlags("status", args)
	if err != nil {
		return err
	}
	day, err := resolveDay(f.configPath, f.date)
	if err != nil {
		return err
	}
	h, err := openHeadless(f.configPath, f.envFile)
	if err != nil {
		return err
	}
	defer h.done()
	if _, err := h.call("open_day", timesheet.Request{Day: day}); err != nil {
		return err
	}
	res, err := h.call("status", timesheet.Request{})
	if err != nil {
		return err
	}
	env, err := h.call("check_environment", timesheet.Request{})
	if err != nil {
		return err
	}
	_, _ = h.call("close", timesheet.Request{})
	entries, err := timesheet.ReadEntries(h.cfg.StatePath, day)
	if err != nil {
		return err
	}
	summary := map[string]any{
		"status":      res,
		"environment": env,
		"date":        day,
		"entries":     0,
	}
	if entries != nil {
		summary["entries"] = len(entries.Entries)
		lines := make([]string, 0, len(entries.Entries))
		for _, e := range entries.Entries {
			lines = append(lines, fmt.Sprintf("%s-%s %s", e.Start, e.End, e.Description))
		}
		summary["recorded"] = lines
		summary["entries_file"] = timesheet.EntriesPath(h.cfg.StatePath, day)
	}
	if choices, err := timesheet.ReadClassificationChoices(h.cfg.StatePath, day); err == nil && len(choices) > 0 {
		summary["classifications"] = statusClassifications(choices)
	}
	return printJSON(summary)
}

type finalizeFlags struct {
	configuredDefault bool
	fromHistory       bool
	configPath        string
	envFile           string
	date              string
	file              string
	revise            bool
}

func parseFinalizeFlags(name string, args []string) (*finalizeFlags, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	f := &finalizeFlags{}
	flags.StringVar(&f.configPath, "config", "", "configuration path (optional; defaults to $HOME/.timesheet/config.json)")
	flags.StringVar(&f.envFile, "env-file", "", "environment-file path (optional; defaults to $HOME/.timesheet/.env.local when it exists)")
	flags.StringVar(&f.date, "date", "", dateFlagUsage(name))
	flags.BoolVar(&f.revise, "revise", false, "stage only: replace a finalized day's confirmed intervals with a fresh derivation from the entries ledger and re-finalize at a new revision")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("%s does not accept positional arguments", name)
	}
	configPath, envFile, err := resolveStoreFlags(name, f.configPath, f.envFile)
	if err != nil {
		return nil, err
	}
	f.configPath, f.envFile = configPath, envFile
	if err := validateDate(name, f.date, dateSupplied(flags)); err != nil {
		return nil, err
	}
	return f, nil
}

// acknowledgeFlags carries the acknowledge inputs. The collection_id must be
// copied verbatim from a prior collect receipt; the acknowledge_source op
// fails closed on a missing or stale id and this command passes that error
// through untouched.
type acknowledgeFlags struct {
	configPath   string
	envFile      string
	date         string
	source       string
	collectionID string
}

func parseAcknowledgeFlags(args []string) (*acknowledgeFlags, error) {
	flags := flag.NewFlagSet("acknowledge", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	f := &acknowledgeFlags{}
	flags.StringVar(&f.configPath, "config", "", "configuration path (optional; defaults to $HOME/.timesheet/config.json)")
	flags.StringVar(&f.envFile, "env-file", "", "environment-file path (optional; defaults to $HOME/.timesheet/.env.local when it exists)")
	flags.StringVar(&f.date, "date", "", "day to operate on, YYYY-MM-DD (required)")
	flags.StringVar(&f.source, "source", "", "collected source being reviewed: github or calendar (required)")
	flags.StringVar(&f.collectionID, "collection-id", "", "collection_id copied verbatim from the prior collect receipt (required)")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("acknowledge does not accept positional arguments")
	}
	configPath, envFile, err := resolveStoreFlags("acknowledge", f.configPath, f.envFile)
	if err != nil {
		return nil, err
	}
	f.configPath, f.envFile = configPath, envFile
	if f.date == "" {
		return nil, fmt.Errorf("acknowledge requires --date")
	}
	if _, err := time.Parse("2006-01-02", f.date); err != nil {
		return nil, fmt.Errorf("acknowledge: --date must be YYYY-MM-DD")
	}
	if f.source != timesheet.SourceGithub && f.source != timesheet.SourceCalendar {
		return nil, fmt.Errorf("acknowledge: --source must be github or calendar")
	}
	if f.collectionID == "" {
		return nil, fmt.Errorf("acknowledge requires --collection-id copied verbatim from the prior collect receipt")
	}
	return f, nil
}

// decideFlags carries the decide inputs. The decision surface here is the
// attendance verdict only (attended or skipped); the protocol's pending value
// is a collection-time state, never a CLI decision.
type decideFlags struct {
	configPath   string
	envFile      string
	date         string
	meetingID    string
	decision     string
	collectionID string
}

func parseDecideFlags(args []string) (*decideFlags, error) {
	flags := flag.NewFlagSet("decide", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	f := &decideFlags{}
	flags.StringVar(&f.configPath, "config", "", "configuration path (optional; defaults to $HOME/.timesheet/config.json)")
	flags.StringVar(&f.envFile, "env-file", "", "environment-file path (optional; defaults to $HOME/.timesheet/.env.local when it exists)")
	flags.StringVar(&f.date, "date", "", "day to operate on, YYYY-MM-DD (required)")
	flags.StringVar(&f.meetingID, "meeting-id", "", "meeting ref copied verbatim from the reviewed draft or collect receipt (required)")
	flags.StringVar(&f.decision, "decision", "", "attendance decision: attended or skipped (required)")
	flags.StringVar(&f.collectionID, "collection-id", "", "collection_id copied verbatim from the prior collect receipt (required)")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("decide does not accept positional arguments")
	}
	configPath, envFile, err := resolveStoreFlags("decide", f.configPath, f.envFile)
	if err != nil {
		return nil, err
	}
	f.configPath, f.envFile = configPath, envFile
	if f.date == "" {
		return nil, fmt.Errorf("decide requires --date")
	}
	if _, err := time.Parse("2006-01-02", f.date); err != nil {
		return nil, fmt.Errorf("decide: --date must be YYYY-MM-DD")
	}
	if f.meetingID == "" {
		return nil, fmt.Errorf("decide requires --meeting-id")
	}
	if f.decision != "attended" && f.decision != "skipped" {
		return nil, fmt.Errorf("decide: --decision must be attended or skipped")
	}
	if f.collectionID == "" {
		return nil, fmt.Errorf("decide requires --collection-id copied verbatim from the prior collect receipt")
	}
	return f, nil
}

// StageBlockRef pairs one recorded block with a staged interval it maps to.
// Blocks the Gradion four-hour policy split into chunks (issue #167) carry one
// ref per chunk: Chunk is the 1-based chunk number and Chunks the total, both
// omitted for unsplit single-interval blocks.
type StageBlockRef struct {
	BlockID    string `json:"block_id,omitempty"`
	IntervalID string `json:"interval_id,omitempty"`
	Chunk      int    `json:"chunk,omitempty"`
	Chunks     int    `json:"chunks,omitempty"`
}

// chunkRef returns the (chunk, chunks) annotation for one staged interval of a
// block rendered as n spans: (0, 0) — omitted — when the block was not split.
func chunkRef(i, n int) (int, int) {
	if n <= 1 {
		return 0, 0
	}
	return i + 1, n
}

// StageSkip is one recorded entry stage did not convert, with the reason.
// Entries without time anchors are listed honestly and never given invented
// times; a contributor whose span was merged into an overlapping cluster is
// listed naming the head, so a merge never silently drops a block.
type StageSkip struct {
	BlockID string `json:"block_id"`
	Reason  string `json:"reason"`
}

// StageReceipt is stage's stdout receipt. Every list is deterministic: staged,
// updated, unchanged and removed follow head block_id order, and skipped lists
// the day's unusable entries in block_id order, then the merged-away
// contributors.
type StageReceipt struct {
	Date            string          `json:"date"`
	EntriesFile     string          `json:"entries_file"`
	StageFile       string          `json:"stage_file"`
	Staged          []StageBlockRef `json:"staged"`
	Updated         []StageBlockRef `json:"updated"`
	Unchanged       []StageBlockRef `json:"unchanged"`
	Removed         []StageBlockRef `json:"removed"`
	Skipped         []StageSkip     `json:"skipped"`
	Revision        int             `json:"revision"`
	StagedIntervals int             `json:"staged_intervals"`
	// --revise only: the finalized set this stage replaced, and the new durable
	// revision the day was re-finalized at.
	Replaced    int    `json:"replaced,omitempty"`
	Refinalized bool   `json:"refinalized,omitempty"`
	Note        string `json:"note,omitempty"`
	// The overnight carry (issue #277, ADR 0011): segments carried in from the
	// previous date and staged here, and segments this day's derivation
	// carries into the next date. CarryNote flags a carry into a saved day.
	CarriedIn  []StageCarry `json:"carried_in,omitempty"`
	CarriedOut []StageCarry `json:"carried_out,omitempty"`
	CarryNote  string       `json:"carry_note,omitempty"`
}

type StageCarry struct {
	From        string `json:"from"`
	To          string `json:"to"`
	BlockID     string `json:"block_id"`
	Start       string `json:"start"`
	End         string `json:"end"`
	Minutes     int    `json:"minutes"`
	Description string `json:"description"`
}

func stageCarries(carries []timesheet.CarrySegment) []StageCarry {
	var out []StageCarry
	for _, c := range carries {
		out = append(out, StageCarry{
			From: c.SourceDate, To: c.Date, BlockID: c.BlockID(), Start: c.Start, End: c.End,
			Minutes: c.Minutes, Description: c.Description,
		})
	}
	return out
}

func applyStage(h *headlessSession, cfg *timesheet.Config, name, date string, summary timesheet.DaySummary) (*StageReceipt, error) {
	revision := summary.Revision
	stagedIDs := map[string]bool{}
	segmentByID := map[string]timesheet.Segment{}
	for _, seg := range summary.Staged.Segments {
		if seg.OriginDate == date {
			stagedIDs[seg.IntervalID] = true
			segmentByID[seg.IntervalID] = seg
		}
	}
	ledger, err := timesheet.ReadEntries(cfg.StatePath, date)
	if err != nil {
		return nil, err
	}
	carryIn, err := timesheet.ReadCarryInto(cfg.StatePath, date)
	if err != nil {
		return nil, err
	}
	if err := timesheet.CheckCarryTimezone(carryIn, summary.Timezone); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if err := checkCarryIn(cfg.StatePath, date, carryIn); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if ledger == nil && len(carryIn) == 0 {
		return nil, fmt.Errorf("%s: no recorded entries file for %s; run record first", name, date)
	}
	var own []timesheet.EntryRecord
	if ledger != nil {
		own = ledger.Entries
	}
	entries := dayStageEntries(own, carryIn)
	stageMap, err := timesheet.ReadStageMap(cfg.StatePath, date)
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(entries))
	byID := make(map[string]timesheet.EntryRecord, len(entries))
	for _, e := range entries {
		ids = append(ids, e.BlockID)
		byID[e.BlockID] = e
	}
	sort.Strings(ids)

	// The Gradion remote forbids overlapping entries, so the bridge resolves
	// the day's spans before deriving intervals: entries that strictly overlap
	// become one cluster over the union span (issue #183), its intervals carry
	// the joined description, and every merged-away contributor is reported
	// instead of staged. The entries ledger keeps the human's overlapping
	// truth untouched. An overnight entry takes part like any other (issue
	// #176). Each union span is then chunked to the four-hour policy below.
	clusters, dayLongIDs, pastNextIDs := timesheet.ResolveEntrySpansOn(date, entries)
	dayLong := make(map[string]bool, len(dayLongIDs))
	for _, id := range dayLongIDs {
		dayLong[id] = true
	}
	pastNext := make(map[string]bool, len(pastNextIDs))
	for _, id := range pastNextIDs {
		pastNext[id] = true
	}

	receipt := &StageReceipt{
		Date:        date,
		EntriesFile: timesheet.EntriesPath(cfg.StatePath, date),
		StageFile:   timesheet.StagePath(cfg.StatePath, date),
		Staged:      []StageBlockRef{},
		Updated:     []StageBlockRef{},
		Unchanged:   []StageBlockRef{},
		Removed:     []StageBlockRef{},
		Skipped:     []StageSkip{},
	}

	type stageUnit struct {
		id    string
		text  string
		spans [][2]string
	}
	var units []stageUnit
	for _, id := range ids {
		e := byID[id]
		if e.Start == "" || e.End == "" {
			missing := "start"
			if e.Start != "" {
				missing = "end"
			}
			receipt.Skipped = append(receipt.Skipped, StageSkip{
				BlockID: id,
				Reason:  fmt.Sprintf("entry has no %s time (kind %s); times are never invented", missing, e.Kind),
			})
			continue
		}
		startMin, endMin, ok := timesheet.EntrySpanBounds(e.Start, e.End)
		if !ok {
			receipt.Skipped = append(receipt.Skipped, StageSkip{
				BlockID: id,
				Reason:  fmt.Sprintf("entry's times cannot be parsed (start %q, end %q); times are never invented", e.Start, e.End),
			})
			continue
		}
		if timesheet.EntrySpansADayOrMore(e.Start, e.End) {
			receipt.Skipped = append(receipt.Skipped, StageSkip{
				BlockID: id,
				Reason:  fmt.Sprintf("entry spans 24 hours or more (start %q, end %q); an interval crosses at most one midnight and is never truncated to the entry's clocks; times are never invented", e.Start, e.End),
			})
			continue
		}
		if endMin == startMin {
			receipt.Skipped = append(receipt.Skipped, StageSkip{
				BlockID: id,
				Reason:  fmt.Sprintf("entry has zero duration (start %q, end %q); times are never invented", e.Start, e.End),
			})
			continue
		}
		if dayLong[id] {
			receipt.Skipped = append(receipt.Skipped, StageSkip{
				BlockID: id,
				Reason:  fmt.Sprintf("overlapping entries span 24 hours or more (this entry: start %q, end %q); an interval crosses at most one midnight, so they are never merged or truncated; times are never invented", e.Start, e.End),
			})
		}
		if pastNext[id] {
			// Stamped on the next date and running past that date's
			// midnight: a carry crosses at most one midnight (issue #277).
			receipt.Skipped = append(receipt.Skipped, StageSkip{
				BlockID: id,
				Reason:  fmt.Sprintf("entry runs past the next date's midnight (start %q, end %q); a carry crosses at most one midnight, so it is never carried or truncated; times are never invented", e.Start, e.End),
			})
		}
	}
	sort.Slice(clusters, func(i, j int) bool { return clusters[i].BlockID < clusters[j].BlockID })
	var carries []timesheet.CarrySegment
	for _, c := range clusters {
		spans, carry := timesheet.SplitCluster(date, summary.Timezone, c)
		if carry != nil {
			carries = append(carries, *carry)
		}
		if len(spans) == 0 {
			receipt.Skipped = append(receipt.Skipped, StageSkip{
				BlockID: c.BlockID,
				Reason:  fmt.Sprintf("starts after midnight (stamped on %s): carried whole into %s as block %s", carry.Date, carry.Date, carry.BlockID()),
			})
		} else {
			units = append(units, stageUnit{id: c.BlockID, text: c.Description, spans: spans})
		}
		for _, merged := range c.BlockIDs[1:] {
			receipt.Skipped = append(receipt.Skipped, StageSkip{
				BlockID: merged,
				Reason:  fmt.Sprintf("merged into block %s: its minutes are covered by the union span", c.BlockID),
			})
		}
	}

	next := map[string][]string{}
	desired := map[string]bool{}
	for _, u := range units {
		id := u.id
		spans := u.spans
		desired[id] = true
		mapped := stageMap[id]
		if len(mapped) == len(spans) && len(spans) > 0 {
			inPlace := true
			for _, ivID := range mapped {
				if !stagedIDs[ivID] {
					inPlace = false
					break
				}
			}
			if inPlace {
				for i, span := range spans {
					ch, cs := chunkRef(i, len(spans))
					seg := segmentByID[mapped[i]]
					if seg.Start == span[0] && seg.End == span[1] && seg.Description == u.text {
						receipt.Unchanged = append(receipt.Unchanged, StageBlockRef{BlockID: id, IntervalID: mapped[i], Chunk: ch, Chunks: cs})
						continue
					}
					text := fmt.Sprintf("%s %s-%s %s", date, span[0], span[1], u.text)
					if _, err := h.call("edit_interval", timesheet.Request{
						Day: date, Revision: &revision, IntervalID: mapped[i], Text: text,
					}); err != nil {
						return nil, err
					}
					receipt.Updated = append(receipt.Updated, StageBlockRef{BlockID: id, IntervalID: mapped[i], Chunk: ch, Chunks: cs})
				}
				next[id] = mapped
				continue
			}
		}
		for _, ivID := range mapped {
			if stagedIDs[ivID] {
				if _, err := h.call("remove_interval", timesheet.Request{
					Day: date, Revision: &revision, IntervalID: ivID,
				}); err != nil {
					return nil, err
				}
				receipt.Removed = append(receipt.Removed, StageBlockRef{BlockID: id, IntervalID: ivID})
			}
		}
		added := make([]string, 0, len(spans))
		for i, span := range spans {
			text := fmt.Sprintf("%s %s-%s %s", date, span[0], span[1], u.text)
			res, err := h.call("add_interval", timesheet.Request{Day: date, Revision: &revision, Text: text})
			if err != nil {
				return nil, err
			}
			mutation, ok := res.(timesheet.IntervalMutation)
			if !ok {
				return nil, fmt.Errorf("%s: unexpected add_interval payload", name)
			}
			ch, cs := chunkRef(i, len(spans))
			receipt.Staged = append(receipt.Staged, StageBlockRef{BlockID: id, IntervalID: mutation.IntervalID, Chunk: ch, Chunks: cs})
			added = append(added, mutation.IntervalID)
		}
		next[id] = added
	}

	stale := make([]string, 0, len(stageMap))
	for id := range stageMap {
		if !desired[id] {
			stale = append(stale, id)
		}
	}
	sort.Strings(stale)
	for _, id := range stale {
		for _, ivID := range stageMap[id] {
			if stagedIDs[ivID] {
				if _, err := h.call("remove_interval", timesheet.Request{
					Day: date, Revision: &revision, IntervalID: ivID,
				}); err != nil {
					return nil, err
				}
				receipt.Removed = append(receipt.Removed, StageBlockRef{BlockID: id, IntervalID: ivID})
			}
		}
	}

	if err := timesheet.WriteStageMap(cfg.StatePath, timesheet.AppVersion, date, next, time.Now()); err != nil {
		return nil, err
	}
	if err := timesheet.WriteCarry(cfg.StatePath, timesheet.AppVersion, date, carries, time.Now()); err != nil {
		return nil, err
	}
	receipt.CarriedIn = stageCarries(carryIn)
	receipt.CarriedOut = stageCarries(carries)
	if len(carries) > 0 {
		target := carries[0].Date
		if day := h.store.State().Days[target]; day != nil && len(day.Confirmed) > 0 {
			receipt.CarryNote = fmt.Sprintf("%s is already saved; a carry is never applied to a saved day silently: "+
				"if its saved intervals do not include this carry, next stops on %s until the human runs `stage --revise --date %s`",
				target, target, target)
		}
	}
	res, err := h.call("open_day", timesheet.Request{Day: date})
	if err != nil {
		return nil, err
	}
	final, ok := res.(timesheet.DaySummary)
	if !ok {
		return nil, fmt.Errorf("%s: unexpected open_day payload", name)
	}
	for _, seg := range final.Staged.Segments {
		if seg.OriginDate == date {
			receipt.StagedIntervals++
		}
	}
	receipt.Revision = final.Revision
	return receipt, nil
}

func finalizedSummary(summary timesheet.DaySummary) bool {
	for _, seg := range summary.Confirmed.Segments {
		if seg.OriginDate == summary.Date {
			return true
		}
	}
	return false
}

func openFinalizeDay(h *headlessSession, name, date string) (timesheet.DaySummary, error) {
	res, err := h.call("open_day", timesheet.Request{Day: date})
	if err != nil {
		return timesheet.DaySummary{}, err
	}
	summary, ok := res.(timesheet.DaySummary)
	if !ok {
		return timesheet.DaySummary{}, fmt.Errorf("%s: unexpected open_day payload", name)
	}
	if finalizedSummary(summary) {
		return timesheet.DaySummary{}, fmt.Errorf(
			"%s: %s already has confirmed intervals; the day is finalized and confirmed work is never restaged", name, date)
	}
	return summary, nil
}

func noteStageSkips(name string, receipt *StageReceipt) {
	for _, s := range receipt.Skipped {
		fmt.Fprintf(os.Stderr, "%s: skipped block %s: %s\n", name, s.BlockID, s.Reason)
	}
}

func runStage(args []string) error {
	f, err := parseFinalizeFlags("stage", args)
	if err != nil {
		return err
	}
	cfg, err := timesheet.LoadConfig(f.configPath)
	if err != nil {
		return err
	}
	h, err := openHeadless(f.configPath, f.envFile)
	if err != nil {
		return err
	}
	defer h.done()
	res, err := h.call("open_day", timesheet.Request{Day: f.date})
	if err != nil {
		return err
	}
	summary, ok := res.(timesheet.DaySummary)
	if !ok {
		return fmt.Errorf("stage: unexpected open_day payload")
	}
	var receipt *StageReceipt
	if finalizedSummary(summary) {
		if !f.revise {
			return fmt.Errorf(
				"stage: %s already has confirmed intervals; the day is finalized and confirmed work is never restaged (stage --revise replaces the finalized set from the entries ledger and re-finalizes at a new revision)", f.date)
		}
		receipt, err = reviseStage(h, cfg, f.date, summary)
	} else {
		receipt, err = applyStage(h, cfg, "stage", f.date, summary)
	}
	if err != nil {
		return err
	}
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(receipt)
}

func reviseStage(h *headlessSession, cfg *timesheet.Config, date string, summary timesheet.DaySummary) (*StageReceipt, error) {
	revision := summary.Revision
	removed := make([]StageBlockRef, 0, len(summary.Staged.Segments))
	for _, seg := range summary.Staged.Segments {
		if seg.OriginDate != date {
			continue
		}
		if _, err := h.call("remove_interval", timesheet.Request{
			Day: date, Revision: &revision, IntervalID: seg.IntervalID,
		}); err != nil {
			return nil, err
		}
		removed = append(removed, StageBlockRef{IntervalID: seg.IntervalID})
	}
	res, err := h.call("open_day", timesheet.Request{Day: date})
	if err != nil {
		return nil, err
	}
	fresh, ok := res.(timesheet.DaySummary)
	if !ok {
		return nil, fmt.Errorf("stage: unexpected open_day payload")
	}
	receipt, err := applyStage(h, cfg, "stage", date, fresh)
	if err != nil {
		return nil, err
	}
	receipt.Removed = append(removed, receipt.Removed...)
	confirmed, err := h.call("confirm", timesheet.Request{Day: date, Revision: &summary.Revision})
	if err != nil {
		return nil, err
	}
	confirmReceipt, ok := confirmed.(timesheet.ConfirmReceipt)
	if !ok {
		return nil, fmt.Errorf("stage: unexpected confirm payload")
	}
	saved, err := h.call("save", timesheet.Request{Day: date, Revision: &confirmReceipt.Revision})
	if err != nil {
		return nil, err
	}
	saveReceipt, ok := saved.(timesheet.SaveReceipt)
	if !ok {
		return nil, fmt.Errorf("stage: unexpected save payload")
	}
	replaced := 0
	for _, seg := range summary.Confirmed.Segments {
		if seg.OriginDate == date {
			replaced++
		}
	}
	receipt.Replaced = replaced
	receipt.Refinalized = true
	receipt.Revision = saveReceipt.Revision
	receipt.Note = fmt.Sprintf("day was already finalized; %d confirmed interval(s) were replaced by a fresh derivation from the entries ledger and the day was re-finalized at revision %d", replaced, saveReceipt.Revision)
	return receipt, nil
}

// runAcknowledge wraps the acknowledge_source op: it marks this day's recorded
// attempt for the named source as reviewed, bound to the exact collection_id
// from the prior collect receipt. A missing or stale id fails closed in the op
// and the error is passed through untouched.
func runAcknowledge(args []string) error {
	f, err := parseAcknowledgeFlags(args)
	if err != nil {
		return err
	}
	h, err := openHeadless(f.configPath, f.envFile)
	if err != nil {
		return err
	}
	defer h.done()
	if _, err := h.call("open_day", timesheet.Request{Day: f.date}); err != nil {
		return err
	}
	res, err := h.call("acknowledge_source", timesheet.Request{
		Day: f.date, Source: f.source, CollectionID: f.collectionID,
	})
	if err != nil {
		return err
	}
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(res)
}

// runDecide wraps the decide_meeting op: it records the explicit attended or
// skipped decision for one proposed meeting, bound to the exact collection_id
// from the prior collect receipt. A missing or stale id fails closed in the op
// and the error is passed through untouched.
func runDecide(args []string) error {
	f, err := parseDecideFlags(args)
	if err != nil {
		return err
	}
	h, err := openHeadless(f.configPath, f.envFile)
	if err != nil {
		return err
	}
	defer h.done()
	if _, err := h.call("open_day", timesheet.Request{Day: f.date}); err != nil {
		return err
	}
	res, err := h.call("decide_meeting", timesheet.Request{
		Day: f.date, MeetingID: f.meetingID, Decision: f.decision, CollectionID: f.collectionID,
	})
	if err != nil {
		return err
	}
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(res)
}

func runClassifications(args []string) error {
	flags := flag.NewFlagSet("classifications", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configFlag := flags.String("config", "", "configuration path (optional; defaults to $HOME/.timesheet/config.json)")
	envFlag := flags.String("env-file", "", "environment-file path (optional; defaults to $HOME/.timesheet/.env.local when it exists)")
	query := flags.String("query", "", "search text for the bounded classification read (required unless --candidates)")
	candidates := flags.Bool("candidates", false, "read the catalog for the day's staged intervals and write the day's candidates.json")
	date := flags.String("date", "", "the day for --candidates (YYYY-MM-DD; defaults to today)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("classifications does not accept positional arguments")
	}
	configPath, envFile, err := resolveStoreFlags("classifications", *configFlag, *envFlag)
	if err != nil {
		return err
	}
	if *candidates {
		if *query != "" {
			return fmt.Errorf("classifications accepts either --query or --candidates, not both")
		}
		return runCandidates(configPath, envFile, *date)
	}
	if strings.TrimSpace(*query) == "" {
		return fmt.Errorf("classifications requires --query")
	}
	h, err := openHeadless(configPath, envFile)
	if err != nil {
		return err
	}
	defer h.done()
	res, err := h.call("classifications", timesheet.Request{Query: *query})
	if err != nil {
		return err
	}
	receipt, ok := res.(timesheet.ClassificationsReceipt)
	if !ok {
		return fmt.Errorf("classifications: unexpected classifications payload")
	}
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(receipt)
}

// ClassifyApplied is one selection classify applied to a staged interval.
type ClassifyApplied struct {
	IntervalID       string `json:"interval_id"`
	CatalogID        string `json:"catalog_id"`
	ClassificationID string `json:"classification_id"`
	// Classification and Code are the chosen option's semantics as the
	// catalog returned them; CatalogComplete is the catalog's own honesty
	// flag (a partial catalog cannot prove an option absent).
	Classification  string `json:"classification,omitempty"`
	Code            string `json:"code,omitempty"`
	CatalogComplete bool   `json:"catalog_complete"`
	// Source is where the selection came from: judgment, history or default.
	Source string `json:"source,omitempty"`
}

// ClassifyReceipt is classify's stdout receipt, in interval_id order.
type ClassifyReceipt struct {
	Date string `json:"date"`
	File string `json:"file"`
	// SidecarFile is the sidecar the not-yet-finalized path wrote the
	// selections to. Empty on the finalized-day path: no sidecar is written
	// there because its selections become durable in the same process.
	SidecarFile string            `json:"sidecar_file"`
	Applied     []ClassifyApplied `json:"applied"`
	Selections  int               `json:"selections"`
	// Revision is the day's revision after the command: the unchanged open
	// revision on the not-yet-finalized path and on a finalized-day no-op, or
	// the NEW saved revision a finalized-day re-finalize produced.
	Revision        int `json:"revision"`
	StagedIntervals int `json:"staged_intervals"`
	// Refinalized is true only on the finalized-day path when confirm and
	// save ran: the day was already durable and is now durable at the new
	// Revision with the selections applied to its confirmed intervals.
	Refinalized bool `json:"refinalized"`
	// Note carries the honest no-op wording when the file named no selections
	// and the finalized-day re-finalize wording; it is empty when the
	// not-yet-finalized path applied at least one selection.
	Note string `json:"note,omitempty"`
}

func runClassify(args []string) error {
	f, err := parseClassifyFlags(args)
	if err != nil {
		return err
	}
	if f.configuredDefault {
		return runClassifyDefault(f)
	}
	if f.fromHistory {
		return runClassifyHistory(f)
	}
	cfg, err := timesheet.LoadConfig(f.configPath)
	if err != nil {
		return err
	}
	h, err := openHeadless(f.configPath, f.envFile)
	if err != nil {
		return err
	}
	defer h.done()
	receipt, err := classifyFileDay(h, cfg, f.date, f.file)
	if err != nil {
		return err
	}
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(receipt)
}

func classifyFileDay(h *headlessSession, cfg *timesheet.Config, date, answerFile string) (ClassifyReceipt, error) {
	data, err := os.ReadFile(answerFile)
	if err != nil {
		return ClassifyReceipt{}, fmt.Errorf("classify: the choices file cannot be read")
	}
	selections, err := timesheet.ParseClassificationChoices(data)
	if err != nil {
		return ClassifyReceipt{}, err
	}
	res, err := h.call("open_day", timesheet.Request{Day: date})
	if err != nil {
		return ClassifyReceipt{}, err
	}
	summary, ok := res.(timesheet.DaySummary)
	if !ok {
		return ClassifyReceipt{}, fmt.Errorf("classify: unexpected open_day payload")
	}
	if finalizedSummary(summary) {
		if err := checkSavedClassificationBinding(h, date, data); err != nil {
			return ClassifyReceipt{}, err
		}
		return runClassifyFinalized(h, date, answerFile, summary, selections)
	}
	mergeHistorySelections(cfg.StatePath, date, data, selections)
	receipt, err := classifyFileCore(h, cfg, date, summary, selections)
	if err != nil {
		return ClassifyReceipt{}, err
	}
	receipt.File = answerFile
	return receipt, nil
}

func classifyFileCore(h *headlessSession, cfg *timesheet.Config, date string, summary timesheet.DaySummary, selections map[string]timesheet.ClassificationChoiceEntry) (ClassifyReceipt, error) {
	staged, err := applyStage(h, cfg, "classify", date, summary)
	if err != nil {
		return ClassifyReceipt{}, err
	}
	noteStageSkips("classify", staged)
	applied, err := applySelections(h, date, summary.Revision, selections)
	if err != nil {
		return ClassifyReceipt{}, err
	}
	blocks, err := stagedBlocks(cfg, date)
	if err != nil {
		return ClassifyReceipt{}, err
	}
	bound := make(map[string]timesheet.ClassificationChoiceEntry, len(selections))
	for _, a := range applied {
		e, ok := blocks[a.IntervalID]
		if !ok {
			return ClassifyReceipt{}, fmt.Errorf("classify: interval %s cannot be bound to a recorded block; review again; re-run stage + classify", a.IntervalID)
		}
		sel := selections[a.IntervalID]
		sel.ContentDigest = timesheet.BlockContentDigest(date, e)
		bound[a.IntervalID] = sel
	}
	if err := timesheet.WriteClassificationChoices(cfg.StatePath, timesheet.AppVersion, date, bound, time.Now()); err != nil {
		return ClassifyReceipt{}, err
	}
	receipt := ClassifyReceipt{
		Date:            date,
		SidecarFile:     timesheet.ChoicesPath(cfg.StatePath, date),
		Applied:         applied,
		Selections:      len(applied),
		Revision:        summary.Revision,
		StagedIntervals: staged.StagedIntervals,
	}
	if len(applied) == 0 {
		receipt.Note = "no selections in the choices file; nothing was classified"
	}
	return receipt, nil
}

func runClassifyFinalized(h *headlessSession, date, answerFile string, summary timesheet.DaySummary, selections map[string]timesheet.ClassificationChoiceEntry) (ClassifyReceipt, error) {
	applied, err := applySelections(h, date, summary.Revision, selections)
	if err != nil {
		return ClassifyReceipt{}, err
	}
	durable := 0
	for _, seg := range summary.Confirmed.Segments {
		if seg.OriginDate == date {
			durable++
		}
	}
	receipt := ClassifyReceipt{
		Date:            date,
		File:            answerFile,
		Applied:         applied,
		Selections:      len(applied),
		Revision:        summary.Revision,
		StagedIntervals: durable,
	}
	if len(applied) == 0 {
		receipt.Note = fmt.Sprintf("no selections in the choices file; nothing was classified; the saved day was left unchanged at revision %d", summary.Revision)
		return receipt, nil
	}
	confirmed, err := h.call("confirm", timesheet.Request{Day: date, Revision: &summary.Revision})
	if err != nil {
		return ClassifyReceipt{}, err
	}
	confirmReceipt, ok := confirmed.(timesheet.ConfirmReceipt)
	if !ok {
		return ClassifyReceipt{}, fmt.Errorf("classify: unexpected confirm payload")
	}
	saved, err := h.call("save", timesheet.Request{Day: date, Revision: &confirmReceipt.Revision})
	if err != nil {
		return ClassifyReceipt{}, err
	}
	saveReceipt, ok := saved.(timesheet.SaveReceipt)
	if !ok {
		return ClassifyReceipt{}, fmt.Errorf("classify: unexpected save payload")
	}
	receipt.Refinalized = true
	receipt.Revision = saveReceipt.Revision
	receipt.Note = fmt.Sprintf("day was already finalized; the selections keyed off the durable interval ids and the day was re-finalized at revision %d", saveReceipt.Revision)
	return receipt, nil
}

func parseClassifyFlags(args []string) (*finalizeFlags, error) {
	flags := flag.NewFlagSet("classify", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	f := &finalizeFlags{}
	flags.BoolVar(&f.configuredDefault, "configured-default", false, "fill unselected unsaved intervals with the configured cached default")
	flags.BoolVar(&f.fromHistory, "from-history", false, "pre-select intervals from the owner's confirmed submissions on other days (writes the day's history.json)")
	flags.StringVar(&f.configPath, "config", "", "configuration path (optional; defaults to $HOME/.timesheet/config.json)")
	flags.StringVar(&f.envFile, "env-file", "", "environment-file path (optional; defaults to $HOME/.timesheet/.env.local when it exists)")
	flags.StringVar(&f.date, "date", "", "day to operate on, YYYY-MM-DD (required)")
	flags.StringVar(&f.file, "file", "", "choices JSON path: {\"selections\": {\"<interval_id>\": {\"catalog_id\": \"...\", \"classification_id\": \"...\"}}} (mutually exclusive with --configured-default)")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("classify does not accept positional arguments")
	}
	configPath, envFile, err := resolveStoreFlags("classify", f.configPath, f.envFile)
	if err != nil {
		return nil, err
	}
	f.configPath, f.envFile = configPath, envFile
	if f.date == "" {
		return nil, fmt.Errorf("classify requires --date")
	}
	if _, err := time.Parse("2006-01-02", f.date); err != nil {
		return nil, fmt.Errorf("classify: --date must be YYYY-MM-DD")
	}
	modes := 0
	for _, on := range []bool{f.file != "", f.configuredDefault, f.fromHistory} {
		if on {
			modes++
		}
	}
	if modes != 1 {
		return nil, fmt.Errorf("classify requires exactly one of --file, --configured-default or --from-history")
	}
	return f, nil
}

// applySelections applies each selection through select_classification, in
// interval_id order, against the day's current staged set. Errors pass
// through verbatim: the op fails closed on a catalog that is not stored, a
// classification_id the catalog did not return, a stale revision, or an
// interval_id that is not in the staged day (renumbered by an entries edit
// after the stage receipt printed, or simply unknown).
func applySelections(h *headlessSession, date string, revision int, selections map[string]timesheet.ClassificationChoiceEntry) ([]ClassifyApplied, error) {
	ids := make([]string, 0, len(selections))
	for id := range selections {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	applied := make([]ClassifyApplied, 0, len(ids))
	for _, id := range ids {
		sel := selections[id]
		res, err := h.call("select_classification", timesheet.Request{
			Day: date, Revision: &revision, IntervalID: id,
			CatalogID: sel.CatalogID, ClassificationID: sel.ClassificationID,
		})
		if err != nil {
			var app *timesheet.AppError
			if errors.As(err, &app) {
				return nil, &timesheet.AppError{Code: app.Code, Message: "interval " + id + ": " + app.Message}
			}
			return nil, err
		}
		mutation, ok := res.(timesheet.ClassificationSelection)
		if !ok {
			return nil, fmt.Errorf("classify: unexpected select_classification payload")
		}
		applied = append(applied, ClassifyApplied{
			IntervalID: id, CatalogID: sel.CatalogID, ClassificationID: sel.ClassificationID,
			Classification: mutation.Classification, Code: mutation.Code,
			CatalogComplete: mutation.CatalogComplete, Source: sel.Source,
		})
	}
	return applied, nil
}

func stagedBlocks(cfg *timesheet.Config, date string) (map[string]timesheet.EntryRecord, error) {
	artifacts, err := timesheet.ReadDayArtifacts(cfg.StatePath, date)
	if err != nil {
		return nil, err
	}
	stageMap := artifacts.StageMap
	entries := artifactStageEntries(artifacts)
	effective := map[string]timesheet.EntryRecord{}
	for _, c := range timesheet.EffectiveEntrySpansOn(date, entries) {
		effective[c.BlockID] = timesheet.EntryRecord{
			BlockID: c.BlockID, Date: date, Start: c.Start, End: c.End,
			Kind: c.Kind, Description: c.Description,
		}
	}
	// Entries the projection excludes (an unparsable, zero-duration or
	// day-or-longer entry, never staged) keep their own record under their own
	// id.
	for _, e := range entries {
		if _, ok := effective[e.BlockID]; !ok {
			effective[e.BlockID] = e
		}
	}
	byInterval := map[string]timesheet.EntryRecord{}
	for unitID, ivIDs := range stageMap {
		if e, ok := effective[unitID]; ok {
			for _, ivID := range ivIDs {
				byInterval[ivID] = e
			}
		}
	}
	return byInterval, nil
}

func applyPersistedSelections(h *headlessSession, cfg *timesheet.Config, name, date string, revision int) (int, error) {
	selections, err := timesheet.ReadClassificationChoices(cfg.StatePath, date)
	if err != nil {
		return 0, err
	}
	if len(selections) == 0 {
		return 0, nil
	}
	blocks, err := stagedBlocks(cfg, date)
	if err != nil {
		return 0, err
	}
	// Verify every content binding before applying anything.
	ids := make([]string, 0, len(selections))
	for id := range selections {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		e, ok := blocks[id]
		if !ok {
			return 0, fmt.Errorf("%s: classification selection for interval %s cannot be bound to a recorded block; review again; re-run stage + classify", name, id)
		}
		if selections[id].ContentDigest != timesheet.BlockContentDigest(date, e) {
			return 0, fmt.Errorf("%s: block %s changed since classify; the recorded selection no longer matches its content (interval %s); review again; re-run stage + classify", name, e.BlockID, id)
		}
	}
	if _, err := applySelections(h, date, revision, selections); err != nil {
		return 0, err
	}
	return len(selections), nil
}

// noteClassifyReplays mirrors the replay count on stderr for confirm and
// save, so the durable carry of selections is visible without changing the
// stdout receipts' shape.
func noteClassifyReplays(name string, n int) {
	if n > 0 {
		fmt.Fprintf(os.Stderr, "%s: replayed %d classification selection(s) from %s\n", name, n, "the classification sidecar")
	}
}

func runConfirm(args []string) error {
	f, err := parseFinalizeFlags("confirm", args)
	if err != nil {
		return err
	}
	cfg, err := timesheet.LoadConfig(f.configPath)
	if err != nil {
		return err
	}
	h, err := openHeadless(f.configPath, f.envFile)
	if err != nil {
		return err
	}
	defer h.done()
	summary, err := openFinalizeDay(h, "confirm", f.date)
	if err != nil {
		return err
	}
	if _, _, err := stageAndReplay(h, cfg, "confirm", f.date, summary); err != nil {
		return err
	}
	receipt, _, err := confirmAndSave(h, "confirm", f.date, summary.Revision, false)
	if err != nil {
		return err
	}
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(receipt)
}

func stageAndReplay(h *headlessSession, cfg *timesheet.Config, name, date string, summary timesheet.DaySummary) (*StageReceipt, int, error) {
	staged, err := applyStage(h, cfg, name, date, summary)
	if err != nil {
		return nil, 0, err
	}
	noteStageSkips(name, staged)
	replayed, err := applyPersistedSelections(h, cfg, name, date, summary.Revision)
	if err != nil {
		return nil, 0, err
	}
	noteClassifyReplays(name, replayed)
	return staged, replayed, nil
}

func confirmAndSave(h *headlessSession, name, date string, revision int, throughSave bool) (timesheet.ConfirmReceipt, *timesheet.SaveReceipt, error) {
	confirmed, err := h.call("confirm", timesheet.Request{Day: date, Revision: &revision})
	if err != nil {
		return timesheet.ConfirmReceipt{}, nil, err
	}
	receipt, ok := confirmed.(timesheet.ConfirmReceipt)
	if !ok {
		return timesheet.ConfirmReceipt{}, nil, fmt.Errorf("%s: unexpected confirm payload", name)
	}
	if !throughSave {
		return receipt, nil, nil
	}
	saved, err := h.call("save", timesheet.Request{Day: date, Revision: &receipt.Revision})
	if err != nil {
		return timesheet.ConfirmReceipt{}, nil, err
	}
	saveReceipt, ok := saved.(timesheet.SaveReceipt)
	if !ok {
		return timesheet.ConfirmReceipt{}, nil, fmt.Errorf("%s: unexpected save payload", name)
	}
	return receipt, &saveReceipt, nil
}

func runPlan(args []string) error {
	f, err := parseHeadlessFlags("plan", args)
	if err != nil {
		return err
	}
	h, err := openHeadless(f.configPath, f.envFile)
	if err != nil {
		return err
	}
	defer h.done()
	res, err := h.call("preview_plan", timesheet.Request{Day: f.date})
	if err != nil {
		return err
	}
	receipt, ok := res.(timesheet.PlanPreview)
	if !ok {
		return fmt.Errorf("plan: unexpected preview_plan payload")
	}
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(receipt)
}

func runReconcile(args []string) error {
	f, err := parseHeadlessFlags("reconcile", args)
	if err != nil {
		return err
	}
	h, err := openHeadless(f.configPath, f.envFile)
	if err != nil {
		return err
	}
	defer h.done()
	res, err := h.call("reconcile_remote", timesheet.Request{Day: f.date})
	if err != nil {
		return err
	}
	receipt, ok := res.(timesheet.ReconcileReceipt)
	if !ok {
		return fmt.Errorf("reconcile: unexpected reconcile_remote payload")
	}
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(receipt)
}

type resolveFlags struct {
	configPath string
	envFile    string
	date       string
	file       string
}

func parseResolveFlags(args []string) (*resolveFlags, error) {
	flags := flag.NewFlagSet("resolve", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	f := &resolveFlags{}
	flags.StringVar(&f.configPath, "config", "", "configuration path (optional; defaults to $HOME/.timesheet/config.json)")
	flags.StringVar(&f.envFile, "env-file", "", "environment-file path (optional; defaults to $HOME/.timesheet/.env.local when it exists)")
	flags.StringVar(&f.date, "date", "", "day to operate on, YYYY-MM-DD (required)")
	flags.StringVar(&f.file, "file", "", "decision JSON path: {\"revision\": <n>, \"observation_id\": \"...\", \"decision\": \"adopt|accept_observed|foreign\", \"remote_id\": \"...\", \"local_id\": \"...\"} (required; local_id omitted for foreign)")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("resolve does not accept positional arguments")
	}
	configPath, envFile, err := resolveStoreFlags("resolve", f.configPath, f.envFile)
	if err != nil {
		return nil, err
	}
	f.configPath, f.envFile = configPath, envFile
	if f.date == "" {
		return nil, fmt.Errorf("resolve requires --date")
	}
	if _, err := time.Parse("2006-01-02", f.date); err != nil {
		return nil, fmt.Errorf("resolve: --date must be YYYY-MM-DD")
	}
	if f.file == "" {
		return nil, fmt.Errorf("resolve requires --file")
	}
	return f, nil
}

func runResolve(args []string) error {
	f, err := parseResolveFlags(args)
	if err != nil {
		return err
	}
	if _, err := os.Stat(f.file); err != nil {
		return fmt.Errorf("resolve: the decision file cannot be read")
	}
	h, err := openHeadless(f.configPath, f.envFile)
	if err != nil {
		return err
	}
	defer h.done()
	receipt, err := resolveFileDay(h, f.date, f.file)
	if err != nil {
		return err
	}
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(receipt)
}

func resolveFileDay(h *headlessSession, date, file string) (timesheet.ResolutionReceipt, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return timesheet.ResolutionReceipt{}, fmt.Errorf("resolve: the decision file cannot be read")
	}
	decision, err := timesheet.ParseResolutionDecision(data)
	if err != nil {
		return timesheet.ResolutionReceipt{}, err
	}
	res, err := h.call("resolve_remote", timesheet.Request{
		Day: date, Revision: &decision.Revision, ObservationID: decision.ObservationID,
		RemoteID: decision.RemoteID, LocalID: decision.LocalID, Decision: decision.Decision,
	})
	if err != nil {
		return timesheet.ResolutionReceipt{}, err
	}
	receipt, ok := res.(timesheet.ResolutionReceipt)
	if !ok {
		return timesheet.ResolutionReceipt{}, fmt.Errorf("resolve: unexpected resolve_remote payload")
	}
	return receipt, nil
}

func runSave(args []string) error {
	f, err := parseFinalizeFlags("save", args)
	if err != nil {
		return err
	}
	cfg, err := timesheet.LoadConfig(f.configPath)
	if err != nil {
		return err
	}
	h, err := openHeadless(f.configPath, f.envFile)
	if err != nil {
		return err
	}
	defer h.done()
	saved, err := saveDay(h, cfg, "save", f.date)
	if err != nil {
		return err
	}
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(*saved)
}

func saveDay(h *headlessSession, cfg *timesheet.Config, name, date string) (*timesheet.SaveReceipt, error) {
	summary, err := openFinalizeDay(h, name, date)
	if err != nil {
		return nil, err
	}
	if _, _, err := stageAndReplay(h, cfg, name, date, summary); err != nil {
		return nil, err
	}
	_, saved, err := confirmAndSave(h, name, date, summary.Revision, true)
	if err != nil {
		return nil, err
	}
	return saved, nil
}

func renderPlanBody(p timesheet.PlanPreview) string {
	var b strings.Builder
	for _, op := range p.Operations {
		fmt.Fprintf(&b, "  op %s %s %s %s-%s %s [%s]", op.Kind, op.Date, op.LocalID,
			op.StartTime, op.EndTime, op.Description, op.ClassificationID)
		if op.Task != "" {
			fmt.Fprintf(&b, " task=%s", op.Task)
		}
		if op.Reason != "" {
			fmt.Fprintf(&b, " reason=%s", op.Reason)
		}
		b.WriteString("\n")
	}
	for _, bl := range p.Blockers {
		fmt.Fprintf(&b, "  blocker %s: %s\n", bl.Code, bl.Detail)
	}
	return b.String()
}

func runApprove(args []string) error {
	f, err := parseHeadlessFlags("approve", args)
	if err != nil {
		return err
	}
	term, err := interactiveFactory("approve", "approve")
	if err != nil {
		return err
	}
	h, err := openHeadless(f.configPath, f.envFile)
	if err != nil {
		return err
	}
	defer h.done()
	receipt, err := approveDay(h, term, f.date)
	if err != nil {
		return err
	}
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(receipt)
}

func approveDay(h *headlessSession, term *interactive, date string) (timesheet.ApprovalReceipt, error) {
	res, err := h.call("preview_plan", timesheet.Request{Day: date})
	if err != nil {
		return timesheet.ApprovalReceipt{}, err
	}
	preview, ok := res.(timesheet.PlanPreview)
	if !ok {
		return timesheet.ApprovalReceipt{}, fmt.Errorf("submit: unexpected preview_plan payload")
	}
	prompt := fmt.Sprintf("About to approve the current plan for %s\n  plan_id: %s\n  state:   %s\n%s",
		date, preview.PlanID, preview.State, renderPlanBody(preview))
	if err := term.confirm(prompt, preview.PlanID); err != nil {
		return timesheet.ApprovalReceipt{}, err
	}
	res, err = h.call("approve_plan", timesheet.Request{Day: date, PlanID: preview.PlanID})
	if err != nil {
		return timesheet.ApprovalReceipt{}, err
	}
	receipt, ok := res.(timesheet.ApprovalReceipt)
	if !ok {
		return timesheet.ApprovalReceipt{}, fmt.Errorf("submit: unexpected approve_plan payload")
	}
	fmt.Fprintf(term.out, "approved: approval_id %s for plan %s (%d operation(s))\n",
		receipt.ApprovalID, receipt.PlanID, receipt.Operations)
	return receipt, nil
}

type submitFlags struct {
	configPath string
	envFile    string
	date       string
	approvalID string
}

func parseSubmitFlags(args []string) (*submitFlags, error) {
	flags := flag.NewFlagSet("submit", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	f := &submitFlags{}
	flags.StringVar(&f.configPath, "config", "", "configuration path (optional; defaults to $HOME/.timesheet/config.json)")
	flags.StringVar(&f.envFile, "env-file", "", "environment-file path (optional; defaults to $HOME/.timesheet/.env.local when it exists)")
	flags.StringVar(&f.date, "date", "", "day to operate on, YYYY-MM-DD (required)")
	flags.StringVar(&f.approvalID, "approval-id", "", "approval_id copied verbatim from the approve receipt (required)")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("submit does not accept positional arguments")
	}
	configPath, envFile, err := resolveStoreFlags("submit", f.configPath, f.envFile)
	if err != nil {
		return nil, err
	}
	f.configPath, f.envFile = configPath, envFile
	if f.date == "" {
		return nil, fmt.Errorf("submit requires --date")
	}
	if _, err := time.Parse("2006-01-02", f.date); err != nil {
		return nil, fmt.Errorf("submit: --date must be YYYY-MM-DD")
	}
	if f.approvalID == "" {
		return nil, fmt.Errorf("submit requires --approval-id")
	}
	return f, nil
}

func runSubmit(args []string) error {
	f, err := parseSubmitFlags(args)
	if err != nil {
		return err
	}
	term, err := interactiveFactory("submit", "submit")
	if err != nil {
		return err
	}
	if err := confirmSubmission(term, f.date, f.approvalID); err != nil {
		return err
	}
	h, err := openHeadless(f.configPath, f.envFile)
	if err != nil {
		return err
	}
	defer h.done()
	receipt, err := submitApproved(h, term, f.date, f.approvalID)
	if err != nil {
		return err
	}
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(receipt)
}

func confirmSubmission(term *interactive, date, approvalID string) error {
	prompt := fmt.Sprintf("About to submit day %s using approval %s\nThe submission dispatches the approved operations to Gradion; writes are never retried and unknown outcomes stay unresolved until you resolve them.", date, approvalID)
	return term.confirm(prompt, approvalID)
}

func submitApproved(h *headlessSession, term *interactive, date, approvalID string) (timesheet.SubmissionReceipt, error) {
	res, err := h.call("submit_plan", timesheet.Request{Day: date, ApprovalID: approvalID})
	if err != nil {
		return timesheet.SubmissionReceipt{}, err
	}
	receipt, ok := res.(timesheet.SubmissionReceipt)
	if !ok {
		return timesheet.SubmissionReceipt{}, fmt.Errorf("submit: unexpected submit_plan payload")
	}
	fmt.Fprintf(term.out, "submission status: %s (confirmed %d, unresolved %d, unattempted %d)\n",
		receipt.Status, receipt.Confirmed, receipt.Unresolved, receipt.Unattempted)
	return receipt, nil
}

func runSubmitStatus(args []string) error {
	f, err := parseHeadlessFlags("submit-status", args)
	if err != nil {
		return err
	}
	h, err := openHeadless(f.configPath, f.envFile)
	if err != nil {
		return err
	}
	defer h.done()
	res, err := h.call("submission_status", timesheet.Request{Day: f.date})
	if err != nil {
		return err
	}
	receipt, ok := res.(timesheet.SubmissionStatusResult)
	if !ok {
		return fmt.Errorf("submit-status: unexpected submission_status payload")
	}
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(receipt)
}

func parseResolveWriteFlags(args []string) (*resolveFlags, error) {
	flags := flag.NewFlagSet("resolve-write", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	f := &resolveFlags{}
	flags.StringVar(&f.configPath, "config", "", "configuration path (optional; defaults to $HOME/.timesheet/config.json)")
	flags.StringVar(&f.envFile, "env-file", "", "environment-file path (optional; defaults to $HOME/.timesheet/.env.local when it exists)")
	flags.StringVar(&f.file, "file", "", "decision JSON path: {\"write_ref\": \"...\", \"observation_id\": \"...\", \"decision\": \"adopt|not_applied\", \"remote_id\": \"...\"} (required; remote_id required for adopt) (required)")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("resolve-write does not accept positional arguments")
	}
	configPath, envFile, err := resolveStoreFlags("resolve-write", f.configPath, f.envFile)
	if err != nil {
		return nil, err
	}
	f.configPath, f.envFile = configPath, envFile
	if f.file == "" {
		return nil, fmt.Errorf("resolve-write requires --file")
	}
	return f, nil
}

type writeResolutionDecision struct {
	WriteRef      string `json:"write_ref"`
	ObservationID string `json:"observation_id"`
	Decision      string `json:"decision"`
	RemoteID      string `json:"remote_id"`
}

func parseWriteResolutionDecision(data []byte) (writeResolutionDecision, error) {
	var d writeResolutionDecision
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return writeResolutionDecision{}, fmt.Errorf(
			"resolve-write: the decision file is not valid JSON for the expected schema {\"write_ref\", \"observation_id\", \"decision\", \"remote_id\"}")
	}
	if strings.TrimSpace(d.WriteRef) == "" {
		return writeResolutionDecision{}, fmt.Errorf("resolve-write: the decision file requires write_ref copied verbatim from submission_status")
	}
	if strings.TrimSpace(d.ObservationID) == "" {
		return writeResolutionDecision{}, fmt.Errorf("resolve-write: the decision file requires observation_id of the current complete observation (run reconcile first)")
	}
	if d.Decision != timesheet.DecisionWriteAdopt && d.Decision != timesheet.DecisionWriteNotApplied {
		return writeResolutionDecision{}, fmt.Errorf("resolve-write: decision must be adopt or not_applied")
	}
	if d.Decision == timesheet.DecisionWriteAdopt && strings.TrimSpace(d.RemoteID) == "" {
		return writeResolutionDecision{}, fmt.Errorf("resolve-write: adopt requires remote_id of the explicitly selected observed entry")
	}
	return d, nil
}

func runResolveWrite(args []string) error {
	f, err := parseResolveWriteFlags(args)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(f.file)
	if err != nil {
		return fmt.Errorf("resolve-write: the decision file cannot be read")
	}
	decision, err := parseWriteResolutionDecision(data)
	if err != nil {
		return err
	}
	term, err := interactiveFactory("resolve-write", "resolve writes")
	if err != nil {
		return err
	}
	prompt := fmt.Sprintf("About to record decision %q for write reference %s against observation %s",
		decision.Decision, decision.WriteRef, decision.ObservationID)
	if err := term.confirm(prompt, decision.WriteRef); err != nil {
		return err
	}
	h, err := openHeadless(f.configPath, f.envFile)
	if err != nil {
		return err
	}
	defer h.done()
	res, err := h.call("resolve_write", timesheet.Request{
		WriteRef: decision.WriteRef, ObservationID: decision.ObservationID,
		Decision: decision.Decision, RemoteID: decision.RemoteID,
	})
	if err != nil {
		return err
	}
	receipt, ok := res.(timesheet.WriteResolutionReceipt)
	if !ok {
		return fmt.Errorf("resolve-write: unexpected resolve_write payload")
	}
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(receipt)
}
