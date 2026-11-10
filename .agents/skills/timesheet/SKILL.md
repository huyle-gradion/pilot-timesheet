---
name: timesheet
description: Log today's work into the personal timesheet from real git, pull-request and calendar evidence. Run when the user asks to "log today's work" or "log the timesheet", or a past working day.
---

# Pilot Timesheet

The binary owns the pipeline; the model owns only the judgment it asks for and the human-gate report. Drive the day with `next --do`: one invocation executes every deterministic step in-process and stops at the one judgment or the human gates.

## Setup (one shell invocation)

`SKILL_DIR` is this skill's absolute directory, supplied by the loader. Pass `--config <path>` / `--env-file <path>` to `next` only to override the defaults. Never read, print or copy the env file: only the binary reads it.

```sh
SKILL_DIR="<absolute directory of this loaded skill>"
TS_BINARY="$("$SKILL_DIR/scripts/ts-env")"       # must succeed; its error names the fix
TS_ROOT="$("$SKILL_DIR/scripts/ts-env" --root)"  # repository root, for the build and token script
"$TS_BINARY" next --max-actions 1 --help >/dev/null 2>&1 || {
  ( cd "$TS_ROOT" && { command -v go >/dev/null 2>&1 && go build -trimpath -o bin/timesheet ./cmd/timesheet || mise exec -- go build -trimpath -o bin/timesheet ./cmd/timesheet; } )
  TS_BINARY="$TS_ROOT/bin/timesheet"
  "$TS_BINARY" next --max-actions 1 --help >/dev/null 2>&1
}
"$TS_BINARY" next --do            # add --date YYYY-MM-DD for a past day
```

Capture `DAY` from the first report's `day` and resume with `"$TS_BINARY" next --do --date "$DAY"` and the same config/env/project overrides, so a run stays on its original day across midnight. If the check or build fails, stop and report it. Build only here, never mid-loop. A mise-managed Go is environment troubleshooting, not a project dependency.

## The loop

Run `"$TS_BINARY" next --do --date "$DAY"` (add `--project <id>` when the configuration defines several projects and one is chosen) once per turn. Each invocation executes every deterministic action the binary licenses and prints ONE bounded JSON report: the terminal `state`, plus `ran[]`, one short receipt per executed action. Obey exactly one `state` per report.

- `judgment` — do the model work below, writing only `judgment.answer_file`, then run the same command again. Judgments are files: there is no `--answer` flag. Copy every digest field from the report verbatim; never recompute it.
- `ask_human` — a decision only the user makes (`ask.kind`: `source_acknowledgement`, `project` or `resolve`). Relay `ask.question` and `ask.options` to the user verbatim, with `ask.last_error` when present, and stop for their reply. Never answer it yourself, never pick an option they did not state, and never infer consent. When they reply, write exactly their answer to `ask.answer_file` as `ask.protocol` shows, copying `ask.digest` verbatim and setting `"surface":"chat"`, then run `next --do` again: the binary performs exactly that one decision. If they do not answer, run Terminal capture and stop.
- `stopped` — print `reason` verbatim: it names the precise blocker and remedy. Never silently acknowledge a source, guess an attendance verdict, choose an identity binding, or reuse a stale plan. Run Terminal capture and stop.
- `human_gates` — when `human.status` is present, use it directly: it is the read-only submit-status result, already fetched for this day. Run `human.status_command` only when `human.status` is absent. Surface its unresolved writes and `next_safe_action`. Report the plan is ready for review, run Terminal capture and stop. The user can follow up with `/submit` or ask to submit, which invokes the separate `submit` skill. Logging never authorizes submission. The legacy `approve`/`submit`/`resolve-write` commands remain interactive human steps; this skill never executes them.

**Failure discipline.** Obey the report; the binary owns retries. A judgment that carries `last_error` means your previous answer for this same content was rejected: fix exactly what it names and write the answer again. `judgment_failed: <kind>: <error>` and `action_failed: <step>: <error>` are stops: print `reason` verbatim and stop; never retry as-is or spin. If a report still carries `state: action`, run its `action.cmd` as one command and resume.

## Judgment

### `block_descriptions`

