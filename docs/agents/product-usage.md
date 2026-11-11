# Product usage: local and connected (remote) daily workflow

This guide includes historical CLI examples. Use the [README](../../README.md)
and [interface contract](timesheet-interface.md) for the current command surface.
The regular timesheet skill stops at a ready plan and always attempts local
metrics capture on terminal outcomes. The separate `/submit` workflow supports
explicit chat confirmation without terminal commands, as specified in
[ADR 0009](../adr/0009-chat-confirmed-submission.md). Historical terminal-only
submission statements below do not apply to that opt-in path.

This is the operator guide for the `timesheet` CLI and the `/timesheet` skill.
The first sections (build and setup, configuration, sources, running a day)
describe the current binary. Everything under
[Historical reference](#historical-reference-retired-cli) documents the retired
`run`/`review`/`preview`/`approve` CLI and is kept for context only.

## Build and setup

Any Go toolchain matching the `go` directive in `go.mod` builds the binary; the
terminal UI's Charm modules are pinned there and need no separate install.
Build once from your checkout and reuse the binary:

```bash
cd /path/to/pilot-timesheet
go build -trimpath -o bin/timesheet ./cmd/timesheet
bin/timesheet version
```

`go run ./cmd/timesheet` also works for a one-off command, but it recompiles on
every invocation; a multi-step session should use the built binary.

Create the home store once with `init` (full walkthrough, modes and recovery
rules: [first-run setup](../setup-linux.md)):

```bash
bin/timesheet init
```

Then `bin/timesheet log` runs the day (see [Running a day](#running-a-day)).

`init` creates `~/.timesheet/` (`0700`), a valid `~/.timesheet/config.json`
(`0600`) with your host timezone, an absolute `state_path` and
`required_env` (`GITHUB_TOKEN`, `GCAL_ICS_URL`), and
`~/.timesheet/.env.local` (`0600`) listing credential **names** as comments and
no values. It never overwrites an existing file. Every command then finds its
configuration as `--config` > `./timesheet.json` in the cwd >
`~/.timesheet/config.json`, and prints the chosen file on stderr. Records live
in the single state file named by `state_path`; there is no separate records
directory or `--storage-dir` flag.

Credential values go only in the env file, which you edit yourself
(`$EDITOR ~/.timesheet/.env.local`); only the compiled binary reads it. The
configuration holds variable names, never a value, and there are no Keychain
references. Never commit, paste or print a credential value or a private feed
URL, and do not reuse the owner's private configuration. See
[credentials](credentials.md).

## Configs and templates

| Starting point | Purpose |
| --- | --- |
| `timesheet init` | Writes a valid configuration with your `timezone`, an absolute `state_path` and `required_env: ["GITHUB_TOKEN", "GCAL_ICS_URL"]`, and no sources; add sections as needed. With `--seed-config` it copies the seed configuration instead, including its `sources`. |
| `cmd/timesheet/timesheet.example.json` | Synthetic template with `required_env`, GitHub and calendar `sources`, a disabled `migration` and a disabled `gradion` boundary; it has no `work_hours`, `judge` or `daily` section (see the key table below). Replace every placeholder, including the `/path/to/home` paths. |

Top-level keys (all non-secret; the loader rejects any key not listed):

| Key | Meaning |
| --- | --- |
| `timezone` | **Required** IANA timezone that defines the recorded day. |
| `state_path` | **Required** state file. An absolute path is recommended; a relative one resolves against the process cwd. |
| `projects` | Retired ([ADR 0012](../adr/0012-retire-the-local-project.md)); an interval carries no project. Every command except `init`, including `status` and the `daily` timer (its notice goes to the service journal), removes it from the file on first load, keeping `<config>.bak` or the next free `.bak.N`, or ignores it when the file cannot be rewritten safely. |
| `required_env` | Optional credential variable **names** to report when missing; never values. |
| `sources` | Optional evidence collectors (GitHub, calendar); see below. |
| `gradion` | Optional remote boundary: `enabled`, `endpoint` (absolute http(s) URL), `token_env` (variable name, default `GRADION_MCP_TOKEN`), optional `default_classification` object (`catalog_id`, `classification_id`, `code`, `name`, `activity_type`). |
| `migration` | Optional, disabled by default: explicit legacy import (`enabled`, `legacy_root`, `legacy_timezone`, `classification_map`; a retired `project_map` is ignored). |
| `work_hours` | Optional presentation-only main-work-time policy (`days`, `windows`). |
| `judge` | Optional binary-owned judge (ADR 0010): `backend`, `command`, `usage_format` (command backend only: empty, the default, estimates tokens; `claude-json` reads exact counts from the harness's JSON envelope), `base_url`, `model`, `api_key_env`, `timeout_seconds`, `daily_token_cap`, `tokens_csv`. |
| `daily` | Optional scheduled-run policy: `log_after`, `days`, `desktop`, `desktop_command`, `webhook_env`, `webhook_kind`, `notify_empty`. |

Every credential is referenced by variable name (`gradion.token_env`,
`judge.api_key_env`, `daily.webhook_env`) or is fixed by the collector
(`GITHUB_TOKEN`, `GCAL_ICS_URL`); its value is resolved from the env file or the
inherited environment at use time and is never persisted.

## Source configuration

Evidence collection is the `sources` section. Full key semantics and examples:
[collection configuration](collection-config.md).

| Key | Meaning |
| --- | --- |
| `sources.enabled` | Master switch for every collector. |
| `sources.github` | `enabled`, `user` (GitHub login), `repositories` (1–50 host-less `owner/name`), optional `api_base`. Credential: `GITHUB_TOKEN`. |
| `sources.calendar` | `enabled`, optional `user` (invitation identity). The feed URL is the credential `GCAL_ICS_URL`, never a config key. |

A missing credential leaves that collector `missing_credentials` (naming the
variable only); the other collector still runs. There is no local-checkout git
collector, journal source or per-source timezone in the current build.

The remote boundary is the top-level `gradion` block: with `enabled: true` it
reads `endpoint` and the credential named by `token_env`. Absent or disabled
means a local-only workflow.

### Source statuses

Each enabled collector records one attempt per collection with a status:

- `complete` — fully collected (an empty result is legitimate, not an error).
- `incomplete` — collected, but a bound or limit was hit; partial coverage is
  never presented as complete.
- `failed` — the collector could not run (for example an unusable feed URL).
- `missing_credentials` — the named variable is unset or empty.
- `disabled` — reported for both collectors when no source is enabled; the day
  is then recorded manually and no acknowledgment is needed.

A source that is not `complete` must be explicitly acknowledged before the day
can be confirmed and saved (`source_unacknowledged`). The `log` flow asks the
human that question (`ask_human`); it never acknowledges on its own.

## Running a day

```bash
bin/timesheet log                      # today in the configured timezone
bin/timesheet log --date 2026-11-10    # a past working day
```

`log` runs the day end to end and reports one state: `human_gates` (the day is
ready for review and submission), `ask_human` (a question only you can answer)
or `stopped` (with the reason). With a `judge` section configured the binary
answers its own judgments (ADR 0010); without one it stops at the first
judgment for the agent path. Submission is separate: `bin/timesheet submit`
in a terminal, or the chat-confirmed `/submit` workflow (ADR 0009).
`bin/timesheet status` and `bin/timesheet submit status` are read-only.

## Historical reference (retired CLI)

> Everything below documents the retired `run`/`review`/`preview`/`approve`
> CLI, its `evidence.sources` schema (journal, git `mode`, `--offline`,
> `--ack`) and its workflow modes. Those flags and keys do not exist in the
> current build, and the verbs are no longer the public surface (`run` survives
> only as an internal step `log` drives); use the sections above.

Two workflow modes, defined by the owner:

- **Local** — collect evidence from **both** local and configured **remote**
  sources, then review and export. **Do not submit to Gradion.**
- **Remote** (connected) — the same evidence and review, then an **exact preview**,
  an **explicit approval**, and a **Gradion submission**.

Two traps to keep straight:

- The workflow words **local/remote** are not the git source's `config.mode`
  (`local` | `remote` | `both`). A "local workflow" may still collect from a
  remote git source and a calendar feed.
- **`--offline` is not a synonym for the local workflow.** `--offline` only skips
  the **Gradion** remote seam (its reads and writes). Configured **source**
  fetches — calendar ICS, git (including remote GitHub), and the journal note —
  still run, and a remote git source still uses the network.

> Current session authority: **production submission is blocked.** The connected
> (remote) commands below are documented so the gated steps are known, but they
> must **not** be executed. Approval and submission are always separate, explicit
> steps and are never performed by `run`.

### Evidence count, no sources, and incomplete sources (retired)

`review` prints one line per enabled source that is not complete/skipped, plus one
line per warning on a complete source, and distinguishes conditions:

- **No sources configured** → `No evidence sources configured.` (distinct from a
  configured day that legitimately returned nothing).
- **Configured but no evidence** → an empty draft; not an error.
- **Incomplete source** (a missing journal note `JOURNAL_NOTE_MISSING`, a missing
  vault `JOURNAL_VAULT_MISSING`, an unreadable note, a calendar/remote failure)
  → the source is shown and the preview **blocks** with `incomplete_collection`
  until acknowledged with `--ack <source-id>` (or a decisions `acknowledge`
  entry). A missing journal note is non-fatal and never aborts the run.

## Workflow modes and the matrix

> **Historical (retired CLI).** This section describes the pre-R6 pipeline.
> `run --decisions`, `review`, `preview`, `approve --plan` and the
> decision-file `project` fields no longer exist, and intervals carry no
> project ([ADR 0012](../adr/0012-retire-the-local-project.md)). Use
> `timesheet log` ([Running a day](#running-a-day)).

`run` picks a mode by precedence (highest first): `--decisions FILE` /
`--non-interactive` → scripted; `--plain` → safe line-based; `--interactive` →
terminal UI (only when stdin **and** stdout are real terminals and the environment
is not CI, else it **refuses**, never silently falls back); default → scripted, so
headless behavior and exit codes are unchanged. `--plain` and `--interactive`
apply to `run` only; the guided run reviews one day at a time.

| Workflow | Entry | Command | Who |
| --- | --- | --- | --- |
| Local | CLI guided (real TTY) | `bin/timesheet run --interactive --config CFG --date D` | A human at a terminal |
| Local | CLI plain (pipe/CI safe) | `bin/timesheet run --plain --config CFG --date D` | Human over a pipe; EOF aborts |
| Local | CLI scripted (explicit) | `bin/timesheet run --decisions decisions.json --non-interactive --config CFG --date D` | Automation |
| Local | Skill | `/timesheet` (prompt) | Human via the skill |
| Remote | CLI guided/plain | same `run` as local, then `preview` → `approve --plan` → `submit` | Human (future gated submit) |
| Remote | CLI scripted | `run --decisions …`, then `preview` → `approve --plan` → `submit` | Automation (future gated submit) |
| Remote | Skill | `/timesheet` (prompt) | Human via the skill |

`run` never approves or submits. The guided run stops at the exact preview and
export and prints a single `approve --plan <PLANID>` follow-up. Details:
[guided workflow](guided-workflow.md).

## Running the local workflow

> **Historical (retired CLI).** This section describes the pre-R6 pipeline.
> `run --decisions`, `review`, `preview`, `approve --plan` and the
> decision-file `project` fields no longer exist, and intervals carry no
> project ([ADR 0012](../adr/0012-retire-the-local-project.md)). Use
> `timesheet log` ([Running a day](#running-a-day)).

Throughout this section, `CFG` is the path to your config file and `D` is the
target date:

```bash
CFG=./timesheet.local.json
D=2026-10-06
```

### 1. Discover the day

The first pass collects evidence and prints the proposal. Run it once to
**discover the actual block IDs** before writing decisions:

```bash
bin/timesheet run --interactive --config "$CFG" --date "$D"      # guided
# or, non-interactive discovery:
bin/timesheet review --config "$CFG" --date "$D" --no-editorial
```

`review` prints `work`/`meeting`/`manual` blocks with their stable IDs, plus
untimed **narrative** journal candidates (section, provenance, `duration=unknown`).

### 2. Apply review decisions (scripted)

`--decisions FILE` applies a serializable decision set through the same
`internal/review` actions the guided run uses. `{date}` in the path is substituted
per day. Example (the actual `run.ReviewDecisions` fields):

```json
{
  "confirm":     [{"block_id": "<BLOCK_ID>", "project": "Proj", "attendance": true}],
  "exclude":     [{"evidence_id": "<EVIDENCE_ID>", "reason": "user_excluded"}],
  "join":        [{"work_block_id": "<BLOCK_ID>", "meeting_ev_id": "<EVIDENCE_ID>"}],
  "trim":        [{"block_id": "<BLOCK_ID>", "new_start": "09:15", "new_end": "09:45"}],
  "project":     [{"block_id": "<BLOCK_ID>", "project": "Proj"}],
  "description": [{"block_id": "<BLOCK_ID>", "text": "User-authored description"}],
  "classification": [{"block_id": "<BLOCK_ID>", "classification_id": "<ID>", "activity_type": "<derived>"}],
  "manual":      [{"uuid": "<UUID>", "start": "2026-10-06T09:00:00Z", "end": "2026-10-06T11:00:00Z", "project": "Proj", "description": "Pairing", "evidence_ids": ["<EVIDENCE_ID>"], "confirm": true}],
  "mark_empty":  false,
  "acknowledge": ["journal-work"]
}
```

Apply it in one non-interactive pass (or use the explicit `review` step):

```bash
bin/timesheet run --decisions decisions.json --non-interactive --config "$CFG" --date "$D"
# equivalently, the explicit review step:
bin/timesheet review --config "$CFG" --date "$D" --decisions decisions.json
```

Notes (exact keys, from the current source):

- **Confirm** requires an explicit `confirm` decision; a proposed block is never
  confirmed automatically. `attendance` applies to meetings.
- **Manual** — `confirm` defaults to **false**: without `"confirm": true` the
  interval is only *proposed* and needs a later confirm. `evidence_ids` links one
  user-timed interval to several supporting records (for example journal items);
  the links add **no minutes** and the interval's own span is authoritative. Pass
  a stable `uuid` to edit the same interval.
- **Description** and **Classification** reach every fragment of a split block.
- **Classification** changes invalidate a prior approval for the date.

### Bulk actions in the guided review

The guided review (`run --interactive` / `run --plain`) offers an optional **Bulk
actions** step, placed between the per-block review and the classification step.
It offers exactly four owner-confirmed actions over exactly one mutually-exclusive
scope:

- **Bulk-confirm** eligible blocks.
- **Bulk-exclude** blocks.
- **Bulk-assign a project** (the label states `assign-project X (confirms the
  block)`, because assigning a project stages the block's confirmation too).
- **Bulk-assign a classification** (one action per run): the user picks a single
  classification through the same searchable catalog UI, and the step previews the
  **exact classifiable intervals and counts** before consent. It stages the
  classification on the same atomic review only; an already-classified interval is
  retained (skipped), and an abort stages nothing. The per-block classification step
  does **not** re-prompt an interval the bulk action staged (it is disclosed as
  bulk-classified and skipped): per-block override holds at the **data level**
  (add-if-absent staging; a per-block decision for the same interval replaces the
  bulk choice), and the UI override path is the printed
  `timesheet classify select --block-id <ID>` follow-up.

**Effective eligible set.** Bulk-confirm/assign eligibility is "a **known
duration**, not a carryover, and **preselected**". In the current proposal only an
**accepted meeting** satisfies all three: work blocks are `duration=unknown`,
manual blocks and joint blocks are `Preselected=false`. So the effective
bulk-confirm/assign set is **accepted meetings with a known duration**; a `work`,
`manual`, or `joint` scope is **inert** for confirm/assign (its blocks land in the
`skip` buckets, not the applied set). Bulk-exclude applies to any evidence-backed
block.

Scopes: `all`, `by kind` (`work`/`meeting`/`joint`/`manual`), `by project`, or an
explicit block-id list. The step prints the **exact affected-block list** with each
block's per-block action and the counts (`confirm N`, `skipped unknown-duration M`,
`requires explicit collision K`) **before** requiring an explicit consent. Nothing
is staged until that consent, and nothing is written until the single atomic review
apply, so an abort applies nothing.

Bulk never resolves a conflict silently:

- an **unknown-duration** block (a git observation window) is never bulk-confirmed;
- a **carryover**, a non-preselected meeting, or a joint block requires an explicit
  per-block confirmation;
- a **meeting that overlaps a work/manual interval that is already confirmed
  (durably, or per-block in this session) or that this bulk action would confirm**
  is excluded and surfaced as requiring an explicit collision decision (joining
  stays per-block). An overlapping in-scope block that is merely **skipped** is not
  a collision, because confirming the meeting alone creates no overlap;
- a **bulk project assignment** never bypasses the normal project resolution;
- a **nonempty day is never marked empty**.

Per-block decisions always win: bulk staging is add-if-absent, so a per-block choice
is never overwritten regardless of order. An empty scope prints
`no blocks match this scope; nothing staged` and stages nothing.

Known boundaries of the bulk step:

- A bare per-block **"Leave unconfirmed"** stages nothing, so a later bulk-confirm
  **is** authoritative over it (the preview shows the block in the applied set).
- The exclude warning scans the **durable** confirmed intervals, not in-session
  staged confirms; an in-session confirm that a later bulk-exclude would drop is
  applied as consented, without a warning.
- The pre-existing "no staged work ⇒ mark empty" rule still applies: if every
  in-scope block is skipped and nothing else is confirmed, the day is marked
  reviewed-empty (bulk never sets `MarkEmpty` itself).
- A degenerate **zero-length** confirmed interval (already flagged
  `unconfirmed_duration` by readiness) is treated by the collision gate as a
  full-day span, so it can over-block a meeting; fix the interval rather than
  relying on the bulk step to resolve it.
- The collision gate compares **same-day clock-minute spans**: a genuinely
  post-midnight confirmed interval is not numerically compared against a
  same-clock-day meeting. This is largely precluded by the day-scoped record
  model (a cross-midnight interval is split at local midnight).

### 3. Classification (explicit, authoritative)

Classification is an explicit choice; config maps and recent selections are
**suggestions only** and are never auto-applied. An unset classification blocks
the preview (`unresolved_classification`).

```bash
bin/timesheet classify list --config CFG                 # live; --cache reads the cache; --json for machine output
bin/timesheet classify select --config CFG --date D --classification "<CODE|ID>" --block-id "<BLOCK_ID>"
bin/timesheet classify select --config CFG --date D --classification "<CODE|ID>" --all   # explicit bulk
```

`classify list` prints `Name (Code) [Activity Type] · <id-prefix>` with a
`complete|partial` marker and provenance/freshness. The Activity Type is **derived
by the server** from the classification and is never sent. `classify select`
requires an **exact unique** match (id, code, or unambiguous id-prefix); a
**stale** `--cache` catalog is refused unless `--force`. Both `classify list` and
the guided classification step surface, labelled `unapplied`, up to the **3 most
recent** selections, or — when there is none — the owner's **first-run** suggestion
`INTGRADI2610 (Gradion Intern Academy 2026)` with its derived `internal project`
type (resolved against the fetched catalog; never fabricated). A recent selection
takes precedence over the first-run suggestion. In the guided step a catalog search
with no match says the catalog is **partial** and offers search again — it never
asserts absence. The guided run prints a
`timesheet classify select --date D --block-id <ID> --classification <CODE|ID>`
follow-up to change an interval that already carries a classification. See
[classification selection](classification-selection.md).

### 4. Preview and export

```bash
bin/timesheet preview --config CFG --date D     # zero remote writes; prints plan_id, operations, total
bin/timesheet export  --config CFG --date D     # re-render the managed note; never contacts the remote
```

`preview` prints the exact creates/edits, intervals, projects, and total. In the
local workflow you stop here (or after the guided run's export). **Approval is
optional locally** because nothing is submitted; approval exists only to gate a
remote submission.

## Running the connected (remote) workflow

> **Historical (retired CLI).** `preview` and `approve --plan` no longer
> exist. Submit a logged day with `timesheet submit` in a terminal or
> `/submit` in chat.

Connected mode adds a `gradion` endpoint. The evidence and review steps are
**identical**; only the final gated steps differ. Run the same `run`/`review`
above, then:

```bash
# FUTURE GATED STEPS — do not execute while production submission is blocked.
bin/timesheet preview --config CFG --date D
bin/timesheet approve --config CFG --date D --plan "<PLANID>"     # binds approval to the exact plan hash
bin/timesheet submit  --config CFG --date D                       # executes only the record's ACTIVE approval
```

- **Approval is bound to the exact plan content hash** and is per-day. Any local,
  plan, or remote change invalidates it; re-run `preview` and `approve` with the
  new plan id.
- **Submit re-proves every selected classification against a fresh LIVE remote
  result** before any write (a fresh cache never proves identity). Each choice is
  looked up by its recorded code (or name); a legacy selection with no recorded
  code/name fails closed with a `reselect_classification` diagnostic and zero
  writes, unless a fresh live full complete catalog proves it. See
  [classification selection](classification-selection.md).
- **Submit is currently blocked**: live Gradion create/edit semantics are
  unverified and this session holds no write authority. The command is documented
  for the future gated step only.
- **Local connected mode still does live reads.** With a Gradion endpoint and a
  resolved credential, `preflight`/`review`/`preview` perform read-only MCP calls
  (capabilities, catalog, remote listing) **when the user authorizes them** — but
  no submission. `--offline` disables exactly those Gradion calls.
- **Uncertain writes stop the run** and are never retried blindly. An
  identity-only write response is not a confirm: content is verified by a bounded
  read-back, and a mismatch or an unverifiable read-back leaves the write
  **content-unverified** and unresolved. An uncertain, unidentified, or
  content-unverified write **STOPS the operational session**; recovering from it
  requires SEPARATE owner authority — no self-adoption, no new-plan approval, no
  source hot-patching, and no debugging writes without it. A conversational "go"
  confirms ONLY the immediately presented exact plan, and ONLY when submission
  authority is already established; it cannot authorize a different plan and never
  implicitly grants debugging or source-repair authority. Reconcile later with an
  explicit `resolve` decision (a future gated step).

## Skill usage

The skill delegates to the same binary; it parses no vault and performs no
accounting. It never auto-confirms, auto-approves, or auto-submits, and a skill
request is **not** standing remote-write authority.

- **Local, interactive (human at a terminal):** the skill runs `run --interactive`;
  you confirm each choice, and the run stops at the preview.
- **Local, headless (agent/automation):** the skill uses the scripted path
  (`run --decisions … --non-interactive`, or `review` then `export`). Agents
  cannot drive the TUI, so a headless run uses `--decisions` (or `--plain` over a
  pipe).
- **Remote:** the same `/timesheet` flow, then the explicit `preview` →
  `approve --plan` → `submit` gate — documented, not executed here.

Example prompts (any of the skill's triggers work; the skill picks the mode from
the environment — a real terminal for `--interactive`, otherwise the headless
path):

| Workflow | Context | Prompt |
| --- | --- | --- |
| Local | Human at a terminal | `/timesheet review today from my journal and git, no submission` |
| Local | Agent / headless | `/timesheet daily timesheet for 2026-10-06, no editorial, no submission` |
| Remote | Human at a terminal | `/timesheet review today and prepare the submission plan` |
| Remote | Agent / headless | `/timesheet review 2026-10-06 and prepare the submission plan for approval` |

In every case the skill delegates to the binary and stops before submission; a
remote prompt prepares the exact plan for a human to `approve`/`submit` later.

## Privacy and editorial

- **Headless:** pass `--no-editorial` (or configure no editor) for
  deterministic-only text.
- **Configured editorial:** the optional agent proposes titles/descriptions only
  (TEXT ONLY); it can never alter intervals, attendance, projects, evidence, or
  approvals. **Private journal note text is never forwarded** to the editorial
  service, including a manually linked journal description. Fallback to
  deterministic text is automatic on any failure.

## Export and persistence

- The **journal collector is read-only**; `run` may separately **write the managed
  export** (the `<!-- timesheet:begin v1 -->` … `<!-- timesheet:end -->` section
  in `<vault>/<daily_dir>/<date>.md`), preserving everything outside the markers
  byte-for-byte.
- **Aborting a guided run** applies **no unfinished confirmation, approval, or
  submission** before the final deliberate local-review confirmation, and prior
  reviewed state is preserved. Collection and optional editorial synthesis may
  already have persisted their own durable state.
- **Existing confirmed work is safe:** a reviewed-empty day stays empty with no
  invented zero-hour block, and a refresh never drops confirmed work.
- **Journal evidence is narrative and untimed:** zero start/end, unknown duration,
  never a working time. Editing an implicit item changes its identity, so the old
  item is **detached** history (retained, never re-proposed); an optional explicit
  `<!-- evidence_id: X -->` keeps identity across edits. Duplicate explicit or
  implicit ids in one section **fail visibly** rather than silently collapsing.
- **Only recorded work is captured** — this never establishes a complete day.

## Release note (upgrade behavior)

**State schema 4 is a one-way upgrade** ([ADR 0012](../adr/0012-retire-the-local-project.md)).
The first command that writes the store after the upgrade copies the old file
to `state.json.schema3.bak` (or `.schema1.bak`/`.schema2.bak`; `0600`; the
next free `.bak.1`, `.bak.2`, ... when that name already holds something
else) and prints one stderr line naming the file it used. `0.2.0` cannot read
the new file and stops with `state_invalid: local state file is not valid JSON
for the supported schema`. To go back to `0.2.0`: restore the backup over
`state.json` (`cp ~/.timesheet/state.json.schema3.bak ~/.timesheet/state.json`)
and put a `projects` list back in the configuration (or restore
the `config.json.bak` (or `.bak.N`) the stderr line named if the new build
removed it), because `0.2.0` requires at
least one project. Work saved by the new build after the upgrade is not in the
backup.

The `content_observed` flag on a remote observation is an **additive** field. On
upgrade, a pre-existing mapping or observation recorded before this field existed
decodes with `content_observed` absent (treated as **false** = content unknown).

1. **Legacy mappings are content-unknown, not conflicts.** A legacy mapping's
   stored baseline is no longer asserted as a `remote_conflict` merely because it
   lacks the flag: an unprovable conflict is not raised. Remote change detection
   for such a mapping stays **inactive** — it is refreshed only by an explicit
   `resolve`/adopt of an unresolved operation, or by a confirmed edit that rebuilds
   the mapping; there is **no automatic refresh on `preview`/`plan`** (those never
   persist `last_observed`). There is **no record migration and no silent trust
   upgrade** — the legacy baseline is simply treated as unknown. An
   observation-derived baseline whose content truly differs still conflicts as
   before.
2. **Legacy persisted edit plans fail closed.** A persisted edit plan whose
   `expected_remote` is unobserved (a pre-field plan echo) now validates as
   `stale_remote` (`edit_remote_changed`) and must be **re-planned and re-approved**.
   This is **fail-closed and safe**: approvals are single-use and short-lived, no
   already-submitted work is affected, and no false confirm is possible. No
   migration is required.

## Troubleshooting

Exit codes: `0` success, `1` error, `2` usage error, `3` blocked.

| Symptom | Cause / fix |
| --- | --- |
| `No evidence sources configured.` | No enabled sources — add sources or accept an empty draft |
| `JOURNAL_NOTE_MISSING` / `JOURNAL_VAULT_MISSING` | Journal note/vault absent — fix the path or `--ack <source-id>` |
| `JOURNAL_DATE_UNSTATED` | The note exists but its frontmatter states no `date` — add `date: <YYYY-MM-DD>`; the source stays complete but its items are not date-certified |
| `incomplete_collection` blocks preview | An incomplete source needs `--ack <source-id>` |
| `unresolved_classification` | An interval has no classification — `classify select` |
| `readiness:unconfirmed_duration` | Supply and explicitly confirm a valid working interval; do not infer duration from narrative evidence |
| `readiness:not_attended_as_work` | A meeting marked not attended is still included as work; exclude it or correct the reviewed attendance |
| `readiness:unresolved_project` | Resolve the interval's project explicitly |
| Catalog `partial` / `STALE` | The catalog is truncated or old — `classify list` (live) or `--force` deliberately |
| `uncertain` submission | A write's outcome is unknown — the run STOPS the operational session; **never retry blindly**; recovering needs SEPARATE owner authority (no self-adoption, no source hot-patching); `resolve` later (future gated) |
| `content-unverified` write | The write only acknowledged an entry's identity; its content was not verified by the bounded read-back — the run STOPS unresolved, never a false confirm |
| Conversational "go" | Confirms ONLY the immediately presented exact plan, and ONLY when submission authority is already established — it cannot authorize a different plan or grant debugging/source-repair authority |
| `--interactive` refused | Not a real terminal or under CI — use `--plain` or `--decisions` |
| Piped/`tee`d output | Not a TTY — the TUI will not start; use `--plain` |

## Reference

- [Guided workflow](guided-workflow.md) — `run --interactive`/`--plain` modes and review contents
- [Journal source](journal-source.md) — journal keys, identity, active/detached, privacy
- [Collection configuration](collection-config.md) — the `sources` section (GitHub and calendar keys)
- [Classification selection](classification-selection.md) — catalog and explicit selection
- [ADR 0007](../adr/0007-journal-evidence-source.md) — journal evidence decision

## Hermetic retest

The named offline harness drives the real orchestration with a sanitized note and
no credential, remote, or live service. It is **not** a live or real-terminal
verification:

```bash
go test ./cmd/timesheet -run '^TestGuidedOfflineHarness$' -count=1 -v
```
