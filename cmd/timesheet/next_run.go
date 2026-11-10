package main

// runNext is the `next` verb boundary: flag parsing, the observation pass, the
// optional --do loop, and the typed JSON print. The read-only path closes the
// store before deriving or printing; --do never holds a store across an act
// (store.go's flock is per open file descriptor, so a handle held across an act
// would self-lock).

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// nextFlags is the surface shared by the next and tui verbs: both take the
// store-path flags, --date and --project, and both resolve an omitted --date
// through the binary's existing helper. --do and --max-actions are next's
// flags; tui refuses both in its own runner.
type nextFlags struct {
	configPath    string
	envFile       string
	date          string
	project       string
	do            bool
	maxActions    int
	maxActionsSet bool
}

func parseNextFlags(name string, args []string) (*nextFlags, error) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	f := &nextFlags{}
	flags.StringVar(&f.configPath, "config", "", "configuration path (optional; defaults to $HOME/.timesheet/config.json)")
	flags.StringVar(&f.envFile, "env-file", "", "environment-file path (optional; defaults to $HOME/.timesheet/.env.local when it exists)")
	flags.StringVar(&f.date, "date", "", "day to operate on, YYYY-MM-DD (optional; omitted means today in the configured timezone)")
	flags.StringVar(&f.project, "project", "", "project id for staged/classified intervals (required when the configuration defines multiple projects)")
	flags.BoolVar(&f.do, "do", false, "execute each derived action in-process until a judgment, stop or human gate")
	flags.IntVar(&f.maxActions, "max-actions", defaultMaxActions, "with --do: the most actions one run may execute before stopping")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	flags.Visit(func(fl *flag.Flag) {
		if fl.Name == "max-actions" {
			f.maxActionsSet = true
		}
	})
	if flags.NArg() != 0 {
		return nil, fmt.Errorf("%s does not accept positional arguments", name)
	}
	if f.do && f.maxActions < 1 {
		return nil, fmt.Errorf("%s: --max-actions must be at least 1", name)
	}
	configPath, envFile, err := resolveStoreFlags(name, f.configPath, f.envFile)
	if err != nil {
		return nil, err
	}
	f.configPath, f.envFile = configPath, envFile
	if dateSupplied(flags) {
		if _, err := time.Parse("2006-01-02", f.date); err != nil {
			return nil, fmt.Errorf("%s: --date must be YYYY-MM-DD", name)
		}
	}
	return f, nil
}

// runNext derives and prints ONE typed action. Without --do it is read-only:
// no artifact, store, sidecar or config is written. With --do it executes each
// deterministic action the derivation licenses, opening and closing the store
// around every act, and stops at the first judgment, stop or human gate.
//
// Reporting boundary: an underivable STATE (unreadable durable fact, artifact
// or sidecar) prints one typed `stopped` document on stdout; only CLI/flag
// errors, config load failures and the store's own lock refusal
// (store_locked) remain nonzero process errors.
func runNext(args []string) error {
	f, err := parseNextFlags("next", args)
	if err != nil {
		return err
	}

	configPath, err := filepath.Abs(f.configPath)
	if err != nil {
		return fmt.Errorf("next: the configuration path cannot be made absolute")
	}
	envFile := ""
	if f.envFile != "" {
		if envFile, err = filepath.Abs(f.envFile); err != nil {
			return fmt.Errorf("next: the environment-file path cannot be made absolute")
		}
	}
	cfg, err := timesheet.LoadConfig(configPath)
	if err != nil {
		return err
	}
	day, err := resolveDay(configPath, f.date)
	if err != nil {
		return err
	}
	if f.do {
		return runNextDo(f, cfg, configPath, envFile, day)
	}
	store, err := timesheet.OpenStore(cfg.StatePath)
	if err != nil {
		return err
	}
	obs, stop, err := buildNextObservation(cfg, configPath, envFile, day, f.project, store)
	if closeErr := store.Close(); closeErr != nil {
		return fmt.Errorf("next: the store lock cannot be released")
	}
	if err != nil {
		return err
	}
	if stop != nil {
		return printJSON(*stop)
	}
	return printJSON(deriveDayNext(obs))
}

