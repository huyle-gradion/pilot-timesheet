package timesheet

import (
	"testing"
	"time"
)

func TestTodayInUsesTheConfiguredZone(t *testing.T) {
	instant := mustParseInstant(t, "2026-10-26T18:00:00Z")

	cases := []struct {
		zone string
		want string
	}{
		{"Asia/Ho_Chi_Minh", "2026-10-27"},
		{"UTC", "2026-10-26"},
	}
	for _, tc := range cases {
		got, err := TodayIn(tc.zone, instant)
		if err != nil {
			t.Fatalf("TodayIn(%q): %v", tc.zone, err)
		}
		if got != tc.want {
			t.Fatalf("TodayIn(%q) = %q, want %q", tc.zone, got, tc.want)
		}
	}
}

func TestTodayInRejectsAnUnusableZone(t *testing.T) {
	instant := mustParseInstant(t, "2026-10-26T18:00:00Z")
	for _, zone := range []string{"", "  ", "Not/AZone"} {
		if _, err := TodayIn(zone, instant); err == nil {
			t.Fatalf("TodayIn(%q) must fail closed", zone)
		}
	}
}

func mustParseInstant(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return parsed
}
