package timesheet

import (
	"strings"
	"time"
)

// DailyConfig is the optional scheduled-run policy (issue #243). Every field
// has a default, so `timesheet daily` works without the section. It holds no
// secret: the webhook URL is named by variable, never stored.
type DailyConfig struct {
	// LogAfter is the local HH:MM after which today is logged (default 17:00);
	// earlier runs log only the previous working day, so a morning start never
	// saves a half-finished day.
	LogAfter string `json:"log_after,omitempty"`
	// Days are the working weekdays, mon..sun tokens (default mon..fri).
	Days []string `json:"days,omitempty"`
	// Desktop sends a desktop notification (default true).
	Desktop *bool `json:"desktop,omitempty"`
	// DesktopCommand replaces the platform notifier; the message is appended
	// as the last argument.
	DesktopCommand []string `json:"desktop_command,omitempty"`
	// WebhookEnv names the env-file variable holding a Discord or Slack
	// incoming-webhook URL; WebhookKind is "discord" (default) or "slack".
	WebhookEnv  string `json:"webhook_env,omitempty"`
	WebhookKind string `json:"webhook_kind,omitempty"`
	// NotifyEmpty sends a "nothing to log" notification for a day with no
	// evidence (default true).
	NotifyEmpty *bool `json:"notify_empty,omitempty"`
}

const DefaultDailyLogAfter = "17:00"

var defaultDailyDays = []string{"mon", "tue", "wed", "thu", "fri"}

// EffectiveDaily returns the daily policy with defaults applied.
func (c *Config) EffectiveDaily() DailyConfig {
	d := DailyConfig{}
	if c.Daily != nil {
		d = *c.Daily
	}
	if d.LogAfter == "" {
		d.LogAfter = DefaultDailyLogAfter
	}
	if len(d.Days) == 0 {
		d.Days = append([]string(nil), defaultDailyDays...)
	}
	yes := true
	if d.Desktop == nil {
		d.Desktop = &yes
	}
	if d.NotifyEmpty == nil {
		d.NotifyEmpty = &yes
	}
	if d.WebhookKind == "" {
		d.WebhookKind = "discord"
	}
	return d
}

func (c *Config) validateDaily() error {
	d := c.Daily
	if d == nil {
		return nil
	}
	if d.LogAfter != "" {
		if _, err := time.Parse("15:04", d.LogAfter); err != nil {
			return newError(CodeConfigInvalid, "config: daily.log_after must be HH:MM")
		}
	}
	for _, day := range d.Days {
		if !validWorkDayTokens[day] {
			return newError(CodeConfigInvalid, "config: daily.days contains "+quoteToken(day)+"; use mon, tue, wed, thu, fri, sat or sun")
		}
	}
	if d.WebhookEnv != "" && !envNameRE.MatchString(d.WebhookEnv) {
		return newError(CodeConfigInvalid, "config: daily.webhook_env is not a valid variable name")
	}
	switch strings.TrimSpace(d.WebhookKind) {
	case "", "discord", "slack":
	default:
		return newError(CodeConfigInvalid, `config: daily.webhook_kind must be "discord" or "slack"`)
	}
	if len(d.DesktopCommand) > 0 && strings.TrimSpace(d.DesktopCommand[0]) == "" {
		return newError(CodeConfigInvalid, "config: daily.desktop_command must name an executable")
	}
	return nil
}

func quoteToken(s string) string { return `"` + s + `"` }

// WorkingDayToken is the mon..sun token of a weekday.
func WorkingDayToken(w time.Weekday) string {
	return [...]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}[w]
}
