package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

type ledgerUnit struct {
	BlockID     string
	Date        string
	Start       string
	End         string
	Description string
	Spans       [][2]string
	CarriedFrom string `json:",omitempty"`
}

// dayStageEntries is a day's stage input: its own recorded entries plus each
// segment carried in from the previous date as one entry (ADR 0011). The
// ledger slice is not modified.
func dayStageEntries(entries []timesheet.EntryRecord, carryIn []timesheet.CarrySegment) []timesheet.EntryRecord {
	if len(carryIn) == 0 {
		return entries
	}
	out := make([]timesheet.EntryRecord, 0, len(entries)+len(carryIn))
	out = append(out, entries...)
	for _, c := range carryIn {
		out = append(out, c.Entry())
	}
	return out
}

func artifactStageEntries(a timesheet.DayArtifacts) []timesheet.EntryRecord {
	var own []timesheet.EntryRecord
	if a.Entries != nil {
		own = a.Entries.Entries
	}
	return dayStageEntries(own, a.CarryIn)
}

func clusterCarriedFrom(blockIDs []string) string {
	for _, id := range blockIDs {
		if d := timesheet.CarriedSourceDate(id); d != "" {
			return d
		}
	}
	return ""
}

func ledgerBlocks(entries []timesheet.EntryRecord, date string) []ledgerUnit {
	var units []ledgerUnit
	for _, c := range timesheet.EffectiveEntrySpansOn(date, entries) {
		units = append(units, ledgerUnit{
			BlockID: c.BlockID, Date: date, Start: c.Start, End: c.End,
			Description: c.Description, CarriedFrom: clusterCarriedFrom(c.BlockIDs),
		})
	}
	return units
}

// deriveLedger derives the stage-time units with the chunk spans applyStage
// would stage, and the segments it would carry into the next date. timezone is
// the day's recorded timezone (the one open_day reports): an overnight span is
// chunked and cut in real minutes. It allocates no interval ids: it is the
// content projection, enough to prove the stored stage map, the carry record
// and the saved content are current.
func deriveLedger(entries []timesheet.EntryRecord, date, timezone string) ([]ledgerUnit, []timesheet.CarrySegment) {
	var units []ledgerUnit
	var carries []timesheet.CarrySegment
	for _, c := range timesheet.EffectiveEntrySpansOn(date, entries) {
		spans, carry := timesheet.SplitCluster(date, timezone, c)
		if carry != nil {
			carries = append(carries, *carry)
		}
		if len(spans) == 0 {
			continue
		}
		units = append(units, ledgerUnit{
			BlockID: c.BlockID, Date: date, Start: c.Start, End: c.End,
			Description: c.Description, Spans: spans, CarriedFrom: clusterCarriedFrom(c.BlockIDs),
		})
	}
	return units, carries
}

func ledgerUnits(entries []timesheet.EntryRecord, date, timezone string) []ledgerUnit {
	units, _ := deriveLedger(entries, date, timezone)
	return units
}

func legacyUnits(units []ledgerUnit) []ledgerUnit {
	out := make([]ledgerUnit, len(units))
	for i, u := range units {
		out[i] = u
		if u.End <= u.Start { // HH:MM clocks: the end is on the next date
			out[i].Spans = [][2]string{{u.Start, u.End}}
		}
	}
	return out
}

func legacyDerivations(entries []timesheet.EntryRecord, date, timezone string) [][]ledgerUnit {
	var chunked []ledgerUnit
	for _, c := range timesheet.EffectiveEntrySpans(entries) {
		chunked = append(chunked, ledgerUnit{
			BlockID: c.BlockID, Date: date, Start: c.Start, End: c.End, Description: c.Description,
			Spans: timesheet.LegacyStageSpans(date, timezone, c.Start, c.End),
		})
	}
	return [][]ledgerUnit{legacyUnits(chunked), chunked}
}

func ledgerDerivesSaved(units []ledgerUnit, legacy [][]ledgerUnit, confirmed []timesheet.Interval) (current, legacyOK bool) {
	saved := confirmedContentSpans(confirmed)
	if sameContent(ledgerContentSpans(units), saved) {
		return true, false
	}
	for _, l := range legacy {
		if sameContent(ledgerContentSpans(l), saved) {
			return false, true
		}
	}
	return false, false
}

