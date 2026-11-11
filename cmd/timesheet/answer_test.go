package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

type writtenAnswer struct {
	Digest  string `json:"digest"`
	Answer  string `json:"answer"`
	Surface string `json:"surface"`
}

func readWrittenAnswer(t *testing.T, path string) writtenAnswer {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the answer file: %v", err)
	}
	var a writtenAnswer
	if err := json.Unmarshal(data, &a); err != nil {
		t.Fatalf("the answer file is not JSON: %v\n%s", err, data)
	}
	return a
}

func pendingAsk(t *testing.T, cfg string) *askHuman {
	t.Helper()
	rep, err := advanceDoc(t, "--config", cfg, "--date", testDate, "--do")
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if rep.State != stateAskHuman || rep.Ask == nil {
		t.Fatalf("next = %+v, want ask_human", rep.nextAction)
	}
	return rep.Ask
}

func logDay(t *testing.T, cfg string) advanceReport {
	t.Helper()
	out, _, err := captureOutput(t, func() error { return runLog([]string{"--config", cfg, "--date", testDate}) })
	if err != nil {
		t.Fatalf("log: %v\n%s", err, out)
	}
	var rep advanceReport
	if err := json.Unmarshal(out, &rep); err != nil {
		t.Fatalf("log did not print one JSON report: %v\n%s", err, out)
	}
	return rep
}

func assertAcknowledged(t *testing.T, cfg string, rep advanceReport) {
	t.Helper()
	acked := false
	for _, r := range rep.Ran {
		if r.Step == "acknowledge" && r.Result == "ok" {
			acked = true
		}
	}
	if !acked || rep.State == stateAskHuman {
		t.Fatalf("log ran %+v and ended at %q, want acknowledge ok and the gate passed", rep.Ran, rep.State)
	}
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if !store.Evidence(testDate).Attempts[0].Acknowledged {
		t.Fatal("the answered acknowledgement was not recorded in the store")
	}
}

func runAnswerCapture(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	out, _, err := captureOutput(t, func() error { return runAnswer(args) })
	return out, err
}

func TestAnswerTerminalWritesTheBoundAnswerAndLogConsumesIt(t *testing.T) {
	cfg := incompleteSourceDay(t)
	q := pendingAsk(t, cfg)
	screen := injectTTYScreen(t, "acknowledge")
	out, err := runAnswerCapture(t, "--config", cfg, "--date", testDate, "--kind", askSourceAck, "acknowledge")
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	for _, want := range []string{q.Question, "options: acknowledge"} {
		if !strings.Contains(screen.String(), want) {
			t.Fatalf("answer did not show %q before writing:\n%s", want, screen)
		}
	}
	var receipt answerReceipt
	if err := json.Unmarshal(out, &receipt); err != nil || receipt.Surface != "terminal" || receipt.AnswerFile != q.AnswerFile {
		t.Fatalf("receipt = %+v (%v)\n%s", receipt, err, out)
	}
	if want := "timesheet log --config " + shellJoin([]string{cfg}) + " --date " + testDate; receipt.Next != want {
		t.Fatalf("receipt next = %q, want %q", receipt.Next, want)
	}
	got := readWrittenAnswer(t, q.AnswerFile)
	if got != (writtenAnswer{Digest: q.Digest, Answer: "acknowledge", Surface: "terminal"}) {
		t.Fatalf("answer file = %+v, want the current digest, the answer and surface terminal", got)
	}
	assertAcknowledged(t, cfg, logDay(t, cfg))
}

