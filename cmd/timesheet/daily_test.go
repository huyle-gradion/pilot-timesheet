package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func setClock(t *testing.T, now time.Time) {
	t.Helper()
	prev := clockNow
	clockNow = func() time.Time { return now }
	t.Cleanup(func() { clockNow = prev })
}

func TestDailyTargetsCatchUpAndNeverLogAHalfDay(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Ho_Chi_Minh")
	policy := (&timesheet.Config{}).EffectiveDaily()
	cases := map[string]string{
		"2026-10-26 09:00": "2026-10-23",
		"2026-10-26 21:59": "2026-10-23",
		"2026-10-26 22:30": "2026-10-23,2026-10-26",
		"2026-10-27 22:00": "2026-10-26,2026-10-27",
		"2026-10-31 23:00": "2026-10-30",
	}
	for at, want := range cases {
		now, _ := time.ParseInLocation("2006-01-02 15:04", at, loc)
		days, err := dailyTargets("Asia/Ho_Chi_Minh", policy, now)
		if err != nil || strings.Join(days, ",") != want {
			t.Errorf("%s: days = %v, %v; want %s", at, days, err, want)
		}
	}
}

func TestDailyMessageCoversEveryOutcome(t *testing.T) {
	cfg := &timesheet.Config{StatePath: filepath.Join(t.TempDir(), "state.json")}
	policy := cfg.EffectiveDaily()
	gates := nextAction{Day: "d", State: stateHumanGates, Human: &nextHumanGates{ApproveCommand: "timesheet approve --date d"}}
	if msg, send := dailyMessage(cfg, policy, "d", advanceReport{nextAction: gates}, nil); !send || !strings.Contains(msg, "ready to submit") || !strings.Contains(msg, "timesheet approve --date d") {
		t.Fatalf("gates = %q %v", msg, send)
	}
	ask := nextAction{Day: "d", State: stateAskHuman, Ask: &askHuman{Question: "acknowledge github?", Options: []string{"acknowledge"}, AnswerFile: "/s/ask.json"}}
	if msg, _ := dailyMessage(cfg, policy, "d", advanceReport{nextAction: ask}, nil); !strings.Contains(msg, "acknowledge github?") || !strings.Contains(msg, "/s/ask.json") {
		t.Fatalf("ask = %q", msg)
	}
	empty := nextAction{Day: "d", State: stateStopped, Reason: "the draft artifact at x carries no blocks, so record would reject the day"}
	if msg, send := dailyMessage(cfg, policy, "d", advanceReport{nextAction: empty}, nil); !send || !strings.Contains(msg, "nothing to log") {
		t.Fatalf("empty = %q %v", msg, send)
	}
	no := false
	policy.NotifyEmpty = &no
	if _, send := dailyMessage(cfg, policy, "d", advanceReport{nextAction: empty}, nil); send {
		t.Fatal("notify_empty false still sends the empty-day notification")
	}
	stop := nextAction{Day: "d", State: stateStopped, Reason: "judgment_failed: block_descriptions: boom"}
	if msg, _ := dailyMessage(cfg, policy, "d", advanceReport{nextAction: stop}, nil); !strings.Contains(msg, "stopped: judgment_failed") {
		t.Fatalf("stop = %q", msg)
	}
	if msg, _ := dailyMessage(cfg, policy, "d", advanceReport{}, errors.New("config broken")); !strings.Contains(msg, "could not be logged: config broken") {
		t.Fatalf("error = %q", msg)
	}
}

func TestDesktopNotifierPassesTheMessageAsAnArgument(t *testing.T) {
	argv := desktopArgv(nil, `say "hi"; rm -rf /`)
	if argv[len(argv)-1] != `say "hi"; rm -rf /` {
		t.Fatalf("argv = %q, want the message as one argument", argv)
	}
	if got := desktopArgv([]string{"my-notify", "-u"}, "m"); strings.Join(got, " ") != "my-notify -u m" {
		t.Fatalf("custom argv = %q", got)
	}
}

