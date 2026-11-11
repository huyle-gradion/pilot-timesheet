package main

// Content proofs for the `next` planner. The planner is read-only, so it cannot
// re-stage to learn the truth: it proves freshness from the ledger's CONTENT —
// the same projection the stage verb derives, and the same content-digest
// binding classify and save already validate. Published timestamps are second
// resolution and are deliberately NOT used for any prefix decision.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sort"

	"github.com/huyle-gradion/pilot-timesheet/internal/timesheet"
)

// ledgerUnit is one stage-time unit: a recorded block's effective content and
// the span(s) a fresh stage would turn into intervals. It mirrors applyStage's
// unit selection (headless.go): each overlap-projection cluster, same-day or
// overnight, becomes one unit under its cluster id, split by the four-hour
// policy (timesheet.StageSpans).
type ledgerUnit struct {
	BlockID     string
	Date        string
	Start       string
	End         string
	Description string
	Spans       [][2]string
}

// ledgerBlocks derives the current ledger's stage-time units without their
// chunk spans: the content binding (stagedBlocksFrom) needs only each unit's
// id and effective content, which do not depend on the day's timezone.
func ledgerBlocks(entries []timesheet.EntryRecord, date string) []ledgerUnit {
	var units []ledgerUnit
	for _, c := range timesheet.EffectiveEntrySpans(entries) {
		units = append(units, ledgerUnit{
			BlockID: c.BlockID, Date: date, Start: c.Start, End: c.End,
			Description: c.Description,
		})
	}
	return units
}

// ledgerUnits derives the current ledger's stage-time units with the chunk
// spans applyStage would stage. timezone is the day's recorded timezone (the
// one open_day reports): an overnight span is chunked in real minutes. It
// allocates no interval ids: it is the content projection, enough to prove the
// stored stage map and the saved content are current.
func ledgerUnits(entries []timesheet.EntryRecord, date, timezone string) []ledgerUnit {
	units := ledgerBlocks(entries, date)
	for i := range units {
		units[i].Spans = timesheet.StageSpans(date, timezone, units[i].Start, units[i].End)
	}
	return units
}

// legacyUnits is the derivation before issue #176: every overnight unit staged
// as ONE unsplit span. A day finalized under that rule (e.g. 21:00→03:00 saved
// as one interval, a valid 180+180 remote plan) must still prove current, or
// next/log and submit --chat would demand a `stage --revise` of work the remote
// may already hold. It is consulted only for finalized content; a same-day unit
// is unchanged. The units are copies; the input is not modified.
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

// ledgerDerivesSaved is the finalized-day content proof: the ledger derives the
// saved intervals under the current rule, or under the legacy unsplit
// overnight rule the day may have been saved with (legacyUnits).
func ledgerDerivesSaved(units []ledgerUnit, confirmed []timesheet.Interval) bool {
	saved := confirmedContentSpans(confirmed)
	return sameContent(ledgerContentSpans(units), saved) ||
		sameContent(ledgerContentSpans(legacyUnits(units)), saved)
}

// stageMapMatchesUnits proves the stored stage map was derived from the current
// ledger content. A fresh stage allocates one interval per span, in an order
// that is a function of the unit ids, so equality of the unit-id set and each
// unit's span count is exactly "this map is what applyStage would write now".
// It is order-insensitive and timestamp-free.
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

// stagedBlocksFrom binds each staged interval id from the stored stage map to
// the effective content the current ledger derives for its unit. It is the
// read-only twin of stagedBlocks (headless.go): the same binding classify and
// the confirm/save replay validate, computed from one parse of the ledger and
// one parse of the stage map.
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

// sidecarBinds reports whether every persisted classification selection is
// content-bound to the staged interval it names, exactly as
// applyPersistedSelections checks before replaying. A selection for an interval
// the current content no longer derives, or one whose digest no longer matches,
// is stale: the day must be reviewed and classified again, never silently
// re-bound to the new content.
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
// and the description it carries. Project and classification are configuration
// and review state, not ledger content, so they are not compared.
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

// sameContent reports whether two span lists describe the same staged content.
// It is the content proof that the entries ledger still derives the saved day.
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

// draftDigest binds the descriptions judgment answer to the exact draft bytes
// it was judged from. A re-collect that changes any draft content changes the
// digest, so an older answer file is never merged into the new draft.
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