func TestAnswerTerminalRefusesAWrongAnswerWithTheLastErrorText(t *testing.T) {
	cfg := incompleteSourceDay(t)
	q := pendingAsk(t, cfg)
	injectTTY(t, "sure")
	_, err := runAnswerCapture(t, "--config", cfg, "--date", testDate, "sure")
	if err == nil {
		t.Fatal("an off-menu answer was accepted")
	}
	if _, statErr := os.Stat(q.AnswerFile); !os.IsNotExist(statErr) {
		t.Fatalf("a refused answer wrote the answer file (%v)", statErr)
	}

	body, _ := json.Marshal(map[string]string{"digest": q.Digest, "answer": "sure", "surface": "terminal"})
	writeAnswerFile(t, q.AnswerFile, string(body))
	lastError := pendingAsk(t, cfg).LastError
	if lastError == "" || !strings.Contains(err.Error(), "last_error: "+lastError) {
		t.Fatalf("refusal = %v, want the agent path's last_error %q", err, lastError)
	}
}

func TestAnswerTerminalReplacesAStaleAnswer(t *testing.T) {
	cfg := incompleteSourceDay(t)
	q := pendingAsk(t, cfg)
	stale, _ := json.Marshal(map[string]string{"digest": strings.Repeat("0", 64), "answer": "acknowledge", "surface": "chat"})
	writeAnswerFile(t, q.AnswerFile, string(stale))
	if err := os.Chmod(q.AnswerFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if le := pendingAsk(t, cfg).LastError; !strings.Contains(le, "stale or missing") {
		t.Fatalf("stale answer last_error = %q", le)
	}
	injectTTY(t, "acknowledge")
	if _, err := runAnswerCapture(t, "--config", cfg, "--date", testDate, "acknowledge"); err != nil {
		t.Fatalf("answer over a stale file: %v", err)
	}
	if info, err := os.Stat(q.AnswerFile); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("answer file mode = %v (%v), want 0600 even over a 0644 file", info.Mode().Perm(), err)
	}
	if got := readWrittenAnswer(t, q.AnswerFile); got.Digest != q.Digest || got.Surface != "terminal" {
		t.Fatalf("answer file = %+v, want the current digest", got)
	}
	assertAcknowledged(t, cfg, logDay(t, cfg))
}

