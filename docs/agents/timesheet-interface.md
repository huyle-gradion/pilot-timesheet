# Pilot Timesheet interface (`0.5.0`)

Application version: `0.5.0` (released as `v0.5.0`; the previous release is
`0.4.0`).
State schema: `4`.

This document is the public interface for the current release. The
implemented surface is the accepted R1-R5 slices: the local review-first
workflow, R2 verification-only evidence collection, the R3 migration/read-only
boundary, the R4 native approval ledger and ONE verified submission path, and
the R5 pinned Pi consumer (retired in `0.2.0-dev.10`: the native `tui` verb is
the operator surface; the internal `serve` protocol command retired after it,
see [the historical protocol section](#historical-the-retired-serve-protocol)). R6 changes no
protocol, schema or store behavior: it
retires the legacy CLI, moves the one entry point to `cmd/timesheet` and makes
`bin/timesheet` the production-named child. The stage descriptions below remain
accurate as historical slices.

Release `0.1.0` promotes the verified dev.17 behavior without changing protocol,
schemas or dependency pins. The owner explicitly accepted the known upstream
Pi/Undici risk for that release; the exposure left the repository with the
retired launcher in `0.2.0-dev.10`. See the root
README for current verification and limitations. Historical stage notes below
describe their original snapshots, not new runtime evidence.

The R4 slice adds a native approval ledger (an approval authorizes exactly ONE
rendered plan), durable pre-write intent records, and ONE verified submission
path: every remote write is acknowledged, re-read from the provider,
content-checked, and only then recorded as confirmed — with explicit,
operator-driven recovery for writes whose outcome could not be verified.

No automatic submission exists in any form: approval never dispatches, startup
never resumes, and a restart never replays a batch. Imported legacy approvals
remain inert audit data. A write that cannot be verified stays an explicit
unresolved blocker until a human resolves it by reference against a current
complete observation.

## Command line

The public surface (`timesheet help` prints it):

```text
timesheet init [--seed-config <path>] [--tz <IANA>]
timesheet log [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>] [--max-actions <n>]
timesheet next [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>]
timesheet status [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>]
timesheet answer [--config <path>] [--env-file <path>] --date <YYYY-MM-DD> [--kind <kind>] <answer>
timesheet submit [--config <path>] [--env-file <path>] --date <YYYY-MM-DD> [--approval-id <id>]
timesheet submit --chat [--config <path>] [--env-file <path>] --date <YYYY-MM-DD> [--plan-id <id> --review-id <id> --confirm-chat]
timesheet submit status [--config <path>] [--env-file <path>] --date <YYYY-MM-DD>
timesheet submit resolve [--config <path>] [--env-file <path>] --file <decision.json>
timesheet submit revoke [--config <path>] [--env-file <path>] --date <YYYY-MM-DD> [--approval-id <id>]
timesheet migrate preview [--config <path>] [--env-file <path>] --date <YYYY-MM-DD|FROM..TO>...
timesheet migrate apply [--config <path>] [--env-file <path>] --date <YYYY-MM-DD|FROM..TO>... --migration-id <id>
timesheet tui [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>]
timesheet daily [--config <path>] [--env-file <path>] [--print-units systemd|launchd]
timesheet judge [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>]
timesheet eval export [--config <path>] [--out <file outside any repository>]
timesheet eval run --golden <file> [--out <file outside any repository>] [--config <path>] [--env-file <path>]
timesheet eval score --golden <file> [--candidate <file>]
timesheet version
```

`answer` lets you answer the day's pending `ask_human` question without an
agent (issue #288). It is terminal-only: it refuses before the store opens
unless both stdin and stdout are terminals. It checks the answer against the
current question's closed options, shows the question and its options, asks
you to type the answer again, copies its digest and writes the answer file
atomically (mode 0600) with `"surface":"terminal"`. A mismatched or empty
confirmation aborts with nothing written. The printed `log` command, and the
notification's suggested `answer`/`tui`/`log` commands, carry `--config` and
`--env-file` when they were given. A wrong answer, a different `--kind` or a day with
no pending question is refused and nothing is written; the refusal carries the
same `last_error` text the agent path would see. `log` then consumes the answer
unchanged. The `resolve` question needs the reconcile receipt's identifiers, so
`answer` refuses it and points to its decision file. The judge never answers
`ask_human` (ADR 0010).

Internal commands stay callable but are not part of the public surface: the
steps `next` derives (`run`, `record`, `stage`, `classifications`, `classify`,
`save`, `plan`, `acknowledge`, `decide`, `reconcile`, `resolve`).

Every command is one stateless process (issue #272): it opens the store, runs
its ops through the in-process session dispatcher, prints its result and
exits. Nothing carries between invocations except the store, so every id one
command hands to another (`plan_id`, `review_id`, `approval_id`,
`migration_id`) is either a durable ledger record or a content binding the
next command rebuilds and compares.

A removed command (`setup`, `approve`, `agent-submit`, `submit-status`,
`resolve-write`, `collect`, `draft`, `confirm`, `serve`) exits `2` with one line
naming its replacement. A removed flag (`--project` on `init`, `log`, `next`,
`tui`, `judge`, `stage`, `classify` and `save`, ADR 0012) also exits `2` with
one line telling you to drop it. The `ask_human` kind `project` is retired with
it; an `ask-project.json` answer left in a day folder is ignored.
`serve`, the long-lived JSONL protocol process, retired
in issue #272; its serve-only ops went to stateless commands or were dropped:

- `migration_preview` / `migration_apply` became `migrate preview` /
  `migrate apply` (see [Explicit legacy migration](#explicit-legacy-migration)).
- `revoke_approval` became `submit revoke`: terminal-only behind a typed
  `approval_id` confirmation, like the other `submit` steps, and never
  reachable through `--chat` or the skill. Without `--approval-id` it takes
  the day's single active approval, and lists and refuses when there are
  several. An already consumed or revoked approval is refused with
  `stale_approval`. The hint after a stopped terminal `submit` names it.
- `check_environment` is reported by `status` under `environment`: required
  variable **names**, which are missing and which are present, never values.
- `cancel` was dropped: staged edits live only in the process that made them,
  and every command is one process, so they are discarded when it exits and
  there is nothing to cancel.

`version` prints the application version and exits 0. It loads no
configuration, environment file, credentials, or evidence. There is no
separate protocol schema any more: no process speaks a wire protocol to the
binary.

`--config` and `--env-file` are process inputs for `tui` and the headless
subcommands. There are no request fields for paths, executable names, or
arbitrary operations. The subcommands default `--config` and treat
`--env-file` as optional (no file values when omitted and no home default
exists).

Exit codes: `0` for a normal `version` run and on success; `2` for a missing or
unknown top-level command, a removed command, extra `version` arguments and
usage errors; `1` for validation, collection and recording failures reported
as `timesheet: <code>: <message>` on stderr.

Only one process may open a given state file. A second store-opening command
against the same state fails with `store_locked`; it does not race or
overwrite the first. `record` opens no store.

## The `tui` verb: a native terminal UI over the loop

`tui` is a native Go terminal UI (issue #221) over the same store, session and
derivation every other command uses. It opens the store once and holds the
exclusive lock for the whole process; a second store-opening process meets
`store_locked`. It refuses to start
unless BOTH stdin and stdout are terminals, and every refusal (non-terminal
stdio, `store_locked`, configuration failure) happens before the alternate
screen starts. There is no AI harness and no model call on this path: each
tick re-derives the day with the same `next` pipeline
(`buildNextObservation` + `deriveDayNext`) and renders the resulting action
verbatim — state, step, why, the judgment protocol text, the human-gate
commands and the store-wide unresolved-write counts. Every act is dispatched
from the derived step; the TUI duplicates zero pipeline decisions and re-runs
the verbs' own entry cores (`collectDraft`, `composeDescriptionsOnly`,
`applyStage`, `classifyFileDay`, `classifyDefaultDay`, `saveDay`,
`preview_plan`) over its held session.

Concurrent-CLI-record race: the `record` path never opens the store (it writes
the lock-free entries ledger directly), so a `record` process may run while
the TUI holds the lock. This is the engine's own design, not a new hazard: the
mitigation is per-tick re-derivation (every screen is rebuilt from the
artifacts and the durable snapshot) plus the fail-closed revalidation every
consuming verb already performs on its own inputs. Pressing `r` re-derives
explicitly after an outside change.

Screens and key bindings:

| Screen | Keys | Effect |
| --- | --- | --- |
| Day (the loop state) | `enter` | Run the act the derived step licenses: run (collect + draft), record, stage, classify (`--file` or `--configured-default`), save, plan; on a judgment step open the authoring form; on `human_gates` open the approve gate |
| Day | `s` | Open the submit gate (`human_gates` only) |
| Day | `r` | Re-derive the loop from the current artifacts and store |
| Day | `q`, `ctrl+c` | Send the `close` op and exit; unsaved session state and bench text are discarded and nothing replays on restart |
| Day (any state) | resize | The screen is stateless and repaints; resize is handled by the driver |
| Bench (authoring) | printable, paste, left/right/backspace | Edit the pre-filled answer document (the exact JSON the judgment's protocol names, the `draft_digest` or `ledger_digest` copied verbatim into its first field) |
| Bench | `ctrl+s` | Write the answer file exactly as authored — no digest is recomputed and no second validation runs; a lost digest or an empty document refuses the write, and the loop's next tick plus the verbs' fail-closed replay checks stay the only validators |
| Bench | `esc` | Abandon the form; the operator text dies |
| Gate (approve) | printable | Type the echo |
| Gate (approve) | `enter` | Re-run `preview_plan` immediately before anything is emitted: the op fires only when the fresh `plan_id` equals the typed echo exactly; any drift emits nothing and re-renders |
| Gate (submit) | `enter` | Re-run `submission_status` immediately before anything is emitted: `submit_plan` fires only for an `approval_id` still ACTIVE on the fresh status, so a revoked or consumed approval cannot be submitted |
| Gate | `esc` | Dismiss the dialog; nothing was emitted |

On `human_gates` the day screen also renders the day's `submission_status`:
every unresolved write with its `write_ref`, the store-wide blockers, and the
active approvals — an operator who dismissed the submit receipt re-acquires
the active `approval_id` there. `submit_plan` is emitted at most once per
confirmation and is never retried; an unknown outcome stays a durable
unresolved write for the human to resolve (see the approval and verified
submission section). The TUI never invokes the terminal-gated `submit` or
`submit resolve` CLI commands, and it never runs `select_classification`
outside the answer-file path: recorded days stay visible to `next` through the
same content-bound sidecar.

`stale_revision` from any act re-runs `review`, re-derives, and reopens the
authoring form fresh when the loop still asks for the same judgment; the loop
moving past that judgment closes the form instead of showing a dead end.

Presentation is one SGR-only style owner bound to one renderer; under the
Ascii profile (`NO_COLOR`, `TERM=dumb`, a pipe) every style renders as identity
and the frame is plain text. Recorded content is sanitized at render
composition, once per field, so a control rune in evidence, a description, a
protocol string, a plan body, an approval row or an error line never drives the
terminal while the UI's own styling escapes survive. Windowing is height-only:
the day screen pins the judgment's instruction surface and the gate commands,
counts and next-line, clipping only the block and status row lists; a gate pins
its head and typed echo; the bench fits the document around the caret. Clipping
names its hidden count with an honest `+N lines` indicator inside the budget.
Interactive scrolling is not part of this release.

## Headless skill surface

`run`, `record`, `status` and the other steps `next` derives are the
deterministic [Script] layer of the timesheet skill (`.agents/skills/timesheet/`). They construct
one session per invocation and drive its request dispatcher in-process; there
is no wire protocol and no new request grammar.

Draft blocks and the staged intervals derived from them share one merge rule
(issue #183): work and evidence blocks that overlap become one block over the
union span with every contributing POW named, a meeting absorbs the work and
evidence it overlaps, and blocks that merely touch stay separate. A merged
block's id is the union-derived `kind-start-end`, the same one `run`,
`record` and `stage` use, so recording a draft replaces the entry a recovery
merge already wrote instead of duplicating it; a single block keeps its own
id. A meeting's title keeps the wording of absorbed work that has no topic
candidate, and another cluster names a titleless contributor by its first
topic candidate, so no POW disappears from the title. The stage derivation
applies the same merge, so a staged interval covers the union span and each
other contributor is reported as merged into its union block rather than dropped; classification selections are bound to the union span and joined
description, so changing any contributor fails the replay closed.

`run`, `record` and `status` treat an omitted `--date` as
today in the configured `timezone`, resolved by the binary rather than the host
clock; an explicit `--date` always wins. The resolved date appears as `date` in
the draft, in the record receipt, and in the status summary.

- `run` performs exactly one bounded collection attempt for the day and then
  builds the compact one-day draft (the retired `collect` and `draft` commands
  were its two halves): time blocks with topic candidates, inline PR identities, bounded
  source references and per-source completeness. The partitioning rule is
  fixed: eligible calendar meetings become blocks in start order; confirmed
  intervals become work blocks; evidence topics attach to the first block
  their time span overlaps; remaining topics form one evidence block; a day
  with no time anchors still yields one whole-day block. When `work_hours` is
  configured, blocks are additionally annotated
  (see [Work-hours annotation](#work-hours-annotation-optional-non-blocking)).
  This draft is the skill's entire model input. stdout carries ONLY the draft
  JSON (the collection source report is not printed; its substance is already in
  the draft's flags and per-block completeness), and the same draft bytes are
  written to the store's day folder
  `<state dir>/days/<date>/draft.json` (created as needed); the artifacts
  path is printed on stderr, so redirecting stdout yields exactly the draft
  bytes.
- `record` validates the reviewed draft (every block keeps its `block_id`
  and carries a non-empty `description`; a block with PRs must keep the
  inline `PRs:` list) and merges it into `<state dir>/entries/<date>.json`
  atomically. The reviewed draft's `date` must equal the resolved day; a
  mismatch fails with an error naming that day. Recording is idempotent per block: an existing block is
  updated in place and never duplicated, so a retried run still counts as
  one day.
- `status` prints the store status and the day's recorded entries. The day is
  `--date` when given, otherwise today in the configured timezone; either way
  the summary names the resolved date.

The collection commands above never approve or submit. Finalization verbs
drive saved plans. `submit` (terminal mode: approve, then submit) and `submit resolve` remain
TTY-gated. The separate `submit --chat --date DAY` returns `{plan, review_id}` without
approval or remote mutation. With all of `--plan-id`, `--review-id` and
`--confirm-chat`, it checks the exact review binding before approving and
submitting once under one store lock. The binding includes day, plan, absolute
store path and the existing endpoint/credential context digest. No protocol or
state schema changes. Chat consent is agent-enforced, not human-presence proof;
see [ADR 0009](../adr/0009-chat-confirmed-submission.md).

Token usage comes from the binary: each `log` report that used the judge carries
`usage`, judge calls are logged in the day folder, and `judge.tokens_csv`
appends one row per run to `benchmarks/tokens.csv`. `scripts/tokens.py`
remains only to import usage from historical agent-session transcripts
(Claude Code, Pi and pig stores) into the same file; it keeps existing rows,
never blocks the day, and copies no transcript content.

## Collection and the incomplete-source gate

`collect{day}` runs each enabled collector against the day's local time window:

- **Window** is `[day 00:00 local, next day 00:00 local)`, resolved in the day's
  recorded timezone. A 25-hour fall-back day collects a 25-hour window; a day
  whose midnight is skipped starts at its first real instant.
- **GitHub** `status` is `complete` (all enabled families fetched to exhaustion),
  `incomplete` (a family hit an honest bound — page cap, search 1000-result cap
  or `incomplete_results`, the submitted-review candidate ceiling, the
  issue-target lookup bound, the events bound (a 422 past the retained window
  or the collector's event page ceiling), an HTTP failure such as a 403 on one
  repository, an omitted unverifiable commit ownership, or topic items lost
  beyond the overflow ceilings), or `missing_credentials`. A topic with more
  items than the draft lists is not by itself incomplete: retained overflow
  is summarized in the draft (below) and the detail notes it. Attribution is strictly by
  provider-named actor,
  verified in the provider's own response bodies (`author` filter pulses
  verified against the commit's GitHub user document, `login` qualifier plus
  `user.login`, `merged_by.login` on merged pull requests, `reviewer
  user.login` on the reviews endpoint, `actor.login`); a response that names
  another actor is dropped, and a commit with no user document is omitted (a
  commit-author email is a filter value, not verified ownership) with the
  omission reported in the family detail.
  The five team proof-of-work shapes are: open PR (authored pull request),
  merge PR (`merged_pull_request`), comment in PR
  (`pr_conversation_comment`), comment in issue (`issue_comment`), and attend
  calendar event. Families: commits (`/repos/{o}/{r}/commits`), authored pull
  requests (search `author:`), merged pull requests (search `merged:` date
  range, then client-side `merged_by.login` and exact `merged_at` verification
  inside the day window; there is no `merged-by:` qualifier), submitted PR
  reviews (search `reviewed-by:` candidates across every PR the user
  reviewed, independent of when the resource was last updated; each candidate
  verified at `/repos/{o}/{r}/pulls/{pull_number}/reviews` and filtered by
  the exact `submitted_at` instant — approval-only reviews appear regardless
  of the events feed, and a review submitted on the day is retained after
  later PR activity), PR review comments (`/repos/{o}/{r}/pulls/comments`),
  issue comments (`/repos/{o}/{r}/issues/comments`), and the bounded events
  feed (`/repos/{o}/{r}/events`, reported `windowed` in its steady state by
  provider design: only the past 30 days, up to 300 events per timeline; a 422
  past the retained window or the collector's page ceiling reports
  `incomplete`). One provider action seen through two families carries one
  canonical event identity and its provenance
  is merged (issue comment `gh:{repo}:issue-comment:{id}`, PR review comment
  `gh:{repo}:pr-review-comment:{id}`, submitted review
  `gh:{repo}:review:{id}`, merge `gh:{repo}:pr-merge:{number}`); distinct
  actions keep distinct identities. The issues/comments document carries no
  `pull_request` field, so each retained comment's number is resolved against
  `GET /repos/{o}/{r}/issues/{n}` (whose non-null `pull_request` key is the
  authority): a PR conversation comment becomes `pr_conversation_comment` with
  `pr:{repo}:{n}`, otherwise the item stays `issue_comment` with
  `issue:{repo}:{n}`. Resolutions are cached per run and bounded to 50
  lookups per repository per attempt; an unknown answer or the ceiling leaves
  the item an issue comment and reports the family `incomplete` with the
  count, never a guess. Topics group by retained provider resource identity
  (`resource` field). A topic lists at most 50 items; items beyond that are
  retained durably as topic overflow (up to 150 per topic and 600 per
  collection attempt), and items beyond those ceilings are counted as lost.
  Such a topic records `truncated`, and its draft `topic_candidates` line
  carries one deterministic summary, for example
  `Other attributable activity (o/r) — overflow: +6 more items (event 6),
  02:07–09:40`: the count of retained items beyond the cap with per-kind
  counts in alphabetical order, `N lost` when items fell beyond the
  ceilings, and the first–last overflow time in the day's timezone. A grouped
  or collapsed line sums its topics' summaries, and the `+N more` line sums
  the lines it omits. Overflow times still shape the day's evidence blocks
  like any other item time, so summarized activity is never left without a
  block; only the block's listed sources stay limited to the first 50 items. Retained overflow alone leaves the source `complete`;
  any lost item makes it `incomplete`, which asks for acknowledgment as
  usual. Item `kind`s: `commit`,
  `authored_pull_request`, `merged_pull_request`, `submitted_pr_review`,
  `pr_review_comment`, `pr_conversation_comment`, `issue_comment`,
  `discussion`, `discussion_comment`, `event`.
- **Calendar** `status` is `complete`/`incomplete`/`empty`/`missing_credentials`.
  Series expand over the full recurrence set RRULE ∪ RDATE − EXDATE with each
  property's own TZID/value form and de-duplication, for timed and all-day
  series (pinned rrule-go/golang-ical; unsupported value forms report
  `incomplete`, never silent drops). All-day DTEND is exclusive: a one-day all-
  day event with DTEND the next date covers exactly its DTSTART date, with no
  DTEND it covers its DTSTART date; the displayed all-day `end` is that
  exclusive end date. Date-only events keep their declared DATE labels:
  occurrence and boundary instants are resolved from the dates with the same
  first-real-instant rule as the day window, date-only recurrence expands on
  civil dates, and the exclusive end is resolved from its own date rather than
  by carrying a normalized start clock into another date — so a declared
  September 6 event never appears on September 5 or on its exclusive-end
  September 7 because local midnight was skipped or repeated.
  RECURRENCE-ID overrides suppress their replaced master
  occurrence; a scheduled override appears where its own span overlaps the day,
  and a cancelled override appears only when the replaced original instance or
  the override's own span touches the queried day — unrelated days do not
  emit unrelated cancellations. Recurrence candidate selection is
  span/duration-aware: the expansion look-back is derived from the series'
  occurrence duration or calendrical span and exact interval overlap decides
  membership, so a long timed or all-day occurrence that began before the day
  is retained (interior days of a three-day all-day instance, the tenth day of
  an eleven-day timed instance). A cancelled master event or override is still
  a proposal with `status: "cancelled"`; attendance is the attendee's own
  PARTSTAT. An event / override / recurrence value that cannot be parsed,
  resolved, or expanded marks the source `incomplete` and never silently
  drops it.
- Details in receipts are status-only (HTTP status, bound counts); feed URLs,
  tokens, and raw provider documents are never echoed.

`confirm` and `save` fail `source_unacknowledged` while any enabled source's
attempt for the day is neither `complete` nor explicitly acknowledged — this
includes the never-collected state (run `collect` first), as well as
`incomplete`, `failed`, and `missing_credentials` attempts. Missing
credentials are an explicit source failure requiring acknowledgment exactly
like any other incomplete coverage; a `disabled` source imposes no gate. Each
`collect` creates a new attempt ID and resets earlier acknowledgments — a
fresh attempt always requires a fresh human look.

`acknowledge_source{day,source,collection_id}` carries and verifies the
exact reviewed collection identity. The collection ID of the reviewed attempt
is returned by `collect` and shown as `collection_id` in the review's
`sources` entries. A request without `collection_id` fails `bad_request`; an
ID that does not match this day's recorded attempt for the source fails
`stale_collection` and mutates nothing — collection may have run since the
call's review, and silently acknowledging whichever attempt is currently
latest is refused. Acknowledgment and meeting decisions are copy-on-write: a
failed write leaves review state, disk, gate state, and the absence of
evidence keys exactly as they were; no failed pre-activation write can create
a record that a later successful write persists as an unreadable null
evidence entry, and preserved post-rename `write_uncertain` behavior is
unchanged.

`decide_meeting` decisions are recorded separately from confirmation and from
calendar attendance flags; `pending`/`attended`/`skipped` never create
minutes. Every attendance decision must carry the `collection_id` of the
reviewed calendar attempt: a request without it fails `bad_request`, and an ID
that is not the day's current calendar attempt fails `stale_collection`, both
without mutation. The required identity is deliberate — an omitted identity is
not treated as permission to target the latest state. The meeting `meeting_id`
(an opaque reference) must also name a proposed meeting in the day's current
evidence; the reference fingerprints the current target, including the
rescheduled start of an overridden occurrence, so a decision built against a
recollected, moved instance fails `unknown_meeting` even when its original
RECURRENCE-ID is unchanged. Recollections re-match persisted decisions by
`UID|Start` identity of the proposed meeting; that carry-over of an unchanged
instance's existing decision is separate from authorizing a new action, and a
replacement that changes the instance identity does not inherit the old
decision.

### Review `evidence` object

`review{day}` returns `result.evidence` for the day's most recent attempt:

- `sources`: one entry per configured source — `source`, `status`
  (`complete`/`incomplete`/`empty`/`missing_credentials`/`disabled`), and a
  status-only `detail` when bounded.
- `topics`: provider-suggested topic candidates grouped by exercise (per-repo
  commits, per-PR items, per-issue items, per-discussion items), each `item`
  carrying an opaque `ref`, a `kind`, and time text — never durations.
- `meetings`: proposed meetings with `uid`, opaque `ref`, start/end (original
  offsets preserved, all-day date-only), `recurrence`
  (`single`/`recurring`/`override`), `status` (`scheduled`/`cancelled`),
  attendance (the attendee's own PARTSTAT), and the explicit human `decision`
  (`pending`/`attended`/`skipped`) when recorded.

## Typed interval grammar

```text
DATE HH:MM-HH:MM DESCRIPTION
```

- `DATE` is `YYYY-MM-DD` and must equal the open day.
- Times use 24-hour `HH:MM`. `END` equal to `START` is rejected as zero duration.
- Times are resolved in the open day's recorded IANA timezone. A newly opened day
  takes the configured timezone; a saved day always keeps the timezone stored in
  its record, so changing configuration never reinterprets saved work.
- There is no project token and no `/CLASSIFICATION` suffix
  ([ADR 0012](../adr/0012-retire-the-local-project.md)). The remote
  classification is the reviewed selection (`classify`), never part of the
  text.
- `DESCRIPTION` is the remaining text. One pair of surrounding single or double
  quotes is stripped. Empty descriptions are rejected.

Examples:

```text
2026-10-22 09:00-10:00 Synthetic review
2026-10-22 10:30-11:15 "Synthetic follow-up"
2026-10-22 22:30-01:15 SYN-2 Late synthetic session
```

`evidence_refs` is an optional array of opaque strings (maximum 20). Multiple
references never add time; R1 does not resolve or fetch them. GitHub duration
inference is absent.

### Midnight and day boundaries

Intervals belong to the day they start. A cross-midnight interval is split for
display into per-day allocations:

```text
2026-10-22 22:30-01:15  =>  2026-10-22 22:30-24:00 (90 min)
                            2026-10-23 00:00-01:15 (75 min)
```

The start day's total counts only 90 minutes; the 75 minutes after midnight are
reported on 2026-10-23 as a `locked` segment and are included in that day's
total. Nothing wraps, disappears, or is counted twice. A locked segment is
edited on its origin day, not on the day it spills into. An `END` of `00:00`
with a later start means the interval ends exactly at midnight and is not a
cross-midnight split.

### Overnight carry into the next day

Stage derives intervals from the entries ledger under these rules
([ADR 0011](../adr/0011-overnight-carry-into-the-next-day.md), issue #277). A
recorded overnight span with 240 real minutes or fewer after midnight stages on
its own date. Its chunks are cut to the four-hour policy, and the chunk that
crosses midnight is the last one. A longer span keeps the part up to midnight
plus up to 240 minutes after it. Its last chunk runs a full 240 minutes, and
the remainder is carried into the next date:

```text
2026-10-26 23:00-05:00  =>  2026-10-26 23:00-03:00 (240 min, staged on 10-26)
                            2026-10-27 03:00-05:00 (120 min, carried)
```

The carry is a derived record, `entries/<source date>.carry.json`. Every
derivation of the source day (`stage`, `confirm`, `save`, `stage --revise`)
replaces it whole, and an empty carry removes it. The ledger is never modified.
The next date stages each carried segment as the block
`carried-<source date>-<source block id>`, next to its own entries. It takes
part in overlap claiming and four-hour chunking like they do, and the stage
receipts list it under `carried_out` and `carried_in`. A block stamped on the
next date is placed after midnight, not 24 hours early. A block that would run
past the next date's midnight is skipped with its own reason.

A carry never revises a saved or submitted day. `next` stops on that day and
names the carry, and `submit --chat` refuses. Only `stage --revise --date
<target>` takes it in. A carry computed in another timezone is refused.

A carry is consumed only while its source still derives it. If the source
ledger was edited without a re-stage, every day that would take the carry in
stops and names `stage --date <source>`. If the source ledger was deleted, it
stops with its own reason. This covers `stage`, `save`, `classify`, `next` and
`submit --chat`.

`daily` also logs a non-working day that has a carry-in. Notifications count
the minutes each day stages. `human_gates` lists the carry under
`human.carried`.

A cut that does not resolve to one instant makes no carry: the span stages as
one interval, and `plan` blocks it with `remote_entry_too_long`. This happens
on DST at midnight (`America/Santiago`, 2026-04-05). It also happens on a
fall-back night whose cutoff lands in the repeated hour (`Europe/Berlin`
`2026-10-24 22:00→05:00`). The binary never chooses an occurrence of a repeated
wall time.

When a carry is made, each interval on the source day is at most 240 minutes.
With exactly 240 minutes after midnight there is no carry: the source day
keeps one longer interval, and each of its per-date allocations is at most 240
minutes.

### Timezone transitions

Elapsed minutes use absolute time, not the wall-clock difference. With the
recorded timezone `Europe/Amsterdam`:

```text
2026-03-29 01:30-03:30  =>  60 min   (02:00-02:59 does not exist)
2026-10-25 01:30-03:30  =>  180 min  (the 02:00-02:59 hour repeats)
```

A start or end wall time that does not exist fails with `nonexistent_time`. A
wall time that occurs twice fails with `ambiguous_time`; this build never chooses
an occurrence. Choose a time outside the transition, or split the work so both
endpoints are unambiguous. An unambiguous interval may still span a transition,
and its allocation to each local date is computed from absolute time. Midnight
allocation and overlap also use each origin day's recorded timezone.

A transition may also skip local midnight. That does not remove the date: the
local day begins at its first real instant, for example local `01:00` on
2026-09-06 in `America/Santiago`. Ordinary work on such a date is accepted and
allocated there, cross-midnight work spills with the actual local continuation
start, and a wholly skipped civil date is not reported as work. A wall time in
the missing midnight hour still fails with `nonexistent_time`.

These rules are deterministic but intentionally incomplete for repeated-hour
work. A later slice may add an explicit offset or occurrence field; R1 does not
store one.

## Review, confirmation, save, revision, cancellation

`review` renders the last confirmed state and the current staged draft:

```json
{
  "date": "2026-10-22",
  "timezone": "Europe/Amsterdam",
  "revision": 1,
  "saved_revision": 1,
  "dirty": false,
  "confirmed": {"segments": [...], "total_minutes": 105},
  "staged": {"segments": [...], "total_minutes": 105},
  "notes": ["..."]
}
```

Each segment shows its interval ID, date, start/end, minutes, description, evidence references, origin date, `locked`, and
cross-midnight allocations when relevant. Interval IDs are unique across the
store (`iv-YYYYMMDD-n` in R1) and remain stable through staged edits. The
review also lists confirmed revision `history` (revision, confirmation time,
interval count, total minutes, and the confirmed interval snapshot) for later
slices; it grants no remote authority.

- Opening a day copies confirmed work into a staged draft. Mutations affect the
  draft only.
- `confirm` requires a valid draft unless the day already has confirmed work. It
  replaces confirmed work, increments `revision`, and appends local history. An
  explicitly reviewed removal of the final interval is therefore confirmable and
  records a `0`-interval, `0`-minute revision. The response has `"durable":
  false`; confirmation is not yet on disk and is never a remote submission.
- `save` writes confirmed work durably. It requires the current revision and the
  same durable revision the session loaded. It writes atomically to a private
  state file with mode `0600`.
- Save validation uses the candidate durable store: persisted state for every
  other date plus this day's confirmed intervals. Unsaved confirmations on other
  open days are not saved implicitly and do not make a save appear valid. If the
  candidate would overlap, the save fails with `overlap` and the response says
  which recorded days conflict; save or correct the origin day first.
- A successful save receipt describes exactly what restart will load for that
  day. Review and status are session views and may include unsaved confirmations
  on other open days, but those are absent from save receipts and from disk until
  their own save succeeds.
- Staged edits are discarded when the process that made them exits; there is
  no `cancel` op since `serve` retired (#272).
- `close` never implies save.
- Restart loads only saved confirmed work. Staged edits, and confirmations not
  followed by a successful save, do not survive a restart.

Zero/negative/invalid intervals and unresolved overlaps are rejected before the
draft changes. Staging and confirmation evaluate overlaps in the session view,
so dependent edits can be planned before either day is saved. Saving re-evaluates
the candidate durable state and rejects a dependency that is still unresolved.
Overlaps are absolute-time ranges across stored days, including cross-midnight
allocations and timezone differences. Back-to-back intervals (`10:00` end,
`10:00` start) are allowed.

### Save failure and durability

A save reports success only after the new file is activated and its directory is
synchronized. Failures are split at the rename boundary:

- `write_failed` before activation: the previous durable file is unchanged, the
  in-memory snapshot is rolled back, and retrying after the cause is fixed is
  safe.
- `write_uncertain` after activation, when the directory cannot be opened or its
  durability cannot be confirmed: the visible file may contain the new revision,
  but no durable receipt is returned. Further saves in that session fail with
  `write_uncertain`; blind retries are blocked. Close and restart to reconcile
  with whatever the state file contains. `status` reports
  `"durability_uncertain": true` until restart.

## Durable state, migration, remote reads, and approval (R3/R4)

### State schema 4

State schema `4` retires the local project
([ADR 0012](../adr/0012-retire-the-local-project.md)). It keeps every schema `3`
section below unchanged and makes a confirmed interval's `project` optional
(`omitempty`): a new interval has none, and an interval saved before schema `4`
keeps its `project` (and its local `classification` label) as an inert legacy
label that nothing reads. A schema `1`, `2` or `3` file is still validated in
its own shape, where every confirmed interval needs a project, then upgraded in
memory. The upgrade adds one receipt to `migration.schema_upgrades`:

```json
{"from_schema": 3, "to_schema": 4, "app_version": "0.3.0", "days": 12,
 "intervals": 40, "legacy_project_labels": 40, "applied_at": "2026-11-11T09:00:00Z"}
```

The receipt becomes durable in the same atomic write that activates schema `4`
(the first successful save or transaction, never startup), and `applied_at` is
stamped by that write. A new store gets no receipt. A file that declares an
older schema but already carries `schema_upgrades` is `state_invalid`. Neither
legacy label appears in review, day or segment output, including history
snapshots.

The upgrade is **one-way**. Before the activating write the older file is
kept at `<state>.schema<N>.bak` (for example `state.json.schema3.bak`,
`0600`). The copy is written to a temporary file, synced and hard-linked into
place, so a backup name is complete or absent. An existing name counts only
when it is a regular file with exactly those bytes; otherwise (a stale backup,
a dangling symlink, an unrelated file) the copy goes to the next free
`.bak.1`, `.bak.2`, and so on. If that copy fails, the write fails and nothing
changes. The activating write prints one stderr line naming the file it
actually used. A
write that fails before activation leaves the older file unchanged (the backup
may remain). A `0.2.0` build reading a schema `4` file fails closed, leaving
it intact, with `state_invalid: local state file is not valid JSON for the
supported schema`: its strict decoder rejects `migration.schema_upgrades`
before the schema check. To downgrade, restore the backup over `state.json`
and put `projects` back in the configuration. Until the `0.3.0` release, the
receipt's `app_version` reads `0.2.0`.

### State schema 3

State schema `3` is the durable contract for migration, remote identity, plan
records and the native approval ledger. It keeps schema `2`'s three optional
sections:

- `migration`: append-only receipts (source hashes, dates/timezone, counts, a
  date-scoped ID crosswalk plus the legacy per-date revisions/audit, inert plans,
  approvals, reviews and revalidation list)
  and a repeat-safe source-identity map, plus unconfirmed legacy carry fragments.
- `remote`: stable bindings keyed by (origin interval, allocation date) — the
  binding snapshot plus `last_observed` content, the latest observation per date,
  explicit resolutions, preserved write outcomes and bounded classification
  catalogs.
- `plans`: immutable content-addressed previews with per-operation exact content
  including the edit audit `reason`. They carry NO approval flags.

and adds three more:

- `approvals`: immutable native approval occurrences (`apr-…` IDs, exact plan
  binding, context digest, state `active`/`executing`/`revoked`, revocation
  timestamps, bound execution ID).
- `intents`: durable pre-write attempt records (`wr-…` references, exact body,
  state `in_flight`/`confirmed`/`uncertain`/`unidentified`, plus additive
  resolution audit `resolved`/`resolution`/`resolved_at`/`resolution_observation_id`).
- `executions`: one bounded run summary per submission (state
  `started`/`completed`/`stopped`, per-attempt counts, outcome).

A schema-1 file is validated in its own shape, upgraded **in memory**, and left
byte-identical on disk. Schema `2` files are converted in memory to schema `3`
(above), and from there to schema `4`. The current schema is activated only by
a successful durable save or transaction (the same atomic rename + directory-sync
path as a work save), never by startup. `status` reports `store_schema: 4` (the running schema) and
`durable_schema` (what is actually on disk: the declared header schema until that
first write).

A schema-2 file fails closed (`state_invalid`) in a schema-1-only loader. That
forward incompatibility is intentional: a build without migration/remote/plan
semantics must not load receipts, identity decisions, unresolved-write history or
plans it cannot honour. A schema-1 declaration that carries any later section is
also rejected. Under the schema-2 conversion, an R2 file is rejected when it
contains `approvals`, `intents` or `executions` sections, or any persisted plan
`approved`/`approval_available` flag that is non-boolean or true (always-false R3
flags are stripped); state claiming approval authority fails closed. A schema-2
file's schema header is not silently upgraded before its own validation.

### Explicit legacy migration

Configuration:

```json
{
  "migration": {
    "enabled": true,
    "legacy_root": "/Users/you/records",
    "legacy_timezone": "Europe/Amsterdam",
    "classification_map": {"legacy-classification-value": "DEV"}
  }
}
```

Migration is disabled by default. When enabled, the authoritative source is
exactly `<legacy_root>/workflow/YYYY-MM-DD.json` declaring legacy
`schema_version: 1`. Superseded files directly at the root are not current
confirmed state; they are left intact and never promoted. The binary reads only
configured, date-named regular files: no recursive discovery, no request-supplied
path, no symlinked `workflow` directory or source file, no escape from the
resolved root, and no write into the legacy root. The old store constructor is
never called.

Dates are selected explicitly with `timesheet migrate preview --date <day>`
(repeatable, or an inclusive `FROM..TO`) and are bounded
(`62` dates). An over-bound or invalid selection is rejected; it is never silently
truncated. Legacy records do not carry their governing timezone, so
`legacy_timezone` is required and recorded on the migrated days; a changed new
configuration timezone never reinterprets them.

The documented consistency precondition is **quiescent legacy writers**. The
binary re-hashes every selected source file immediately before activation and
fails the whole candidate if any byte changed. It does not pretend an unlocked
multi-file read is atomic with an independent writer.

Preview validates the complete candidate without activation and returns bounded
counts/conflicts plus a `migration_id` bound to `{source hashes, dates, timezone,
current durable target fingerprint}`. `timesheet migrate apply --date <same
days> --migration-id <id>` keeps nothing from the preview: it re-reads and
re-hashes the sources, rebuilds the candidate from the store and the current
configuration, and activates it only when the rebuilt `migration_id` equals
`<id>`. The id hashes the selected dates, the legacy timezone, every source
file's bytes, the durable target fingerprint AND the exact payload apply would
commit (days, bindings, outcomes, carries, receipt and conflicts). It is
therefore a binding to the reviewed result, like `submit --chat`'s
`plan_id`/`review_id` under ADR 0009, not a stored grant: an edited
`classification_map` changes the payload and the id. So a preview grants no standing authority, and a changed selection,
configuration, source or durable target is `migration_stale` with nothing written. A
conflicted candidate is `migration_conflict`; an already-migrated selection is
`no_change` on both preview and apply. Activation requires
that **no day is open**, so a draft can never be discarded and no session pointer
can be left stale. The whole candidate is activated in one atomic state write.

Preserved:

- Confirmed work, stable interval IDs (or a deterministic legacy-to-new mapping,
  recorded as a DATE-SCOPED crosswalk in the receipt), descriptions,
  explicit classification values, evidence references plus their non-secret
  provenance pointers, and legacy source revisions/`updated_at` as audit.
  Preserved audit rows are date-associated. A legacy local ID repeated inside one
  source record is `ambiguous_local_id`: it is rejected before any alias or
  binding is guessed, instead of being resolved by row position. A legitimate
  collision across separate dates (or with an existing target ID) uses a
  deterministic date-scoped alias, and mapping/outcome references translate
  through that per-date crosswalk rather than one collapsed map. Time is never
  inferred from evidence and carry proposals are never confirmed.
- Stable local/remote mappings with the binding-versus-last-observed distinction,
  submitted outcomes, legacy plans and approvals. Raw collected provider documents
  are **not** imported; only their count is audited, and archival provenance never
  enables a retired collector.
- Legacy review history PER DATE (not one last-day pointer): the review outcome,
  authored exclusion reasons/timestamps, attendance decisions
  (`evidence_id`, decision, `decided_at`) and acknowledged sources as inert audit.
- Legacy plan history strictly inert: its per-date record, operation contents
  (`remote_id`, date/times, classification, description) and the preconditions it
  was bound to (`local_fingerprint`, record revision, remote observed-at and
  completeness), plus approval/revocation timestamps and status.
- Submitted outcomes with the canonical legacy meaning (see
  `internal/submit/resolve.go`): a resolved row never blocks, and only unresolved
  `in_flight`/`uncertain`/`unidentified` rows block. Confirmed/failed/skipped rows
  are TERMINAL history — their manual-resolution flag being false is not a pending
  write. An unrecognised status is a `malformed_outcome` conflict; it is never
  imported as safe history. Legitimate unresolved rows remain durable blockers,
  survive restart, and are never replayed or automatically cleared.
- Legacy approvals strictly as inert history: `inert: true`, `usable: false`,
  including the approval binding's own `record_revision` and
  `remote_observed_at` (preserved verbatim, never inferred from the linked
  plan, whose values may differ), plus approval/revocation timestamps and
  status. They approve nothing and never authorize anything; only native
  `approve_plan` creates usable approvals.
- Unconfirmed legacy carry fragments as unconfirmed durable carries. A separately
  confirmed carry fragment is an ordinary confirmed interval and is not duplicated.

Repeat safety: identical source bytes and the same declared legacy timezone
for already-imported dates are a
`no_change` result with no duplicate intervals, mappings, receipts or history,
including after restart. Receipt evidence is compared per date against the
receipt covering that date, so a repeated subset of a larger imported batch is
a valid no-change selection when its bytes and timezone agree. Changed source
bytes, the same bytes under a different declared legacy timezone
(`interpretation_conflict`: the recorded day/receipt interpretation is
preserved, and no no-change receipt is issued for the different identity), a
selection mixing imported and new
dates, or an occupied destination date are conflicts; migration never
overwrites,
merges by time slot, or rebinds. `migration_apply` validates the freshly
re-read candidate (source identity, durable target, conflicts) before any
successful `no_change` return, so a source or target changed since the preview
is a structured failure, never a false revalidation receipt. Invalid dates, an
unknown source schema, an
ambiguous/nonexistent endpoint, overlapping confirmed work, dangling or
non-one-to-one identities, an unrepresentable classification, and
malformed carry/outcome records reject the whole candidate without partially
importing the batch. Preserved audit rows are bounded per category
(2000 provenance pointers, 2000 interval audits, 500 plan audits, 500 approval
audits); a selection above a bound is a `migration_capacity` conflict — rows are
never silently truncated and the whole candidate refuses activation. Original
files and the prior target bytes are preserved; the existing pre-rename rollback
and post-rename `write_uncertain` semantics are unchanged.

Future interval allocation cannot reuse a migrated ID: the day's counter is
advanced past preserved `iv-<date>-<n>` shapes and allocation additionally rejects
any staged/confirmed collision.

### Gradion boundary

```json
{
  "gradion": {
    "enabled": true,
    "endpoint": "https://mcp.example.invalid/mcp",
    "token_env": "GRADION_MCP_TOKEN"
  }
}
```

Disabled by default. `endpoint` is operator-configured. `token_env` is a variable
**name** (default `GRADION_MCP_TOKEN`); only the compiled binary resolves its value
through the documented dotenv/inherited precedence, including inherited-empty. A
missing credential reports the name only. No Keychain dependency, real settings
read, or implicit live call exists; `version` works without any of it.

The boundary wraps the existing MCP transport. The read path verifies exactly
`timesheet__get_my_week` and `timesheet__get_classifications`; the submit path
(used only by explicit `submit_plan`) additionally verifies
`timesheet__log_time` and `timesheet__edit_time`. There is no delete tool, no
REST fallback, no arbitrary tool name, and no generic MCP call operation. Writes
are never retried. There are no new retries or dependencies.

The verified week shape is the structured
`{days:[{date,entries:[{id,startTime,endTime,classificationId,description,
durationMinutes,status,task}]}], weekStart,minDate,maxDate,today}` document.
Required containers are validated before any absence is claimed. `days` must be a
list of day objects, and every day object must carry a `date` string and an
`entries` LIST: a day object without `entries` — or with `entries: null` — is
missing/unreadable data, never an empty day. Only an explicit `entries: []` is a
valid empty result for that date. Unknown fields (for example billable state the
read does not expose) stay unknown, never false. A resource-reference envelope,
an empty payload, an unknown shape or a failed read is a read error, never an
empty timesheet. Coverage is explicit: a missing requested date, a provider date
range that excludes it, a malformed entry, duplicate/conflicting IDs or the entry
bound make the observation `complete: false`. A failed fresh observation replaces
the stored complete observation, so an old read can never stand in as current
proof of absence.

`classifications{query}` returns bounded options (the tool's 20-row cap) and a
content-addressed `catalog_id`. Reaching the cap or a truncation header, or a
structured envelope with no text rendering, is `complete: false`; such a catalog
can only show that an option was not returned, never that it is absent.
`select_classification` accepts only an option that this catalog actually
returned, records both the option and catalog identity on the staged interval, and
becomes durable only through `confirm` and `save`. A migrated legacy
classification is a suggestion, never a silent remote selection;
`classification_id` is never derived from a title or model text.

### Reconciliation and explicit resolution

`reconcile_remote{day}` performs one fresh read-only observation, persists it, and
surfaces mapped matches, changed/missing mapped remotes, unbound remotes,
overlap/equal-text candidates, orphaned mappings and unresolved-write history.
Identity is the origin interval AND the date an allocation renders on: a
cross-midnight interval has one binding per allocation date. Orphaned means the
bound local allocation no longer exists — the interval was removed, or an edit
moved the work off the binding's date. A bound remote is never a free slot: when
an unmapped allocation would physically overlap an entry bound to another (or
removed) identity, the reconciliation is blocked with `identity_conflict` (or
`orphaned_mapping`) instead of skipping the entry, and the mapping is never
erased or reassigned. Unrelated dates and unrelated mappings elsewhere stay
scoped: a historical orphan only matters where it physically occupies planned
time. A fresh read alone never binds an unmapped entry: overlap or equal text is
only a candidate. An empty local store proves nothing, and remote-only/orphaned
entries never imply deletion.

`resolve_remote` records one human decision bound to the current observation:
`adopt` binds an observed entry to an unmapped local allocation — the allocation's
OWN date, so the later day of a cross-midnight interval can adopt its actual
remote entry even when the origin date is already bound; `accept_observed`
accepts observed content for the SAME already-mapped remote identity and
allocation date; `foreign` acknowledges a remote entry with no local counterpart
(`local_id` omitted) and cannot erase an active binding. A foreign decision is
bound to the exact current observation: an identical fresh observation keeps it,
and a changed relevant observation makes it stale. It rescinds equal-text hints
only — it never grants permission for physical overlap. Stale revisions or
observation IDs, an unobserved remote ID, a time-slot identity substitution, an
adoption that would steal another allocation's remote ID or replace an existing
binding, and malformed local/remote pairs are all rejected without mutation.
Resolutions never by themselves clear an unresolved-write blocker (only the
explicit `resolve_write` operation does, in R4) and never authorize a
remote mutation.

### Exact plan previews

`preview_plan{day}` uses only saved confirmed durable work, explicit
`select_classification` choices, and a fresh read-only observation of the planned
date. Every plan operation is ONE ALLOCATION: for a cross-midnight interval it
renders the date's own span and minutes (a 23:00-01:00 interval is one
60-minute 23:00-00:00 operation on the origin date and one 60-minute 00:00-01:00
operation on the later date, never two 120-minute origin operations), and each
date's preview observes that remote date. It never plans from an unsaved origin/day
(a dirty open origin fails `dirty_session`), never saves a draft, and never
silently shortens an interval or drops midnight minutes. An allocation whose
verified shape has no representable same-day remote span (a whole-day allocation)
is `unrepresentable_projection` — the work is never altered or declared ready.

- Unchanged mapped work yields no write operation. An interval alone is not a
  remote identity: binding identity is (origin interval, allocation date), so
  both dates may be bound independently.
- A mapped edit carries the stable remote ID, the allocation's intended fields,
  the exact observed precondition, and the remote-only fields the local model
  does not own (`task`, `billable`).
- A create requires complete fresh coverage and resolution of relevant
  identity/overlap ambiguity. Incomplete/unreadable reads, unbound overlap/text
  candidates, entries occupied by another or orphaned identity (`identity_conflict`
  / `orphaned_mapping`), missing/changed mapped remotes (without the matching
  `accept_observed`), unselected classification choices, unresolved-write history
  and any projection the verified contract cannot represent are explicit
  blockers, not altered work.
- Cross-midnight work keeps the origin interval identity and binds the origin
  revision on every per-allocation operation; an equal-text hint is evaluated
  against the allocation's own remote date only.
- Remote-only and orphaned entries are surfaced, never deleted.

The plan identity is a content hash of the previewed day, contributing source
revisions, exact operation content, a remote-content/coverage fingerprint AND the
exact readiness blockers. It excludes audit timestamps, so identical fresh
observations with unchanged decisions keep canonical content and the same
`plan_id`; an actual local or remote change, or a readiness decision, changes it.
A blocked preview and the ready preview that follows an explicit decision are
therefore distinct immutable records — persisting the ready one never reuses or
overwrites the blocked record under the same ID. Persisted plans are immutable and
distinguish `ready` from `blocked`. Every plan operation also carries the exact
deterministic audit `reason` an edit will use (`timesheet review update <date>
<local_id>`), as part of the hashed, approved content.

`approved` and `approval_available` in the RESULT are derived from the native
approval ledger, never a second persisted authority flag: `approved` is true only
while an `active`/`executing` approval binds this exact `plan_id`, and
`approval_available` mirrors `ready`. The persisted plan record carries neither
flag; a state file that does is rejected at load (`state_invalid`).

## Approval and verified submission

The approval ledger (`approvals`, `intents`, `executions` state sections) is the
only authorization authority. Approval IDs are unique occurrences (`apr-…`),
never plan aliases: reapproving identical content after a resolved failure
creates a NEW approval ID and never reactivates or overwrites the old record.
Historical consumed/revoked approvals are immutable audit.

- An approval binds the exact plan content hash, the contributing source
  revisions, the operation digests, and a context digest of the effective
  endpoint/credential configuration (hash-only; raw values are never stored or
  printed). Any effective endpoint/credential change invalidates the approval
  even if the provider returns identical data.
- Approval is gated on fresh proof at approval time: the exact plan identity
  (any drift fails `stale_plan`), complete readable remote coverage
  (`plan_not_ready`), and the currently authoritative catalog still proving every
  selected classification with unchanged relevant content — the option's
  id/code/name/activity-type identity is bound into the preview, the approval
  and the plan identity (`classification_unverified` for non-proof,
  `classification_choice_changed` when the selected option's relevant content
  changed, `classification_choice_unverifiable` when the bound choice cannot be
  established). A relevant change forces explicit reselection and a new
  preview/approval; unrelated catalog rows, queries and timestamps never
  invalidate an unchanged choice. The approval summary records each bound
  choice (`choices`), and every plan operation carries `selected_choice`.
  A zero-operation plan has a legitimate EMPTY proof, so `choices` is encoded
  explicitly as `{}` (never omitted) and reloads as a non-nil empty map; a
  legacy dev.10/dev.11 row whose proof is absent or null reloads as nil, is
  never reconstructed or backfilled as approved, and still fails closed with
  `stale_approval`. Non-empty proofs recorded by dev.11 remain valid and are
  still revalidated against the current catalog.
- Preview, approval and consumption all guard open sessions by saved AND staged
  allocation footprint (dev.11/dev.12): any open session whose saved or unsaved
  (staged/unsaved-confirmed) allocations touch the plan's affected allocation
  dates — including prior-day spill drafts, moved or removed contributions and
  unsaved confirmations — blocks it with zero remote calls until it is saved or
  cancelled. Unrelated dirty dates never block. The DURABLE saved footprint of
  that dirty origin is included as well: an unsaved confirmation that removes
  the final interval (or removes a prior-day spill) empties the session lists
  while the plan still renders the saved work, and it must never authorize that
  stale saved view. Each view is resolved with its own timezone, and the guard
  never persists a draft.
- Approval never dispatches. A persisted unconsumed approval can only be used by
  a new explicit `submit_plan` that passes EVERY current precondition.
- `revoke_approval` explicitly revokes one unconsumed approval; a later
  `submit_plan` is blocked before dispatch with zero remote calls. Revoking an
  executed/unknown approval fails (`stale_approval` / `unknown_approval`); a
  revoked approval can never be revoked again or reactivated.

`submit_plan{day, approval_id}` revalidates everything fresh, and on success runs
the whole batch in one bounded run:

1. Consumption re-renders the plan (fresh observations, dirty-session and
   readiness gates) and requires the identical plan identity; a changed
   precondition is `stale_plan` before any call.
2. A fresh classification proof against the CURRENT catalog gates dispatch and
   must still match the choice the approval bound
   (`classification_unverified` / `classification_choice_changed` /
   `classification_choice_unverifiable` block before any dispatch).
3. Each operation's durable `in_flight` intent (unique `wr-…` reference, exact
   dispatched body digest, operation context) is committed BEFORE dispatch; the
   first committing transition also marks the approval `executing` and appends
   the execution record.
4. One dispatch per operation — a write tool is NEVER retried, whatever the
   failure, and no generic call path exists (only `timesheet__log_time`,
   `timesheet__edit_time`, plus the read tools).
5. A write is `confirmed` only when the tool acknowledgment identifies an entry
   AND a fresh, complete read-back matches the exact intended content
   (date/start/end/classification/description). For an edit the acknowledged
   entry id must ALSO equal the approved bound remote identity: a different
   acknowledged id is `edit_identity_mismatch`, stays unresolved with both ids
   as distinct audit, and never updates the binding or its `LastObserved`. A
   tool error that acknowledges nothing is never proof of absence. Confirmed
   state, the durable observation and the binding are persisted in ONE
   transition per operation.
6. Before each later dispatch the run re-checks the batch against the FULL
   content of the consumption baseline advanced only by verified self-effects:
   an observed change to an existing entry — including a previously confirmed
   self entry — stops the batch explicitly (`external_remote_drift`), and no
   operation is ever silently skipped.
7. A zero-operation plan consumes the approval without dispatching anything.
   Its single persistence transition is reported honestly: a clean commit is
   `all_confirmed`; a post-rename failure keeps the existing sticky
   `durability_uncertain` behavior; a failure BEFORE activation is
   `persistence_failed` — no consumption and no execution are durable, no
   remote call is made, the approval stays `active`, and the same approval must
   be submitted again explicitly after the filesystem is available. Nothing is
   replayed automatically.

Receipt statuses: `all_confirmed`; `partial_stopped` (verified prefix, explicit
stop, honest per-reference outcomes); `blocked_before_dispatch` (zero remote
calls); `durability_uncertain` (only when local persistence is itself
unconfirmed; nothing is claimed, and a restart is required);
`persistence_failed` (a zero-operation consumption whose pre-activation local
commit failed: nothing was consumed, no execution is durable, the approval
remains reusable by a new explicit submit, and no remote call was made). The
receipt carries
per-reference outcome rows (`confirmed`, `uncertain`, `unidentified`,
`in_flight`, `stopped_unattempted`), blocker codes, a bounded call inventory, and
`next_action`. Acknowledgment-shape failures never confirm a write: a response
whose entry id cannot be decoded is `write_unidentified`, and an ack/read-back
content mismatch is `write_content_mismatch` / `write_content_unverified` — all
remain resolving blockers.

Recovery is explicit and human-driven. `submission_status{day}` lists every
unresolved write reference — native intents (`wr-…`) and migrated legacy outcome
rows (`mw-<legacy operation key>`; rows without a stable key get a stable
content-derived reference) — scoped by the date's affected allocations: a row is
listed when its recorded write date, or an allocation its local interval
timezone-resolves, touches the requested date; a row with unknown provenance stays
listed for every date. Every unresolved reference blocks every new approval and
submission whose plan touches the affected dates (dev.11 scopes readiness by
affected allocation dates as well as contributing origins, so a pending
spill-day outcome recorded with its origin's date still blocks an empty spill-day
plan). `resolve_write` requires the CURRENT COMPLETE observation for the date
(`stale_observation` otherwise):

- `adopt` with the explicitly selected observed `remote_id` binds the outcome's
  local allocation to that exact entry: the local identity must be confirmed
  durable work whose timezone-resolved allocation covers the recorded write
  date with the intended representable span, backed by the claimed migration
  crosswalk when one is named, and one-to-one checks apply. Contradictions,
  wrong allocation dates, missing local contributions and unknown crosswalks
  fail `identity_conflict` / `unknown_remote` / `unknown_local` with the
  blocker staying visible.
- `not_applied` records the human attestation that the intended effect is absent;
  it is REFUSED when the current observation already shows a matching effect.
- Never a guess: migrated rows that lack usable intent information remain
  visible blockers rather than resolved facts.
- `resolve_write` is additive audit in every path: the original status, error
  code and content are preserved beside the resolution, decision, resolution time
  and the exact observation identity used. Resolution never dispatches anything.

Schema-2 migration feeds the same gate: legacy unresolved writes are preserved as
migrated blocker rows; legacy rows that are already confirmed/failed history are
not pending. A schema-2 file's obsolete always-false plan authorization flags are
stripped on conversion, and a schema-2 file containing `approvals`, `intents`,
`executions` sections or any persisted `approved`/`approval_available` flag that
is non-boolean or true is rejected with `state_invalid`.

## Configuration


Non-secret JSON only. Unknown fields and invalid values are rejected without
echoing file contents.

```json
{
  "timezone": "Europe/Amsterdam",
  "state_path": "/Users/you/.timesheet/state.json",
  "required_env": ["GITHUB_TOKEN", "GCAL_ICS_URL"],
  "sources": {
    "enabled": true,
    "github": {
      "enabled": true,
      "user": "apilot",
      "repositories": ["your/repo", "org/other-repo"],
      "api_base": "https://api.github.com"
    },
    "calendar": {
      "enabled": true,
      "user": "apilot@example.com"
    }
  },
  "migration": {
    "enabled": true,
    "legacy_root": "/Users/you/records",
    "legacy_timezone": "Europe/Amsterdam",
    "classification_map": {"legacy-classification-value": "DEV"}
  },
  "gradion": {
    "enabled": true,
    "endpoint": "https://mcp.example.invalid/mcp",
    "token_env": "GRADION_MCP_TOKEN"
  },
  "work_hours": {
    "days": ["mon", "tue", "wed", "thu", "fri"],
    "windows": [
      {"start": "09:00", "end": "12:00"},
      {"start": "13:00", "end": "18:00"}
    ]
  }
}
```

| Field | Required | Meaning |
| --- | --- | --- |
| `timezone` | yes | IANA timezone for newly opened day records. Saved days keep their recorded timezone. |
| `state_path` | yes | Private local state file. Parent directories are created `0700`. |
| `projects` | retired | Still loads, matched without regard to case like every key ([ADR 0012](../adr/0012-retire-the-local-project.md)). Every command except `init`, including `status`, `plan` and the `daily` timer, removes it from the file on its first load: the original goes to `<config>.bak` (or the next free `.bak.N`), and one stderr line names it (under the timer, in the service journal). A file it cannot rewrite safely (read-only, a symlink, other hard links) keeps it, ignored, with one stderr line. `init` never writes it. |
| `required_env` | no | Credential variable **names** for collection. R1-era local operations still do not require them. |
| `sources` | no | Optional collection source config. Omitting `sources` entirely reproduces exact R1 behavior (`no_sources` collection, no gate). |
| `sources.enabled` | yes if present | Master switch; when false, collection reports per-source `disabled` and no gate applies. |
| `sources.github.user` | if enabled | GitHub login used for attribution and filters. `[a-zA-Z0-9-]{1,39}`. |
| `sources.github.repositories` | no | `owner/name` allow list; empty means all repos the search regardless of repository. |
| `sources.github.api_base` | no | Defaults to `https://api.github.com`; normalized service base (kept verbatim after scheme/host). |
| `sources.calendar.user` | no | Attendee address used for attendance PARTSTAT interpretation. The private ICS feed URL itself is supplied only as a credential variable value (e.g. `GCAL_ICS_URL` via `required_env`) — never stored in configuration or state. |
| `sources.calendar.user` | no | Attendee address used for attendance PARTSTAT interpretation. |
| `migration` | no | Disabled-by-default explicit legacy migration provision. Omitted or `enabled: false` means no legacy file is read. |
| `migration.legacy_root` | if enabled | Records root containing the authoritative `workflow/YYYY-MM-DD.json` files. Read-only; never created or written. |
| `migration.legacy_timezone` | if enabled | IANA timezone that governed the legacy records. Recorded on migrated days; never replaced by the current `timezone`. |
| `migration.classification_map` | no | Explicit legacy-value translation. A value not directly representable and not mapped is a visible conflict, never an invented replacement. |
| `migration.project_map` | retired | Removed or ignored like `projects`. A migrated interval carries no project; the receipt's legacy interval audit keeps the original value. |
| `gradion` | no | Disabled-by-default remote boundary. Reads verify capability-only; writes exist only behind the approval ledger. |
| `gradion.endpoint` | if enabled | Operator-configured MCP endpoint. Never echoed into a response, receipt, error or state document. |
| `gradion.token_env` | no | Credential variable **name**, default `GRADION_MCP_TOKEN`. Only the binary resolves its value. |
| `work_hours` | no | Optional, presentation-only main-work-time policy for the draft annotation. Omitted means no annotation and byte-identical output. |
| `work_hours.days` | if present | Non-empty list of exactly `mon`, `tue`, `wed`, `thu`, `fri`, `sat`, `sun` (lowercase, three letters; nothing is normalized). |
| `work_hours.windows` | if present | One to four same-day `HH:MM` windows, each with `start` before `end`, none overlapping. |

`status` reports, under `environment` (the retired `check_environment` op), the `required`, `missing`, and `present` **names** only.
It performs no network call. An inherited empty value overrides a file value, so
that name is reported missing; a file-only value is reported present. A `save`
never requires a remote credential.

R1 has no required or optional credential variable names of its own. The
`required_env` list is an owner-declared, name-only reservation for later named
slices; it never causes R1 local operations to fail or reach the network.

### Work-hours annotation (optional, non-blocking)

`work_hours` is an optional, presentation-only policy naming the team's main
work time. It never rejects, warns, blocks, or invents a block, and a
configuration without it renders exactly as before.

Validation is offline and fail-closed with `config_invalid`, naming the
offending value: `days` must be non-empty and use exactly the lowercase
three-letter tokens `mon`…`sun` (nothing is normalised silently); `windows`
must be non-empty and at most four; each `start`/`end` must be 24-hour `HH:MM`
with `start` before `end`; and no two windows may overlap. An unknown key
inside `work_hours` (for example `max_block_minutes` or `outside_allowed`) is
rejected by the strict decoder. The four-hour per-entry bound is the remote's
fixed policy, not a `work_hours` setting, and cannot be raised through
configuration.

`run` then annotates each block against the day's windows in the
day's recorded timezone. Every absolute provider instant — meeting anchors and
evidence-topic anchors/items, which arrive as RFC3339 in the provider's offset —
is resolved into the recorded day's timezone before a clock is derived (a
`07:00Z` anchor on an `Asia/Ho_Chi_Minh` day reads `14:00`), so the block a
reviewer reads, the topic-to-block attachment, the annotation and the recorded
entry all agree. A block that is not entirely inside one window gains
`outside_work_hours: true` and `work_hours_reason`, one of `day_not_in_policy`
(weekday not listed), `before_window`, `between_windows` (a gap such as lunch),
`after_window`, or `partial` (partly inside and partly outside a window, for
example a block spanning the lunch gap). A block entirely inside one window
carries neither field, and a block with no usable same-day start/end is left
untouched. The draft also echoes the policy it applied
(`work_hours`, the same `days`/`windows` shape) and omits that key entirely when
no policy is configured, so every reason token can be checked against the policy
that produced it. Work outside main hours is allowed with no ceiling: the
annotation changes no span, duration, or recorded entry.

## Environment file and precedence

`--env-file` points at a local file (normally `.env.local`). It is read by the
compiled binary as data. No model or agent harness may read it.

Supported syntax:

- blank lines and lines whose first non-space character is `#` are ignored;
- `KEY=VALUE`; keys match `[A-Za-z_][A-Za-z0-9_]*`;
- unquoted values are trimmed and end at a whitespace-prefixed `#` comment;
- single-quoted values are literal;
- double-quoted values support `\\`, `\"`, `\n`, and `\t`; other escapes fail.

Not supported: `export` prefixes, command substitution, variable expansion,
sourcing, or undocumented dotenv features. Invalid syntax reports only a line
number and a reason; it never echoes the raw line or value.

Inherited process environment values override file values for the same name,
including explicitly empty values. Validation is lazy: missing required values
are reported by `status` under `environment` (or an operation that needs them), not
by local review. Diagnostics and errors never contain credential values, raw
dotenv lines, authenticated URLs, or a full config dump.

## Error codes

| Code | Meaning |
| --- | --- |
| `bad_request` | Malformed envelope, missing/extra field, or invalid request ID. |
| `unknown_op` | Operation is not part of this build. |
| `invalid_date` | Date is not a real `YYYY-MM-DD` calendar date. |
| `invalid_grammar` | Typed interval text does not match the documented grammar. |
| `invalid_interval` | Zero/negative duration, invalid time, empty description, or bad evidence reference. |
| `nonexistent_time` | The local start/end wall time is skipped by a timezone transition. |
| `ambiguous_time` | The local start/end wall time occurs twice; this build does not choose an occurrence. |
| `overlap` | Intervals collide in absolute time and require human resolution. |
| `unknown_day` | Day was not opened in this session. |
| `unknown_interval` | Interval ID is not in the staged draft. |
| `stale_revision` | The request's expected revision is not current. |
| `nothing_to_confirm` | Confirmation requested with no draft and no previously confirmed work. |
| `source_unacknowledged` | An incomplete collection attempt on this day is not explicitly acknowledged; `confirm`/`save` fail closed. A never-collected enabled source fails the same gate with the unattempted-source message. |
| `unknown_meeting` | `meeting_id` does not match any proposed meeting for the day. |
| `stale_collection` | The request's `collection_id` does not match this day's recorded attempt; collection changed since the reviewed attempt and no mutation was made (`acknowledge_source` and `decide_meeting`). |
| `store_locked` | Another process owns the single-writer state lock. |
| `state_invalid` | Local state is unreadable, has an unsupported schema, or contains work invalid in its recorded timezone. |
| `write_failed` | Local save failed before activation; previous durable state remains. |
| `write_uncertain` | Local save may have activated a new file but directory durability is unconfirmed; restart before saving again. A dispatched-but-unverifiable remote write attempt stores the same uncertainty durably. |
| `config_invalid` | Configuration is missing, unreadable, or invalid. |
| `dotenv_invalid` | Environment file syntax or parsing failed. |
| `migration_disabled` | Explicit legacy migration is not enabled in configuration. |
| `migration_invalid` | The date selection is missing, invalid, or above the documented bound. |
| `migration_stale` | The preview identity, source bytes, or target state changed; nothing was activated. |
| `migration_conflict` | The candidate has conflicts (overlap, occupied destination, malformed identity, changed source); nothing was activated. |
| `session_open` | Migration activation was requested while a day is open; activation requires a fresh process with no open day. |
| `remote_disabled` | The read-only remote boundary is not enabled in configuration. |
| `remote_unavailable` | A required read tool or the named credential is missing. Carries names/codes only. |
| `remote_incomplete` | The observation does not provide complete coverage for the requested date. |
| `remote_unreadable` | The read failed or returned an unrecognized shape; this is not an empty timesheet. |
| `stale_observation` | The observation identity is not the current successful observation for that date. |
| `unknown_remote` | The remote ID is not present in the current observation. |
| `unknown_local` | The local interval is not confirmed durable work that resolves an allocation contributing to the recorded date (resolution requires the actual allocation identity). |
| `identity_conflict` | The resolution would break one-to-one identity or substitute by time slot. |
| `unknown_classification` | The option was not returned by the named catalog; a partial catalog cannot prove it absent. |
| `stale_catalog` | The catalog identity is not stored; fetch the catalog again. |
| `no_confirmed_work` | There is no saved confirmed work that renders on the requested date. |
| `dirty_session` | An open session's saved or staged allocation footprint touches the plan's affected dates (unsaved changes); save it, or let the process holding the draft exit, before previewing a plan. |
| `stale_plan` | The current rendered plan differs from the plan the request/approval binds (`approve_plan`, `submit_plan`); zero remote mutations follow. |
| `plan_not_ready` | The plan is blocked or its remote coverage is not complete and readable; approval is refused. |
| `classification_unverified` | The current authoritative catalog does not prove a selected classification exists; dispatch is refused. |
| `classification_choice_changed` | The selected classification's relevant content (id/code/name/activity type) changed since the bound choice; explicit reselection and a new preview/approval are required. |
| `classification_choice_unverifiable` | The selected classification's bound choice cannot be established from the referenced catalog or fresh results; explicit reselection and a new preview are required. |
| `unknown_approval` | `approval_id` does not name a native approval for the date. |
| `stale_approval` | The approval is revoked, already executing/spent, or the context changed; it can never be consumed again. |
| `unknown_write` | `write_ref` does not name an addressable unresolved write. |
| `internal` | Unexpected failure with no safe detail. |

## Security boundaries and R4 limits

- stdout carries only each command's JSON result; diagnostics stay on stderr.
- Requests cannot supply paths, executable names, shell commands, or arbitrary
  dispatch.
- No shell is launched. Network access is collection/submission-only and goes
  exclusively to the configured GitHub API base, the credential-held ICS feed
  URL, and the configured MCP endpoint, over HTTPS unless an
  explicitly configured local endpoint targets a local service. Credentials are
  read only
  through the documented dotenv/process precedence and are never logged, echoed,
  or included in receipts, state or errors. Collection/remote receipts and error
  details carry stable codes, HTTP status and bound counts only; raw provider
  documents and authenticated URLs are never echoed into the protocol.
- Migration reads only configured legacy files. It never writes into the legacy
  root, never discovers dates recursively, and never falls back to the old store
  constructor. Raw legacy collected-provider documents are not imported.
- An incomplete-source gate binds `confirm` and `save` to explicit human
  acknowledgment of every incomplete collection attempt; a new attempt always
  requires a fresh acknowledgment.
- Evidence is verify-only: collection never creates, edits, or extends worked
  minutes. Meeting decisions and attendance flags are not minutes.
- The remote boundary verifies exactly FOUR tools: the two read tools plus the
  two write tools (`timesheet__log_time`, `timesheet__edit_time`), which exist
  only behind explicit submit paths. There is no delete, no generic call, and no
  arbitrary dispatch; a write is at most ONE dispatch per operation, never
  retried.
- Approval is the only remote-write authority, it is ledger state (never a plan
  flag or a UI flag), binds exactly one rendered plan, and never auto-fires. A
  write is confirmed only after an identified acknowledgment AND a fresh complete
  read-back matching the intended content; unverifiable writes are durable
  unresolved blockers resolved only by explicit `resolve_write` decisions from a
  human against the current observation. Preserved legacy approvals remain inert
  (`usable: false`) and cannot authorize anything; a state file claiming approval
  authority fails closed.
- State schema is `4`. A schema-1 file loads unchanged and is upgraded in memory;
  a schema-2 file is converted in memory (never by startup activation) after its
  own validation; a schema-3 file is upgraded in memory with a pending
  `migration.schema_upgrades` receipt; schema `4` is written only by a
  successful durable save/transaction. A schema-1-only loader rejects a later-schema file with
  `state_invalid` and leaves it intact. A pre-R2 build rejects an R2 evidence
  file the same way.
- Historical note (superseded by R6): when R4 was reviewed, the then-production
  CLI (`cmd/timesheet`) and the legacy packages were unchanged and the
  development entry point was `cmd/timesheet`. R6 removed the legacy CLI
  and that temporary entry point; `cmd/timesheet` now carries only the
  `version` / `serve` surface. The `serve` protocol in this document is the
  authoritative interface.
- Later slices must add Pi integration. R4's native approval/submission path is
  the implemented one; legacy approval rows are inert audit, never approvals.

## Historical: the retired `serve` protocol

> **Historical.** `serve` retired in issue #272 and nothing speaks this JSONL
> protocol to the binary any more. The envelope and op table are kept as a
> record; most ops still run in-process, through `Session.HandleLine`, for
> `tui` and the commands, and the rows below are updated where the retirement
> changed an op. The commands and their sections above are authoritative.

### Protocol envelope

Request fields are strict. Unknown fields, missing `id`/`op`, trailing JSON, and
fields not allowed for the selected operation are rejected.

```json
{"id":"1","op":"open_day","day":"2026-10-22"}
```

Responses always echo the request ID and carry either `result` or `error`:

```json
{"id":"1","ok":true,"result":{...}}
{"id":"1","ok":false,"error":{"code":"overlap","message":"..."}}
```

If the request cannot be decoded, the response `id` is empty. IDs must be
non-empty, at most 128 bytes, and contain no control characters.

### Operations

| `op` | Fields | Effect |
| --- | --- | --- |
| `open_day` | `day` | Loads or creates the local day record and starts a staged draft. |
| `add_interval` | `day`, `revision`, `text`, optional `evidence_refs` | Parses one typed interval and stages it. |
| `edit_interval` | `day`, `revision`, `interval_id`, `text` | Replaces a staged interval. Evidence references are preserved. |
| `remove_interval` | `day`, `revision`, `interval_id` | Removes a staged interval. |
| `review` | `day` | Returns confirmed and staged segments, totals, cross-midnight allocation, and the `evidence` review object (topics, proposed meetings with decisions, per-source coverage). |
| `confirm` | `day`, `revision` | Promotes the staged draft to a new confirmed revision in memory. Fails `source_unacknowledged` while an incomplete collection attempt on this day is unacknowledged. |
| `save` | `day`, `revision` | Persists confirmed work locally (same gate as `confirm`). Staged changes are not saved. |
| `collect` | `day` | Runs all enabled source collectors for that day's local window (verify-only, no minutes). Receipts carry per-source status/counts. |
| `acknowledge_source` | `day`, `source`, `collection_id` | Explicit human acknowledgment, verified against the exact reviewed collection attempt ID. Missing or stale IDs fail (`bad_request` / `stale_collection`). |
| `decide_meeting` | `day`, `meeting_id`, `decision`, `collection_id` | Records `attended`/`skipped`/`pending` for one proposed meeting. The meeting's provider identity must be present in this day's evidence (`unknown_meeting` otherwise). `collection_id` is required and must match the day's calendar attempt (`bad_request` when missing, `stale_collection` on mismatch); the action is rejected without mutation otherwise. Decisions carry across recollections matched by `UID|Start`. |
| `migration_preview` | `dates` | Validates one explicit legacy date selection without activating anything. Returns bounded counts, conflicts, source hashes and a `migration_id` bound to the source bytes, dates/timezone and current durable target state. |
| `migration_apply` | `dates`, `migration_id` | Stateless since #272: rebuilds the candidate for `dates` and activates it atomically only when the rebuilt `migration_id` matches. Requires that no day is open. The fresh candidate is validated (source identity, durable target, conflicts) before any successful `no_change` return; a repeated identical source is `no_change`, and stale or conflicting input is rejected without mutation. |
| `classifications` | `query` (optional) | One bounded authoritative classification read. Returns a stored `catalog_id`, options and an explicit `complete` flag. A filtered or truncated catalog cannot prove an option absent. |
| `select_classification` | `day`, `revision`, `interval_id`, `catalog_id`, `classification_id` | Records an explicit reviewed remote classification choice for an option the catalog returned, in staged work. Follows confirm/save semantics. |
| `reconcile_remote` | `day` | One fresh read-only observation of the day. Persists the observation and reports mapped, changed, missing and orphaned identities, unbound remotes, overlap/text candidates, unresolved-write history and blockers. A read never binds an identity. |
| `resolve_remote` | `day`, `revision`, `observation_id`, `remote_id`, `local_id` (omitted for `foreign`), `decision` | Records one explicit identity/content decision (`adopt`, `accept_observed`, `foreign`) against the current observation. Rejects stale revision/observation IDs and identity conflicts without mutation. |
| `preview_plan` | `day` | Renders exact create/edit operations and blockers from saved confirmed durable work, explicit classification choices and a fresh read-only observation. Never plans from an unsaved day, never saves a draft. Reports `approved`/`approval_available` as DERIVED ledger facts (never persisted authorities). |
| `approve_plan` | `day`, `plan_id` | Creates one NEW native approval occurrence (a unique `approval_id`) bound to the exact current rendered plan identity, after fresh precondition proof. Creates no remote write and never auto-submits. |
| `revoke_approval` | `day`, `approval_id` | Explicitly revokes one unconsumed approval. Revocation is immutable audit. |
| `submit_plan` | `day`, `approval_id` | Revalidates every precondition fresh, then executes the approved batch: one durable pre-write intent per operation, one dispatch, one verification read-back per write. Returns a bounded receipt; never retries a dispatched write. |
| `submission_status` | `day` | Lists this date's approval ledger, execution audit and every unresolved write reference (native `wr-…` and migrated `mw-…`). Unresolved references block all further writing. |
| `resolve_write` | `write_ref`, `observation_id`, `decision` (`adopt` requires `remote_id`) | Explicitly resolves ONE unresolved write reference against the CURRENT COMPLETE observation: `adopt` binds the observed remote ID (one-to-one checks apply), `not_applied` records a human attestation that the intended effect is absent (refused when a matching effect is visible). Never dispatches. |
| `cancel` | — | Removed with `serve` (#272): staged edits end with the process that made them. |
| `status` | none | Reports loaded days, `app_version` (the protocol schema retired with `serve`), `store_schema`/`durable_schema`, `durability_uncertain`, and compact migration/binding/unresolved-write/plan/approval counts (`approvals`, `native_unresolved_writes`). |
| `check_environment` | none | Reports required variable **names** and which are missing; reached through `timesheet status` (`environment`) since #272. |
| `close` | none | Ends the in-process session; under `serve` it also ended the process. Unsaved state is not written. |

A day must be opened before its other operations. `revision` is required for
every mutation and for `save`; a mismatch fails with `stale_revision`.

## R5 — first pinned Pi consumer (`0.1.0-dev.14` → `0.1.0-dev.16`, historical stage)

R5 does not change the `serve` protocol, schemas, or any Go behavior except the
`AppVersion` bump. It adds the first protocol consumer: a restricted Pi
launcher/adapter under `pi/`, pinned to `@earendil-works/pi-coding-agent`
`1.4.0` / `pi-tui` `1.4.0` / `typebox` `1.3.27`, Node `>=22.19.0`.

Launcher (`pi/launch.mjs`, the only daily entry point):

- Spawns the pinned Pi CLI with `spawn` (`shell: false`), absolute trusted
  paths only, and a fixed isolation set: isolated working directory, dedicated
  profile directory (`PI_CODING_AGENT_DIR`), `--no-extensions` plus the single
  trusted adapter path, `--no-context-files`, `--no-skills`,
  `--no-prompt-templates`, `--no-mcp`, `--no-builtin-tools`,
  `--tools timesheet_prepare`, `--no-session`, `PI_OFFLINE=1`,
  `PI_SKIP_VERSION_CHECK=1`, `PI_TELEMETRY=0`. No free-form Pi arguments pass
  through.
- Provisioning inputs are operator-only and path-shaped: `--binary`,
  `--config`, `--env-file`, `--profile-dir`, `--workdir`, the optional
  dedicated-profile model selection (`--provider`, `--model`), and forwarded
  credential NAMES (`--forward-env`). The shipped launcher has no test
  provisioning flags; acceptance fixtures are provisioned externally in a
  generated dedicated profile. The launcher and
  adapter never read the config or dotenv contents; the child receives the
  paths through `TIMESHEET_CHILD_{BINARY,CONFIG,ENVFILE,WORKDIR}` and its
  environment is a whitelist (required OS variables plus explicitly named
  credential variables, values preserved verbatim). Chat-provider secrets are
  never forwarded wholesale and never logged.

Adapter (`pi/timesheet.ts` + `pi/transport.ts` + `pi/view.ts`):

- One session-scoped compiled child per Pi session. The transport is
  serialized single-flight JSONL with bounded frames/queue, exact id
  correlation, idempotent shutdown, and a fail-closed identity negotiation
  against this document's identity fields; any mismatch, malformed frame,
  extra id or EOF fails the transport with no automatic respawn or replay.
  Readiness means identity negotiation FINISHED: concurrent callers share one
  startup, and no ordinary operation is dispatched before it passes. Every
  active or queued caller settles exactly once on failure, cancellation,
  shutdown or replacement; a failure after dispatch is reported as an unknown
  outcome, never as a proven no-op. Incoming frames are decoded from buffered
  raw bytes with a strict UTF-8 decoder (invalid bytes fail closed; no silent
  replacement text), so a UTF-8 character split across pipe chunks is still
  preserved. The byte bound applies to each complete RAW frame before decoding
  or dispatch and to an unterminated remainder, so a newline, trailing
  whitespace, chunk alignment or a pause cannot exempt an oversized frame.
  Envelope structure (`id`, strictly boolean `ok`, a well-formed error object)
  and the minimal status identity (`app_version`, `protocol_schema`,
  `store_schema`) are validated before use: malformed negotiation fails the
  transport, stops the child, settles every startup/pending/queued caller and
  requires an explicit reopen; it never spawns a second child implicitly and
  never fabricates a child refusal. `write()` backpressure is not treated as a
  rejected write. Frames and callbacks from a replaced child are ignored by
  generation. Reopening after interruption is an explicit human action;
  unresolved writes stay unresolved.
- Consumer boundary corrections (dev.14): evidence `topics`/`meetings`/
  `sources` may be JSON `null` on a valid R1-only/empty day and render as
  empty; interval editing prefills from the canonical interval (a
  `23:00-01:00` interval stays `23:00-01:00`, a midnight-ending interval is
  `23:00-00:00`, never the `24:00` display label) and sends the origin day,
  interval id and current origin revision together after a locked-spill
  navigation; action availability follows dirty/revision state rather than
  nonempty segment lists (`Cancel` survives a final removal, durable `Save`
  survives a confirmed empty draft); `accept_observed` offers mapped and
  changed entries and sends the actual `remote_id` plus `local_id`,
  observation and revision; address-bound `resolve_write` adoption can select
  an already-bound observed identity (Go still enforces content and one-to-one
  checks); submission receipts consume the child's per-reference `status`,
  distinguish `persistence_failed` (non-consumption) from
  `durability_uncertain`, and never present a transport failure as "zero
  remote mutations".
- Consumer boundary corrections (dev.15): the submit target is the exact active
  approval for the CURRENT day and exact plan, selected by one bounded
  `submission_status` read scoped to that day; the previous unkeyed
  `lastApprovalId` cache is removed, so an approved day A cannot name its
  approval in an already-approved day B's dialog after switching days without a
  restart. Consumed, revoked and changed-plan approvals are not selectable, and
  the displayed approval id is the id sent. Only a validated Go error envelope
  receives the `from_child` refusal tag; malformed wire data is a transport
  failure. Detail paging measures the display-cell/grapheme-aware physical rows
  produced by the pinned Pi wrapper at the real content width and subtracts the
  pinned Pi rows reserved outside the custom editor slot, so long
  ASCII/CJK/emoji/combining rows stay reachable at narrow widths on both the
  edit picker and the plan/detail surface, with no forced minimum window and no
  modification or truncation of stored work.
- Every operator capability is local-only (`/timesheet` forms): typed interval
  authoring including cross-midnight locked spill plus origin-day navigation,
  confirmation vs durable save, attendance decisions, attempt-bound source
  acknowledgment, classification search/select against the bounded catalog,
  read-only reconciliation with explicit `adopt` / `accept_observed` /
  `foreign` decisions, exact plan preview → display-before-ask approval →
  one-shot submission with honest receipts, submission status with explicit
  address-bound `resolve_write` recovery, and migration preview/apply. None of
  these are registered as model tools — not hidden, not deferred.
- The model sees exactly one tool, `timesheet_prepare`, taking one civil date.
  It drives `open_day` → `collect` → `review` for the human's local UI and
  returns a ≤512-byte bounded status with only opaque local ids
  (`collection_id`), ending the turn. Defense in depth: the adapter checks
  `pi.getActiveTools()` at `session_start` and calls `ctx.shutdown()` on any
  mismatch, blocks and terminates on any other `tool_call`, and refuses all
  user `!`/`!!` shell dispatch from the `user_bash` gate without executing.
  Child errors reach the model only as stable codes, sanitized.
- The adapter keeps no accounting of its own; the compiled child remains
  authoritative. Non-TUI extension modes fail safely instead of auto-answering
  forms. Untrusted provider/record text is rendered control-free: the child's
  sanitization strips controls at collection time and the adapter strips
  remaining escape sequences before rendering.
- Acceptance evidence for `0.1.0-dev.14` (real pinned Pi TUI over tmux, the
  compiled Go child, synthetic loopback ICS/MCP/model fixtures, generated
  dedicated profiles; no real credentials, live accounts or production
  writes): controlled-child and real-child probes prove one shared startup,
  fail-closed identity/extra-id/EOF handling, exact UTF-8 across split chunks,
  `write(false)` backpressure, stale-child isolation, pending/queued
  settlement and queued cancellation. The real TUI exercised manual-only and
  configured-but-empty evidence days, final removal/cancel/confirm-empty/save
  and restart, canonical 23:00-01:00 and 23:00-00:00 edits, spill-to-origin
  removal without `stale_revision`, mapped and changed `accept_observed`,
  `foreign`, a stale-observation refusal, and a bound-edit lost-ack recovery
  that resolved against the already-bound observed identity with zero
  additional provider mutations. A real create and a real edit produced
  explicit approval, one-shot submission, `all_confirmed` and
  `partial_stopped` per-reference statuses; a zero-op approval survived
  restart and a commit-boundary fault barrier produced `persistence_failed`
  with no consumption, followed by an explicit `all_confirmed` retry.
  Migration imported nonempty legacy work and a repeat was `no_change` with
  legacy bytes unchanged and legacy plans/approvals inert. Direct workflows
  made zero model requests; one natural-language initiation made exactly one
  request whose only declared tool was `timesheet_prepare`, with no follow-up,
  and no config/env/credential/record content appeared in a model-visible
  payload (only the pinned Pi documentation path and the isolated cwd). macOS,
  live accounts and authenticated subscription readiness remain unverified.

- Acceptance evidence for `0.1.0-dev.15` (second R5 repair; Linux, real pinned
  Pi 1.4.0 TUI over tmux, compiled dev.15 child, generated loopback MCP/provider;
  no real credentials, live accounts or production writes): controlled protocol
  children prove that a null/missing status identity and a malformed envelope
  (non-boolean `ok`, malformed error object) fail the transport, stop the child,
  settle concurrent startup plus pending/queued callers, never implicitly spawn
  a second child and reopen exactly one clean child; a valid child refusal is
  preserved while malformed wire data is never fabricated as one. Each complete
  oversized frame (including the reported "exactly limit, pause, remainder plus
  newline" shape), an unterminated oversized remainder and an at-limit frame
  with trailing whitespace are rejected before decode/stop/settle, while a frame
  exactly at the byte bound, split multi-byte UTF-8 and `write(false)`
  backpressure still succeed. In the real TUI with two seeded current zero-op
  plans/active approvals, approving A and then switching to already-approved B
  without restarting showed and sent B's approval; B's zero-op submission was
  `all_confirmed` with zero provider writes, a pre-submit cancel dispatched
  nothing, A's approval stayed active and untouched, a consumed approval was not
  selectable, and a same-day changed plan exposed only its own fresh approval
  (Go refused the old id as `stale_plan` with 0 calls). A saved
  `START-CJK` + 240 `界` + `END-CJK-ROW` row and a mixed
  ASCII/emoji/combining row were fully reachable in the actual edit picker and
  plan/detail surface at 40×24 and 60×26 and after restoring 100×36, with the
  paging label and controls usable and the durable store hash unchanged.

- Runtime replacement contract (`0.1.0-dev.16`, C1-L1). Pi 1.4.0's
  `AgentSession.reload` awaits `session_shutdown` handlers and then replaces
  the extension runtime; its interactive loop awaits each submitted
  `session.prompt` (command handlers included). The adapter therefore owns
  disposal as a generation boundary:
  - Factory-invocation state, not module state. A stale continuation cannot
    observe or mutate a replacement generation's transport, day, review,
    collection, guards or draft.
  - `session_shutdown` returns the transport shutdown so Pi joins the old child
    before the replacement's `session_start` spawns a new one. This is required
    because the child holds the state store's exclusive `flock`; a replacement
    child started earlier exits with `store_locked`, which the adapter reports
    as `transport_failed: child closed stdout (EOF)`. Shutdown stays bounded and
    never replays a dispatched operation.
  - Pending UI is generation-owned: signal-aware input/confirm dialogs are
    aborted, custom screens are closed through their `done` callback, and every
    awaited UI call also races a per-generation closed signal because the pinned
    `ui.custom()` and `ui.editor()` promises are not settled when Pi detaches
    their component. A disposed generation never notifies or opens a dialog, and
    the command handler unwinds instead of leaving Pi's input loop blocked.
  - A transient child startup failure is still explicit and requires an explicit
    Reopen; there is no automatic respawn, retry or replay.
  The diagnosed failing sequence on dev.15 was: a held collect was interrupted
  by `/reload`; its catch opened a recovery custom screen after Pi had already
  reset extension UI; reload's editor restore hid that screen without calling
  its callback; the old `/timesheet` handler never returned; Pi's input loop
  stayed blocked on it, so the visibly entered recovery command was queued in
  `pendingUserInputs` and never dispatched. Separately, the un-awaited shutdown
  left the old child holding the store lock while the replacement child started,
  producing the visible startup EOF. Both are fixed by the generation boundary;
  the recovery command now runs in the same Pi process.

- Acceptance evidence for `0.1.0-dev.16` (Linux, real pinned Pi 1.4.0 TUI via
  `pi/launch.mjs`, compiled dev.16 child, generated isolated profile/config and
  synthetic loopback ICS/MCP/model fixtures; no real credentials, accounts,
  profile or production writes): a real collect held at the loopback calendar
  across `/reload` now recovers through a visibly entered `/timesheet <date>`
  in the same Pi process, with the replacement child started only after the old
  child exited and exactly one fresh provider request (no replay); a pending
  classification read across reload followed by a different-date command opened
  that date's review with no stale catalog screen and usable controls; an
  approved exact plan was submitted with the loopback provider applying the
  write and withholding its acknowledgment, and after reload the replacement
  showed read-only submission status with the unresolved reference, refused a
  contradictory `not_applied` (`identity_conflict`) and resolved by adopting the
  observed entry with exactly one `log_time` call; `/new` during a pending model
  preparation left no orphan child and the replacement human workflow opened its
  own day with a fresh child; idle reload and quit-with-pending-work left no
  orphan. Cross-day approval selection and the UTF-8/byte-bound transport paths
  were not rerun (their source is unchanged from the dev.15 review);
  `durability_uncertain` UI remains source-reviewed plus accepted backend
  evidence, not a newly driven UI case. macOS, live accounts and authenticated
  subscription readiness remain unverified.

## R6 — single-product cutover (`0.1.0-dev.17`)

R6 changes no protocol, schema or store behavior. `AppVersion` moves to
`0.1.0-dev.17` in `internal/timesheet/model.go` and `pi/transport.ts`, and the
adapter package version stamp moves with it; dependency pins and lock
integrity are unchanged.

- One Go target: `cmd/timesheet` carries only `version` and
  `serve --config --env-file`, and `bin/timesheet` is the production-named
  binary the launcher receives via `--binary`. The temporary
  `cmd/timesheet` entry point and the legacy CLI subcommands
  (`preflight`, `review`, `preview`, `approve`, `submit`, `resolve`, `export`,
  `run`, `classify`, `metrics`, `config`) are retired; there is no alias or
  wrapper.
- The legacy package trees, their tests, `cmd/fetch-{git,calendar}`,
  `cmd/benchmark`, the old product skill (`SKILL.md`/`workflow.md` and its
  `.claude/skills/timesheet` symlink), `scripts/sync-vault.sh` and the tracked
  `scripts/vaultgen` source/test fixtures are retired. `go list ./...` is
  exactly `cmd/timesheet`, `internal/timesheet`, `internal/mcp`.
- `go.mod`/`go.sum` keep only dependencies still reachable from those
  packages; no dependency version, module directive or toolchain changed.
  `cmd/timesheet/timesheet.example.json` is now a minimal update-schema
  example with synthetic placeholders.
- The optional pinned Pi launcher and its adapter (`pi/`) retired in
  `0.2.0-dev.10`. The native `tui` verb is the operator surface, and `serve`
  remained as an internal command, the only route to legacy migration,
  `cancel`, `check_environment` and `revoke_approval`, until it retired in issue #272. The local daily flow, migration,
  reconciliation, approval, submission and recovery remain the current
  contract.
- Readiness is unchanged and honest: the dev.17 proof is Linux, hermetic and
  synthetic. macOS verification of the migration/local flow remains a mandatory
  separate gate for full R6 acceptance (the pinned-Pi leg retired with the
  launcher in `0.2.0-dev.10`), and live account/model readiness is a separate
  named authorization gate. No RC/stable/production claim follows from source
  review.
