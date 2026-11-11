package main

// tui is the `tui` verb: the native terminal UI over the same store, config
// and session surface every other command uses. It opens the store once and
// holds the exclusive lock for the whole process; every
// refusal (non-terminal stdio, store_locked) happens before the alt screen
// starts.

import (
	"fmt"
	"os"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// ttyGate is the interactive-only gate, overridable in tests the way
// interactiveFactory is for the human-gate verbs.
var ttyGate = func() bool { return isTerminal(os.Stdin) && isTerminal(os.Stdout) }

func runTui(args []string) error {
	f, err := parseNextFlags("tui", args)
	if err != nil {
		return err
	}
	if f.do {
		return fmt.Errorf("tui: --do is next's reserved flag; the tui verb takes no action flag")
	}
	if f.maxActionsSet {
		return fmt.Errorf("tui: --max-actions is next's --do flag; the tui verb takes no action flag")
	}
	if !ttyGate() {
		return fmt.Errorf("tui is interactive-only: run it in a terminal (both stdin and stdout must be terminals)")
	}
	m, err := preflightTui(f)
	if err != nil {
		return err
	}
	defer func() { _ = m.env.h.store.Close() }()
	return runTuiProgram(m)
}

// preflightTui does everything that must succeed before the alt screen starts:
// load, resolve the day, open the store once and hold the lock, build the
// session over it, derive the first snapshot. A store_locked refusal returns
// here, before any screen exists.
func preflightTui(f *nextFlags) (tuiModel, error) {
	configPath, err := filepath.Abs(f.configPath)
	if err != nil {
		return tuiModel{}, fmt.Errorf("tui: the configuration path cannot be made absolute")
	}
	envFile := ""
	if f.envFile != "" {
		if envFile, err = filepath.Abs(f.envFile); err != nil {
			return tuiModel{}, fmt.Errorf("tui: the environment-file path cannot be made absolute")
		}
	}
	cfg, err := timesheet.LoadConfig(configPath)
	if err != nil {
		return tuiModel{}, err
	}
	day, err := resolveDay(configPath, f.date)
	if err != nil {
		return tuiModel{}, err
	}
	store, err := timesheet.OpenStore(cfg.StatePath)
	if err != nil {
		return tuiModel{}, err
	}
	fileEnv := map[string]string{}
	if envFile != "" {
		loaded, err := timesheet.LoadEnvFile(envFile)
		if err != nil {
			_ = store.Close()
			return tuiModel{}, err
		}
		fileEnv = timesheet.MergeEnv(loaded)
	}
	env := tuiEnv{
		cfg: cfg, configPath: configPath, envFile: envFile, day: day, project: f.project,
		styles: stdoutStyles(),
		// One held session over the TUI's own store, built literally.
		h: &headlessSession{
			session: timesheet.NewSession(cfg, fileEnv, store),
			store:   store, cfg: cfg,
			close: func() { _ = store.Close() },
		},
	}
	first, err := env.tick()
	if err != nil {
		_ = store.Close()
		return tuiModel{}, err
	}
	return tuiModel{env: env, snap: first.snap}, nil
}

func runTuiProgram(m tuiModel) error {
	final, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	if model, ok := final.(tuiModel); ok && model.closeErr != nil {
		fmt.Fprintln(os.Stderr, "tui:", model.closeErr)
	}
	if err != nil {
		// The program died rather than quit: release the session best-effort.
		_, _ = m.env.h.call("close", timesheet.Request{})
		return err
	}
	return nil
}

// Init seeds nothing: the first snapshot is already on the model.
func (m tuiModel) Init() tea.Cmd { return nil }

// Update adapts the driver's messages onto the reducer's events and runs at
// most one effect.
func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	ev := tuiEventFromMsg(msg)
	if ev == nil {
		return m, nil
	}
	next, eff := update(m, ev)
	if next.stop {
		return next, tea.Quit
	}
	if eff == nil {
		return next, nil
	}
	return next, func() tea.Msg { return eff() }
}

func (m tuiModel) View() string {
	switch m.bench.screen {
	case screenBench:
		return renderBench(m)
	case screenGate:
		return renderGate(m)
	default:
		return renderDay(m)
	}
}

// tuiEventFromMsg adapts the driver's messages onto the reducer's events.
// Only KeyRunes and KeySpace messages with Alt unset are text; every other
// key, structural or modified, stays a named keyEvent, so a key like home or
// delete can never reach an open document as text.
func tuiEventFromMsg(msg tea.Msg) any {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		return sizeEvent{height: m.Height}
	case tea.KeyMsg:
		if !m.Alt {
			switch m.Type {
			case tea.KeyRunes:
				return textEvent{runes: m.Runes}
			case tea.KeySpace:
				// The parser keeps the space rune on KeySpace, but a
				// directly built message may not; a space is a space.
				return textEvent{runes: []rune{' '}}
			}
		}
		return keyEvent{key: m.String()}
	default:
		return msg
	}
}
