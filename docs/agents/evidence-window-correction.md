# Evidence-window correction (`EVIDENCE-WINDOW-CORRECTION-1`)

> **Historical.** The `internal/run` and `internal/evidence` code and commands this
> procedure cites were removed in the CLI cut-over (f2c80880); the current flow is
> `timesheet log` (see the [README](../../README.md)).

This note is the owner-gated, owner-run procedure for correcting **one day**
whose git evidence was collected under the pre-`4e50ae6` day window. It reflects
the current CLI (`go run ./cmd/timesheet help`) and the code it names; every
command below is an existing subcommand with existing flags.

`timesheet` throughout means the built binary. Build it once with
`go build -o ./bin/timesheet ./cmd/timesheet` and invoke `./bin/timesheet …`, or
substitute `go run ./cmd/timesheet` (which recompiles on every call).

It is a procedure, not a feature. **No corrective CLI exists**: there is no
`collect`, `invalidate`, `recollect`, or `fix` subcommand, and nothing in this
document has been executed against production. Production submission remains
blocked.

## What was wrong

Before GIT-DATE-WINDOW-1 (`4e50ae6`, committed 2026-10-12 15:20 +0700) the git
collector anchored a day's window to UTC instead of the configured
`evidence.timezone`. In a non-UTC zone a commit made near local midnight could
therefore be attributed to the neighbouring calendar day: for a zone ahead of
UTC, early-morning local commits fell out of the day; for a zone behind UTC,
late-evening local commits did.

`4e50ae6` fixed the collector, not the past. The window is now the calendrical
half-open interval `[localMidnight, nextLocalMidnight)` in the configured zone
(`evidence.CollectRequest.Location`, set by the runner from
`evidence.timezone`), and both the git and calendar plugins derive the day window
from that zone. Evidence already stored in a record is untouched by the fix, and
re-collection does not remove it (see "Collection is additive" below).

## Scope: day-scoped, owner-named days

- **One date per run.** Exactly one owner-named date. Ranges (`--from`/`--to`)
  are out of scope: a range re-collects every day in it.
- **Evidence-window only.** The trigger is git evidence that belongs to a
  neighbouring local day, or missing git evidence that the corrected window would
  have included. A wrong project, description, classification, or interval is an
  ordinary review decision, not this procedure.
- Calendar evidence is in scope only in the same sense: its day window also comes
  from the configured zone, and a per-source calendar `timezone` must match that
  zone or be absent (`internal/evidence/plugins/calendar.go`) — a conflicting
  override fails the source rather than quietly re-anchoring it.
- Days not named by the owner are never touched.

## Identifying affected days (bounded, read-only)

The fix landed at `2026-10-12T08:20:04Z` (`4e50ae6`, 15:20:04 +0700). Two record
fields bound the search: `Record.UpdatedAt` (`internal/workflow/models.go:414`)
and the per-source `started_at`/`finished_at` of the last collection
(`internal/evidence/registry.go:87-88`).

```bash
STORAGE="${STORAGE_DIR:-$HOME/.timesheet/records}"
jq -r '[.date, .updated_at,
        (([.collection.sources[]? | .started_at | select(. != null)] | min) // "-"),
        (([.collection.sources[]? | .finished_at | select(. != null)] | max) // "-")]
       | @tsv' "$STORAGE"/workflow/*.json | sort
```

Then keep the days whose last collection started before `2026-10-12T08:20:04Z`.
Filtering out nulls matters: a **skipped** source (disabled) records
`finished_at` but no `started_at`, so a plain `min` over the raw array yields null
and hides a genuinely collected pre-fix day.

This list is a **starting point, not proof**: `updated_at` is the last write to the
record of any kind (collect, review, classify, resolve, approve); `export` only
reads the record, so it does not move it. `started_at` describes only the most
recent collection. A day re-collected after the fix while still holding rows
from the old window looks identical here. The private note below is the record of
what actually happened to a day.

## Preconditions and owner gating

1. **The owner names the day.** Not an agent, not a schedule, not a heuristic.
2. **The owner states why**, in one sentence: which commit(s) or events appear on
   the wrong day, and what the corrected day should contain.