func TestAnswerRefusesAStaleFormDigestAndAnUnknownAsk(t *testing.T) {
	cfg := incompleteSourceDay(t)
	q := pendingAsk(t, cfg)
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatal(err)
	}
	conf, err := timesheet.LoadConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, kind, digest, want string }{
		{"stale digest", askSourceAck, strings.Repeat("0", 64), "stale or missing"},
		{"another kind", askResolve, q.Digest, "the pending ask is " + askSourceAck},
		{"an unknown kind", "project", q.Digest, "the pending ask is " + askSourceAck},
	}
	for _, tc := range cases {
		_, err := answerAsk(conf, cfg, "", testDate, store, pendingAnswer{
			Kind: tc.kind, Digest: tc.digest, Answer: "acknowledge", Surface: "tui",
		}, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
	_ = store.Close()
	if _, statErr := os.Stat(q.AnswerFile); !os.IsNotExist(statErr) {
		t.Fatalf("a refused answer wrote the answer file (%v)", statErr)
	}
}

func TestAnswerRefusesADayWithNoPendingAsk(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	cfg := writeTestConfig(t)
	injectTTY(t, "acknowledge")
	if _, err := runAnswerCapture(t, "--config", cfg, "--date", testDate, "acknowledge"); err == nil ||
		!strings.Contains(err.Error(), "no ask_human question is pending") {
		t.Fatalf("answer with nothing asked = %v", err)
	}
}

func TestAnswerIsTerminalOnlyBeforeTheStoreOpens(t *testing.T) {
	cfg := incompleteSourceDay(t)
	q := pendingAsk(t, cfg)
	store, err := timesheet.OpenStore(statePathFor(cfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	old := interactiveFactory
	interactiveFactory = newInteractive
	t.Cleanup(func() { interactiveFactory = old })
	if _, err := runAnswerCapture(t, "--config", cfg, "--date", testDate, "acknowledge"); err == nil ||
		!strings.Contains(err.Error(), "interactive-only") {
		t.Fatalf("non-terminal answer with the store held = %v, want the TTY refusal first", err)
	}
	if _, statErr := os.Stat(q.AnswerFile); !os.IsNotExist(statErr) {
		t.Fatalf("a refused answer wrote the answer file (%v)", statErr)
	}
}

func TestAnswerUsage(t *testing.T) {
	injectTTY(t, "acknowledge")
	for _, args := range [][]string{
		{"--date", testDate},
		{"--date", testDate, "a", "b"},
		{"acknowledge"},
		{"--date", "11/11/2026", "acknowledge"},
	} {
		if _, err := runAnswerCapture(t, args...); err == nil {
			t.Errorf("answer %v was accepted", args)
		}
	}
}

func TestTuiAnswerFormWritesSurfaceTuiAndLogConsumesIt(t *testing.T) {
	cfg := incompleteSourceDay(t)
	pendingAsk(t, cfg)
	pretendTTY(t)
	m, err := preflightTui(&nextFlags{configPath: cfg, date: testDate})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	q := m.snap.action.Ask
	if m.snap.action.State != stateAskHuman || q == nil {
		t.Fatalf("first snapshot = %+v, want ask_human", m.snap.action)
	}
	m = drive(t, m, "enter")
	if m.bench.screen != screenAsk || m.bench.digest != q.Digest {
		t.Fatalf("bench = %+v, want the answer form bound to the ask's digest", m.bench)
	}
	frame := stripANSI(m.View())
	for _, want := range []string{q.Question, "acknowledge"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("the answer form lacks %q:\n%s", want, frame)
		}
	}
	m = drive(t, m, "down")
	m = drive(t, m, "enter")
	if m.err != nil {
		t.Fatalf("answer form write: %v", m.err)
	}
	if got := readWrittenAnswer(t, q.AnswerFile); got != (writtenAnswer{Digest: q.Digest, Answer: "acknowledge", Surface: "tui"}) {
		t.Fatalf("answer file = %+v, want surface tui", got)
	}
	if m.bench.screen != screenDay || m.snap.action.Step != "acknowledge" {
		t.Fatalf("after the answer = %+v, want the day screen at the acknowledge action", m.snap.action)
	}
	_ = m.env.h.store.Close()
	assertAcknowledged(t, cfg, logDay(t, cfg))
}

func TestTuiAnswerFormRefusesAStaleDigestAndShowsIt(t *testing.T) {
	cfg := incompleteSourceDay(t)
	pendingAsk(t, cfg)
	pretendTTY(t)
	m, err := preflightTui(&nextFlags{configPath: cfg, date: testDate})
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	defer func() { _ = m.env.h.store.Close() }()
	answerFile := m.snap.action.Ask.AnswerFile
	m = drive(t, m, "enter")
	m.bench.digest = strings.Repeat("0", 64)
	m = drive(t, m, "enter")
	if m.err == nil || !strings.Contains(m.err.Error(), "stale or missing") || m.bench.screen != screenAsk {
		t.Fatalf("stale form = err %v screen %v, want the digest refusal on the form", m.err, m.bench.screen)
	}
	if frame := stripANSI(m.View()); !strings.Contains(frame, "last error") || !strings.Contains(frame, "stale or missing") {
		t.Fatalf("the form does not show the refusal:\n%s", frame)
	}
	if _, statErr := os.Stat(answerFile); !os.IsNotExist(statErr) {
		t.Fatalf("a refused answer wrote the answer file (%v)", statErr)
	}
	m = drive(t, m, "esc")
	if m.bench.screen != screenDay {
		t.Fatalf("esc left screen %v", m.bench.screen)
	}
}

func TestTuiAnswerFormOffersNoFormForResolve(t *testing.T) {
	m := dayModel()
	m.snap.action = nextAction{Day: m.snap.day}.withAsk(askHuman{Kind: askResolve, Options: resolveDecisions})
	m, eff := update(m, keyEvent{key: "enter"})
	if eff != nil || m.bench.screen != screenDay || m.err == nil || !strings.Contains(m.err.Error(), "submit resolve") {
		t.Fatalf("resolve ask enter = screen %v err %v, want a refusal naming the resolve route", m.bench.screen, m.err)
	}
}

func TestTuiAnswerFormSanitizesRecordedContent(t *testing.T) {
	const hostile = "evil\x1b[31mred\x07tail"
	m := tuiModel{env: tuiEnv{styles: testStyles()}, err: errForTest(hostile), bench: benchState{
		screen: screenAsk, kind: hostile, question: hostile, digest: hostile, answerFile: hostile,
		options: []string{hostile, hostile},
	}}
	frame := stripANSI(m.View())
	for _, r := range frame {
		if unicode.IsControl(r) && r != '\n' {
			t.Fatalf("the answer form carried control rune %q:\n%q", r, frame)
		}
	}
	if !strings.Contains(frame, "[31m") || !strings.Contains(frame, "evil") || !strings.Contains(frame, "tail") {
		t.Fatalf("the answer form lost recorded payload:\n%q", frame)
	}
}

func TestAskNotificationNamesTheAnswerCommand(t *testing.T) {
	cfg := &timesheet.Config{}
	policy := cfg.EffectiveDaily()
	ask := nextAction{Day: "d", State: stateAskHuman, Ask: &askHuman{
		Kind: askSourceAck, Question: "acknowledge github?", Options: []string{"acknowledge"}, AnswerFile: "/s/ask.json",
	}}
	msg, _ := dailyMessage(cfg, policy, nil, "d", advanceReport{nextAction: ask}, nil)
	if !strings.Contains(msg, "timesheet answer --date d --kind "+askSourceAck+" acknowledge") || !strings.Contains(msg, "timesheet tui --date d") {
		t.Fatalf("ask notification = %q, want the answer command and the tui", msg)
	}
	store := storeFlagArgs("/c/config.json", "/c/.env.local")
	msg, _ = dailyMessage(cfg, policy, store, "d", advanceReport{nextAction: ask}, nil)
	for _, want := range []string{
		"timesheet answer --config /c/config.json --env-file /c/.env.local --date d --kind " + askSourceAck + " acknowledge",
		"timesheet tui --config /c/config.json --env-file /c/.env.local --date d",
		"timesheet log --config /c/config.json --env-file /c/.env.local --date d",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("ask notification = %q, want %q", msg, want)
		}
	}
	multi := nextAction{Day: "d", State: stateAskHuman, Ask: &askHuman{Kind: "future_kind", Question: "q?", Options: []string{"a", "b"}}}
	if msg, _ := dailyMessage(cfg, policy, nil, "d", advanceReport{nextAction: multi}, nil); strings.Contains(msg, "<") ||
		!strings.Contains(msg, "timesheet answer --date d --kind future_kind ") {
		t.Fatalf("multi-option notification = %q, want the command with no shell-redirect placeholder", msg)
	}
	resolve := nextAction{Day: "d", State: stateAskHuman, Ask: &askHuman{
		Kind: askResolve, Question: "which?", Options: resolveDecisions, AnswerFile: "/s/ask-resolve.json",
	}}
	if msg, _ := dailyMessage(cfg, policy, nil, "d", advanceReport{nextAction: resolve}, nil); strings.Contains(msg, "timesheet answer") ||
		!strings.Contains(msg, "/s/ask-resolve.json") {
		t.Fatalf("resolve notification = %q, want the answer file and no answer command", msg)
	}
}

func injectTTYScreen(t *testing.T, lines ...string) *bytes.Buffer {
	t.Helper()
	screen := &bytes.Buffer{}
	old := interactiveFactory
	interactiveFactory = func(name, verb string) (*interactive, error) {
		return &interactive{in: bufio.NewReader(strings.NewReader(strings.Join(lines, "\n") + "\n")), out: screen}, nil
	}
	t.Cleanup(func() { interactiveFactory = old })
	return screen
}

func TestAnswerTerminalRequiresTheTypedConfirmation(t *testing.T) {
	for _, typed := range []string{"nope", ""} {
		cfg := incompleteSourceDay(t)
		q := pendingAsk(t, cfg)
		injectTTY(t, typed)
		if _, err := runAnswerCapture(t, "--config", cfg, "--date", testDate, "acknowledge"); err == nil || !strings.Contains(err.Error(), "aborted") {
			t.Fatalf("typed %q: answer = %v, want an aborted confirmation", typed, err)
		}
		if _, statErr := os.Stat(q.AnswerFile); !os.IsNotExist(statErr) {
			t.Fatalf("typed %q: an aborted answer wrote the answer file (%v)", typed, statErr)
		}
	}
}

func TestAnswerTerminalShowsTheQuestionSanitized(t *testing.T) {
	var screen bytes.Buffer
	term := &interactive{in: bufio.NewReader(strings.NewReader("acknowledge\n")), out: &screen}
	q := &askHuman{Kind: askSourceAck, Question: "evil\x1b[31mred\x07tail", Options: []string{"ack\x1bnowledge"}}
	if err := confirmAnswerOnTerminal(term, "d", q, "acknowledge"); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	for _, r := range screen.String() {
		if unicode.IsControl(r) && r != '\n' {
			t.Fatalf("the terminal prompt carried control rune %q:\n%q", r, screen.String())
		}
	}
	if !strings.Contains(screen.String(), "evil[31mredtail") {
		t.Fatalf("the prompt lost the question:\n%q", screen.String())
	}
}

func TestAnswerTerminalRefusesAResolveAsk(t *testing.T) {
	cfg, _, _ := classifiedSavedDay(t, "Build the bridge",
		[]map[string]any{remoteWeekEntry("r-1", "09:00", "10:00", "cat-1", "Someone else's work")})
	if first := runPlanCapture(t, []string{"--config", cfg, "--date", testDate}); first.State != "blocked" {
		t.Fatalf("plan = %+v, want blocked", first)
	}
	q := pendingAskNoDo(t, cfg)
	if q.Kind != askResolve {
		t.Fatalf("pending ask = %+v, want resolve", q)
	}
	injectTTY(t, "adopt")
	if _, err := runAnswerCapture(t, "--config", cfg, "--date", testDate, "--kind", askResolve, "adopt"); err == nil ||
		!strings.Contains(err.Error(), "submit resolve") {
		t.Fatalf("answer to a resolve ask = %v, want the submit-resolve refusal", err)
	}
	if _, statErr := os.Stat(q.AnswerFile); !os.IsNotExist(statErr) {
		t.Fatalf("a refused resolve answer wrote the answer file (%v)", statErr)
	}
}

func pendingAskNoDo(t *testing.T, cfg string) *askHuman {
	t.Helper()
	rep, err := advanceDoc(t, "--config", cfg, "--date", testDate)
	if err != nil || rep.State != stateAskHuman || rep.Ask == nil {
		t.Fatalf("next = %+v (%v), want ask_human", rep.nextAction, err)
	}
	return rep.Ask
}

func TestTuiFooterOffersTheAnswerFormOnlyWhenAnswerable(t *testing.T) {
	m := tuiModel{env: tuiEnv{styles: testStyles()}}
	m.snap.action = nextAction{Day: "d"}.withAsk(askHuman{Kind: askSourceAck, Options: []string{"acknowledge"}})
	if f := stripANSI(renderFooter(m)); !strings.Contains(f, "[enter] answer form") {
		t.Fatalf("answerable ask footer = %q", f)
	}
	m.snap.action = nextAction{Day: "d"}.withAsk(askHuman{Kind: askResolve, Options: resolveDecisions})
	if f := stripANSI(renderFooter(m)); strings.Contains(f, "answer form") || !strings.Contains(f, "[r] re-derive") {
		t.Fatalf("resolve ask footer = %q, want no answer form", f)
	}
}
