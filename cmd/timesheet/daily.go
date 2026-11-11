package main

import (
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

const (
	dailyLockFile   = "daily.lock"
	dailyNoticeFile = "notified.sha256"
)

type dailyDay struct {
	Day         string `json:"day"`
	State       string `json:"state,omitempty"`
	Message     string `json:"message"`
	Notified    bool   `json:"notified"`
	NotifyError string `json:"notify_error,omitempty"`
	Error       string `json:"error,omitempty"`
}

type dailyReceipt struct {
	Days []dailyDay `json:"days"`
	Note string     `json:"note,omitempty"`
}

func runDaily(args []string) error {
	flags := flag.NewFlagSet("daily", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	configFlag := flags.String("config", "", "configuration path (optional; defaults to $HOME/.timesheet/config.json)")
	envFlag := flags.String("env-file", "", "environment-file path (optional; defaults to $HOME/.timesheet/.env.local when it exists)")
	units := flags.String("print-units", "", "print the scheduler unit files instead of running: systemd or launchd")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("daily does not accept positional arguments")
	}
	configPath, envFile, err := resolveStoreFlags("daily", *configFlag, *envFlag)
	if err != nil {
		return err
	}
	if configPath, err = filepath.Abs(configPath); err != nil {
		return fmt.Errorf("daily: the configuration path cannot be made absolute")
	}
	if envFile != "" {
		if envFile, err = filepath.Abs(envFile); err != nil {
			return fmt.Errorf("daily: the environment-file path cannot be made absolute")
		}
	}
	cfg, err := timesheet.LoadConfig(configPath)
	if err != nil {
		return err
	}
	if *units != "" {
		return printUnits(os.Stdout, *units, cfg, configPath, envFile)
	}
	if cfg.Judge == nil {
		return fmt.Errorf("daily: the scheduled log needs a judge section in the configuration (ADR 0010)")
	}
	unlock, held, err := lockDaily(cfg)
	if err != nil {
		return err
	}
	if !held {
		return printJSON(dailyReceipt{Days: []dailyDay{}, Note: "another daily run is in progress; nothing done"})
	}
	defer unlock()

	policy := cfg.EffectiveDaily()
	days, err := dailyTargets(cfg.Timezone, policy, clockNow())
	if err != nil {
		return err
	}
	today, afterCutoff, err := dailyToday(cfg.Timezone, policy, clockNow())
	if err != nil {
		return err
	}
	receipt := dailyReceipt{Days: []dailyDay{}}
	var firstErr error
	for i := 0; i < len(days); i++ {
		day := days[i]
		f := &nextFlags{configPath: configPath, envFile: envFile, date: day, do: true, auto: true, maxActions: defaultAutoMaxActions}
		report, runErr := advanceDayReport(f, cfg, configPath, envFile, day)
		entry := dailyDay{Day: day, State: report.State}
		if runErr != nil {
			entry.Error = boundedNotice(runErr.Error())
			if firstErr == nil {
				firstErr = runErr
			}
		}
		msg, send := dailyMessage(cfg, policy, day, report, runErr)
		entry.Message = msg
		if send {
			entry.Notified, entry.NotifyError = notifyOnce(cfg, policy, envFile, day, msg)
		}
		receipt.Days = append(receipt.Days, entry)
		// Logging a day may carry its overnight remainder into the next date
		// (ADR 0011). That date is logged in this same run even when it is not
		// a working day, so a carried part is never left unvisited.
		if next, ok := carryFollowUp(day, today, afterCutoff, days, func(d string) bool {
			in, err := timesheet.ReadCarryInto(cfg.StatePath, d)
			return err == nil && len(in) > 0
		}); ok {
			days = append(days, next)
		}
	}
	if len(days) == 0 {
		receipt.Note = "no working day to log at this time"
	}
	if err := printJSON(receipt); err != nil {
		return err
	}
	return firstErr
}

func dailyTargets(tz string, policy timesheet.DailyConfig, now time.Time) ([]string, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, err
	}
	local := now.In(loc)
	working := func(t time.Time) bool { return slices.Contains(policy.Days, timesheet.WorkingDayToken(t.Weekday())) }
	var days []string
	for back := 1; back <= 7; back++ {
		if d := local.AddDate(0, 0, -back); working(d) {
			days = append(days, d.Format("2006-01-02"))
			break
		}
	}
	after, err := time.ParseInLocation("15:04", policy.LogAfter, loc)
	if err != nil {
		return nil, err
	}
	cutoff := time.Date(local.Year(), local.Month(), local.Day(), after.Hour(), after.Minute(), 0, 0, loc)
	if working(local) && !local.Before(cutoff) {
		days = append(days, local.Format("2006-01-02"))
	}
	return days, nil
}

func dailyToday(tz string, policy timesheet.DailyConfig, now time.Time) (string, bool, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return "", false, err
	}
	local := now.In(loc)
	after, err := time.ParseInLocation("15:04", policy.LogAfter, loc)
	if err != nil {
		return "", false, err
	}
	cutoff := time.Date(local.Year(), local.Month(), local.Day(), after.Hour(), after.Minute(), 0, 0, loc)
	return local.Format("2006-01-02"), !local.Before(cutoff), nil
}

func carryFollowUp(day, today string, afterCutoff bool, targets []string, hasCarry func(string) bool) (string, bool) {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		return "", false
	}
	next := t.AddDate(0, 0, 1).Format("2006-01-02")
	if slices.Contains(targets, next) || next > today || (next == today && !afterCutoff) {
		return "", false
	}
	return next, hasCarry(next)
}

// lockDaily takes a non-blocking exclusive lock next to the store, so two
// scheduled runs never work on the same day at once.
func lockDaily(cfg *timesheet.Config) (func(), bool, error) {
	path := filepath.Join(filepath.Dir(cfg.StatePath), dailyLockFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("daily: %s cannot be opened: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("daily: %s cannot be locked: %w", path, err)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, true, nil
}

// notifyOnce sends msg unless the same message was already delivered for the
// day. A failed send is reported and never changes the timesheet outcome.
func notifyOnce(cfg *timesheet.Config, policy timesheet.DailyConfig, envFile, day, msg string) (bool, string) {
	dir, err := dayArtifactsDir(cfg.StatePath, day)
	if err != nil {
		return false, err.Error()
	}
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(msg)))
	marker := filepath.Join(dir, dailyNoticeFile)
	if prev, err := os.ReadFile(marker); err == nil && string(prev) == sum {
		return false, ""
	}
	if err := sendNotification(policy, envFile, msg); err != nil {
		fmt.Fprintf(os.Stderr, "daily: notification for %s not sent: %v\n", day, err)
		return false, boundedNotice(err.Error())
	}
	if err := ensureDayArtifactsDir("daily", dir); err == nil {
		_ = os.WriteFile(marker, []byte(sum), 0o600)
	}
	return true, ""
}
