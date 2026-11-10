package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// Scheduler units for `timesheet daily` (issue #243), printed with this
// binary's absolute path and the resolved config/env paths so they run from
// any working directory. systemd: a user timer at log_after on the working
// days, Persistent so a missed run fires at the next start, and a run shortly
// after login. launchd: the same calendar slots plus RunAtLoad.
const (
	unitName      = "pilot-timesheet-daily"
	launchdLabel  = "com.pilot-timesheet.daily"
	startupDelay  = "3min"
	systemdUnitAt = "~/.config/systemd/user/"
	launchdUnitAt = "~/Library/LaunchAgents/"
)

func printUnits(w io.Writer, kind string, cfg *timesheet.Config, configPath, envFile string) error {
	exe, err := absoluteExecutable()
	if err != nil {
		return err
	}
	argv := []string{exe, "daily", "--config", configPath}
	if envFile != "" {
		argv = append(argv, "--env-file", envFile)
	}
	policy := cfg.EffectiveDaily()
	at, err := time.Parse("15:04", policy.LogAfter)
	if err != nil {
		return err
	}
	switch kind {
	case "systemd":
		_, err = io.WriteString(w, systemdUnits(argv, policy.Days, at, cfg.Timezone))
	case "launchd":
		_, err = io.WriteString(w, launchdPlist(argv, policy.Days, at))
	default:
		return fmt.Errorf("daily: --print-units must be systemd or launchd")
	}
	return err
}

var systemdDay = map[string]string{"mon": "Mon", "tue": "Tue", "wed": "Wed", "thu": "Thu", "fri": "Fri", "sat": "Sat", "sun": "Sun"}

func systemdUnits(argv, days []string, at time.Time, tz string) string {
	names := make([]string, 0, len(days))
	for _, d := range days {
		names = append(names, systemdDay[d])
	}
	exec := make([]string, 0, len(argv))
	for _, a := range argv {
		exec = append(exec, systemdQuote(a))
	}
	return "# file: " + systemdUnitAt + unitName + ".service\n" +
		"[Unit]\n" +
		"Description=Pilot Timesheet daily log\n" +
		"Wants=network-online.target\n" +
		"After=network-online.target\n\n" +
		"[Service]\n" +
		"Type=oneshot\n" +
		"ExecStart=" + strings.Join(exec, " ") + "\n\n" +
		"# file: " + systemdUnitAt + unitName + ".timer\n" +
		"[Unit]\n" +
		"Description=Run the Pilot Timesheet daily log on working days and after login\n\n" +
		"[Timer]\n" +
		"OnCalendar=" + strings.Join(names, ",") + " *-*-* " + at.Format("15:04") + ":00 " + tz + "\n" +
		"OnStartupSec=" + startupDelay + "\n" +
		"Persistent=true\n\n" +
		"[Install]\n" +
		"WantedBy=timers.target\n"
}

// systemdQuote quotes one ExecStart word when it holds a space or quote.
func systemdQuote(s string) string {
	if !strings.ContainsAny(s, " \t\"'\\") {
		return s
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

var launchdWeekday = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

func launchdPlist(argv, days []string, at time.Time) string {
	var b strings.Builder
	b.WriteString("<!-- file: " + launchdUnitAt + launchdLabel + ".plist -->\n")
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n")
	b.WriteString("  <key>Label</key><string>" + launchdLabel + "</string>\n")
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, a := range argv {
		b.WriteString("    <string>" + xmlEscape(a) + "</string>\n")
	}
	b.WriteString("  </array>\n  <key>RunAtLoad</key><true/>\n  <key>StartCalendarInterval</key>\n  <array>\n")
	for _, d := range days {
		fmt.Fprintf(&b, "    <dict><key>Weekday</key><integer>%d</integer><key>Hour</key><integer>%d</integer><key>Minute</key><integer>%d</integer></dict>\n",
			launchdWeekday[d], at.Hour(), at.Minute())
	}
	b.WriteString("  </array>\n</dict>\n</plist>\n")
	return b.String()
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;").Replace(s)
}