// buildNextObservation assembles one day's nextObservation over the caller's
// already-open store. The caller owns the store's lifetime: runNext closes it
// before deriving or printing, and the tui verb holds the lock across ticks.
// An underivable STATE (unreadable durable fact, artifact or sidecar) returns
// a typed stop document the caller prints; only process-level failures (the
// executable path, project resolution) return an error.
func buildNextObservation(cfg *timesheet.Config, configPath, envFile, day, project string, store *timesheet.Store) (nextObservation, *nextAction, error) {
	exe, err := absoluteExecutable()
	if err != nil {
		return nextObservation{}, nil, err
	}
	dir, err := dayArtifactsDir(cfg.StatePath, day)
	if err != nil {
		return nextObservation{}, nil, err
	}
	obs := nextObservation{
		Day:              day,
		Exe:              exe,
		ConfigPath:       configPath,
		EnvFile:          envFile,
		DraftPath:        filepath.Join(dir, "draft.json"),
		DescriptionsPath: filepath.Join(dir, "descriptions.json"),
		ChoicesPath:      filepath.Join(dir, "choices.json"),
		SidecarPath:      timesheet.ChoicesPath(cfg.StatePath, day),
		GradionEnabled:   cfg.Gradion != nil && cfg.Gradion.Enabled,
	}
	obs.Project, obs.ProjectBlocked, obs.ProjectIDs, err = resolveNextProject(cfg, project)
	if err != nil {
		return nextObservation{}, nil, err
	}

	// A day left in the pre-#233 temp location is reported, never restarted.
	legacy, err := legacyDayArtifactsStop(cfg.StatePath, day, dir)
	if err != nil {
		return nextObservation{}, nil, err
	}
	if legacy != "" {
		return obs, stoppedAction(day, legacy), nil
	}

	// Durable facts are read under the store's exclusive lock, which the caller
	// holds. An unreadable durable fact is an underivable state and returns a
	// typed stop document; only CLI/flag errors and the store's own lock
	// refusal (store_locked) stay nonzero process errors.
	snapshot, snapErr := timesheet.ReadWorkflowSnapshot(cfg, store, day)
	if snapErr != nil {
		return obs, stoppedAction(day, "cannot read the day's durable workflow facts: "+snapErr.Error()), nil
	}
	obs.Workflow = snapshot

	// The durable snapshot is read under the store lock while the artifact
	// files are lock-free by the binary's existing design (record never opens
	// the store). A writer racing between the two reads can only produce a
	// conservative derivation: every derived verb re-validates its own inputs
	// fail-closed, and next mutates nothing.
	ledger, err := timesheet.ReadDayArtifacts(cfg.StatePath, day)
	if err != nil {
		return obs, stoppedAction(day, "cannot read the day's recorded artifacts: "+err.Error()), nil
	}
	obs.Artifacts = ledger
	if ledger.Entries != nil {
		obs.Units = ledgerUnits(ledger.Entries.Entries, day)
	}
	if !snapshot.Finalized && snapshot.ConfiguredDefaultPresent && ledger.EntriesPresent && len(ledger.Entries.Entries) > 0 {
		return obs, nil, nil
	}

	// One read per artifact. The draft is read for its content digest (the
	// descriptions answer is bound to it); each answer file is read once for
	// its binding. Nothing is read twice, so a file cannot change between a
	// presence check and a content check.
	draft, err := os.ReadFile(obs.DraftPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return obs, stoppedAction(day, "cannot read the draft artifact "+obs.DraftPath+": "+err.Error()), nil
	default:
		obs.DraftPresent = true
		obs.DraftDigest = draftDigest(draft)
		// Parse the SAME type record unmarshals: an unreadable draft is not
		// judged or recorded against, it is re-collected.
		var parsed timesheet.Draft
		if err := json.Unmarshal(draft, &parsed); err == nil {
			obs.DraftValid, obs.DraftBlocks, obs.Draft = true, len(parsed.Blocks), &parsed
		}
	}
	obs.DescriptionsPresent, obs.DescriptionsBinding, err = readAnswerBinding(obs.DescriptionsPath, "draft_digest")
	if err != nil {
		return obs, stoppedAction(day, "cannot read the descriptions answer "+obs.DescriptionsPath+": "+err.Error()), nil
	}
	answer, err := readClassificationAnswer(obs.ChoicesPath)
	if err != nil {
		return obs, stoppedAction(day, "cannot read the classification answer "+obs.ChoicesPath+": "+err.Error()), nil
	}
	obs.ChoicesPresent, obs.ChoicesBinding = answer.Present, answer.Binding
	obs.Choices, obs.ChoicesError = answer.Selections, answer.SelectionError
	return obs, nil, nil
}

// stoppedAction is withStop in the pointer form buildNextObservation returns;
// nil stays the derived case.
func stoppedAction(day, reason string) *nextAction {
	n := nextAction{Day: day}.withStop(reason)
	return &n
}

// resolveNextProject returns the exact project id derived commands carry. It
// never guesses: several configured projects require an explicit --project, and
// an explicit id must be one the configuration defines.
func resolveNextProject(cfg *timesheet.Config, explicit string) (string, bool, []string, error) {
	ids := make([]string, 0, len(cfg.Projects))
	for _, p := range cfg.Projects {
		ids = append(ids, p.ID)
	}
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		for _, id := range ids {
			if id == explicit {
				return id, false, ids, nil
			}
		}
		return "", false, ids, fmt.Errorf("next: --project %s is not configured (configured: %s)", explicit, strings.Join(ids, ", "))
	}
	if len(ids) == 1 {
		return ids[0], false, ids, nil
	}
	return "", true, ids, nil
}

// absoluteExecutable is the running binary's absolute path, so a derived argv
// stays executable after the caller changes directory.
func absoluteExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("next: the running executable path cannot be resolved")
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if !filepath.IsAbs(exe) {
		return "", fmt.Errorf("next: the running executable path is not absolute")
	}
	return exe, nil
}
