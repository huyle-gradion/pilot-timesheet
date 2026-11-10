package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

const (
	notifyTitle       = "Pilot Timesheet"
	notifyRuneLimit   = 600
	notifyTimeout     = 10 * time.Second
	noEvidenceMarker  = "carries no blocks"
	maxNotifyQuestion = 300
)

// dailyMessage is the one notification for a day's result, and whether to
// send it. It carries only the day, counts, the reason or question, and the
// command to run: never evidence, credentials or anything that approves.
func dailyMessage(cfg *timesheet.Config, policy timesheet.DailyConfig, day string, r advanceReport, runErr error) (string, bool) {
	switch {
	case r.State == stateHumanGates && r.Human != nil:
		entries, minutes := dayLedgerTotals(cfg, day)
		return clipNotice(fmt.Sprintf("%s: %d entr%s, %dh%02dm ready to submit. Review and approve in a terminal: %s",
			day, entries, plural(entries, "y", "ies"), minutes/60, minutes%60, r.Human.ApproveCommand)), true
	case r.State == stateAskHuman && r.Ask != nil:
		msg := day + " needs your answer: " + clipRunes(r.Ask.Question, maxNotifyQuestion)
		if len(r.Ask.Options) > 0 {
			msg += " Options: " + strings.Join(r.Ask.Options, ", ") + "."
		}
		return clipNotice(msg + " Answer it in your agent, or write the answer file " + r.Ask.AnswerFile +
			" as its protocol shows, then run: timesheet next --do --auto --date " + day), true
	case r.State == stateStopped && strings.Contains(r.Reason, noEvidenceMarker):
		return day + ": nothing to log (no evidence was collected).", *policy.NotifyEmpty
	case r.State == stateStopped:
		return clipNotice(day + " stopped: " + r.Reason), true
	case runErr != nil:
		return clipNotice(day + " could not be logged: " + runErr.Error()), true
	}
	return clipNotice(day + " is at " + r.State + "; run: timesheet next --do --auto --date " + day), true
}

func dayLedgerTotals(cfg *timesheet.Config, day string) (int, int) {
	f, err := timesheet.ReadEntries(cfg.StatePath, day)
	if err != nil || f == nil {
		return 0, 0
	}
	minutes := 0
	for _, e := range f.Entries {
		if s, end, ok := timesheet.EntrySpanBounds(e.Start, e.End); ok {
			if end < s {
				end += 24 * 60
			}
			minutes += end - s
		}
	}
	return len(f.Entries), minutes
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func clipNotice(s string) string { return clipRunes(s, notifyRuneLimit) }

// sendNotification delivers msg to every configured channel and reports the
// first failure; a channel that is not configured is skipped.
func sendNotification(policy timesheet.DailyConfig, envFile, msg string) error {
	var errs []error
	if *policy.Desktop {
		if err := desktopNotify(policy.DesktopCommand, msg); err != nil {
			errs = append(errs, err)
		}
	}
	if policy.WebhookEnv != "" {
		if err := webhookNotify(policy, envFile, msg); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// desktopArgv is the platform notifier with the message as its last argument,
// never interpolated into a script.
func desktopArgv(custom []string, msg string) []string {
	if len(custom) > 0 {
		return append(append([]string(nil), custom...), msg)
	}
	if runtime.GOOS == "darwin" {
		return []string{"osascript", "-e", "on run argv", "-e",
			`display notification (item 1 of argv) with title "` + notifyTitle + `"`, "-e", "end run", msg}
	}
	return []string{"notify-send", "--app-name=" + notifyTitle, notifyTitle, msg}
}

func desktopNotify(custom []string, msg string) error {
	argv := desktopArgv(custom, msg)
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("desktop notifier %s failed: %v", argv[0], exitSummaryOf(err))
	}
	return nil
}

func exitSummaryOf(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Sprintf("exit status %d", exit.ExitCode())
	}
	if errors.Is(err, exec.ErrNotFound) {
		return "not installed"
	}
	return "could not start"
}

// webhookNotify posts to a Discord or Slack incoming webhook whose URL is read
// from the env file by name. The URL is a secret: it never appears in an error.
func webhookNotify(policy timesheet.DailyConfig, envFile, msg string) error {
	env := map[string]string{}
	if envFile != "" {
		fileEnv, err := timesheet.LoadEnvFile(envFile)
		if err != nil {
			return err
		}
		env = timesheet.MergeEnv(fileEnv)
	}
	url, ok := timesheet.ResolveEnv(env, policy.WebhookEnv)
	if !ok || url == "" {
		return fmt.Errorf("the webhook variable %s is not set", policy.WebhookEnv)
	}
	key := "content"
	if policy.WebhookKind == "slack" {
		key = "text"
	}
	body, _ := json.Marshal(map[string]string{key: notifyTitle + ": " + msg})
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("the webhook URL in %s is not usable", policy.WebhookEnv)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("the webhook is unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("the webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}
