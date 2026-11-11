package timesheet

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// --- Overnight carry (issue #277, ADR 0011) ------------------------------------
//
// An overnight stage unit that runs more than 240 real minutes past its date's
// midnight keeps the part up to midnight plus up to 240 minutes after it; the
// remainder is carried into the next date, where it stages like that day's own
// entries. The source ledger is never modified. The carry is a derived,
// durable record keyed to its source unit: entries/<source date>.carry.json,
// replaced whole by every derivation of the source day. Like the stage map it
// carries no state-schema weight and is never read by the session dispatcher or
// the plan builder; nothing reaches the remote except through the target day's
// own human-gated submission.

// CarrySegment is one carried part of a source-day stage unit. Start and End
// are clocks on Date (the source date's next date) in Timezone (the source
// day's recorded timezone); Minutes is the real elapsed time between them.
type CarrySegment struct {
	SourceDate    string `json:"source_date"`
	SourceBlockID string `json:"source_block_id"`
	Date          string `json:"date"`
	Start         string `json:"start"`
	End           string `json:"end"`
	Minutes       int    `json:"minutes"`
	Description   string `json:"description"`
	Kind          string `json:"kind,omitempty"`
	Timezone      string `json:"timezone"`
}

// carriedPrefix starts every carried block id on the target day.
const carriedPrefix = "carried-"

// BlockID is the carried segment's block id on the target day:
// carried-<source date>-<source block id>. It names the source, so the target
// day's stage map, receipts and classification binding show where it came
// from, and it never collides with a ledger block id the draft assigns.
func (c CarrySegment) BlockID() string {
	return carriedPrefix + c.SourceDate + "-" + c.SourceBlockID
}

// Entry is the carried segment as one stage input entry of the target day.
func (c CarrySegment) Entry() EntryRecord {
	return EntryRecord{
		BlockID: c.BlockID(), Date: c.Date, Start: c.Start, End: c.End,
		Description: c.Description, Kind: c.Kind,
	}
}

// CarriedSourceDate returns the source date a carried block id names, or "".
func CarriedSourceDate(blockID string) string {
	rest, ok := strings.CutPrefix(blockID, carriedPrefix)
	if !ok || len(rest) < len("2006-01-02") {
		return ""
	}
	if _, err := parseDate(rest[:10]); err != nil {
		return ""
	}
	return rest[:10]
}

// PreviousDate is the calendar date before date ("" when date is not one).
func PreviousDate(date string) string {
	t, err := parseDate(date)
	if err != nil {
		return ""
	}
	return t.AddDate(0, 0, -1).Format("2006-01-02")
}

// StageSpans chunks one stage-time span recorded under date to the Gradion
// four-hour policy (issue #167) and returns the spans staged on date. A
// same-day span is exactly SplitEntrySpans. An overnight span with 240 real
// minutes or fewer after midnight is LegacyStageSpans (#176). A longer one is
// cut at its carry cutoff (ADR 0011); SplitCluster returns the carried rest.
func StageSpans(date, timezone, start, end string) [][2]string {
	spans, _, _ := splitSpan(date, timezone, start, end)
	return spans
}

// SplitCluster splits one stage-time cluster resolved under date into the spans
// staged on date and the segment carried into the next date (nil when nothing
// is carried). A cluster that starts after midnight (NextDay) is carried whole.
func SplitCluster(date, timezone string, c EntryCluster) ([][2]string, *CarrySegment) {
	seg := CarrySegment{
		SourceDate: date, SourceBlockID: c.BlockID, Date: nextDate(date),
		Description: c.Description, Kind: c.Kind, Timezone: timezone,
	}
	if c.NextDay {
		seg.Start, seg.End = c.Start, c.End
		seg.Minutes = wallOrRealMinutes(seg.Date, timezone, c.Start, c.End)
		return nil, &seg
	}
	spans, carry, minutes := splitSpan(date, timezone, c.Start, c.End)
	if carry == nil {
		return spans, nil
	}
	seg.Start, seg.End, seg.Minutes = carry[0], carry[1], minutes
	return spans, &seg
}

// splitSpan is the carry arithmetic. The source day is chunked in real minutes
// greedily from the start in 240-minute steps while the next chunk would still
// start before midnight; the last chunk that starts before midnight runs a
// full 240 minutes (it crosses midnight or ends exactly at it), and its end is
// the cutoff. [cutoff, end] is the carry. Any boundary that does not resolve
// to one instant falls back to the single original span and no carry, so
// plan's remote_entry_too_long guard reports it loudly.
func splitSpan(date, timezone, start, end string) ([][2]string, *[2]string, int) {
	sm, okS := draftClock(start)
	em, okE := draftClock(end)
	if !okS || !okE || em >= sm {
		return SplitEntrySpans(start, end), nil, 0
	}
	single := [][2]string{{start, end}}
	loc, err := dayLocation(timezone)
	if err != nil {
		return single, nil, 0
	}
	from, to, err := Interval{Date: date, Start: clockString(sm), End: clockString(em)}.resolveEndpoints(loc)
	if err != nil {
		return single, nil, 0
	}
	next := nextDate(date)
	midnight, err := localDayStart(next, loc)
	if err != nil {
		return single, nil, 0
	}
	limit := time.Duration(maxRemoteEntryMinutes) * time.Minute
	if to.Sub(midnight) <= limit {
		return LegacyStageSpans(date, timezone, start, end), nil, 0
	}
	clocks := []string{clockString(sm)}
	b := from
	for n := b.Add(limit); n.Before(midnight); n = b.Add(limit) {
		clock := n.In(loc).Format("15:04")
		if at, err := resolveLocal(date, clock, loc, "start"); err != nil || !at.Equal(n) {
			return single, nil, 0
		}
		clocks = append(clocks, clock)
		b = n
	}
	cutoff := b.Add(limit)
	cutClock := cutoff.In(loc).Format("15:04")
	lastFrom, lastTo, err := Interval{Date: date, Start: clocks[len(clocks)-1], End: cutClock}.resolveEndpoints(loc)
	if err != nil || !lastFrom.Equal(b) || !lastTo.Equal(cutoff) {
		return single, nil, 0
	}
	carryFrom, carryTo, err := Interval{Date: next, Start: cutClock, End: clockString(em)}.resolveEndpoints(loc)
	if err != nil || !carryFrom.Equal(cutoff) || !carryTo.Equal(to) {
		return single, nil, 0
	}
	clocks = append(clocks, cutClock)
	spans := make([][2]string, 0, len(clocks)-1)
	for i := 1; i < len(clocks); i++ {
		spans = append(spans, [2]string{clocks[i-1], clocks[i]})
	}
	return spans, &[2]string{cutClock, clockString(em)}, int(to.Sub(cutoff) / time.Minute)
}

