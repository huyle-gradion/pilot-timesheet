package main

import (
	"strings"
	"testing"
	"time"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

func TestSplitClusterCarry(t *testing.T) {
	cases := []struct {
		start, end     string
		spans          [][2]string
		carryS, carryE string
		minutes        int
	}{
		{"23:00", "05:00", [][2]string{{"23:00", "03:00"}}, "03:00", "05:00", 120},
		{"20:00", "06:00", [][2]string{{"20:00", "00:00"}}, "00:00", "06:00", 360},
		{"15:00", "05:00", [][2]string{{"15:00", "19:00"}, {"19:00", "23:00"}, {"23:00", "03:00"}}, "03:00", "05:00", 120},
		{"22:00", "04:00", [][2]string{{"22:00", "04:00"}}, "", "", 0},
	}
	for _, c := range cases {
		spans, carry := timesheet.SplitCluster(testDate, overnightTZ, timesheet.EntryCluster{BlockID: "b", Start: c.start, End: c.end})
		if strings.Join(flattenSpans(spans), ",") != strings.Join(flattenSpans(c.spans), ",") {
			t.Fatalf("%s-%s spans = %v, want %v", c.start, c.end, spans, c.spans)
		}
		if c.carryS == "" {
			if carry != nil {
				t.Fatalf("%s-%s carry = %+v, want none", c.start, c.end, carry)
			}
			continue
		}
		if carry == nil || carry.Date != carryTarget || carry.Start != c.carryS || carry.End != c.carryE || carry.Minutes != c.minutes {
			t.Fatalf("%s-%s carry = %+v, want %s %s-%s (%d min)", c.start, c.end, carry, carryTarget, c.carryS, c.carryE, c.minutes)
		}
	}

	if spans, carry := timesheet.SplitCluster("2026-10-24", "Europe/Berlin", timesheet.EntryCluster{BlockID: "b", Start: "22:00", End: "05:00"}); carry != nil ||
		len(spans) != 1 || spans[0] != [2]string{"22:00", "05:00"} {
		t.Fatalf("Berlin fall-back 22:00-05:00 = %v carry %+v, want one span and no carry", spans, carry)
	}

	sweeps := []struct{ date, tz string }{
		{testDate, overnightTZ},
		{"2026-03-28", "Europe/Berlin"},    // springs forward at 02:00 on the next date
		{"2026-10-24", "Europe/Berlin"},    // falls back at 03:00 on the next date
		{"2026-04-04", "America/Santiago"}, // falls back at midnight
		{"2026-09-05", "America/Santiago"}, // skips midnight
	}
	carried := 0
	for _, s := range sweeps {
		loc, err := time.LoadLocation(s.tz)
		if err != nil {
			t.Fatal(err)
		}
		for sm := 12 * 60; sm < 24*60; sm += 15 {
			for em := 0; em < sm && em <= 12*60; em += 15 {
				start, end := sweepClock(sm), sweepClock(em)
				total, err := (timesheet.Interval{Date: s.date, Start: start, End: end}).Duration(loc)
				if err != nil {
					continue
				}
				spans, carry := timesheet.SplitCluster(s.date, s.tz, timesheet.EntryCluster{BlockID: "b", Start: start, End: end})
				where := s.tz + " " + s.date + " " + start + "-" + end
				got := 0
				prevEnd := ""
				for i, sp := range spans {
					if i > 0 && sp[0] != prevEnd {
						t.Fatalf("%s: chunks not contiguous: %v", where, spans)
					}
					prevEnd = sp[1]
					m, err := (timesheet.Interval{Date: s.date, Start: sp[0], End: sp[1]}).Duration(loc)
					if err != nil {
						t.Fatalf("%s: chunk %v does not resolve: %v", where, sp, err)
					}
					if carry != nil && m > 240 {
						t.Fatalf("%s: chunk %v = %d min beside a carry", where, sp, m)
					}
					got += m
				}
				if carry != nil {
					carried++
					if carry.Start != prevEnd || carry.End != end {
						t.Fatalf("%s: carry %s-%s does not continue %v", where, carry.Start, carry.End, spans)
					}
					m, err := (timesheet.Interval{Date: carry.Date, Start: carry.Start, End: carry.End}).Duration(loc)
					if err != nil || m != carry.Minutes {
						t.Fatalf("%s: carry %+v resolves to %d (%v)", where, carry, m, err)
					}
					got += m
				}
				if got != total {
					t.Fatalf("%s: source+carry = %d min, recorded %d (spans %v, carry %+v)", where, got, total, spans, carry)
				}
			}
		}
	}
	if carried == 0 {
		t.Fatalf("the sweep produced no carry at all")
	}
}

func flattenSpans(spans [][2]string) []string {
	out := make([]string, 0, len(spans))
	for _, s := range spans {
		out = append(out, s[0]+"-"+s[1])
	}
	return out
}

func sweepClock(m int) string {
	return time.Date(2000, 1, 1, m/60, m%60, 0, 0, time.UTC).Format("15:04")
}
