package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func captureOutput(t *testing.T, fn func() error) (stdout, stderr []byte, err error) {
	t.Helper()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	savedOut, savedErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	defer func() { os.Stdout, os.Stderr = savedOut, savedErr }()

	outDone := make(chan []byte, 1)
	errDone := make(chan []byte, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(outR)
		outDone <- buf.Bytes()
	}()
	go func() {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(errR)
		errDone <- buf.Bytes()
	}()

	runErr := fn()
	outW.Close()
	errW.Close()
	return <-outDone, <-errDone, runErr
}

func writeRunConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	body := `{
  "timezone": "Asia/Ho_Chi_Minh",
  "state_path": "` + filepath.Join(dir, "state.json") + `",
  "sources": {"enabled": true, "calendar": {"enabled": true, "user": "me@example.com"}}
}`
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func startFeed(t *testing.T) (*httptest.Server, *int64) {
	t.Helper()
	var fetches int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&fetches, 1)
		fmt.Fprint(w, "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:evt-1@test\r\nDTSTAMP:20261020T000000Z\r\nDTSTART:20261020T030000Z\r\nDTEND:20261020T040000Z\r\nSUMMARY:Standup\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &fetches
}

// writeRunEnvFile writes a synthetic env file naming the loopback feed. The
// test writes this file as a fixture; it never reads an operator credential
// file and never echoes the value back beyond the fixture itself. It also pins
// GCAL_ICS_URL in the process environment, because an inherited value takes
// precedence over the file and would otherwise point the run at a developer's
// real feed.
func writeRunEnvFile(t *testing.T, feedURL string) string {
	t.Helper()
	t.Setenv("GCAL_ICS_URL", feedURL)
	path := filepath.Join(t.TempDir(), "env.local")
	if err := os.WriteFile(path, []byte("GCAL_ICS_URL="+feedURL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunMatchesDraftByteForByte(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	cfg := writeRunConfig(t)
	srv, fetches := startFeed(t)
	envFile := writeRunEnvFile(t, srv.URL)

	day := "2026-10-20"

	runOut, runErrOut, err := captureOutput(t, func() error {
		return runRun([]string{"--config", cfg, "--env-file", envFile, "--date", day})
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := atomic.LoadInt64(fetches); got != 1 {
		t.Fatalf("collect executed %d times in one run invocation, want exactly 1", got)
	}

	draftOut, _, err := captureOutput(t, func() error {
		return runDraft([]string{"--config", cfg, "--env-file", envFile, "--date", day})
	})
	if err != nil {
		t.Fatalf("draft: %v", err)
	}

	if len(draftOut) == 0 {
		t.Fatal("draft produced no output")
	}
	if !bytes.Equal(runOut, draftOut) {
		t.Fatalf("run stdout differs from draft stdout:\nrun:   %q\ndraft: %q", runOut, draftOut)
	}

	artifacts := filepath.Join(testArtifactDir(t, cfg, day), "draft.json")
	if !strings.Contains(string(runErrOut), artifacts) {
		t.Fatalf("stderr %q does not name the artifacts path %q", runErrOut, artifacts)
	}
	if strings.Contains(string(runOut), "sources") || strings.Contains(string(runOut), "artifacts") {
		t.Fatalf("stdout is not pure draft JSON: %q", runOut)
	}
	stored, err := os.ReadFile(artifacts)
	if err != nil {
		t.Fatalf("read artifacts file: %v", err)
	}
	if !bytes.Equal(stored, runOut) {
		t.Fatalf("artifacts file differs from run stdout:\nfile:  %q\nstdout: %q", stored, runOut)
	}
	if !strings.Contains(string(runOut), `"kind": "meeting"`) {
		t.Fatalf("draft does not contain the synthetic meeting block: %s", runOut)
	}
}

func TestRunCollectsExactlyOncePerInvocation(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	cfg := writeRunConfig(t)
	srv, fetches := startFeed(t)
	envFile := writeRunEnvFile(t, srv.URL)

	for i := 1; i <= 3; i++ {
		if _, _, err := captureOutput(t, func() error {
			return runRun([]string{"--config", cfg, "--env-file", envFile, "--date", "2026-10-20"})
		}); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if got := atomic.LoadInt64(fetches); got != int64(i) {
			t.Fatalf("after run %d: %d collection fetches, want exactly %d per invocation", i, got, i)
		}
	}
}

func TestRunFlagErrors(t *testing.T) {
	pinClock(t)
	t.Setenv("TMPDIR", t.TempDir())
	// The missing-config case resolves the $HOME default, so pin HOME to an
	// empty directory: the default config never exists there and the test
	// stays hermetic no matter what the invoking machine has installed.
	t.Setenv("HOME", t.TempDir())
	// No enabled sources: the omitted-date case below executes a real run and
	// must never reach an inherited feed URL or the network.
	cfg, _ := writeSyntheticConfig(t, "Asia/Ho_Chi_Minh")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing config", args: []string{"--date", "2026-10-20"}, want: "no configuration at"},
		{name: "bad date format", args: []string{"--config", cfg, "--date", "20-10-2026"}, want: "run: --date must be YYYY-MM-DD"},
		{name: "positional args", args: []string{"--config", cfg, "--date", "2026-10-20", "extra"}, want: "run does not accept positional arguments"},
	}
	for _, tc := range cases {
		err := runRun(tc.args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	if !errors.Is(runRun([]string{"--help"}), flag.ErrHelp) {
		t.Fatalf("--help should surface flag.ErrHelp, got %v", runRun([]string{"--help"}))
	}
	out, _, err := captureOutput(t, func() error {
		return runRun([]string{"--config", cfg})
	})
	if err != nil {
		t.Fatalf("run without --date: %v", err)
	}
	var draft timesheet.Draft
	if err := json.Unmarshal(out, &draft); err != nil {
		t.Fatalf("run stdout is not the draft JSON: %v", err)
	}
	if draft.Date != "2026-10-27" {
		t.Fatalf("draft date = %q, want the pinned Asia/Ho_Chi_Minh day 2026-10-27", draft.Date)
	}
}

func TestCollectRunStderrCarriesCollectionID(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	cfg := writeRunConfig(t)
	srv, _ := startFeed(t)
	envFile := writeRunEnvFile(t, srv.URL)
	day := "2026-10-20"

	runOut, runErrOut, err := captureOutput(t, func() error {
		return runRun([]string{"--config", cfg, "--env-file", envFile, "--date", day})
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	runID := stderrCollectionID(t, string(runErrOut), "run")
	if strings.Contains(string(runOut), "collection_id") || strings.Contains(string(runOut), "collection_id") {
		t.Fatalf("run stdout is not pure draft JSON: %q", runOut)
	}

	statePath := filepath.Join(filepath.Dir(cfg), "state.json")
	store, err := timesheet.OpenStore(statePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	ev := store.Evidence(day)
	if ev == nil {
		t.Fatal("no evidence recorded for the synthetic day")
	}
	runStoreID := ""
	for _, a := range ev.Attempts {
		if a.Source == "calendar" {
			runStoreID = a.CollectionID
		}
	}
	if runStoreID == "" {
		t.Fatalf("calendar SourceAttempt has no collection_id: %+v", ev.Attempts)
	}
	if runID != runStoreID {
		t.Fatalf("run stderr id %q does not match the store attempt %q", runID, runStoreID)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	collectOut, collectErrOut, err := captureOutput(t, func() error {
		return runCollect([]string{"--config", cfg, "--env-file", envFile, "--date", day})
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	collectID := stderrCollectionID(t, string(collectErrOut), "collect")
	if strings.Contains(string(collectOut), "run:") || strings.Contains(string(collectOut), "collect:") {
		t.Fatalf("collect stdout is not pure collect JSON: %q", collectOut)
	}
	if collectID == "" || collectID == runID {
		t.Fatalf("collect id %q is empty or reused", collectID)
	}

	_, draftErrOut, err := captureOutput(t, func() error {
		return runDraft([]string{"--config", cfg, "--env-file", envFile, "--date", day})
	})
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if strings.Contains(string(draftErrOut), "collection_id") {
		t.Fatalf("draft stderr unexpectedly carries an id line: %q", draftErrOut)
	}
}

func stderrCollectionID(t *testing.T, stderr, cmd string) string {
	t.Helper()
	want := cmd + ": collection_id "
	for _, line := range strings.Split(stderr, "\n") {
		if after, ok := strings.CutPrefix(line, want); ok {
			return strings.TrimSpace(after)
		}
	}
	t.Fatalf("%s stderr %q lacks the %q line", cmd, stderr, want)
	return ""
}

func TestStatusDateSurfacesCollectionAttempt(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	cfg := writeRunConfig(t)
	srv, _ := startFeed(t)
	envFile := writeRunEnvFile(t, srv.URL)
	day := "2026-10-20"

	_, runErrOut, err := captureOutput(t, func() error {
		return runRun([]string{"--config", cfg, "--env-file", envFile, "--date", day})
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	runID := stderrCollectionID(t, string(runErrOut), "run")

	statusOut, _, err := captureOutput(t, func() error {
		return runStatus([]string{"--config", cfg, "--env-file", envFile, "--date", day})
	})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var summary struct {
		Status timesheet.StatusResult `json:"status"`
	}
	if err := json.Unmarshal(statusOut, &summary); err != nil {
		t.Fatalf("status output is not the expected summary JSON: %v\n%s", err, statusOut)
	}
	var dayStatus *timesheet.DayStatus
	for i := range summary.Status.Days {
		if summary.Status.Days[i].Date == day {
			dayStatus = &summary.Status.Days[i]
		}
	}
	if dayStatus == nil {
		t.Fatalf("status --date does not carry the requested day: %+v", summary.Status.Days)
	}
	bySource := map[string]timesheet.DaySourceStatus{}
	for _, s := range dayStatus.Sources {
		bySource[s.Source] = s
	}
	cal, ok := bySource["calendar"]
	if !ok {
		t.Fatalf("status --date does not carry the calendar source: %+v", dayStatus.Sources)
	}
	if cal.CollectionID != runID {
		t.Fatalf("status collection_id %q does not match the run stderr id %q", cal.CollectionID, runID)
	}
	if cal.Status == "" || cal.Status == "unattempted" {
		t.Fatalf("calendar attempt status = %q, want the recorded attempt's status", cal.Status)
	}
	if gh, ok := bySource["github"]; ok && gh.Status != "unattempted" {
		t.Fatalf("disabled github source surfaced with status %q", gh.Status)
	}
}