// wallOrRealMinutes is the real duration of [start, end] on date in timezone,
// or the wall-clock difference when it does not resolve (the carry's Minutes
// is informational; the target day's interval op validates the span itself).
func wallOrRealMinutes(date, timezone, start, end string) int {
	if loc, err := dayLocation(timezone); err == nil {
		if m, err := (Interval{Date: date, Start: start, End: end}).Duration(loc); err == nil {
			return m
		}
	}
	sm, _ := draftClock(start)
	em, _ := draftClock(end)
	if em <= sm {
		em += minutesPerDay
	}
	return em - sm
}

// CarryFile is the durable carry record of one source day.
type CarryFile struct {
	SourceDate string         `json:"source_date"`
	AppVersion string         `json:"app_version"`
	UpdatedAt  string         `json:"updated_at"`
	Carries    []CarrySegment `json:"carries"`
}

// CarryPath returns the source day's carry record path:
// entries/<source date>.carry.json, next to its entries file and stage map.
func CarryPath(statePath, sourceDate string) string {
	return strings.TrimSuffix(EntriesPath(statePath, sourceDate), ".json") + ".carry.json"
}

// ReadCarry loads the carry record written by sourceDate's derivation. A
// missing file is (nil, nil).
func ReadCarry(statePath, sourceDate string) ([]CarrySegment, error) {
	data, err := os.ReadFile(CarryPath(statePath, sourceDate))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, newError(CodeStateInvalid, "the carry record cannot be read")
	}
	var f CarryFile
	if err := json.Unmarshal(data, &f); err != nil || f.SourceDate != sourceDate {
		return nil, newError(CodeStateInvalid, "the carry record is not valid for this date")
	}
	for _, c := range f.Carries {
		if c.SourceDate != sourceDate || c.Date != nextDate(sourceDate) {
			return nil, newError(CodeStateInvalid, "the carry record is not valid for this date")
		}
	}
	return f.Carries, nil
}

// ReadCarryInto returns the segments carried into date from the date before.
func ReadCarryInto(statePath, date string) ([]CarrySegment, error) {
	prev := PreviousDate(date)
	if prev == "" {
		return nil, nil
	}
	return ReadCarry(statePath, prev)
}

// WriteCarry replaces sourceDate's carry record atomically with carries, in
// source block id order. No carry removes the record, so a day with nothing to
// carry has no file and re-staging never leaves a stale or duplicate carry.
func WriteCarry(statePath, appVersion, sourceDate string, carries []CarrySegment, now time.Time) error {
	path := CarryPath(statePath, sourceDate)
	if len(carries) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return newError(CodeStateInvalid, "the carry record cannot be removed")
		}
		return nil
	}
	sorted := append([]CarrySegment(nil), carries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].SourceBlockID < sorted[j].SourceBlockID })
	f := CarryFile{
		SourceDate: sourceDate, AppVersion: appVersion,
		UpdatedAt: now.UTC().Format(time.RFC3339), Carries: sorted,
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return newError(CodeStateInvalid, "the entries directory cannot be created")
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return newError(CodeInternal, "the carry record cannot be encoded")
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return newError(CodeStateInvalid, "the carry record cannot be written")
	}
	if err := os.Rename(tmp, path); err != nil {
		return newError(CodeStateInvalid, "the carry record cannot be replaced")
	}
	return nil
}

// CheckCarryTimezone refuses carried segments computed in a timezone other
// than the target day's: their clocks would be read in the wrong zone.
func CheckCarryTimezone(carries []CarrySegment, timezone string) error {
	for _, c := range carries {
		if c.Timezone != timezone {
			return fmt.Errorf("the segment carried from %s (%s-%s) was computed in timezone %s, but %s is recorded in timezone %s; "+
				"its clocks are never reinterpreted: re-stage %s in %s or record the part by hand",
				c.SourceDate, c.Start, c.End, c.Timezone, c.Date, timezone, c.SourceDate, timezone)
		}
	}
	return nil
}

// SameCarries reports whether two carry lists hold the same segments,
// regardless of order.
func SameCarries(a, b []CarrySegment) bool {
	if len(a) != len(b) {
		return false
	}
	key := func(c CarrySegment) string {
		return strings.Join([]string{c.SourceDate, c.SourceBlockID, c.Date, c.Start, c.End,
			fmt.Sprint(c.Minutes), c.Description, c.Kind, c.Timezone}, "\x00")
	}
	seen := make(map[string]int, len(a))
	for _, c := range a {
		seen[key(c)]++
	}
	for _, c := range b {
		k := key(c)
		if seen[k] == 0 {
			return false
		}
		seen[k]--
	}
	return true
}
