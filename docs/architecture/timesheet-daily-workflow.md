# Pilot Timesheet daily workflow

Status: implemented architecture for the `next` planner and the regular Pi
skill. This note records the chosen design and its accepted limits; the source
is authoritative, not a copy pasted here.

## 1. Chosen architecture

The daily workflow is a **stateless planner**: every `next` invocation
re-derives one typed step from durable content that already exists. There is no
progress journal, no cursor, no resume file and no hidden state between
invocations. The binary is read-only in this role; the model writes only the
judgment file the binary asks for; the human owns the irreversible gates.

Consequences:

- A crash or restart loses no workflow position because none was stored.
- Re-running `next` with unchanged content is deterministic.
- "Current" means content-equal to what a fresh verb would derive, not "the
  newest timestamp". Published timestamps are second-resolution and cannot
  distinguish a same-second edit.
- `next` never executes a verb, never opens a shell, and never mutates the
  store or artifacts.

## 2. Usage: one state per invocation

```text
timesheet next [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>] [--project <id>]
```

Omitted `--date` means today in the configured timezone. The skill invokes
`next`, obeys exactly one `state`, then invokes `next` again.

| State | Meaning | Caller action |
|---|---|---|
| `action` | `action.cmd` is the exact argv | run the vector verbatim, never a shell string |
| `judgment` | a model decision is required | write only `judgment.answer_file`, then `next` again |
| `stopped` | progression cannot be derived or is human-owned | print `reason`; do not guess |
| `human_gates` | the day is ready for approval/submission | run read-only `submit-status`, report ready plan, capture metrics, stop |

There is no `--answer` flag and no `--do` dispatcher. A judgment is the file at
`judgment.answer_file`; the following `next` invocation consumes it only when
its binding digest matches the current content.

`run` and `next` share the store's day folder `<state dir>/days/<date>/` for
the draft and judgment answers, next to `<state dir>/entries/<date>.json`, so a
wiped temp directory never loses a day in progress and different stores never
share drafts or answers. A day still in the old temp location of earlier releases
(`$TMPDIR/timesheet/<store-identity>/<date>/`) is reported as `stopped` with the
one-line move command; it is never silently restarted. Use the paths printed by the binary.

## 3. Digest-bound judgment envelopes

Both answer files carry the binding the report published, copied verbatim:

```json
{"draft_digest":"<judgment.draft_digest>","notes":"<string>","descriptions":{"<block_id>":"<prose>"}}
```

```json
{"ledger_digest":"<judgment.ledger_digest>","selections":{"<interval_id>":{"catalog_id":"...","classification_id":"..."}}}
```

- Descriptions are judged from the draft path named in `judgment.protocol`; the
  answer is bound to those exact draft bytes.
- Classifications are judged from `judgment.blocks[]` (`interval_id`,
  `block_id`, `start`, `end`, `description`, `candidates`) and
  `judgment.candidates[]`. The judgment is self-contained: no draft,
  descriptions file, temporary artifact or catalog query is needed.
- Before that judgment, `next` derives the deterministic `candidates` action
  (`classifications --date <day> --candidates`). It reads the live catalog once
  per distinct topic (the block description without its `PRs:` list, first
  line, at most 80 characters; `candidateQuery` is the one place to change
  that), caches each catalog in the store as `classifications --query` does,
  and writes `candidates.json` in the day folder bound to the staged ledger's
  `ledger_digest`. A catalog failure fails the step closed and writes nothing.
  At most 12 distinct topics are read and at most 10 options are kept per
  topic, so the judgment carries at most 120 options however many intervals the
  day has; a capped or provider-truncated set says `complete: false`, and
  intervals beyond the topic bound are named in `judgment.candidates_note`. A
  ledger edit makes the file stale, and `next` asks for the step again.
- An absent, malformed or stale binding is re-judged. A stale answer is never
  handed to `record` or `classify`, and `next` emits the `classify` action only
  after the file's `ledger_digest` matches the current staged ledger.
- `{"selections":{}}` is the honest no-op and still carries the digest.

## 4. Responsibility map

| Owner | Owns | Never does |
|---|---|---|
| Compiled binary | derive state and content, read ledger/stage map/answers/store, construct argv | write artifacts, execute actions, decide identity or attendance |
| Timesheet skill | the two bounded judgments, reporting blockers or readiness, terminal metrics | shell-evaluate `action.cmd`, approve or submit during logging, resolve-write |
| Human | source acknowledgement, attendance verdicts, identity resolution, approval, submission | rely on the agent to perform a TTY gate |