Judge only `judgment.draft.blocks` in the report; the same document is on disk at `draft.json` beside `judgment.answer_file`, but never read it separately. Never judge raw git/calendar/prior-day evidence or a remembered draft. Write to `judgment.answer_file`:

```json
{"draft_digest":"<judgment.draft_digest copied verbatim>","notes":"<string>","descriptions":{"<block_id>":"<prose>"}}
```

One grouped-topic entry per draft block; no draft fields, block times or `PRs:` list (`record` appends `prs_inline`); `notes` is one JSON string or omitted. A block may carry `outside_work_hours` + `work_hours_reason` (`day_not_in_policy`, `before_window`, `between_windows`, `after_window`, `partial`): informational only — out-of-policy time is allowed, so never reject, skip, shift or edit a block for it. Never alter evidence. A stale or missing `draft_digest` sends `next` back to this judgment; rewrite the whole answer with the current digest.

### `classifications`

Configuration may supply a cached default: when `next --do` executes `classify --configured-default` itself, the report's next state already reflects it. That path uses the operator's `gradion.default_classification` binding for every unselected unsaved interval, including meetings and split chunks; existing explicit selections win, and a missing/mismatched cached binding or stale sidecar stops with a repair instruction. Configuration changes affect future unselected intervals only. Saved-day classification recovery remains a reviewed judgment.

Without a configured default, `next --do` first runs `classify --from-history` itself: intervals whose repository (or, without one, exact title) carries one consistent classification across the owner's confirmed submissions on other days are pre-selected, after a live catalog check. They are left out of the judgment and kept; judge only the `blocks[]` the report lists.

A `classifications` judgment is model work. Use only `judgment.blocks[]` (each entry's `interval_id`, `block_id`, `start`, `end`, `description` and `candidates`) and `judgment.candidates[]`. The report is self-contained; do not read a draft, descriptions file or temporary artifact for this judgment. The binary has already read the live catalog (the `candidates` action that `next --do` runs itself): each block's `candidates` names one set in `judgment.candidates[]` by its `ref`, and each set carries its `catalog_id`, `complete` flag and `options[]` (`classification_id`, `label`, `code`). Pick at most one option per interval from the set its block names; do not query the catalog. Omit an interval whose block has no `candidates` ref or no fitting option; a set with `complete: false` (see its `note`, and `judgment.candidates_note`) cannot prove an option absent, so report that rather than guess. Only a saved-day recovery judgment, which carries no `candidates`, still needs `"$TS_BINARY" classifications --query "<topic text>"` with the same `--config`/`--env-file` flags given to `next`. Then write to `judgment.answer_file`:

```json
{"ledger_digest":"<judgment.ledger_digest copied verbatim>","selections":{"<interval_id>":{"catalog_id":"<catalog_id>","classification_id":"<option id>"}}}
```

Copy `catalog_id` from the chosen set (or the classifications receipt) and `classification_id` from the chosen option verbatim; never invent an id or topic, silently default, or add fields the receipt does not carry. `{"selections":{}}` is the honest no-op when nothing fits, and still carries the exact `ledger_digest`. `next --do` executes `classify` only after the file's `ledger_digest` matches the current staged ledger; never run `classify` on an unbound or stale file.

## Terminal capture (mandatory, non-blocking)

Before the final response on EVERY terminal outcome, including a ready plan,
an unresolved blocker, setup/build failure or failed action, run this exact
helper once. Do this after reporting the primary outcome internally, not
instead of planning or error handling. Collection failure never changes the
timesheet outcome or prevents a later chat-confirmed submission.

```sh
"$SKILL_DIR/scripts/ts-metrics"
```

Include the helper's collection result briefly in the final response. Missing
usage is unavailable, not zero-cost success. The helper scans ordinary Pi,
pig and Claude stores without a launcher and refreshes repository-local
`benchmarks/tokens.csv` independently of cwd. It captures usage already flushed
to disk, so an ongoing session is a snapshot that later invocations refresh.
Totals cover matching sessions, not isolated judgments or elapsed-time
benchmarks. No timing or speedup is inferred. Keep judgment within the target
of **1,000 tokens/day (input + output)**, with cache separate.
