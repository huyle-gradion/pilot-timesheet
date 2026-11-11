---
status: accepted
date: 2026-11-11
deciders:
  - Huy Le Minh
related:
  - 0005
  - 0008
  - 0010
---

# Overnight carry into the next day

An overnight entry with more than 240 minutes after midnight (for example
`23:00→05:00`) used to stage as one interval whose next-day allocation broke
the remote's four-hour policy. `plan` then blocked the day with
`remote_entry_too_long`, after the human had already reviewed it, and the only
fix was to re-record the after-midnight part by hand on the next date (issue
#277, follow-up to #176). The owner chose an automatic, deterministic carry
(2026-11-11). No earlier ADR owns the interval model, so this one records it.

## Decisions

1. **The interval model stays as it is.** An interval belongs to the date it
   starts on and crosses at most one midnight. Remote entries are its per-date
   allocations. The carry is a stage-time derivation, not a new kind of
   interval.

2. **When a carry exists.** A source-day stage unit (one overlap cluster) is
   carried in part when it runs more than 240 real minutes past the source
   date's midnight. A unit that starts after that midnight is carried whole.
   Only entries stamped on the next date can form such a unit (decision 9).
   Units with 240 minutes or fewer after midnight stage exactly as under #176.

3. **The split.** The source day is chunked in real minutes in its timezone,
   greedily from the start in 240-minute steps while the next chunk would still
   start before midnight. The last chunk that starts before midnight runs a
   full 240 minutes: it crosses midnight or ends exactly at it. That end is the
   cutoff. The source day keeps the part up to midnight plus up to 240 minutes
   after it, and every interval it stages is at most 240 minutes. The remainder
   `[cutoff, end]` is the carry. Total minutes across both days equal the
   recorded span exactly.

4. **Where a carry lives.** The source day's entries ledger is never modified.
   The carry is a derived, durable record keyed to its source:
   `entries/<source date>.carry.json`, next to the ledger and the stage map. It
   is written by every derivation of the source day (`stage`, `confirm`,
   `save`, `stage --revise`) and replaced whole each time, so re-staging never
   duplicates a carry. An empty carry removes the file. Each segment records
   the source date, the source unit's block id, the target date, the start and
   end clocks on the target date, the real minutes, the description, the kind
   and the source day's timezone.

5. **How the next day consumes it.** The target day's stage input is its own
   ledger entries plus each carried segment as an entry with the block id
   `carried-<source date>-<source block id>`. Carried segments take part in
   overlap claiming and four-hour chunking like that day's own entries, and
   `next`/`log`, `classify`, `submit --chat` and `stage` derive the same
   units. A target day whose sources were collected but whose draft has no
   blocks stages its carried segments alone. Before that, the target day is
   logged through its own collect, describe and record steps as usual. A carry
   never short-circuits them.

6. **A human sees it at review.** The source day's stage receipt lists
   `carried_out`, and the target day's lists `carried_in`, each with the source
   date, the target date, the clocks and the minutes. `next`'s classification
   judgment labels a carried block with `carried_from`, and its `human_gates`
   report lists the day's carried segments. Nothing reaches the remote except
   through the target day's normal human-gated `submit` (ADR 0008, ADR 0009).

7. **A saved or submitted target day is never revised silently.** When a carry
   reaches a day that already has confirmed intervals, `next`/`log` on that day
   stops and names the carry. `submit --chat` refuses because the ledger no
   longer derives the saved day. The source day's stage receipt notes it too.
   Only the human's explicit `stage --revise --date <target>` takes the carry
   in. On a submitted day, that leads to an edit plan through the normal gates.
   The same stop applies when a carry the saved day already includes changes
   or disappears.

8. **The source record must match its derivation.** On a source day that is
   not finalized, a carry record that differs from what the ledger derives
   makes the stage not current, so `next` routes `stage`, which rewrites it.
   On a finalized source day saved under this rule, a differing record is a
   stop.

9. **After-midnight stamps sit on the extended timeline.** An entry whose RFC
   3339 start stamp falls on the next date is placed 24 hours later on the
   source day's timeline, not 24 hours early. Inside an overnight entry it
   joins that cluster and follows the carry if it falls in the carried part.
   On its own it forms a unit that starts after midnight and is carried whole.
   A unit that would run past the next date's midnight is skipped with its own
   reason.

10. **Upgrade.** A day finalized before this change stays current under the
    rule it was saved with: the pre-#176 unsplit overnight interval, or the
    #176 chunking whose midnight-crossing chunk keeps the whole after-midnight
    remainder. No carry record exists for such a day until a human runs
    `stage --revise`. `plan` still blocks its over-long allocation as before.

11. **Fallbacks stay loud.** A cut whose wall time does not resolve to one
    instant (DST at midnight, for example `America/Santiago` falling back on
    2026-04-05) produces no carry. The unit stages as one span, exactly as
    under #176, and `plan` blocks it with `remote_entry_too_long`. A carry whose
    source timezone differs from the target day's recorded timezone is
    refused by `stage` and stops `next`. Its clocks would otherwise be read in
    the wrong zone.

## Alternatives rejected

- **Warn at `stage` only.** This moves the failure earlier but leaves the
  manual re-record. The owner chose the carry.
- **Derive the carry from the source ledger on the fly in the target day.**
  This is stateless, but a day saved before the upgrade would become stale as
  soon as the binary changed, which is the #276 upgrade hazard. With a
  durable record, a carry exists only after the source day was staged under
  this rule.
- **Write the carried part into the target day's ledger.** The ledger is the
  human's log and is never modified by derivation.
- **Cut the source day at midnight.** Moving every after-midnight minute would
  let the target day's own entries claim all of them. The issue keeps up to 240
  minutes after midnight on the source day, so an existing short overnight day
  stays unchanged.

## Consequences

`next` stays a stateless reducer: the carry record is one more observed store
file (factor 5), and the split is a pure function of the ledger and the
timezone (factor 12). A target day staged before its source day was staged
gets the carry later, and the stop in decision 7 surfaces it. The golden-day
export (`eval`) derives units from the ledger alone. It honestly skips a
carried day's classifications as a stale stage map instead of including them.
A day is finalized only by confirmed intervals of its own. The locked
segment that spills in from the previous date's midnight-crossing interval
does not count, so the target day can still stage its carry. Carrying across
more than one midnight and changing the remote four-hour policy are out of
scope.

## Open questions for the owner

- The carry is written at `stage` (a preview), not only at `save`. A target day
  can take in a carry from a source day that is staged but not yet saved.
- A carry into a saved day is a `stopped` state, not `ask_human`. Revising is
  already the existing explicit command (`stage --revise`), so no new question
  type was added.
- A next-date-stamped entry without an overnight entry is also carried, not
  skipped. Previously it staged 24 hours early.