func checkCarryIn(statePath, date string, carryIn []timesheet.CarrySegment) error {
	if len(carryIn) == 0 {
		return nil
	}
	source := carryIn[0].SourceDate
	src, err := timesheet.ReadDayArtifacts(statePath, source)
	if err != nil {
		return fmt.Errorf("the segment(s) carried from %s cannot be checked against its ledger: %w", source, err)
	}
	if !src.EntriesPresent && len(src.CarryIn) == 0 {
		return fmt.Errorf("%d segment(s) are recorded as carried from %s into %s, but %s has no entries ledger; "+
			"a carry is never staged without its source: restore the ledger of %s and run `stage --date %s` "+
			"(or remove %s if the overnight work was not done)",
			len(carryIn), source, date, source, source, source, timesheet.CarryPath(statePath, source))
	}
	_, derived := deriveLedger(artifactStageEntries(src), source, carryIn[0].Timezone)
	if !timesheet.SameCarries(derived, carryIn) {
		return fmt.Errorf("the %d segment(s) recorded as carried from %s into %s no longer match what the ledger of %s derives (%d); "+
			"the source changed without a re-stage: run `stage --date %s` (or `stage --revise --date %s` if it is saved) before %s",
			len(carryIn), source, date, source, len(derived), source, source, date)
	}
	return nil
}

func carryOutCurrent(current bool, derived, recorded []timesheet.CarrySegment) bool {
	if !current {
		derived = nil
	}
	return timesheet.SameCarries(derived, recorded)
}

func stageMapMatchesUnits(stageMap map[string][]string, units []ledgerUnit) bool {
	if len(stageMap) != len(units) {
		return false
	}
	for _, u := range units {
		if len(stageMap[u.BlockID]) != len(u.Spans) {
			return false
		}
	}
	return true
}

func stagedBlocksFrom(units []ledgerUnit, stageMap map[string][]string) map[string]timesheet.EntryRecord {
	byUnit := make(map[string]timesheet.EntryRecord, len(units))
	for _, u := range units {
		byUnit[u.BlockID] = timesheet.EntryRecord{
			BlockID: u.BlockID, Date: u.Date, Start: u.Start, End: u.End, Description: u.Description,
		}
	}
	byInterval := map[string]timesheet.EntryRecord{}
	for unitID, ivIDs := range stageMap {
		e, ok := byUnit[unitID]
		if !ok {
			continue
		}
		for _, ivID := range ivIDs {
			byInterval[ivID] = e
		}
	}
	return byInterval
}

func sidecarBinds(date string, selections map[string]timesheet.ClassificationChoiceEntry, blocks map[string]timesheet.EntryRecord) bool {
	for id, sel := range selections {
		e, ok := blocks[id]
		if !ok {
			return false
		}
		if sel.ContentDigest != timesheet.BlockContentDigest(date, e) {
			return false
		}
	}
	return true
}

// contentSpan is the comparable staged content of one interval: what it covers
// and the description it carries. Classification is review state and a legacy
// project label (ADR 0012) is inert, so neither is ledger content or compared.
type contentSpan struct {
	Start       string
	End         string
	Description string
}

func ledgerContentSpans(units []ledgerUnit) []contentSpan {
	var spans []contentSpan
	for _, u := range units {
		for _, s := range u.Spans {
			spans = append(spans, contentSpan{Start: s[0], End: s[1], Description: u.Description})
		}
	}
	return sortSpans(spans)
}

func confirmedContentSpans(confirmed []timesheet.Interval) []contentSpan {
	spans := make([]contentSpan, 0, len(confirmed))
	for _, iv := range confirmed {
		spans = append(spans, contentSpan{Start: iv.Start, End: iv.End, Description: iv.Description})
	}
	return sortSpans(spans)
}

func sortSpans(spans []contentSpan) []contentSpan {
	sort.Slice(spans, func(i, j int) bool {
		a, b := spans[i], spans[j]
		if a.Start != b.Start {
			return a.Start < b.Start
		}
		if a.End != b.End {
			return a.End < b.End
		}
		return a.Description < b.Description
	})
	return spans
}

func sameContent(a, b []contentSpan) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func draftDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// readAnswerBinding reads one judgment answer file once and returns its
// presence and its binding value (empty when the file is absent, not JSON, or
// carries no binding). An unbound answer is treated as stale: the judgment is
// requested again rather than a stale file handed to the consuming verb.
func readAnswerBinding(path, key string) (present bool, binding, sha string, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, "", "", nil
	}
	if err != nil {
		return false, "", "", err
	}
	sha = answerSHA(data)
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return true, "", sha, nil
	}
	var value string
	if err := json.Unmarshal(envelope[key], &value); err != nil {
		return true, "", sha, nil
	}
	return true, value, sha, nil
}
