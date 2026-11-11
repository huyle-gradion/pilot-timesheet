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
timesheet next [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>]
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
| Compiled binary | derive state and content, read ledger/stage map/answers/store, construct argv | write artifacts, execute actions, decide identity or attendance |
| Timesheet skill | starting `timesheet log`, relaying `ask_human`, reporting blockers or readiness (and, without a judge, the two bounded judgments) | answer `ask_human`, approve or submit during logging, resolve-write |
| Human | source acknowledgement, attendance verdicts, identity resolution, approval, submission | rely on the agent to perform a TTY gate |

The skill is a thin adapter: locate the binary, run the argv, write the answer
file, report. The binary is the only owner of step order.

The separate `submit` skill adds an opt-in follow-up after logging. It reviews
all operations, waits for explicit chat confirmation, then invokes
`submit --chat` once with the exact reviewed plan and context binding. Consent
is agent-enforced, not proof of presence. Legacy terminal commands retain
their gates, and the native `tui` verb carries the operator surface the retired
Pi adapter once rendered.
See [ADR 0009](../adr/0009-chat-confirmed-submission.md).

## 5. Explicit blockers

`stopped` states name the real blocker and its remedy, including:

- enabled source with no collection attempt, or an incomplete attempt that
  needs an explicit acknowledgement;
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
not as free-text stops. There is no `project` question: intervals carry no
project ([ADR 0012](../adr/0012-retire-the-local-project.md)), and an
`ask-project.json` left in a day folder is ignored.

| `kind` | When | Options | A valid answer licenses |
|---|---|---|---|
| `source_acknowledgement` | an enabled source's attempt is incomplete and unacknowledged (one question per source) | `acknowledge` | `acknowledge --source <s> --collection-id <id>` |
| `resolve` | the newest plan is blocked and nothing is newer | `adopt`, `accept_observed`, `foreign` | `resolve --file <answer_file>`; the answer file is the resolve decision file plus `digest` and `surface` |