3. **The owner acknowledges the blast radius**: re-collection updates the day's
   evidence, changes what the proposal offers, and can require new confirmed
   intervals, a new plan, and a new approval.
4. **An approved day is never re-collected without deliberately invalidating that
   approval.** The procedure below does that as a required step, not an optional
   one. Silent handling is not available: as shown under "Approval invalidation",
   a re-collection on its own does **not** move an approval, so stopping after the
   re-collect would leave a stale approval active.
5. **Do not run `submit` between steps 2 and 3.** After the re-collect the local
   fingerprint is still the pre-correction one, so `submit.ValidateApproval` still
   passes against the pre-correction plan and `submit` would execute the **wrong**
   plan with real remote writes.
6. **A submitted day is out of scope.** If the day carries a submitted interval or
   an unresolved remote operation, the correction stops and escalates; the
   `reset` decision is refused in exactly that state.
7. `timesheet preflight --config "$CFG"` passes for the config being used,
   including the timezone. The runner fails closed on an unresolvable
   `evidence.timezone` before collecting (`internal/run/collect.go`), so a bad
   zone blocks rather than silently collecting in UTC.

## What is recorded as provenance (there is no provenance field)

These are the fields relevant to window provenance; the list is not exhaustive.
No record field stores the collection window or its timezone:

| Where | What it actually holds |
| --- | --- |
| `evidence.CollectRequest.Location` | `json:"-"` — carried in memory only, **never persisted** |
| `evidence.CollectionSummary` | `date`, `sources`, `complete` |
| `evidence.SourceResult` | source id/type, status, evidence count, error code, message, checks, warnings, `started_at`, `finished_at` |
| `evidence.Evidence` | id, kind, title, description, start/end, all-day, status, invitation, duration, `source_refs`, `observed_at`, topic/project hints, `journal_state`, `linked_evidence_ids` — no window, no zone |
| `workflow.Record.Collection` | the summary above, for the day |

So the old and new windows are **recorded by the owner in a private note**, not
in the record. Write it to a file the store never reads, e.g.
`<storage-dir>/corrections/<date>.md` (the store only reads
`<storage-dir>/workflow/<date>.json`).

Rules:

- **Reading the record is allowed and required**; editing it is not. Every
  `jq`/`shasum` command in this note is read-only. Do **not** hand-edit the record
  JSON: it is revision-checked, atomically replaced, and validated by the store, so
  an out-of-band edit is exactly the silent implementation this procedure forbids.
- Do **not** put the note inside the journal source's `daily_dir`: the journal
  adapter reads `<vault>/<daily_dir>/<date>.md`, so technical prose added there
  becomes narrative evidence on the next collection.

Minimum note content: the date, the correction run number, the configured
`evidence.timezone`, the old and new windows in **both** forms (e.g.
`2026-10-08T00:00:00+07:00` = `2026-10-07T17:00:00Z` through
`2026-10-09T00:00:00+07:00` = `2026-10-08T17:00:00Z`), the pre-correction record
path + `shasum -a 256` preservation hash, the affected evidence IDs with the
reason each moved, the pre-correction plan/approval IDs, and the owner's
one-sentence statement from the preconditions.

## Procedure

`CFG` is the config path and `D` is the single affected date — the date whose
evidence was collected **before** the fix, or it cannot hold old-window evidence:

```bash
CFG=./timesheet.local.json
D=2026-10-08
STORAGE="${STORAGE_DIR:-$HOME/.timesheet/records}"
REC="$STORAGE/workflow/$D.json"
```

### 1. Freeze the day and record old provenance

Read-only. Copy the results into the private note before anything writes.

```bash
shasum -a 256 "$REC"
jq -r '[.date, .revision, (.collection.complete|tostring),
        ([.collection.sources[]? | (.source_id+":"+(.status|tostring))] | join(",")),
        (.collected_evidence|length|tostring)] | @tsv' "$REC"
jq -r '.plans[]?.plan_id, .approvals[]?.approval_id' "$REC"
```

The hash is the rollback reference: a correction that goes wrong is diagnosed
against it, never by rewriting over it.

### 2. Re-collect the named day (and look at the proposal)

