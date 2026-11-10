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
| `ask_human` | a typed decision only the human makes | relay `ask.question`/`ask.options`; write only the human's answer to `ask.answer_file` |
| `stopped` | a real blocker: progression cannot be derived | print `reason`; do not guess |
| `human_gates` | the day is ready for approval/submission | report the ready day, its `usage` and the attached submission status, stop |

There is no `--answer` flag. Plain `next` is read-only: it derives one state
and runs nothing. `next --do` is the dispatcher: it runs each derived action
in-process and stops at the first judgment, `ask_human`, stop or human gate,
and `--auto` also answers judgments with the configured judge. A judgment is
the file at `judgment.answer_file`; the following invocation consumes it only
when its binding digest matches the current content.

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
- Classification priority, highest first: an explicit selection already in
  the sidecar; the configured default (`gradion.default_classification`),
  which fills every unselected interval, so nothing below it runs when one is
  configured; approved history; the judgment.
- Approved history (`classify --from-history`, derived only when the store
  holds provider-confirmed submissions for other days and no default is
  configured). Each staged block is keyed from its description alone: the one
  repository its inline `PRs:` list names (`repo:<repo>`), or, with no
  repository, the lower-cased whitespace-collapsed title (`title:<title>`);
  several repositories give no key. A key whose confirmed submissions carry
  exactly one classification (the verified remote content, newest submission
  per interval) is pre-selected once the live catalog, read by that option's
  name, still returns the id; an ambiguous key or a retired id is skipped with
  its reason. The step writes `history.json` bound to the ledger digest, and,
  when history decides every interval, the bound `choices.json` answer, so no
  judgment is asked. Otherwise the judgment lists only the remaining intervals,
  and `classify --file` keeps the history picks for intervals the answer does
  not name. Every sidecar selection records its `source` (`history`,
  `default` or `judgment`), and `status` lists them before approval.
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
| Compiled binary | derive state and content, read ledger/stage map/answers/store, construct argv; under `next --do`, run the derived actions (and, with `--auto`, answer judgments) | decide identity or attendance, answer `ask_human`, approve or submit |
| Timesheet skill | starting `next --do --auto`, relaying `ask_human`, reporting blockers or readiness (and, without a judge, the two bounded judgments) | answer `ask_human`, approve or submit during logging, resolve-write |
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
decision is performed exactly once where safe.

Repair-once is enforced by `next --do`, not by skill text. When `record` or
`classify` rejects a bound answer's content (`bad_request`, `unknown_interval`,
`unknown_classification`, `stale_catalog`), the failure is saved in the day
folder as `judgment-failures.json` (`kind`, judged `digest`, rejected
`answer_sha`, `failures`, `last_error`). The next report repeats the same
judgment with `judgment.last_error`, one line of at most 300 characters. While
the answer bytes are unchanged, the act is not re-run. A second rejected answer
for the same judged content stops with `judgment_failed: <kind>: <error>`; a
rewritten answer after that stop is tried again, so the day stays resumable. A
change in the judged content resets the count, and a successful act removes
the record. Every other act failure keeps `action_failed: <step>: <error>` with
no retry.

### Human questions (`ask_human`)

Decisions only the owner makes are reported as `state: ask_human` with
`ask: {kind, question, options, answer_file, digest, protocol, last_error}`,
not as free-text stops:

| `kind` | When | Options | A valid answer licenses |
|---|---|---|---|
| `source_acknowledgement` | an enabled source's attempt is incomplete and unacknowledged (one question per source) | `acknowledge` | `acknowledge --source <s> --collection-id <id>` |
| `project` | several configured projects and no `--project` | the project ids | the day's project (an explicit `--project` still wins) |
| `resolve` | the newest plan is blocked and nothing is newer | `adopt`, `accept_observed`, `foreign` | `resolve --file <answer_file>`; the answer file is the resolve decision file plus `digest` and `surface` |

The answer is `{"digest":"<ask.digest>","answer":"<option>","surface":"terminal|chat|tui|notification"}`
(the resolve file carries `decision` instead of `answer`). A stale digest, an
answer outside the options, an unparsable file or a missing surface asks the
same question again with a one-line `last_error`; a valid one turns the
question into that one action, which `next --do` runs. The answer file stays
in the day folder as the record of what was decided and through which surface.
The agent relays the question and never answers it. Approval, submission and
resolve-write stay TTY-only human gates and are never asked this way. Meeting
attendance has no derived question yet: the planner does not derive it today.
The TUI renders the same question.

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

## 12. Binary-owned judge (`timesheet judge`)