func TestWebhookUsesTheNamedVariableAndNeverEchoesTheURL(t *testing.T) {
	var body map[string]string
	status := http.StatusNoContent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	env := filepath.Join(t.TempDir(), ".env.local")
	if err := os.WriteFile(env, []byte("SYNTHETIC_HOOK="+srv.URL+"/secret-path\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	policy := (&timesheet.Config{Daily: &timesheet.DailyConfig{WebhookEnv: "SYNTHETIC_HOOK", WebhookKind: "slack"}}).EffectiveDaily()
	if err := webhookNotify(policy, env, "hello"); err != nil || !strings.Contains(body["text"], "hello") {
		t.Fatalf("slack post = %v body %v", err, body)
	}
	status = http.StatusInternalServerError
	err := webhookNotify(policy, env, "hello")
	if err == nil || strings.Contains(err.Error(), "secret-path") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("err = %v", err)
	}
}

func TestPrintUnitsCarryTheScheduleAndAbsolutePaths(t *testing.T) {
	cfg := &timesheet.Config{Timezone: "Asia/Ho_Chi_Minh", Daily: &timesheet.DailyConfig{LogAfter: "18:30", Days: []string{"mon", "wed"}}}
	var b strings.Builder
	if err := printUnits(&b, "systemd", cfg, "/home/u/.timesheet/config.json", "/home/u/.timesheet/.env.local"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"OnCalendar=Mon,Wed *-*-* 18:30:00 Asia/Ho_Chi_Minh", "Persistent=true", "OnStartupSec=3min",
		"daily --config /home/u/.timesheet/config.json --env-file /home/u/.timesheet/.env.local", "WantedBy=timers.target"} {
		if !strings.Contains(b.String(), want) {
			t.Fatalf("systemd units lack %q:\n%s", want, b.String())
		}
	}
	b.Reset()
	if err := printUnits(&b, "launchd", cfg, "/c.json", ""); err != nil {
		t.Fatal(err)
	}
	if out := b.String(); !strings.Contains(out, "<key>RunAtLoad</key><true/>") || strings.Count(out, "<key>Weekday</key>") != 2 || !strings.Contains(out, "<integer>18</integer><key>Minute</key><integer>30</integer>") {
		t.Fatalf("launchd plist:\n%s", out)
	}
	if err := printUnits(&b, "cron", cfg, "/c.json", ""); err == nil {
		t.Fatal("an unknown unit kind was accepted")
	}
}

func recordingNotifier(t *testing.T) ([]string, string) {
	t.Helper()
	log := filepath.Join(t.TempDir(), "notifications.log")
	return []string{"sh", "-c", `printf '%s\n' "$1" >> "$0"`, log}, log
}

func TestDailyLogsThePreviousDayToTheGatesAndNotifiesOnce(t *testing.T) {
	stub := &stubRemote{entries: map[string][]map[string]any{testDate: {}}, classifications: stubCatalogText}
	cfg := candidatesFixture(t, stub, timedBlock("work-1", "09:00", "10:00", "Build the bridge"))
	ivRE := regexp.MustCompile(`"interval_id":"([^"]+)"`)
	chat := &fakeChat{answer: func(p string) string { return `{"selections":{"` + ivRE.FindStringSubmatch(p)[1] + `":"cls-dev"}}` }}
	addJudge(t, cfg, map[string]any{"backend": "openai", "base_url": judgeServer(t, chat), "model": "m"})
	notifier, log := recordingNotifier(t)
	addSection(t, cfg, "daily", map[string]any{"desktop_command": notifier})
	loc, _ := time.LoadLocation("Asia/Ho_Chi_Minh")
	setClock(t, time.Date(2026, 10, 27, 9, 0, 0, 0, loc))

	run := func() dailyReceipt {
		out, _, err := captureOutput(t, func() error { return runDaily([]string{"--config", cfg}) })
		if err != nil {
			t.Fatalf("daily: %v\n%s", err, out)
		}
		var r dailyReceipt
		if err := json.Unmarshal(out, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := run()
	if len(r.Days) != 1 || r.Days[0].Day != testDate || r.Days[0].State != stateHumanGates || !r.Days[0].Notified {
		t.Fatalf("receipt = %+v", r)
	}
	data, _ := os.ReadFile(log)
	if !strings.Contains(string(data), testDate+": 1 entry, 1h00m ready to submit") || !strings.Contains(string(data), "approve") {
		t.Fatalf("notification = %q", data)
	}
	if r = run(); r.Days[0].Notified {
		t.Fatalf("an unchanged result was notified twice: %+v", r)
	}
	if data, _ = os.ReadFile(log); strings.Count(string(data), "\n") != 1 {
		t.Fatalf("notifications = %q, want exactly one", data)
	}
}

func TestDailyNeverRunsTwiceAtOnceAndSurvivesANotifierFailure(t *testing.T) {
	cfg := writeTestConfig(t)
	addJudge(t, cfg, map[string]any{"backend": "command", "command": []string{"true"}})
	addSection(t, cfg, "daily", map[string]any{"desktop_command": []string{"false"}})
	c, err := timesheet.LoadConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	unlock, held, err := lockDaily(c)
	if err != nil || !held {
		t.Fatalf("first lock = %v %v", held, err)
	}
	out, _, err := captureOutput(t, func() error { return runDaily([]string{"--config", cfg}) })
	if err != nil || !strings.Contains(string(out), "in progress") {
		t.Fatalf("second run = %s, %v", out, err)
	}
	unlock()

	policy := c.EffectiveDaily()
	notified, notifyErr := notifyOnce(c, policy, "", testDate, "msg")
	if notified || !strings.Contains(notifyErr, "exit status 1") {
		t.Fatalf("failed notifier = %v %q", notified, notifyErr)
	}
}

func addSection(t *testing.T, cfg, key string, value any) {
	t.Helper()
	data, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	doc[key] = value
	out, _ := json.Marshal(doc)
	if err := os.WriteFile(cfg, out, 0o600); err != nil {
		t.Fatal(err)
	}
}