`review` re-collects the date: it calls `Runner.Collect`, then `Runner.Propose`,
and prints the proposal. `run` collects too, but it also exports, so this
procedure uses `review` and keeps export to step 6.

```bash
timesheet review --config "$CFG" --date "$D" --no-editorial
```

Use `--offline` only when no Gradion remote should be contacted (source fetches
still run). `--storage-dir DIR` if the records do not live at the default.
`--ack SOURCE_ID,...` acknowledges a source whose gap the owner accepts; it never
substitutes for reading the reported status lines.

**Collection is additive — for git and calendar.** `mergeEvidenceIntoRecord`
upserts by evidence ID: a corrected re-collect adds and updates rows, and it does
**not** purge rows that the old UTC window collected and the new window excludes.
Those stale rows remain in `collected_evidence` and still feed the proposal, so
step 3 is mandatory, not housekeeping.

The journal source is the exception: a journal source re-collected to
`StatusComplete` flips its previously collected rows to `detached`
(`internal/run/collect.go`), and a detached row is retained as visible history but
is never re-proposed (`internal/proposal/build.go`). Journal rows are narrative
and untimed, so this does not move work minutes — but it is a real difference, and
a failed or partial journal collection detaches nothing.

### 3. Reconcile the stale rows explicitly

You need the full evidence IDs of rows that are **not yet** excluded, and no CLI
lists those in full: `review` prints **block** IDs, plus the proposal's own
exclusions (full id, e.g. a detached journal row), and the guided run shows an
evidence id only for exclusions, truncated to 10 characters (`shortID`). Review
exclusions of git rows are not printed at all. Extract the rows read-only:

```bash
jq -r '.collected_evidence[]? | [.id, .kind, .start, .end, .title] | @tsv' "$REC"
```

The timestamps are **per kind**, and this matters for the window comparison:

- `local_git` carries only `.start`, in **UTC** (the commit's timestamp). It sets
  no end time at all, and `omitempty` does not omit a zero `time.Time`, so a real
  row serializes `"end":"0001-01-01T00:00:00Z"`. That value is meaningless —
  never compare it to a window.
- `remote_pr` carries `.start`/`.end` in **UTC** (first and last commit of the PR).
- Only `calendar` rows keep the event's own zone/offset (`.start`/`.end` as
  parsed from the feed); compare them by instant, not by clock text.

So compare `.start` (git) and `.start`/`.end` (PR, calendar) against the **UTC**
boundaries in the note. Every git/PR row whose `.start` falls outside the window
belongs in the exclude list.

Exclude those rows by ID, and confirm what the corrected evidence now supports:

```json
{
  "exclude": [{"evidence_id": "<OLD_OUT_OF_WINDOW_ID>", "reason": "window_correction"}],
  "confirm": [{"block_id": "<CORRECTED_BLOCK_ID>", "project": "Proj", "attendance": true}]
}
```

```bash
timesheet review --config "$CFG" --date "$D" --decisions corrections.json
```

Notes, exact keys from `run.ReviewDecisions`:

- `exclude` retains the evidence as history and only removes it from work; it does
  not delete the row. **Side effect:** a confirmed interval that loses ALL of its
  non-manual evidence-backed references is dropped with it
  (`review.ExcludeEvidence`). The guided bulk flow warns before that happens; the
  scripted path does not, so check `.confirmed` after applying.
- Apply this **after** step 2: the block and evidence IDs must exist first.
- The command prints only `<date>: review applied`, and it prints no proposal.
  Re-running `review` without `--decisions` does not show the exclusions applied
  either: the plain path builds the proposal with `Runner.Propose`, which ignores
  `Review.ExcludedEvidence` (only the guided run and the editorial path recompute
  through `CurrentProposal`). Verify the decisions against `$REC` (step 7) and the
  plan content in step 4; the guided run is the surface that renders exclusions.
- If the existing decision set is wrong rather than incomplete, `{"reset": true}`
  clears the day's local review (`confirmed`, reviewed/empty flags, exclusions,
  attendance, acknowledgments) so it can be redone; a reset that stages no
  replacement work leaves the day unreviewed. Reset is refused when the day has a
  submitted interval or an unresolved remote operation.
- Acknowledge (`"acknowledge": ["<source-id>"]`) only gaps the owner accepts.
  Acknowledgment does not change the schedule and does not invalidate an approval.