The answer is `{"digest":"<ask.digest>","answer":"<option>","surface":"terminal|chat|tui|notification"}`
(the resolve file carries `decision` instead of `answer`). A stale digest, an
answer outside the options, an unparsable file or a missing surface asks the
same question again with a one-line `last_error`; a valid one turns the
question into that one action, which `next --do` runs. The answer file stays
in the day folder as the record of what was decided and through which surface.
The agent relays the question and never answers it. Approval, submission and
`submit resolve` stay TTY-only human gates and are never asked this way. Meeting
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
[--config] [--env-file] [--date]` answers the day's current
`block_descriptions` or `classifications` judgment itself and writes the
judgment's answer file; `next --do` then consumes it exactly like an
agent-written one. Without the section the verb refuses and nothing else
changes.

`next --do --auto` (issue #242) runs the same judge in-process: at each
`block_descriptions` or `classifications` judgment it answers with the
configured judge and continues, so one invocation goes from collection to
`human_gates`, `ask_human` or `stopped`. The report carries
`usage: {calls, input_tokens, output_tokens, exact}` for the run. A judge
failure (after at most one retry), the daily cap, or the same judgment coming back
unchanged stops with `judgment_failed: <kind>: <reason>`, and the judgment stays
open: a later `next --do` without `--auto` hands it to the agent path. A
judgment that returns with `last_error` (repair-once, #240) is asked again with
that note and without the cached reply. `--auto` never answers `ask_human` and
never runs `submit` or `submit resolve`. Without a `judge` section it
refuses. `timesheet log` is the public form: it runs `next --do`, with `--auto`
when a judge is configured. Its default action budget is 24 (`--max-actions` overrides it). When
`judge.tokens_csv` names a file, each run appends one row in the
`benchmarks/tokens.csv` columns (`session_id` `timesheet-auto:<UTC time>`,
`cache_tokens` the harness-reported cache tokens, else 0; `total_tokens` input
plus output plus cache); `scripts/tokens.py` keeps those rows when it refreshes the
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
removed from its environment and stderr discarded. Its usage is estimated from
the prompt and reply sizes (`exact: false`) unless `usage_format` names the
harness's output envelope: with `"usage_format": "claude-json"` (and the
harness run as `claude -p --output-format json`), stdout must be one JSON
object of `type` `result`; its `result` string is the reply, validated as
before. Counts come from `modelUsage`, summed over every model
(`inputTokens`, `outputTokens`, `cacheCreationInputTokens`,
`cacheReadInputTokens`), or from `usage` (`input_tokens`, `output_tokens`,
`cache_creation_input_tokens`, `cache_read_input_tokens`) when `modelUsage` is
absent (`exact: true`). Input and output are the non-cache counts; the two
cache counts are reported as `cache_tokens` and never count toward
`daily_token_cap`. A count the envelope lacks is estimated and the usage is
marked inexact. Output that is not such an object is a permanent backend
failure (not retried); `is_error: true` is a transient one, except the
`error_max_turns` and `error_max_budget_usd` subtypes, which are permanent.
Any other `usage_format` value is a configuration error. The `openai` backend sends one
chat-completions request; its key is read from the env file by name.

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
  own candidate set. A call is retried at most once (#285). An invalid reply
  is sent back with a one-line note of what was wrong. A transient backend
  failure (timeout, HTTP 408, 429 or 5xx, an unreachable endpoint, a harness
  that exits non-zero or replies over 1 MiB) is sent again unchanged, without
  the note, after one second or the provider's `Retry-After` when that is
  longer, up to ten seconds; a longer `Retry-After` gets no retry. A permanent
  failure (any other HTTP 4xx, a 200 that is not a chat completion, a harness
  that is missing or cannot start) is not retried. The daily cap is checked
  on an estimate before each call, the retry included; a cap stop before the
  retry keeps the first call's failure in its reason. A failure that is not
  retried, or a second failure, leaves the judgment open for the agent path.
- **Response cache**: `<day folder>/judge/<kind>-<key>.json`, keyed by the
  template bytes, the backend identity and the rendered context. An unchanged
  prompt reuses the stored reply with no call; editing a template, changing
  the backend or model, or changing the day's content misses the cache.
- **Usage**: every call appends `{at, kind, cached, ok, usage}` to
  `<day folder>/judge-usage.jsonl`; usage is exact from the HTTP backend and
  estimated (about four bytes per token) from a harness. A harness that ran
  but failed (timeout, non-zero exit, oversized reply) still counts its
  estimated input tokens, since the prompt was sent; a failed HTTP call and a
  harness that never started count the call with no tokens. Non-cached usage at or
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
stage map that no longer matches the ledger, no bound catalog candidates, or no
accepted classification is
listed in `skipped` with its reason and never counts as a pass. An empty store
gives an empty dataset and a note. A dataset of 0 days is still written, so its
skip reasons stay inspectable, with a one-line warning on stderr (#282). `eval
run` refuses a dataset with no replayable day (no days, or every part would be
skipped before a judge call) before any judge call and writes no candidate;
`eval score` refuses a dataset with no accepted classification or description
instead of printing rates of 0.

The dataset holds personal evidence: it defaults to `<state dir>/eval/golden.json`,
any path inside a git work tree is refused, and it never carries configuration,
credentials or env-file contents. Repository fixtures stay synthetic.

`timesheet eval score --golden <file> [--candidate <file>]` reports the
classification exact-match rate per interval and, for descriptions, coverage
(every block answered) and mean word-set Jaccard similarity. Without
`--candidate` it scores the accepted answers against themselves: the 100%
self-check. The scorer is `timesheet.ScoreGolden`, a pure function the binary's
own judgments (#241) call in tests.

Schema 2 (#279) records, for the classifications part, the intervals the
judgment saw (the staged blocks minus those decided from approved history) and
the catalog candidate sets from the day's `candidates.json`, each interval
naming its set. The sets came from live catalog reads when the day was logged,
so they are recorded rather than re-read: a ledger without bound candidates is
skipped with its reason, and the replay needs no network beyond the judge. A
schema 1 dataset is refused with "export it again".

`timesheet eval run --golden <file> [--out <file>] [--config <path>]
[--env-file <path>]` replays every dataset day through the configured judge
and writes a candidate `eval score --candidate` reads unchanged (default:
`candidate.json` next to the dataset). It uses the production path: the owned
prompts, the parsers, one retry, the response cache and the per-day token cap.
Its cache and usage ledger live in `judge-cache/<date>/` next to the
candidate, never in the store's day folders, and the ledger is reset per
replayed day so the cap applies to each day of this run. A cached reply's
original cost is written back into that ledger, so a rerun counts the same
per-day tokens and stops at the cap where the first run did. Description
answers are composed as `record` stores them (the inline PRs list appended). A
reply that does not validate is listed in `skipped` and scores as missing. A
cap hit or a backend failure stops the run, writes the partial candidate and
exits non-zero. The receipt reports `usage` and `fresh_tokens` (calls made
now), `tokens` and per-day `tokens` (cached cost included), `max_day_tokens`
and `daily_token_cap`. Without a `judge` section it refuses; an `--out` inside
a git work tree, or equal to `--golden`, is refused. It opens no store and
makes no remote call besides the judge. With export's default dataset path,
the default candidate and `judge-cache/` sit in `<state dir>/eval/`.

`eval score` also reports `classification_unwinnable`: truth intervals with no
candidate set, or whose accepted answer is not an option of their set. They
remain misses in the exact rate; the count shows how much of the rate the
judge could never win.

## 13. Scheduled daily log (`timesheet daily`)

Issue #243. `timesheet daily [--config] [--env-file]` is what the scheduler
runs. It needs a `judge` section (§12) and logs, through `next --do --auto`:

- the **previous working day**, every run, so a machine that was off in the
  evening catches up at the next start; and
- **today**, only at or after `daily.log_after`, so a morning start never saves
  a half-finished day (a recorded day is not re-judged).

Each day then gets one notification, and the same result is never notified
twice (`<day folder>/notified.sha256`). A second run while one holds
`<state dir>/daily.lock` exits at once. The notification carries only the day,
counts, the reason or question, and the command to run:

| Result | Notification |
|---|---|
| `human_gates` | `<day>: <n> entries, <h>h<mm>m ready to submit. Review and approve in a terminal: <approve command>` |
| `ask_human` | the question, its options, the answer file, and the command to resume |
| no evidence | `<day>: nothing to log` (skipped with `"notify_empty": false`) |
| `stopped` or an error | the reason |

Approval stays in the terminal: no notification or webhook can approve or
write. A failed notification is logged to stderr and never changes the
timesheet outcome.

```json
"daily": {
  "log_after": "22:00",
  "days": ["mon", "tue", "wed", "thu", "fri"],
  "desktop": true,
  "webhook_env": "TIMESHEET_WEBHOOK_URL",
  "webhook_kind": "discord",
  "notify_empty": true
}
```

Every field is optional; the values above are the defaults, except that
`webhook_env` is unset by default. Desktop notifications use `notify-send` on
Linux and `osascript` on macOS (or `desktop_command`, with the message
appended as its last argument). The webhook URL is a secret, read from the env
file by the variable name in `webhook_env`; `webhook_kind` is `discord` or
`slack`.

### Install (Linux, systemd user timer)

`timesheet daily --print-units systemd` prints the service and timer with this
binary's absolute path and the resolved config and env paths. The timer runs
at `log_after` on the working days, three minutes after login
(`OnStartupSec`), and catches up a missed run at the next start
(`Persistent=true`).

```bash
timesheet daily --print-units systemd > /tmp/pilot-units.txt
# split at the "# file:" lines into ~/.config/systemd/user/pilot-timesheet-daily.{service,timer}
systemd-analyze verify --user ~/.config/systemd/user/pilot-timesheet-daily.{service,timer}   # dry run
systemd-analyze calendar "$(grep ^OnCalendar ~/.config/systemd/user/pilot-timesheet-daily.timer | cut -d= -f2)"
systemctl --user daemon-reload
systemctl --user enable --now pilot-timesheet-daily.timer
systemctl --user list-timers pilot-timesheet-daily.timer
```

`notify-send` needs the session bus in the user manager's environment. Most
desktops import it; if notifications don't appear, run
`systemctl --user import-environment DISPLAY WAYLAND_DISPLAY DBUS_SESSION_BUS_ADDRESS`
from the session's startup. Run once by hand with
`systemctl --user start pilot-timesheet-daily.service` and read the result with
`journalctl --user -u pilot-timesheet-daily.service`.

### Install (macOS, launchd agent)

```bash
timesheet daily --print-units launchd > ~/Library/LaunchAgents/com.pilot-timesheet.daily.plist   # drop the first comment line
plutil -lint ~/Library/LaunchAgents/com.pilot-timesheet.daily.plist                              # dry run
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.pilot-timesheet.daily.plist
```

The agent runs at login (`RunAtLoad`) and at `log_after` on each working day.

The scheduled run uses `--auto`, so the same gate applies as in §12: it is not
recommended in the README until the judge meets the owner's golden-day eval
threshold.