The skill is a thin adapter: locate the binary, run the argv, write the answer
file, report. The binary is the only owner of step order.

The separate `submit` skill adds an opt-in follow-up after logging. It reviews
all operations, waits for explicit chat confirmation, then invokes
`agent-submit` once with the exact reviewed plan and context binding. Consent
is agent-enforced, not proof of presence. Legacy terminal commands retain
their gates, and the native `tui` verb carries the operator surface the retired
Pi adapter once rendered.
See [ADR 0009](../adr/0009-chat-confirmed-submission.md).

## 5. Explicit blockers

`stopped` states name the real blocker and its remedy, including:

- enabled source with no collection attempt, or an incomplete attempt that
  needs an explicit acknowledgement;
- multiple configured projects without a chosen `--project`;
- a blocked plan, with reconcile/resolve as the human path;
- a finalized ledger that no longer derives the saved intervals
  (`stage --revise` is a human decision);
- planning without the configured remote boundary.

The agent never silently acknowledges a source, guesses a meeting verdict,
binds an identity or reuses a stale plan. An explicitly user-authorized
decision is performed exactly once where safe. A repeated failed action is
repaired only when the error is an answer-file validation error; otherwise it
stops with one line rather than looping.

## 6. Freshness proofs

Freshness is proven from content, not timestamps:

- staging is current when the stored stage map matches `ledgerUnits`'
  projection (unit ids and per-unit span counts);
- classification is current when each sidecar selection's
  `BlockContentDigest` binds to the block `stagedBlocksFrom` derives;
- judgments bind via `draftDigest` and `timesheet.StagedLedgerDigest`;
- the finalized tail compares ledger spans with confirmed spans.

The durable plan/observation/resolution ordering is the one place timestamps
remain the fact, because ordering is genuinely what licenses a re-preview.

## 7. A vs B synthesis

- **A — prose-orchestrated pipeline:** the agent holds the step order and calls
  verbs in sequence. Flexible, but order lives in prose, drifts across runs and
  lets stale artifacts be replayed.
- **B — fully automatic binary flow:** one command runs collect through plan.
  Rejected: it would either need a model call inside the binary or would
  automate human-only judgment and gates.
- **Chosen synthesis:** the binary owns deterministic progression and content
  proofs (B's rigor), the model owns only the two bounded judgments, and the
  human owns the gates (A's separation). The skill carries no step order.

## 8. Accepted implementation deviations

- Content projection replaces timestamp freshness for the prefix; timestamps
  remain only in the plan tail where ordering is the fact.
- `save` reuses the existing confirm+save pair rather than adding a new atomic
  verb; the planner derives the pair as one `save` action.
- The planner stays read-only: no in-process dispatcher, no `--do`, no
  `--answer` flag. The answer is a file; the caller runs `action.cmd`.
- Setup remains once-only with no-replace publication; this work does not
  change its file semantics.
- Residual review fixes were comments-only; no residual review item was
  expanded into new behavior.

## 9. Configuration and cwd

With no flags, config resolution is explicit `--config` → `./timesheet.json`
in the current directory → `~/.timesheet/config.json`; `--env-file` defaults to
`~/.timesheet/.env.local` when present. Generated configurations carry an
absolute `state_path`, so commands using that config do not resolve their
store from cwd. A legacy config with a relative `state_path` keeps its existing
cwd-relative meaning; the workflow does not claim cwd independence for it.

## 10. Source of truth and evidence

Read the implementation, not a pseudocode copy:

- `cmd/timesheet/next.go` — state and report types, observation fields.
- `cmd/timesheet/next_derive.go` — the predicate switch and argv construction.
- `cmd/timesheet/next_units.go` — content projections, digests and bindings.
- `cmd/timesheet/next_run.go` — flag surface and the read-only observation pass.
- `cmd/timesheet/setup.go`, `setup_config.go`, `setup_publish.go` — setup.

Validation for this work: the parent baseline passed all Go tests on Linux;
macOS was not run. The planner tests are hermetic with explicitly seeded
catalog/store fixtures; live remote behavior remains owner-verified.