### 4. Re-plan and inspect

```bash
timesheet preview --config "$CFG" --date "$D"
```

`preview` persists the plan and performs zero remote writes; it prints
`plan_id:`, the operation count, and each planned create/edit with its interval
and total hours. Read every line: this is the content the next step approves.

### 4a. If `preview` blocks

A blocked preview prints `preview blocked: <reasons>` and exits **3** by design
(`exitBlocked`). These are the reasons a correction actually hits; resolve them
with review decisions, never by hand-editing the record, then re-run `preview`:

| Reason | Remedy |
| --- | --- |
| `readiness:not_reviewed` | Apply a decisions set that confirms the work (or `{"mark_empty": true}` for a genuinely empty day), so the day is marked reviewed. |
| `readiness:unresolved_project:<id>` | Add a `project` decision for that interval. |
| `readiness:incomplete_collection` | Re-collect if the source failure was transient; otherwise `--ack <source-id>` (or an `acknowledge` decision) once the owner accepts the gap. |
| `readiness:unconfirmed_duration:<id>` | The confirmed interval has no usable span (a block confirmed without times). `trim` it to real start/end, or `unconfirm` it and add a `manual` interval with explicit times. |
| `readiness:not_attended_as_work:<id>` | A meeting recorded as not attended is still confirmed as work. Drop it with `unconfirm`/`exclude`, or correct the attendance decision. |
| `readiness:overlap:<a>:<b>` | Resolve the overlap with `unconfirm` or `trim`. |
| `unresolved_classification:<id>` | Choose a classification with `timesheet classify select` (or a `classification` decision). |
| `mapped_remote_missing:<id>` | Stop and escalate: the mapped remote entry is absent from the listing. There is **no CLI remedy** — `resolve` only matches decision rows against unresolved *operation outcomes*, so it cannot fix this. Investigate the remote entry; never plan a create over it. |
| `duplicate_looking_remote:<id>` | The overlapping remote entry is unmapped and cannot be adopted from this path (`preview` passes no adoptions), so the remedies are `exclude` or `trim` here, or reconcile the remote entry first. It is never auto-created. |
| `remote_listing_incomplete` | The remote read was partial. Retry when the remote is healthy. `--offline` reports `remote observed: unobserved`, which does not trigger this reason. |
| `unresolved_operation` | `in_flight`/uncertain/unidentified outcome: reconcile with `timesheet resolve` before anything else. |
| `reconcile:<detail>` | Read the reason and resolve it; do not retry blindly. |

### 5. Re-approve the corrected plan

```bash
timesheet approve --config "$CFG" --date "$D" --plan <PLANID_FROM_STEP_4>
```

`approve` requires `--plan`; a bare approval does not exist. Re-approving
supersedes every other approval for that date, so the corrected plan is the only
active one.

### 6. Export the day's note (read-only w.r.t. remote)

```bash
timesheet export --config "$CFG" --date "$D"
```

Do not run `submit`. Production submission remains blocked, and this procedure
does not change that.

### 7. Verify, then close the note

Read-only checks over `$REC`; all of them must hold:

```bash
# 1. every stale id is recorded as an exclusion
jq -r '.review.excluded_evidence[]? | [.evidence_id, .reason] | @tsv' "$REC"

# 2. the newest approval is neither invalidated nor superseded
jq -r '.approvals // [] | sort_by(.approved_at) | last
       | [(.approval_id // "none"), (.plan_id // "none"),
          (.invalidated_at // "none"), (.superseded // false)] | @tsv' "$REC"

# 3. no planned operation belongs to another day
jq -r --arg d "$D" '.plans // [] | sort_by(.created_at) | last
       | .operations[]? | select(.date != $d) | [.local_id, .date] | @tsv' "$REC"

# and the window itself: git/PR rows, by their UTC start instant. The end
# column is filled for remote_pr only; "-" means there is no end to compare.
jq -r '.collected_evidence[]?
       | select(.kind == "local_git" or .kind == "remote_pr")
       | [.kind, .id, .start,
          (if .kind == "remote_pr" then .end else "-" end)] | @tsv' "$REC"
```

