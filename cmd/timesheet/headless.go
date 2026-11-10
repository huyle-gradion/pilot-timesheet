package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// The headless subcommands are the skill's [Script] layer. They construct the
// same session serve uses and drive its request dispatcher in-process; they
// never open a second protocol surface and never touch the network beyond what
// the configured collectors already do inside collect.

// dateOptionalCommands are the commands where an omitted --date means today in
// the configured timezone, resolved by the binary (issue #179). Every other
// command that operates on a day still requires one. This map is the single
// requiredness decision: the flag usage text and every parser read it.
var dateOptionalCommands = map[string]bool{
	"collect": true,
	"draft":   true,
	"run":     true,
	"record":  true,
	"status":  true,
	"next":    true,
}

// dateFlagUsage is the shared --date description, so --help never disagrees
// with the parser about whether the flag may be omitted.
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

// validateDate applies the one requiredness decision to a parsed --date. Only a
// flag that was never given may mean today, and only for dateOptionalCommands.
// Any supplied value, including an empty or whitespace-only one, must parse as
// a real date; nothing is trimmed into acceptance.
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

// recordFlags carries the record subcommand's inputs: --config and --env-file,
// --date (omitted means today in the configured timezone), exactly one input
// mode — the recovery --file reviewed JSON, or the descriptions-only --draft +
// --descriptions pair — and the optional --replace merge mode.
type recordFlags struct {
	configPath   string
	envFile      string
	date         string
	file         string
	draft        string
	descriptions string
	replace      bool
}

// parseRecordFlags parses record's flag surface. The two input modes are
// mutually exclusive and one is required: --file alone, or --draft AND
// --descriptions together. --replace (issue #162) switches the merge mode:
// the reviewed result is the day's complete content and replaces all blocks
// for the resolved day instead of merging — for the re-collected-day case where
// the new generation supersedes the old.
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

// headlessSession mirrors runServe's loading path and drives HandleLine
// directly, one typed request at a time.
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
		// Typed errors survive: the protocol error carries the AppError's own
		// code and message, and Error() renders the same "code: message" bytes
		// the previous string wrap printed.
		return nil, &timesheet.AppError{Code: resp.Error.Code, Message: resp.Error.Message}
	}
	return resp.Result, nil
}

// jsonBytes renders one headless result exactly as it appears on stdout:
// indented JSON plus one trailing newline. run writes these same bytes to the
// artifacts file, so a redirect of run's stdout and the stored artifact are
// byte-identical by construction.
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

// runCollect performs one collection attempt and prints its bounded result.
// The fresh collection_id is printed as one stderr line (issue #162) so
// acknowledge/decide can copy it without reading state.json; stdout stays the
// collect JSON only.
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

// buildDayDraft is the shared core of the draft and run subcommands: it opens
// the store, runs exactly one bounded collection attempt for the day, builds
// the compact draft and closes the store. Neither caller duplicates this flow.
// It also returns the fresh collection_id so run can print it on stderr
// (issue #162); the draft subcommand ignores it.
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

// collectDraft is the collect+review+draft flow over one held session: open
// day, one bounded collect, review, and the compact draft with its work-hours
// annotation. It is the run verb's core and the tui verb's run act; the
// session must be the caller's own.
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

// clockNow is the one clock the headless commands read for date resolution.
// Tests replace it to pin "today"; production leaves time.Now. TodayIn owns the
// timezone arithmetic and resolveDay is this clock's only caller.
var clockNow = time.Now

// resolveDay returns the day a command operates on. An explicit date always
// wins; only an empty date, which means the flag was never given, is today in
// the configured timezone, so a one-command daily driver never has to compute
// the local date from the host clock.
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

// runDraft collects the day and prints the compact draft for the judgment step.
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

// runRun composes the collect and draft subcommands' single shared flow in one
// process: one bounded collection attempt, then the compact draft. stdout
// carries ONLY the draft JSON — the collect source report is not printed; its
// substance is already in the draft's flags and per-block completeness. The
// same draft bytes are also written to the deterministic artifacts directory
// `<state dir>/days/<date>/draft.json` in the store (created as needed), and the artifacts
// path is printed on stderr so stdout stays pure draft JSON: redirecting
// stdout yields exactly the draft bytes. The fresh collection_id is one
// further stderr line (issue #162), so acknowledge/decide can copy it without
// reading state.json.
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
	// The artifacts directory is keyed by the RESOLVED day: with an omitted
	// --date, f.date is empty and the draft would land in an undated path.
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

// runRecord validates the reviewed result and merges it into the day's
// entries. It has two input modes:
//
//   - --draft <draft.json> --descriptions <desc.json>: the descriptions-only
//     judgment step (issue #152). record loads the draft, applies the
//     per-block descriptions and the one notes string, appends each block's
//     prs_inline when not already contained, and runs the exact same
//     validation and merge path as --file.
//   - --file <reviewed.json>: the full reviewed file. Unchanged and still the
//     recovery path.
//
// --date is optional (issue #179): omitted, it means today in the configured
// timezone. --replace (issue #162) switches the merge mode for either input:
// the reviewed result is the day's complete content, so all blocks for the
// resolved day are replaced with the file's blocks (block ids from the file;
// blocks absent from it are removed) instead of merged. Use it after
// re-collecting a day:
// the new generation supersedes the old. Without the flag the merge behavior
// is unchanged (idempotent per block, other blocks preserved).
//
// It opens no store: recording never blocks on, or interferes with, a running
// serve process.
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
	// Load the configuration before the date check so a bad --config reports
	// its own root cause, not a misleading date mismatch.
	cfg, err := timesheet.LoadConfig(f.configPath)
	if err != nil {
		return err
	}
	// The day to record is the explicit --date or, when omitted, today in the
	// configured timezone. The reviewed draft must agree with the RESOLVED day
	// so a wrong day is visible in the error, and the receipt carries that day.
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

