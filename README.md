# Pilot Timesheet

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.27.1-00ADD8?logo=go)](go.mod)
[![State schema](https://img.shields.io/badge/state%20schema-3-informational)](docs/agents/timesheet-interface.md)
[![Submission: human-gated](https://img.shields.io/badge/submission-human--gated-brightgreen)](#recovery-and-no-replay)

Pilot Timesheet is a single Go binary for logging your own working days with
agent help: it collects evidence (Git/GitHub and calendar), records your day,
stages and classifies each time block, plans the entry, and keeps an audit
trail whose approval and submission steps are human-gated. It is built for a
developer logging their own days with agent help. All evidence stays in
`~/.timesheet`. Remote writes require your explicit approval, either through
the terminal workflow or the separate chat-confirmed `/submit` skill.

**What it does.** One binary owns accounting, validation and persistence. It
collects sourced evidence, keeps a typed human-readable ledger under
`<state dir>/entries/`, plans each day, and dispatches a remote submission only
after explicit approval bound to that exact plan.

**What it does not do.** It never writes remotely without that gate, never
retries or replays a dispatched write, never binds a remote identity without an
explicit human decision, and never calls anything outside the configured
plumbing. Release readiness and known gaps are tracked in
[Readiness](#readiness) — this is a development build, not a
production-ready release.

Contributions are welcome: see [Contributing](#contributing), and the
[`good first issue`](https://github.com/huyle-gradion/pilot-timesheet/labels/good%20first%20issue)
and [`help wanted`](https://github.com/huyle-gradion/pilot-timesheet/labels/help%20wanted)
queues for a starting point.

Application version: **`0.2.0`** (released as
[`v0.2.0`](https://github.com/huyle-gradion/pilot-timesheet/releases/tag/v0.2.0);
the previous stable line is `0.1.1`). State schema `3`.

## Quickstart

You need a Go toolchain matching `go.mod` (`go 1.27.1`; tested here with
go1.27.3 on Linux):

```bash
git clone https://github.com/huyle-gradion/pilot-timesheet.git
cd pilot-timesheet
go build -trimpath -o bin/timesheet ./cmd/timesheet
bin/timesheet version          # prints the version, exits 0
```

Create the `~/.timesheet` store once with `init`. It needs no project: the
remote has none, so an interval carries a time span, a description and a
classification ([ADR 0012](docs/adr/0012-retire-the-local-project.md)). Pass
a `--seed-config` to copy an existing configuration; the timezone is an
explicit `--tz <IANA>` or the host zone:

```bash
bin/timesheet init --tz 'America/New_York'
```

Then log the day. It runs every step and ends at a ready plan without
submitting; `next` shows the one step it would take without running it:

```bash
bin/timesheet log
```

The intended daily driver is an agent session using the bundled
[`.agents/skills/timesheet/SKILL.md`](.agents/skills/timesheet/SKILL.md) skill,
which runs `log` for you; see [Daily log](#daily-log-agent-skill)
below. `docs/setup-linux.md` covers first-run setup and configuration
discovery in full.

## Build and command surface

From the repository root, with any Go toolchain matching `go.mod`
(`go 1.27.1`; tested here with go1.27.3 on Linux):

```bash
go build -trimpath -o bin/timesheet ./cmd/timesheet
bin/timesheet version   # prints the application version, exits 0
```

A mise-managed Go (`mise exec -- go build ...`) is environment troubleshooting
on hosts where `go` is not on `PATH`; it is not a project dependency.

`bin/timesheet` is the production-named executable. Its public commands are
`init`, `log`, `next`, `status`, `submit` (with `submit status`,
`submit resolve` and `submit revoke`), `migrate`, `tui`, `daily`, `judge`,
`eval` and `version`; `timesheet help` lists them. Every command is one
stateless process over the local store: it reads the store, acts, and returns.
The steps `next` derives (`run`, `record`, `stage`, `classifications`,
`classify`, `save`, `plan`, `acknowledge`, `decide`, `reconcile`, `resolve`)
stay callable but are internal. A removed command (`setup`, `approve`,
`agent-submit`, `submit-status`, `resolve-write`, `collect`, `draft`,
`confirm`, `serve`) exits `2` with one line naming its replacement. A normal
`version` run exits `0`. No top-level command, an unknown top-level command, or
extra `version` arguments exit `2`. Usage errors make no remote call.

## What lives where

This repository is one Pilot Timesheet product: a single compiled Go binary
owns all accounting, validation and persistence. Two operator surfaces drive
it. The **agent skill** (`.agents/skills/timesheet/SKILL.md`) starts
`timesheet log` and relays its questions, and the
native **`tui` verb** (`bin/timesheet tui`) renders the local operator workflow
in the terminal. Both call the same binary and the same planner; neither is
required by the other. The legacy CLI subcommands, the old product skill and
the vault sync/generator source remain retired and must not be reintroduced.

| Path | What it is |
| --- | --- |
| `cmd/timesheet/` | The single-binary entrypoint: every command listed above, plus the minimal valid example configuration. |
| `internal/` | The Go engine: accounting, validation, persistence, the `next` planner. |
| `.agents/skills/timesheet/` | The runner-agnostic agent skill (`SKILL.md`, `scripts/ts-env`) that drives the binary; linked from `.claude/skills/timesheet` for Claude Code. |
| `docs/` | The authoritative specs: protocol, architecture, setup, credentials. See [Documentation](#documentation). |
| `scripts/` | Optional helpers, e.g. `tokens.py`, which imports token usage from historical agent sessions. |

## Daily log: agent skill

In any agent session with shell access, ask it to log today's work (or a past
day). The skill is one runner-agnostic folder, `.agents/skills/timesheet/`,
picked up by Claude Code through the `.claude/skills/timesheet` link. It
finds or rebuilds the binary with its `scripts/ts-env` helper, then runs one
command and acts on its result:

```bash
bin/timesheet log [--config <path>] [--env-file <path>] [--date <YYYY-MM-DD>]
```

The binary runs every deterministic step and answers its own judgments with
the configured judge (ADR 0010), so the agent only relays and reports:

- `human_gates`: the day is ready. The report carries `usage` and the
  read-only submission status; approval and submission stay with you
  (`timesheet submit` in a terminal, or `/submit` in chat).
- `ask_human`: a decision only you can make (acknowledging an incomplete
  source, a resolve decision). The agent relays the
  question and writes only your answer.
- `stopped`: a real blocker, reported verbatim.

Without a `judge` section, `log` stops at each judgment and the agent answers
it itself (`.agents/skills/timesheet/fallback.md`). Answering with the judge is
not yet the recommended default: it waits until the judge meets the owner's
golden-day eval threshold (`timesheet eval`, #242). The native TUI below is the keyboard-first
alternative over the same loop.

With no flags, config resolution is explicit `--config` → `./timesheet.json` in
the current directory → `~/.timesheet/config.json`, and `--env-file` defaults
to `~/.timesheet/.env.local` when it exists. Omitted `--date` means today in
the configured timezone; a supplied date always wins. A setup-generated
configuration stores an **absolute** `state_path`, so commands using that
config do not resolve their store from cwd.

Entries land in `<state dir>/entries/<date>.json`, merged idempotently per
time block with full source references; `entries/` stays the human ledger and
staging source. The steps `log` runs stay callable for recovery (`timesheet
next` prints the exact command for the current step), but they are internal.

### Submit in a terminal

```bash
bin/timesheet submit --date <day>                    # approve, then submit
bin/timesheet submit --date <day> --approval-id <id> # submit an earlier approval
bin/timesheet submit status --date <day>             # read-only: approvals, writes, next action
bin/timesheet submit resolve --file <decision.json>  # resolve an unknown write outcome
```

`submit` shows the plan and asks you to type its exact `plan_id` to approve it,
then shows the approval and asks you to type its `approval_id` to submit. Both
need a terminal on stdin and stdout (not proof of human presence: a pty
wrapper defeats it), so the agent never runs them. If you stop at the second
prompt, the approval is kept and `--approval-id` resumes it. A submit is never
auto-retried, and unknown outcomes stay unresolved until you run
`submit resolve`.

### Submit in chat

After `/timesheet log my today work`, use `/submit` or ask the agent to submit.
The [submit skill](.agents/skills/submit/SKILL.md) shows the exact day and every
operation, then waits for your explicit confirmation in chat. Logging, status
and loading the skill never authorize writes. No terminal command is required.

The explicit CLI entry point is `submit --chat --date <day>`. Without submission
inputs it returns `{plan, review_id}` and performs no approval or remote write.
After confirmation, the skill invokes it once with `--plan-id <reviewed id>
--review-id <reviewed binding> --confirm-chat`. The review binds day, plan,
store and credential/endpoint context; a stale review requires fresh review
and consent. It reuses existing approval, classification and durable-intent
checks without changing the protocol or the compiled binary's capability set.

Consent is agent-enforced, not cryptographic human-presence proof. Flags alone
do not establish consent. Receipts retain partial and uncertain outcomes,
which are never retried automatically or silently resolved. Existing terminal
gates are unchanged. See [the approval policy](docs/adr/0009-chat-confirmed-submission.md).

### Token usage

The binary reports its own usage: every `log` report that used the judge carries
`usage` (calls, input and output tokens), each judge call is logged in the
day folder, and `judge.tokens_csv` appends one row per run to a local
`benchmarks/tokens.csv` (git-ignored; token data is personal). The skill no
longer scrapes agent session stores. `scripts/tokens.py` stays only to import
usage from historical agent sessions into the same file; it keeps the rows the
binary wrote.

To configure an authoritative default, enable Gradion, explicitly query
`classifications --query <text>`, and review one returned option. Set
`gradion.default_classification` to its `catalog_id`, `classification_id` from
the option's `id`, `code`, `name` and `activity_type`. All five fields are required.
The example config uses synthetic placeholders; replace them with the reviewed
receipt or remove the optional object. Without it, the classification judgment
decides (ADR 0010). The retired `projects[].default_classification` never
reached the remote and is ignored if an old configuration still lists it.

`next` then derives `classify --configured-default` for unselected unsaved
intervals, including meetings and split chunks. It reads only that cached
catalog and preserves existing explicit choices. To override an interval, apply
a reviewed `classify --file` choice before filling the rest with the default.
Missing or mismatched cache bindings stop without fetching; explicitly refresh
and review the binding, or use `--file`. Stale sidecars require reviewed `--file`
repair. Changing the default never rewrites existing choices or saved days.
Approval and submission retain their live semantic verification.

### Measuring the judge

Three commands measure the configured judge against your own saved days. Keep
every file outside any repository (the commands refuse a path inside one):

```bash
bin/timesheet eval export --out ~/timesheet-eval/golden.json
bin/timesheet eval run --golden ~/timesheet-eval/golden.json --out ~/timesheet-eval/candidate.json
bin/timesheet eval score --golden ~/timesheet-eval/golden.json --candidate ~/timesheet-eval/candidate.json
```

`export` records each saved day's judgment inputs and accepted answers,
including the catalog candidates the classifications judgment picked from (a
day without recorded candidates is skipped with a reason). `run` sends every
day through the same owned prompts, cache and per-day token cap as `log`, and
calls nothing but the judge; it writes no store state. `score` prints the
classification exact-match rate, the description coverage and similarity, and
`classification_unwinnable`: intervals whose accepted answer was not among the
candidates the judge could pick from (they count as misses). A reply that does
not validate is listed in `run`'s `skipped` and scores as missing. A judge
that fails (unreachable, HTTP 401 or 429, timeout) stops the run instead. A
dataset with no usable day is never scored as zeros: `export` warns, and `run`
and `score` exit non-zero naming the cause; read `export`'s `skipped` reasons.

For tokens, compare `run`'s `max_day_tokens` (and each entry of `days`) with
`daily_token_cap` (default 10,000, ADR 0010). Replies are cached in
`judge-cache/` next to the candidate file; a rerun reuses them, still counts
their original cost in `tokens` and against the cap, and reports only new
calls in `fresh_tokens`. A cap hit or a judge failure stops the run, keeps the
partial candidate and exits non-zero. The cap is checked on an estimate before
each call, so read the numbers rather than the exit code. Without `--out`
(export's default dataset path is `<state dir>/eval/golden.json`), the
candidate and `judge-cache/` land beside the dataset in that state folder,
outside the store's day folders.

## Native TUI operator flow

`bin/timesheet tui` is the keyboard-first operator surface: a native Go terminal
UI over the same store, session and `next` planner as every other verb. It
carries the Charm presentation layer (one SGR-only style owner that degrades to
plain text under `NO_COLOR`, `TERM=dumb` or a pipe), needs no Node runtime and
makes no model call:

```bash
bin/timesheet tui \
  --config "$HOME/.timesheet/config.json" \
  --env-file "$HOME/.timesheet/.env.local"
```

It opens the store once and holds its exclusive lock for the whole process,
refuses to start unless both stdin and stdout are terminals, and performs every
refusal before the alternate screen starts. The day screen renders the loop's
derived state verbatim: state, step, why, judgment protocol, human-gate
commands and store-wide unresolved-write counts. `enter` runs the act the
derived step licenses, `s` opens the submit gate, `r` re-derives, and `q` sends
`close` and exits, discarding unsaved session state.

`--config` and `--env-file` are process inputs for `tui` and the
headless commands. They are never request fields and never model
arguments: the compiled binary reads the files, and a caller receives the path,
not the contents. Keep existing configured state and credential paths when
updating; these example paths are not a migration instruction.

Every capability the TUI offers is a local form, never a model tool: collect
evidence (GitHub/calendar, optional synthetic or real sources), acknowledge an
incomplete collection bound to the exact collection attempt, record attendance,
author the judgment's answer file with its digest copied verbatim, confirm
versus durable save, classify from the derived choices, and preview the exact
plan. Approve and submit keep the CLI's human-gated contract: display-before-
ask, an exact typed `plan_id`/`approval_id` echo, a freshness re-check
immediately before the op, one-shot emission and no retry. A durable save is the
only thing that makes work eligible for a plan; an unsaved draft is never
planned.

Tall content keeps the pinned chrome and the decision point visible: the day
screen tail-clips its state block with an honest `+N lines` indicator, a gate
keeps its head and typed echo, and the bench fits the document around the caret.
There is no interactive scrolling in this release; scroll keys are a documented
future seam. Width is the terminal driver's per-line truncation, as before.

The TUI never invokes the terminal-gated `submit` or `submit resolve`
verbs. Interactive interval authoring/editing forms, the classification
search/select picker, the migration preview/apply forms and the in-session usage
counters retired with the launcher; the underlying capabilities remain as
headless verbs (`stage`, `classifications --query`, `classify`, `reconcile`,
`resolve`) and as `migrate preview` / `migrate apply`.

## Migration and reconciliation before any write

Switching from legacy records is explicit and read-only until it is applied:

1. `timesheet migrate preview --date <day>` (repeatable, or `FROM..TO`)
   validates legacy records, conflicts, source hashes and the durable target
   and prints a `migration_id`. `timesheet migrate apply --date <same days>
   --migration-id <id>` rebuilds the candidate from the store and the legacy
   files and activates it atomically only if the rebuilt id matches. The id
   hashes the sources, the durable target and the exact payload apply would
   commit, so it binds the reviewed result rather than granting anything: a
   change to the selection, the legacy files, the store or the migration
   config since the preview is `migration_stale` with nothing written. A repeat is `no_change` with the
   legacy bytes unchanged. Migrated legacy
   approvals are inert audit data and can never authorize a write.
2. `reconcile_remote` makes one fresh read-only observation of the day and
   reports mapped, changed, missing and orphaned identities. A read never binds
   an identity: `adopt`, `accept_observed` and `foreign` are explicit human
   decisions against the current observation.
3. `preview_plan` renders the exact create/edit operations from saved confirmed
   work plus that fresh observation. `approve_plan` binds one approval to that
   exact rendered plan and never dispatches; `submit_plan` revalidates
   everything fresh, writes one durable pre-write intent per operation, then
   dispatches once and confirms only an identified acknowledgment plus a
   matching complete read-back.

Never write before the day is reconciled; never plan from an unsaved draft.

## Recovery and no replay

- A dispatched write that cannot be verified stays a durable **unresolved**
  reference. It is never retried, never replayed across a restart, and never
  silently blocks.
- Unresolved references block further writing until a human resolves each one
  with `resolve_write` against the *current complete* observation: `adopt`
  binds an observed remote identity, `not_applied` is a human attestation and
  is refused when a matching effect is visible.
- Restart never resumes an approval, a submission or a draft. Unsaved staged
  interval edits and confirmed-but-unsaved interval changes are session-only;
  only successfully committed state and durable ledgers reload. Startup never
  submits or replays anything on its own.
- A transport failure after dispatch is an unknown remote outcome, never proof
  of zero writes.
- An approval kept after a stopped `submit` is withdrawn in the terminal with
  `timesheet submit revoke --date <day> [--approval-id <id>]`, behind a typed
  confirmation; it is never reachable through `--chat` or the skill.
  Revocation is permanent audit and never undoes a remote effect.

## Readiness

- Verified: Linux hermetic workflows and actual macOS native migration,
  local Pi logging, confirm/save/cancel, saved-versus-staged restart, edit/cancel,
  reload, normal shutdown and lock release. The Darwin runner result was supplied
  by the owner; its remote capsule was not independently downloaded here.
- For the `next`/daily-workflow integration, the parent baseline passed all Go
  tests on Linux; macOS was **not run** for this change.
- The former pinned Pi launcher's bundled Undici advisory exposure is no longer
  shipped: `pi/` and its lockfile were removed in `0.2.0-dev.10`, so
  [R6 / issue #137](https://github.com/huyle-gradion/pilot-timesheet/issues/137)
  is resolved by removal rather than by a patched bundle.
- **Not verified:** live accounts/credentials, authenticated model/subscription
  readiness or real Gradion submission. Local logging does not require a model
  request; remote submission still requires an exact human-approved plan.
- `durability_uncertain` UI behavior remains source-reviewed plus accepted
  backend coverage, not a newly TUI-fault-proven case.

## Contributing

Contributions are welcome. Start with
[CONTRIBUTING.md](CONTRIBUTING.md): it covers the flow (issue first, one topic
per pull request, the required PR body format) and the local checks every PR
must report:

```bash
go build -trimpath -o ./bin-timesheet ./cmd/timesheet
go test -race -shuffle=on ./...
```

The [architecture doc](docs/architecture/timesheet-daily-workflow.md) and the
[interface spec](docs/agents/timesheet-interface.md) are the reference for
how the planner and the binary are expected to behave; a small surface with
strong conventions — safety invariants like human-gated submission, no replay
and no unverified writes are enforced by the binary and the tests, not just
preferred, and a patch that weakens one is not mergeable regardless of how even
it looks.

Before changing safety-relevant behavior, read [Recovery and no
replay](#recovery-and-no-replay) and [Migration and reconciliation before any
write](#migration-and-reconciliation-before-any-write). Bug reports, without a
patch, are contributions too: open an issue with reproduction steps.

## Documentation

- `docs/agents/timesheet-interface.md` — authoritative command surface, config,
  environment precedence, error codes and the accepted stage history.
- `docs/architecture/timesheet-daily-workflow.md` — the stateless `next`
  planner, digest-bound judgments, responsibility map and accepted deviations.
- `docs/setup-linux.md` — first-run setup and configuration discovery on Linux.
- `cmd/timesheet/timesheet.example.json` — minimal valid configuration with
  synthetic placeholders.
- `docs/agents/credentials.md` — the active `.env.local` credential policy.

## License

Pilot Timesheet is licensed under the MIT License — see
[LICENSE](LICENSE). Contributions are licensed under the same license.