ADR 0010. With a `judge` section in the configuration, `timesheet judge
[--config] [--env-file] [--date] [--project]` answers the day's current
`block_descriptions` or `classifications` judgment itself and writes the
judgment's answer file; `next --do` then consumes it exactly like an
agent-written one. Without the section the verb refuses and nothing else
changes.

`next --do --auto` (issue #242) runs the same judge in-process: at each
`block_descriptions` or `classifications` judgment it answers with the
configured judge and continues, so one invocation goes from collection to
`human_gates`, `ask_human` or `stopped`. The report carries
`usage: {calls, input_tokens, output_tokens, exact}` for the run. A judge
failure (after its one retry), the daily cap, or the same judgment coming back
unchanged stops with `judgment_failed: <kind>: <reason>`, and the judgment stays
open: a later `next --do` without `--auto` hands it to the agent path. A
judgment that returns with `last_error` (repair-once, #240) is asked again with
that note and without the cached reply. `--auto` never answers `ask_human` and
never runs approve, submit or resolve-write. Without a `judge` section it
refuses. Its default action budget is 24 (`--max-actions` overrides it). When
`judge.tokens_csv` names a file, each run appends one row in the
`benchmarks/tokens.csv` columns (`session_id` `timesheet-auto:<UTC time>`,
`cache_tokens` 0); `scripts/tokens.py` keeps those rows when it refreshes the
file.

`--auto` is not recommended in the README until it scores at or above the
owner's agreed threshold on the golden-day eval (§11).

```json
"judge": {"backend": "command", "command": ["<harness>", "<print-mode flags>"]}
"judge": {"backend": "openai", "base_url": "https://<host>/v1", "model": "<model>", "api_key_env": "<VAR>"}
```

`timeout_seconds` (default 120) and `daily_token_cap` (default 10000) are
optional. The `command` backend writes the prompt to the harness's stdin and
reads one JSON reply from stdout, with every reserved credential variable
removed from its environment and stderr discarded. The `openai` backend sends
one chat-completions request; its key is read from the env file by name.

- **Prompts** are `internal/llm/prompts/*.tmpl`, embedded in the binary, with a
  `prompt: <kind> v<N>` stamp on the first line. The model sees only the
  blocks (title, topics, PR labels) or the intervals and their candidate sets,
  never a digest, catalog id, credential or path; the binary fills in
  `draft_digest`/`ledger_digest` and the catalog id. Evidence is cut to 300
  runes per title or description, 8 topics of 160 runes and 12 PR labels per
  block. Prose is cut so the appended `PRs:` list still fits the 500-rune
  description limit.
- **Validation**: a descriptions reply must answer every block and name no
  other; a classifications reply may only pick an option from the interval's
  own candidate set. A provider error, timeout or invalid reply is retried once
  with a one-line note; a second failure leaves the judgment open for the
  agent path.
- **Response cache**: `<day folder>/judge/<kind>-<key>.json`, keyed by the
  template bytes, the backend identity and the rendered context. An unchanged
  prompt reuses the stored reply with no call; editing a template, changing
  the backend or model, or changing the day's content misses the cache.
- **Usage**: every call appends `{at, kind, cached, ok, usage}` to
  `<day folder>/judge-usage.jsonl`; usage is exact from the HTTP backend and
  estimated (about four bytes per token) from a harness. Non-cached usage at or
  above `daily_token_cap` stops further calls for the day.
- A classifications judgment without candidates (saved-day recovery) is not
  judged by the binary.

## 11. Golden-day evaluation

`timesheet eval export [--config <path>] [--out <file>]` writes the owner's
saved days as a dataset: for each day, the draft the descriptions judgment saw
with the accepted description per `block_id`, and the staged blocks the
classifications judgment saw with the accepted `classification_id` per
`interval_id`. A provider-confirmed submission's observed remote content is the
truth when present (a later remote edit wins); otherwise the local ledger and
saved selection are. A day part with a missing draft, an unreadable ledger, a
stage map that no longer matches the ledger, or no accepted classification is
listed in `skipped` with its reason and never counts as a pass. An empty store
gives an empty dataset and a note.

The dataset holds personal evidence: it defaults to `<state dir>/eval/golden.json`,
any path inside a git work tree is refused, and it never carries configuration,
credentials or env-file contents. Repository fixtures stay synthetic.

`timesheet eval score --golden <file> [--candidate <file>]` reports the
classification exact-match rate per interval and, for descriptions, coverage
(every block answered) and mean word-set Jaccard similarity. Without
`--candidate` it scores the accepted answers against themselves: the 100%
self-check. The scorer is `timesheet.ScoreGolden`, a pure function the binary's
own judgments (#241) call in tests.