// composeDescriptionsOnly is the descriptions-only judgment input's core,
// shared by the record verb and the tui verb's record act: read the draft and
// the bound descriptions answer, and compose the reviewed draft.
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

// runStatus prints the store status and the recorded entries summary for the
// day: the explicit --date or, when omitted, today in the configured timezone.
// The day is opened first (issue #162) so the status result carries the day's
// per-source latest collection attempt — status and collection_id — for
// acknowledge/decide to copy verbatim.
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
	_, _ = h.call("close", timesheet.Request{})
	entries, err := timesheet.ReadEntries(h.cfg.StatePath, day)
	if err != nil {
		return err
	}
	summary := map[string]any{
		"status":  res,
		"date":    day,
		"entries": 0,
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
	return printJSON(summary)
}

// --- SUBMIT-1 day finalization commands -----------------------------------------
//
// stage/acknowledge/decide/confirm/save drive the existing Session API (the
// same ops serve dispatches) to carry one recorded day from its entries file to
// a durable confirmed day. Each command is one process: open the day, run its
// ops, close. They hold the store's exclusive single-writer lock, so they
// cannot run while a serve process holds the store (unlike record, which opens
// no store). Nothing here approves or submits anything.

// finalizeFlags carries the stage/confirm/save inputs: --config/--env-file/
// --date always, plus the optional --project. Recorded entries carry no project
// of their own, so the staged intervals take the configuration's single
// project, or the --project named when several are defined. classify reuses
// the same flag set plus --file.
type finalizeFlags struct {
	configuredDefault bool
	configPath        string
	envFile           string
	date              string
	project           string
	file              string
	// revise is stage's explicit finalized-day revision flag: the confirmed
	// set is replaced by a fresh derivation from the entries ledger (split
	// included) and the day is re-finalized at a new revision.
	revise bool
}

func parseFinalizeFlags(name string, args []string) (*finalizeFlags, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	f := &finalizeFlags{}
	flags.StringVar(&f.configPath, "config", "", "configuration path (optional; defaults to $HOME/.timesheet/config.json)")
	flags.StringVar(&f.envFile, "env-file", "", "environment-file path (optional; defaults to $HOME/.timesheet/.env.local when it exists)")
	flags.StringVar(&f.date, "date", "", dateFlagUsage(name))
	flags.StringVar(&f.project, "project", "", "project id for staged intervals (required when configuration defines multiple projects)")
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

// resolveProject returns the project id staged intervals carry: the explicit
// --project, or the configuration's only project when none was named. An
// unknown project id fails later in add_interval's own validation, which the
// command passes through untouched.
func resolveProject(name string, cfg *timesheet.Config, explicit string) (string, error) {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		return explicit, nil
	}
	if len(cfg.Projects) == 1 {
		return cfg.Projects[0].ID, nil
	}
	return "", fmt.Errorf("%s: configuration defines %d projects; --project is required", name, len(cfg.Projects))
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
}