Check 3 prints nothing, check 2 prints `none` and `false` (two `none` id fields
mean the day has no approval yet — expected before step 5), and every `.start`
printed by the last command sits inside the **UTC** boundaries in the note or that
row is already listed as excluded. A `remote_pr` row additionally needs its `.end`
inside the window; for `calendar` rows compare the printed instant, not the clock
text. **Never** test a `local_git` row's `.end` (it is always
`0001-01-01T00:00:00Z`, outside every window), and never require an `.end` at all
for local git.

Then record the new `shasum -a 256 "$REC"`, the `plan_id`, the `approval_id`, and
the resulting per-source statuses in the same private note, and stop.

## Approval invalidation: `LocalFingerprint` and `PlanHash`

The mechanism is content hashing, and it is deliberately *not* triggered by
evidence alone. Stated exactly:

- `workflow.LocalFingerprint(rec)` hashes confirmed intervals (id, date, times,
  project, description, attendance, kind, `ClassificationID`, `ActivityType`),
  review decisions (reviewed/empty, exclusions, attendance, acknowledged sources),
  and remote-mapping identity fields. It does **not** hash
  `Record.CollectedEvidence`, `Collection`, `ObservedAt`, or `Record.Revision`.
- `workflow.PlanHash(plan)` is sha256 over `{Date, LocalFingerprint,
  RemoteObservedAt, RemoteComplete, Operations}`; `SubmissionPlan.PlanID` is that
  hash, and `submit.Approve` refuses a plan whose recomputed hash differs from its
  `PlanID`.
- An `Approval` binds `PlanID` plus the plan's `LocalFingerprint` and remote
  assumptions at approval time.

Consequences the operator must hold onto:

1. **A re-collection alone moves nothing**, and therefore does not move an
   approval. Because `CollectedEvidence` is not in `LocalFingerprint`, `review`
   (collect + propose) leaves an existing approval active — the source says so
   directly: collection never calls `ApplyReview`, so an unchanged approved day
   stays valid. Never treat "the re-collect succeeded" as "the correction
   happened". If the approved day's content must change, step 3 has to change it.
2. **A schedule-affecting decision invalidates eagerly.** Any of `confirm`,
   `exclude`, `join`, `trim`, `project`, `description`, `classification`, `manual`,
   `unconfirm`, `reset` calls `invalidateApprovalsForDate` with reason
   `review_selection`; `classify select` does the same with
   `classification_selection`. The approval is then marked
   `invalidated_at`/`invalidated_reason` on the record.
3. **Submission fails closed.** `submit.Execute` re-observes the remote under the
   day lock and runs `submit.ValidateApproval`. `superseded`,
   `invalidated:<reason>`, `plan_missing`, `plan_mutated`
   (`PlanHash(plan) != plan.PlanID`), `local_changed`
   (`LocalFingerprint(rec) != plan.LocalFingerprint`), and `stale_remote` all block
   with **zero writes** (block reason `approval_<status>` plus the stable
   reasons), and the failed approval is marked invalidated with that status.
4. **Revision bumps alone are not changes.** `Record.Revision` is excluded from
   both hashes, so a bump that changes no hashed content neither invalidates an
   approval nor makes a stale one valid.

## Forbidden

- No silent update of a private record: no hand-edited JSON, no editing
  `collected_evidence` in place, no reconstructing a record from a backup.
- No deletion of evidence rows. Exclusion is a review decision and retains the
  row; the record keeps its history.
- No re-collecting an approved day while leaving its approval untouched.
- No `submit` at all in this procedure; between steps 2 and 3 it would execute
  the pre-correction plan (precondition 5).
- No touching days the owner did not name, and no `--from`/`--to` runs.
- No automatic path: no script, agent, or schedule may decide that a correction is
  needed or run one. Every step is owner-run and owner-read.
- No production submission as part of, or as validation of, a correction.

## What this document is not

This document is a procedure only: no corrective CLI exists. There is no
`collect`, `invalidate`, `recollect`, or `fix` subcommand, no provenance field to
fill in, and no automatic invalidation of an approval caused by re-collection.
This note describes an owner-run sequence of existing commands plus a private
note; it adds no behaviour to the tool, and it does not unblock production
submission.