// applyStage converts the day's recorded entries into staged intervals through
// the existing add_interval/edit_interval/remove_interval session ops, using
// the <date>.stage.json sidecar to keep the staged set exactly matching the
// current entries file. The sidecar's edit and remove branches fire only
// within the process that staged (whenever a mapped block's interval already
// exists in the open day's staged set); they are not a cross-process
// persistence mechanism.
//
// Because Day.Staged is session-only by design (it is never durable), every
// command that needs the staged set re-derives it from the entries ledger
// inside its own process; this function is that derivation, and stage, confirm
// and save all run it. A fresh process starts from an EMPTY staged set, so a
// cross-process re-stage re-adds via add_interval; the re-derived ids match the
// previous ones only because nothing was yet durable (the day's NextInterval
// counter is not saved until confirm+save). The caller must have verified via
// openFinalizeDay that the day carries no confirmed intervals.
func applyStage(h *headlessSession, cfg *timesheet.Config, name, date, project string, summary timesheet.DaySummary) (*StageReceipt, error) {
	revision := summary.Revision
	stagedIDs := map[string]bool{}
	segmentByID := map[string]timesheet.Segment{}
	for _, seg := range summary.Staged.Segments {
		if seg.OriginDate == date {
			stagedIDs[seg.IntervalID] = true
			segmentByID[seg.IntervalID] = seg
		}
	}
	entries, err := timesheet.ReadEntries(cfg.StatePath, date)
	if err != nil {
		return nil, err
	}
	if entries == nil {
		return nil, fmt.Errorf("%s: no recorded entries file for %s; run record first", name, date)
	}
	stageMap, err := timesheet.ReadStageMap(cfg.StatePath, date)
	if err != nil {
		return nil, err
	}

	// Deterministic processing order: the recorded blocks sorted by block_id.
	ids := make([]string, 0, len(entries.Entries))
	byID := make(map[string]timesheet.EntryRecord, len(entries.Entries))
	for _, e := range entries.Entries {
		ids = append(ids, e.BlockID)
		byID[e.BlockID] = e
	}
	sort.Strings(ids)

	// The Gradion remote forbids overlapping entries, so the bridge resolves
	// the day's spans before deriving intervals: entries that strictly overlap
	// become one cluster over the union span (issue #183), its intervals carry
	// the joined description, and every merged-away contributor is reported
	// instead of staged. The entries ledger keeps the human's overlapping
	// truth untouched. Each union span is then chunked to the four-hour policy
	// below.
	clusters := timesheet.EffectiveEntrySpans(entries.Entries)

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

	// One staging unit per interval derivation: a single recorded entry (an
	// overnight span) or one overlap cluster under its effective id (the entry's
	// own id for a single entry, the union-derived id for a cluster).
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
		if endMin == startMin {
			// Zero-duration entry, not an overnight span: the interval op
			// rejects start == end ("interval duration must be positive"), so
			// passing it through would abort the whole stage command and block
			// every other entry of the day. Skip it with its own reason.
			receipt.Skipped = append(receipt.Skipped, StageSkip{
				BlockID: id,
				Reason:  fmt.Sprintf("entry has zero duration (start %q, end %q); times are never invented", e.Start, e.End),
			})
			continue
		}
		if endMin < startMin {
			// Overnight span: the cluster projection only reasons about
			// same-day minutes and excludes an end before its start, so this
			// block is absent from the clusters. Pass the recorded span
			// through unsplit — the pre-GAP-SPLIT-1 derivation — and let the
			// interval op resolve the end onto the next day (issue #173).
			units = append(units, stageUnit{
				id: id, text: e.Description, spans: [][2]string{{e.Start, e.End}},
			})
		}
	}
	// Head block_id order keeps the receipt in the documented order for a day
	// that has no merge at all.
	sort.Slice(clusters, func(i, j int) bool { return clusters[i].BlockID < clusters[j].BlockID })
	for _, c := range clusters {
		units = append(units, stageUnit{
			id:    c.BlockID,
			text:  c.Description,
			spans: timesheet.SplitEntrySpans(c.Start, c.End),
		})
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
		// Same-process restage path: the mapped chunks all exist in this
		// process's staged set, so each is kept or edited in place. A fresh
		// process never lands here — its staged set starts empty and the
		// block is re-added below.
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
					if seg.Start == span[0] && seg.End == span[1] && seg.Project == project && seg.Description == u.text {
						receipt.Unchanged = append(receipt.Unchanged, StageBlockRef{BlockID: id, IntervalID: mapped[i], Chunk: ch, Chunks: cs})
						continue
					}
					text := fmt.Sprintf("%s %s-%s %s %s", date, span[0], span[1], project, u.text)
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
		// Fresh (or reshaped) derivation: drop any same-process stale chunks
		// for this block, then add every span.
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
			text := fmt.Sprintf("%s %s-%s %s %s", date, span[0], span[1], project, u.text)
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

	// Entries removed from the file lose their staged intervals (same-process
	// path only; a stale map entry whose interval no longer exists is simply
	// dropped).
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
	// Re-open the day so the receipt reports the staged set exactly as the
	// session now sees it (open_day is idempotent within the process).
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

// openFinalizeDay opens the day for a stage/confirm/save command and refuses
// when it already carries confirmed intervals. The staged set is derived from
// the entries ledger, and a finalized day is never re-derived or silently
// mutated; divergence after confirmation is detected downstream by the plan's
// content hash (SUBMIT-3), never by restaging over confirmed work.
func openFinalizeDay(h *headlessSession, name, date string) (timesheet.DaySummary, error) {
	res, err := h.call("open_day", timesheet.Request{Day: date})
	if err != nil {
		return timesheet.DaySummary{}, err
	}
	summary, ok := res.(timesheet.DaySummary)
	if !ok {
		return timesheet.DaySummary{}, fmt.Errorf("%s: unexpected open_day payload", name)
	}
	if len(summary.Confirmed.Segments) > 0 {
		return timesheet.DaySummary{}, fmt.Errorf(
			"%s: %s already has confirmed intervals; the day is finalized and confirmed work is never restaged", name, date)
	}
	return summary, nil
}

// noteStageSkips mirrors stage's skip list on stderr for confirm and save, so
// the honesty of the never-invented-times rule survives into the commands that
// consume the derived staging. Each block's own receipt reason is printed: a
// merged contributor is not anchorless, and the caller does not assert a cause
// it would have to guess.
func noteStageSkips(name string, receipt *StageReceipt) {
	for _, s := range receipt.Skipped {
		fmt.Fprintf(os.Stderr, "%s: skipped block %s: %s\n", name, s.BlockID, s.Reason)
	}
}

// runStage converts the day's recorded entries file into staged intervals.
// Natural order: run --date (collect) first, then stage, then acknowledge
// and/or decide with the collection_id from that collect receipt, then confirm,
// then save. Holds the store's exclusive lock. On a finalized day, plain stage
// refuses (confirmed work is never silently restaged); the explicit --revise
// flag replaces the confirmed set with a fresh derivation from the entries
// ledger (four-hour chunks included) and re-finalizes the day at a new revision.
func runStage(args []string) error {
	f, err := parseFinalizeFlags("stage", args)
	if err != nil {
		return err
	}
	cfg, err := timesheet.LoadConfig(f.configPath)
	if err != nil {
		return err
	}
	project, err := resolveProject("stage", cfg, f.project)
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
	if len(summary.Confirmed.Segments) > 0 {
		if !f.revise {
			return fmt.Errorf(
				"stage: %s already has confirmed intervals; the day is finalized and confirmed work is never restaged (stage --revise replaces the finalized set from the entries ledger and re-finalizes at a new revision)", f.date)
		}
		receipt, err = reviseStage(h, cfg, f.date, project, summary)
	} else {
		receipt, err = applyStage(h, cfg, "stage", f.date, project, summary)
	}
	if err != nil {
		return err
	}
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(receipt)
}

// reviseStage is stage's explicit finalized-day revision: every staged interval
// (the clone of the durable confirmed set the day opened with) is removed, the
// staged set is re-derived from the CURRENT entries ledger with the four-hour
// chunking applied, and the day is confirmed and saved in-process at a NEW
// revision — mirroring the serve flow's reopen → edit → confirm → save revision
// step and classify's finalized-day pattern. Entries no longer in the ledger
// lose their intervals; the ledger itself is never modified. Every op refusal
// (source gate, stale revision, interval validation) passes through verbatim
// with the durable day untouched at its saved revision.
func reviseStage(h *headlessSession, cfg *timesheet.Config, date, project string, summary timesheet.DaySummary) (*StageReceipt, error) {
	revision := summary.Revision
	// The pre-derivation removals are the durable intervals this revision
	// replaces; they predate the derivation, so they carry their old durable
	// interval id and no block_id. They are reported in receipt.Removed
	// alongside whatever the derivation itself removes (issue #173).
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
	// Re-open the day so the derivation sees the post-removal staged set:
	// applyStage's same-process fast path keys off the summary's staged
	// segments, and the stale pre-removal snapshot would make it treat the
	// removed chunks as still present and skip re-adding them.
	res, err := h.call("open_day", timesheet.Request{Day: date})
	if err != nil {
		return nil, err
	}
	fresh, ok := res.(timesheet.DaySummary)
	if !ok {
		return nil, fmt.Errorf("stage: unexpected open_day payload")
	}
	receipt, err := applyStage(h, cfg, "stage", date, project, fresh)
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

// --- SUBMIT-2 classification commands --------------------------------------------
//
// classifications and classify wire the existing classifications and
// select_classification session ops into the one-shot headless flow. The
// natural order is: run --date (collect) → record → stage → classifications
// (fetch the catalog) → classify (apply selections) → confirm → save.

// runClassifications wraps the classifications op: one bounded authoritative
// catalog read against the configured Gradion workspace, whose result is
// persisted in the store's bounded catalog cache (the most recent
// classifications fetches; see maxStoredCatalogs). This is the one command in
// the stage→classify flow that needs live access: it opens an MCP session and
// performs network IO with the configured credential, so without a Gradion
// configuration (or with a missing credential) it fails closed with the
// op's own error and nothing is fetched or stored. The printed receipt is the
// catalog record's choices exactly as the session returns them: catalog_id,
// query, complete, observed_at, and the options (id, name, code,
// activity_type) a later classify selection references by id.
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

// runClassify applies the day's classification choices through the
// select_classification op, and has two paths:
//
//   - Not yet finalized (the day carries no durable confirmed intervals): it
//     re-derives the day's staged set exactly as stage/confirm/save do (same
//     entries ledger, same sidecar-backed derivation, so interval ids match
//     the ids a stage receipt printed for the same entries state), then
//     applies each selection in choices.json in interval_id order, and
//     persists the selections — each content-bound to the recorded block it
//     was chosen for via BlockContentDigest — to the
//     entries/<date>.classify.json sidecar so confirm and save can replay
//     them. Selection ids must be copied verbatim from the current stage or
//     classify receipt: an unknown or renumbered interval id fails closed
//     with the op's own error, untouched, and the sidecar is not written. A
//     `{}` choices file is a valid honest no-op: nothing is selected, and the
//     sidecar is rewritten empty (clearing any earlier selections). After
//     classify the day remains staged; nothing durable happens until confirm
//     and save.
//
//   - Already finalized (a saved day that carries durable confirmed
//     intervals, e.g. it was confirmed and saved before any classification
//     was selected — previously this day could never become plan-ready, a
//     deadlock): the day is opened normally (open_day semantics, so Staged is
//     the clone of the durable confirmed intervals — never re-derived, never
//     restaged), the selections key off the durable interval ids exactly as
//     `status --date` prints them, and the command drives the day to a NEW
//     durable revision inside this process — confirm then save, the serve
//     flow's reopen → select → confirm → save revision step — because the
//     selections live only on Staged otherwise. No sidecar is written on this
//     path: the selections become durable immediately. A `{}` choices file is
//     still an honest no-op: nothing is selected and the saved day is left
//     untouched at its current revision. A stale or unknown interval id fails
//     closed with the op's own error before anything durable happens
//     (select_classification mutates only the in-memory staged clone, so the
//     saved revision is unchanged and no receipt prints).
func runClassify(args []string) error {
	f, err := parseClassifyFlags(args)
	if err != nil {
		return err
	}
	if f.configuredDefault {
		return runClassifyDefault(f)
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
	// f.project passes through unresolved: only the not-yet-finalized branch
	// needs a project, and it resolves there, so a finalized day still
	// classifies without --project under a multi-project configuration.
	receipt, err := classifyFileDay(h, cfg, f.date, f.project, f.file)
	if err != nil {
		return err
	}
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(receipt)
}

// classifyFileDay is classify --file's whole flow over one held session: read
// the bound answer file, open the day, and either re-finalize the durable
// intervals (no project needed) or resolve the project and re-derive the
// staged set with the content-bound sidecar. The tui verb's classify act is
// the same flow over its own held session.
func classifyFileDay(h *headlessSession, cfg *timesheet.Config, date, project, answerFile string) (ClassifyReceipt, error) {
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
	if len(summary.Confirmed.Segments) > 0 {
		if err := checkSavedClassificationBinding(h, date, data); err != nil {
			return ClassifyReceipt{}, err
		}
		return runClassifyFinalized(h, date, answerFile, summary, selections)
	}
	project, err = resolveProject("classify", cfg, project)
	if err != nil {
		return ClassifyReceipt{}, err
	}
	receipt, err := classifyFileCore(h, cfg, date, project, summary, selections)
	if err != nil {
		return ClassifyReceipt{}, err
	}
	receipt.File = answerFile
	return receipt, nil
}

// classifyFileCore is classify's not-yet-finalized core, shared by the verb
// and the tui verb's classify act: re-derive the staged set, apply each
// selection in interval_id order, and persist the selections content-bound to
// the recorded blocks this derivation mapped them to.
func classifyFileCore(h *headlessSession, cfg *timesheet.Config, date, project string, summary timesheet.DaySummary, selections map[string]timesheet.ClassificationChoiceEntry) (ClassifyReceipt, error) {
	staged, err := applyStage(h, cfg, "classify", date, project, summary)
	if err != nil {
		return ClassifyReceipt{}, err
	}
	noteStageSkips("classify", staged)
	applied, err := applySelections(h, date, summary.Revision, selections)
	if err != nil {
		return ClassifyReceipt{}, err
	}
	// Content-bind each selection to the recorded block this derivation just
	// mapped its interval to, so a later replay can prove the day still holds
	// the exact blocks the choices were made against (staged ids re-derive
	// sequentially; without the binding an entries shift would re-attach a
	// selection to the wrong successor interval).
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

// runClassifyFinalized is classify's finalized-day path. The day was opened
// via open_day, so its staged set is the clone of the durable confirmed
// intervals — stable ids the entries ledger plays no part in — and the
// selections apply directly against those ids at the day's current (saved)
// revision. With at least one applied selection the day is driven to a new
// durable revision: confirm then save within this same process, mirroring the
// serve flow's reopen → select_classification → confirm → save revision step
// (staged selections are session-only, so skipping the re-finalize would
// silently drop them). An empty applied set is an honest no-op: no confirm
// and no save run, and the receipt says the saved day was left unchanged.
// Every op refusal passes through verbatim, so a stale or unknown interval id
// (unknown_interval), a catalog that is not stored (stale_catalog) or a
// classification the catalog did not return (unknown_classification) fails
// closed with the day untouched at its saved revision.
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

// parseClassifyFlags parses classify's flag surface: the finalize flags plus
// either a --file choices path or --configured-default.
func parseClassifyFlags(args []string) (*finalizeFlags, error) {
	flags := flag.NewFlagSet("classify", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	f := &finalizeFlags{}
	flags.BoolVar(&f.configuredDefault, "configured-default", false, "fill unselected unsaved intervals with the configured cached default")
	flags.StringVar(&f.configPath, "config", "", "configuration path (optional; defaults to $HOME/.timesheet/config.json)")
	flags.StringVar(&f.envFile, "env-file", "", "environment-file path (optional; defaults to $HOME/.timesheet/.env.local when it exists)")
	flags.StringVar(&f.date, "date", "", "day to operate on, YYYY-MM-DD (required)")
	flags.StringVar(&f.project, "project", "", "project id for staged intervals (required when configuration defines multiple projects)")
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
	if (f.file != "") == f.configuredDefault {
		return nil, fmt.Errorf("classify requires exactly one of --file or --configured-default")
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
			return nil, err
		}
		mutation, ok := res.(timesheet.ClassificationSelection)
		if !ok {
			return nil, fmt.Errorf("classify: unexpected select_classification payload")
		}
		applied = append(applied, ClassifyApplied{
			IntervalID: id, CatalogID: sel.CatalogID, ClassificationID: sel.ClassificationID,
			Classification: mutation.Classification, Code: mutation.Code,
			CatalogComplete: mutation.CatalogComplete,
		})
	}
	return applied, nil
}

// stagedBlocks binds each staged interval id from the day's stage map to the
// effective merged content it was derived from: the stage projection's union
// span and joined description for a cluster, not the head entry alone. Both
// classify (to content-bind its selections at write time) and the confirm/save
// replay (to re-verify the binding) use it; applyStage has just rewritten the
// stage map in both paths, so the binding reflects the derivation this process
// actually staged. Because the digest covers the whole merged content, changing
// any contributor of a merged interval fails the replay closed.
func stagedBlocks(cfg *timesheet.Config, date string) (map[string]timesheet.EntryRecord, error) {
	entries, err := timesheet.ReadEntries(cfg.StatePath, date)
	if err != nil {
		return nil, err
	}
	stageMap, err := timesheet.ReadStageMap(cfg.StatePath, date)
	if err != nil {
		return nil, err
	}
	effective := map[string]timesheet.EntryRecord{}
	if entries != nil {
		for _, c := range timesheet.EffectiveEntrySpans(entries.Entries) {
			effective[c.BlockID] = timesheet.EntryRecord{
				BlockID: c.BlockID, Date: date, Start: c.Start, End: c.End,
				Kind: c.Kind, Description: c.Description,
			}
		}
		// Entries the projection excludes (an overnight span the caller passes
		// through unsplit, an unparsable or zero-duration entry) keep their own
		// record under their own id.
		for _, e := range entries.Entries {
			if _, ok := effective[e.BlockID]; !ok {
				effective[e.BlockID] = e
			}
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

// applyPersistedSelections replays the day's persisted classification choices
// (entries/<date>.classify.json) against the freshly re-derived staged set.
// This is how selections cross the process boundary: confirm and save call it
// after their own re-derivation, so the intervals they confirm carry
// remote_classification_id and save commits it durably. Only a
// content-identical re-derivation replays: each persisted selection carries
// the BlockContentDigest of the recorded block it was chosen for, and any
// entries change since classify — an insertion shifting the sequential ids
// onto different blocks, a same-count replacement, a time or description
// edit — makes the recomputed digest differ, which fails closed naming the
// block instead of re-attaching the selection to the wrong interval. A sidecar
// id with no block (or no longer in the staged day) fails closed the same
// way; the review-again wording tells the operator the fix: re-run stage +
// classify. A missing or empty sidecar replays nothing (a day never
// classified behaves exactly as SUBMIT-1 left it).
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

// runConfirm derives the staged set from the entries ledger, then wraps the
// confirm op, including its source gate: every enabled source needs a
// collection attempt for the date, and a non-complete attempt needs an explicit
// acknowledgment. A missing acknowledgment fails with the op's gate error
// naming what is missing, passed through verbatim. The receipt is in-memory
// only; save makes it durable.
func runConfirm(args []string) error {
	f, err := parseFinalizeFlags("confirm", args)
	if err != nil {
		return err
	}
	cfg, err := timesheet.LoadConfig(f.configPath)
	if err != nil {
		return err
	}
	project, err := resolveProject("confirm", cfg, f.project)
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
	if _, _, err := stageAndReplay(h, cfg, "confirm", f.date, project, summary); err != nil {
		return err
	}
	receipt, _, err := confirmAndSave(h, "confirm", f.date, summary.Revision, false)
	if err != nil {
		return err
	}
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(receipt)
}

// stageAndReplay is confirm and save's shared prefix: derive the staged set
// from the entries ledger, then replay the persisted classification selections
// against it, with both receipts' honesty notes on stderr.
func stageAndReplay(h *headlessSession, cfg *timesheet.Config, name, date, project string, summary timesheet.DaySummary) (*StageReceipt, int, error) {
	staged, err := applyStage(h, cfg, name, date, project, summary)
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

// confirmAndSave runs the confirm op and, when throughSave, the durable save
// that commits it. It is confirm and save's shared core and the tui verb's
// save act.
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

// --- SUBMIT-3 plan & identity commands -------------------------------------------
//
// plan, reconcile and resolve wire the existing preview_plan, reconcile_remote
// and resolve_remote session ops into the one-shot headless flow. The natural
// order after save is: plan (preview what submission would write) → when the
// plan or reconcile receipt lists identity blockers, reconcile (fresh
// read-only observation) → the human authors resolve's decision file → plan
// again for a clean plan_id. plan and reconcile need live access (a fresh
// remote read through the configured Gradion workspace); resolve does not.

// runPlan wraps the preview_plan op verbatim: one fresh read-only observation
// of every affected date through the configured Gradion workspace (live
// access, like classifications: without a Gradion configuration or credential
// it fails closed with the op's own remote_disabled/remote_unavailable error,
// and live remote behavior is owner-verified), then the exact PlanPreview
// result printed as deterministic JSON — plan_id, state, operations, blockers
// and surfaced remotes, untouched. The op persists the fresh observations and
// the immutable plan record in one durable transaction under this process's
// exclusive store lock, so plan cannot run while a serve process holds the
// store; the durable plan record is exactly what a later approve consumes
// after re-rendering fresh in its own process (nothing is approved here).
// Blockers never auto-clear: each is resolved by an explicit human decision
// (resolve), and because the plan identity includes the exact blockers, the
// re-preview after any resolve yields a NEW plan_id — never reuse the old one.
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

// runReconcile wraps the reconcile_remote op: one fresh read-only observation
// of the date (live access, like plan; fails closed with the op's own
// remote_disabled/remote_unavailable error), then the mapped/changed/missing/
// orphaned identities, unbound remotes and candidates exactly as the op
// returns them. Read-only in the identity sense: the op persists the fresh
// observation record itself (under this process's exclusive store lock, so it
// cannot run while serve holds the store) but it NEVER binds an identity,
// never mutates an existing mapping and never resolves a blocker — the
// receipt's observation_id, revision and unbound/candidate lists are exactly
// the inputs the human copies into resolve's decision file.
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

// resolveFlags carries the resolve inputs. The --file decision JSON is the
// human decision gate: it must be authored by the human after reading the
// reconcile (or plan) receipt, with observation_id, revision and remote_id
// copied verbatim from that receipt.
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

// runResolve wraps the resolve_remote op: it records one explicit human
// identity/content decision (adopt, accept_observed or foreign) against the
// day's current observation. The decision file — authored by the human after
// reading reconcile/plan output — is parsed for shape only; every op-level
// refusal passes through verbatim: a wrong or stale observation_id
// (stale_observation), a remote_id the observation does not carry
// (unknown_remote), adopt without confirmed durable local work that allocates
// onto this date (unknown_local), a decision that would steal or erase an
// active binding (identity_conflict), and a stale day revision
// (stale_revision). resolve performs no remote read; it needs no live access.
// A successful adopt or accept_observed binds (or re-baselines) the mapping
// durably; foreign records the decision without binding. Nothing here approves
// or submits anything.
func runResolve(args []string) error {
	f, err := parseResolveFlags(args)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(f.file)
	if err != nil {
		return fmt.Errorf("resolve: the decision file cannot be read")
	}
	decision, err := timesheet.ParseResolutionDecision(data)
	if err != nil {
		return err
	}
	h, err := openHeadless(f.configPath, f.envFile)
	if err != nil {
		return err
	}
	defer h.done()
	res, err := h.call("resolve_remote", timesheet.Request{
		Day: f.date, Revision: &decision.Revision, ObservationID: decision.ObservationID,
		RemoteID: decision.RemoteID, LocalID: decision.LocalID, Decision: decision.Decision,
	})
	if err != nil {
		return err
	}
	receipt, ok := res.(timesheet.ResolutionReceipt)
	if !ok {
		return fmt.Errorf("resolve: unexpected resolve_remote payload")
	}
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(receipt)
}

// runSave derives the staged set from the entries ledger, confirms it (source
// gate included), and wraps the save op: one durable Commit of the confirmed
// day. After a successful save the day is final; further stage/confirm/save
// runs are refused.
func runSave(args []string) error {
	f, err := parseFinalizeFlags("save", args)
	if err != nil {
		return err
	}
	cfg, err := timesheet.LoadConfig(f.configPath)
	if err != nil {
		return err
	}
	project, err := resolveProject("save", cfg, f.project)
	if err != nil {
		return err
	}
	h, err := openHeadless(f.configPath, f.envFile)
	if err != nil {
		return err
	}
	defer h.done()
	saved, err := saveDay(h, cfg, "save", f.date, project)
	if err != nil {
		return err
	}
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(*saved)
}

// saveDay is the save verb's whole flow over one held session: open the day
// (refusing a finalized one), derive the staged set, replay the persisted
// selections, confirm, and commit durably. The tui verb's save act is the
// same flow over its own held session.
func saveDay(h *headlessSession, cfg *timesheet.Config, name, date, project string) (*timesheet.SaveReceipt, error) {
	summary, err := openFinalizeDay(h, name, date)
	if err != nil {
		return nil, err
	}
	if _, _, err := stageAndReplay(h, cfg, name, date, project, summary); err != nil {
		return nil, err
	}
	_, saved, err := confirmAndSave(h, name, date, summary.Revision, true)
	if err != nil {
		return nil, err
	}
	return saved, nil
}

// --- SUBMIT-4 human-gate commands -------------------------------------------------
//
// approve, submit and resolve-write are the only commands that commit a human
// decision against Gradion remotely: approve binds an authorization, submit
// dispatches the approved operations, and resolve-write judges an unknown
// write outcome. All three are structurally human-only (interactive.go): each
// refuses unless both stdin and stdout are terminals, and each requires
// typing an exact id at an interactive prompt, so agent shells physically
// cannot execute them. The TTY check lives in this CLI wrapper only — the
// Session ops stay transport-agnostic. submit-status is read-only and NOT
// gated. Every Session op refusal passes through verbatim; receipts are
// printed exactly as the op returns them; nothing is ever retried.

// renderPlanBody renders a plan preview's operations and blockers as the
// human-readable summary shown at the approve prompt. The prompt is the
// decision surface, so an operation's audit reason and task — part of the
// exact approved body approve_plan binds — are shown when non-empty.
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

// runApprove renders the CURRENT plan for the date fresh through preview_plan
// (a stored plan record is never trusted or shown — the display and the
// approval both bind to what the remote shows right now), displays the
// plan_id, state, operations and blockers to the human, and requires typing
// the exact plan_id. On confirmation it calls the approve_plan op with the
// day and plan_id; every op refusal (stale_plan, plan_not_ready, an uncertain
// store, a missing credential, a changed catalog, ...) passes through
// verbatim. On success the ApprovalReceipt is printed on stdout, with the
// approval_id — the input to submit — echoed prominently on the terminal.
// The gate fires before the store opens, so a refusal makes zero remote calls.
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
	res, err := h.call("preview_plan", timesheet.Request{Day: f.date})
	if err != nil {
		return err
	}
	preview, ok := res.(timesheet.PlanPreview)
	if !ok {
		return fmt.Errorf("approve: unexpected preview_plan payload")
	}
	prompt := fmt.Sprintf("About to approve the current plan for %s\n  plan_id: %s\n  state:   %s\n%s",
		f.date, preview.PlanID, preview.State, renderPlanBody(preview))
	if err := term.confirm(prompt, preview.PlanID); err != nil {
		return err
	}
	res, err = h.call("approve_plan", timesheet.Request{Day: f.date, PlanID: preview.PlanID})
	if err != nil {
		return err
	}
	receipt, ok := res.(timesheet.ApprovalReceipt)
	if !ok {
		return fmt.Errorf("approve: unexpected approve_plan payload")
	}
	fmt.Fprintf(term.out, "approved: approval_id %s for plan %s (%d operation(s)) — this approval_id is the input to submit\n",
		receipt.ApprovalID, receipt.PlanID, receipt.Operations)
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(receipt)
}

// submitFlags carries the submit inputs: the finalize flags plus the required
// --approval-id copied verbatim from the approve receipt.
type submitFlags struct {
	configPath string
	envFile    string
	date       string
	approvalID string
}

// parseSubmitFlags parses submit's flag surface: the finalize flags plus the
// required --approval-id copied verbatim from the approve receipt.
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

// runSubmit wraps the submit_plan op: it displays the approval id and day,
// requires typing the exact approval id to confirm, then dispatches exactly
// what the approval bound. The SubmissionReceipt is printed faithfully — every
// aggregate count, per-reference status, the blocked_before_dispatch and
// durability_uncertain states, blockers and next_action — and every op
// refusal passes through verbatim. Zero remote calls happen without a
// persisted active approval; the receipt says so.
func runSubmit(args []string) error {
	f, err := parseSubmitFlags(args)
	if err != nil {
		return err
	}
	term, err := interactiveFactory("submit", "submit")
	if err != nil {
		return err
	}
	prompt := fmt.Sprintf("About to submit day %s using approval %s\nThe submission dispatches the approved operations to Gradion; writes are never retried and unknown outcomes stay unresolved until you resolve them.", f.date, f.approvalID)
	if err := term.confirm(prompt, f.approvalID); err != nil {
		return err
	}
	h, err := openHeadless(f.configPath, f.envFile)
	if err != nil {
		return err
	}
	defer h.done()
	res, err := h.call("submit_plan", timesheet.Request{Day: f.date, ApprovalID: f.approvalID})
	if err != nil {
		return err
	}
	receipt, ok := res.(timesheet.SubmissionReceipt)
	if !ok {
		return fmt.Errorf("submit: unexpected submit_plan payload")
	}
	fmt.Fprintf(term.out, "submission status: %s (confirmed %d, unresolved %d, unattempted %d)\n",
		receipt.Status, receipt.Confirmed, receipt.Unresolved, receipt.Unattempted)
	_, _ = h.call("close", timesheet.Request{})
	return printJSON(receipt)
}

// runSubmitStatus wraps the submission_status op verbatim: the date's
// approvals, execution history, every unresolved native or migrated write
// with its write_ref, and the next_safe_action. It is read-only (no remote
// call, no durable write) and deliberately NOT TTY-gated: the agent may run
// it so a human can decide recovery.
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

// parseResolveWriteFlags parses resolve-write's flag surface. There is no
// --date: the write reference carries its own date.
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

// writeResolutionDecision is the human-authored resolve-write decision file.
// Its fields match the resolve_write op's request exactly: the write_ref copied
// verbatim from submission_status, the observation_id of the CURRENT complete
// observation for the write's date (run reconcile — or plan — first; a stale
// or incomplete observation is refused by the op), the decision, and for
// adopt the remote_id of the explicitly selected observed entry.
type writeResolutionDecision struct {
	WriteRef      string `json:"write_ref"`
	ObservationID string `json:"observation_id"`
	Decision      string `json:"decision"`
	RemoteID      string `json:"remote_id"`
}

// parseWriteResolutionDecision parses the decision file for shape only. Unknown
// fields are refused so a typo can never silently drop a decision; every
// op-level refusal still passes through verbatim at resolve time.
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

// runResolveWrite wraps the resolve_write op with a human-authored decision
// file (--file) and a typed confirmation: it is a human judgment on an
// unknown outcome, so it is TTY-gated like approve and submit. The decision
// file is parsed for shape only; every op refusal passes through verbatim —
// a wrong or stale observation_id (stale_observation), an unknown remote_id,
// an adopt whose content does not match the intended operation
// (identity_conflict), a not_applied contradicted by an observed matching
// effect, a confirmed or already-resolved write_ref, and an uncertain store.
// The observation prerequisite: the decision file's observation_id must name
// the CURRENT complete observation for the write's date, which only a prior
// reconcile (or plan) run has persisted.
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
