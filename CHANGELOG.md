# Changelog

Versioning follows [Semantic Versioning 2.0.0](https://semver.org/). This file starts the documented implementation release line; it does not retroactively assign versions to historical commits.

## Unreleased

- The judge's single retry tells failures apart (#285). Only a reply that does
  not validate is resent with the "previous reply was rejected" note. A
  permanent backend failure (HTTP 401, 403, 404, a missing harness) is not
  retried; a transient one (timeout, 429, 5xx) is resent unchanged after a
  short backoff that honors `Retry-After` up to ten seconds. The daily token
  cap is now checked before the retry too, as the README states. A harness
  call that ran but failed now counts its estimated input tokens, and the
  harness reply size limit is enforced (it was bypassed before).
- `internal/mcp` text bounding keeps a multi-byte rune that ends exactly at
  the byte cap (#289). A cut stops before the first invalid UTF-8 byte, so
  bounded text is always valid UTF-8.
- `internal/timesheet` comments are removed (#258). New tests pin the
  constraints they stated, including that a write left in flight blocks the
  plan, its approval and any new submit after a restart.
- `cmd/timesheet` comments are removed (#258). New tests pin the constraints
  they stated, including that the judge command never gets a nil environment.
- `internal/timesheet` constraints that only a comment stated are now pinned
  by tests (#296): strict request decoding, spill-day replay blocking,
  copy-on-write review decisions, issue-lookup caching, review search bounds,
  classification proof consistency, legacy zero-operation approvals, carried
  block ids, and calendar recurrence, timezone and window expansion.

## 0.3.0

Breaking: state schema 4 retires the local project (#185). The first write
upgrades the store one way and keeps a `state.json.schema3.bak`; see the
upgrade and rollback note below.

Retire the local project (#185, ADR 0012). The remote has no project, so an
interval no longer carries one.

- **Breaking:** `--project` is removed from `init`, `log`, `next`, `tui`,
  `judge`, `stage`, `classify` and `save`. Passing it exits `2` with one line
  telling you to drop it. A configuration with several projects no longer
  makes `stage` refuse or `next`/`log` ask.
- **Breaking:** the `ask_human` kind `project` is retired. An
  `ask-project.json` answer left in a day folder is ignored.
- **Breaking:** the staged interval text grammar is
  `DATE HH:MM-HH:MM DESCRIPTION`. The project token and the `/CLASSIFICATION`
  suffix are gone; the `unknown_project` error code is retired.
- **Breaking:** state schema `4`. An interval's `project` is optional: new
  intervals have none, and intervals saved before keep theirs as an inert
  legacy label, never rewritten. A schema 1-3 file is validated in its own
  shape, upgraded in memory, and written as schema `4` by the next successful
  write together with one `migration.schema_upgrades` receipt (from and to
  schema, build version, days, intervals, legacy project labels,
  `applied_at`).
- **Breaking, one-way upgrade:** before that first write, the schema 1-3 file
  is kept at `state.json.schema<N>.bak` (0600; complete or absent; the next
  free `.bak.1`, `.bak.2`, ... when that name holds anything but these exact
  bytes), and the write prints one stderr line naming the file used. `0.2.0` cannot read a schema `4`
  file: it fails closed with `state_invalid: local state file is not valid
  JSON for the supported schema`. To downgrade, restore the backup over
  `state.json` and put `projects` back in the configuration (the old build
  requires at least one). The receipt's `app_version` reads `0.2.0` until the
  `0.3.0` release.
- `init` needs no project and writes none. `--seed-config` no longer copies a
  seed's `projects` or `migration.project_map`.
- The configuration keys `projects` and `migration.project_map` are retired.
  A configuration that lists them still loads. On the first load, every
  command except `init` removes them from the file with an edit that keeps
  every other key and its formatting (names matched without regard to case),
  keeps the original as `<config>.bak` (or the next free `.bak.N`), and prints
  one stderr line. This includes `status`, `plan` and the `daily` timer, whose
  line lands in the service journal. A file it cannot rewrite safely
  (read-only, a symlink, other hard links) keeps them, ignored, with one
  stderr line.
- Review, day and segment output no longer show an interval's legacy
  `project` or local `classification` label. Both stay inert in state.
- Classification comes from `gradion.default_classification` when configured
  and otherwise from the ADR 0010 classification judgment. The retired
  per-project `default_classification` only ever set a local label.
- `migrate preview|apply` no longer requires a legacy project to map to a
  configured one. The migrated interval carries no project, and the receipt's
  legacy interval audit keeps the original value.
- Remote write bodies are unchanged: a test pins the exact bytes a day's
  writes send, captured before the change.

Carry the after-midnight remainder of long overnight entries into the next day
(#277, ADR 0011).

- An overnight entry with more than 240 minutes after midnight (for example
  `23:00→05:00`) no longer stages a next-day allocation that `plan` blocks with
  `remote_entry_too_long`. The source day keeps the part up to midnight plus up
  to 240 minutes after it (`23:00→03:00`). When a carry is made, each interval
  it stages is at most 240 minutes. The remainder (`03:00→05:00`) is carried
  into the next date. Minutes across both days equal the recorded span
  exactly. With exactly 240 minutes after midnight there is no carry, as
  under #176: the source day keeps one longer interval, and each of its
  per-date allocations is at most 240 minutes.
- A carry is consumed only while its source ledger still derives it. A source
  edited without a re-stage, or deleted, stops every day that would take the
  carry in (`stage`, `save`, `classify`, `next`/`log`, `submit --chat`). The
  stop names `stage --date <source>`. `submit --chat` also checks a source
  day's own carry record, as `next` does.
- `daily` also logs a non-working day that has a carry-in, for example the
  Saturday after a Friday `20:00→06:00`. The notification counts the minutes
  each day stages, so Friday reports 4h ready, not 10h. The TUI gate view and
  the agent skill show `human.carried`.
- A fall-back night whose cutoff lands in the repeated hour, like DST at
  midnight, makes no carry and stays loud at `plan`.
- The carry is a derived record, `entries/<date>.carry.json`. Every
  derivation of the source day replaces it, and the ledger is never modified.
  The next date stages each carried segment as
  `carried-<source date>-<block id>`. The segment takes part in overlap
  claiming and four-hour chunking like that day's own entries.
  `next`/`log`, `classify`, `submit --chat` and `stage` all derive it.
- Review shows the carry: stage receipts list `carried_out` and `carried_in`.
  The classification judgment labels a carried block `carried_from`, and the
  `human_gates` report lists `carried` segments.
- A carry into a day that is already saved or submitted is never applied
  silently. `next`/`log` on that day stops and names it, and `submit --chat`
  refuses. The human takes it in with `stage --revise --date <day>`. The
  source day's stage receipt carries a `carry_note`.
- A day whose sources were collected but whose draft has no blocks now stages
  its carried segments alone instead of stopping.
- A block stamped after midnight on an overnight ledger is placed on the
  extended timeline. Previously it staged 24 hours early on the source day's
  early morning. It follows the carry when it falls in the carried part. A
  block that would run past the next date's midnight is skipped with its own
  reason.
- Fix: a day after a saved midnight-crossing interval was refused by `stage`,
  `confirm`, `save` and `classify` as "already finalized", because the
  previous day's locked spill segment counted as its own confirmed work.
- Upgrade note: days saved before this change stay current for `next`/`log`
  and `submit --chat`, under both the pre-#176 unsplit rule and the #176
  chunking. No carry exists for them until a human runs `stage --revise`, and
  `plan` still blocks their over-long allocation as before. A source day
  staged but not yet saved re-stages with the carry at its next
  `stage`/`save`. `eval export` derives a carried target day from its full
  stage input and accepts the legacy derivations.

- Add `timesheet eval run --golden <file> [--out <file>]` (#279): replays
  every golden day through the configured judge with the same owned prompts,
  response cache and per-day token cap as `log`, and writes a candidate file
  `eval score --candidate` reads unchanged. It reports total and per-day tokens
  against the cap (cached replies counted at their original cost, new calls
  as `fresh_tokens`), lists unparseable replies as skipped (scored as
  missing), stops on a cap hit or a failing judge while keeping the partial
  candidate, writes no store state, and refuses an `--out` inside a
  repository or equal to `--golden`.
- `eval score` reports `classification_unwinnable`: intervals whose accepted
  answer was not among the judge's candidates.
- `eval export` writes golden schema 2: the classifications part records the
  catalog candidate sets the judgment picked from, and leaves out intervals
  decided from approved history. A day without bound candidates is skipped
  with a reason. Schema 1 datasets are refused; export them again.
- Remove narrating comments from `internal/mcp` and `internal/llm` (#258).
  The constraints they stated are now tests: MCP status and version errors
  redact the token before bounding, requests accept only `application/json`,
  the default client follows no redirect, bounded strings keep rune
  boundaries, prompt titles are clipped, and no judge backend identity
  carries a credential. The headless judge command now refuses to run
  without a filtered environment instead of inheriting every pipeline
  secret.
- An empty golden dataset no longer reads as zeros with exit 0 (#282).
  `eval run` refuses a dataset with no replayable day (no days, or every part
  would be skipped before a judge call) with one line, before any judge call
  and without writing a candidate. `eval score` refuses a dataset with no
  accepted classification or description instead of printing rates of 0.
  `eval export` still writes a dataset of 0 days, so its skip reasons stay
  inspectable, and warns on stderr.

## 0.2.0

The first final release of the `0.2.0` line. It includes everything in the
`0.2.0-dev` pre-releases below: the binary runs the day and
owns its judgments (the 12-factor redesign, #246). Since `0.2.0-dev.11`:

Retire `serve` (#272): every capability it alone reached is now a stateless
command that reads the store and returns.

- Add `timesheet migrate preview --date <day|FROM..TO>...` and
  `timesheet migrate apply --date ... --migration-id <id>`. Apply keeps
  nothing from the preview: it rebuilds the candidate from the store and the
  legacy files and activates it only when the rebuilt `migration_id` matches.
  The id hashes the dates, legacy timezone, source bytes, durable target and
  the exact payload apply would commit, so an edited project or
  classification map between preview and apply is also stale.
  A mismatch is `migration_stale` with nothing written. The `migration_apply`
  op now takes `dates` and no longer keeps in-process preview state.
- Add `timesheet submit revoke --date <day> [--approval-id <id>]`, terminal-only
  behind a typed confirmation and never reachable through `--chat`. The hint
  after a stopped terminal `submit` names it.
- `status` reports the required environment variable names under
  `environment` (what `check_environment` reported, names only).
- Drop the `cancel` op: staged edits never outlive the one process that made
  them, so there is nothing to cancel.
- **Breaking:** `serve` is removed; it is a retired command (exit `2`) naming
  its replacements, and `Session.Serve` is gone.
- **Breaking:** `status` output no longer carries `protocol_schema`; the
  protocol schema constant is removed with `serve`.
- **Breaking:** `version` prints only `timesheet <version>`, without the
  `(protocol schema N)` suffix.
- The protocol sections of the interface doc are kept under a historical
  banner.
- Docs: start the day with `timesheet log` in the Linux setup, rewrite the guided-workflow note around `tui` and terminal `submit`, retire the journal source note (ADR 0007 amended), and drop the vault-generator line from the lessons (#270).
- Stage overnight entries like same-day ones (#176). An overnight span joins
  the overlap projection, so a block inside it merges into its cluster at
  `stage` instead of failing `save` with `overlap`. It is chunked to the
  four-hour policy in real minutes in the day's timezone (DST-aware), with the
  midnight-crossing chunk last because every interval starts on its staged
  date. `next`/`log` derive the same units, so the day is not reported stale.
  An entry whose RFC 3339 anchors are 24 hours or more apart is skipped with
  its own reason instead of being truncated to its clocks, and so are
  overlapping entries whose union with an overnight span runs 24 hours or more.
  Same-day days stage byte for byte as before.
- Upgrade note: a day already saved with an unsplit overnight interval stays
  current for `next`/`log`, `submit --chat` and `eval`. A day staged but not
  yet saved under the old rule re-stages with the new units at its next
  `stage`/`save`, which can change its interval ids; if it was already
  classified, the classification sidecar may then be stale and the day asks to
  classify again.

## 0.2.0-dev.11

The 12-factor redesign (#246): the binary owns its LLM judgments, runs the day
itself and exposes one small public command surface. This release also
records the changes merged since `0.2.0-dev.10` that shipped without a
version bump.

- Partition the draft by workday and run the deterministic steps in-process
  (#232).
- Keep each day's working files in the timesheet store instead of `$TMP`
  (#233, #247).
- Pre-fetch catalog candidates for the classification judgment, so the
  judgment picks from a bounded set (#234, #248).
- Enforce the repair-once judgment policy in `next --do` (#240, #249).
- Classify known repositories and titles from approved history before any
  judgment is asked (#235, #250).
- Add the golden-day dataset export and the judgment scorer
  (`timesheet eval`) (#236, #251).
- Add a typed `ask_human` state for decisions only the owner makes (#239,
  #252).
- Stop tracking `pi/node_modules` (#238, #253).
- ADR 0010: binary-owned judgments behind one judge interface, with a
  headless agent command and an OpenAI-compatible backend, owned prompts and a
  daily token cap (#237, #254).
- Add `internal/llm` and the `judge` verb, and `next --do --auto`, which
  answers judgments in-process and reports token `usage` (#241, #242, #257).
- Add `timesheet daily`: a scheduled log of the previous working day (and
  today after `daily.log_after`, default 22:00) with notify-only results and
  `--print-units systemd|launchd` (#243, #259).
- Reduce the agent integration to one thin skill front end that runs the
  binary and relays its single state; the Pi prompts and token helper scripts
  go (#244, #261).
- One public command surface: `init`, `log`, `submit` (with `status`,
  `resolve` and `--chat`), plus `status`, `tui`, `daily`, `judge`, `eval` and
  `version`. `setup`, `approve`, `agent-submit`, `submit-status`,
  `resolve-write`, `collect`, `draft` and `confirm` are retired and print
  their replacement (#245, #262).
- Briefly configure, then remove, CodeRabbit review (#263, #264).
- Re-untrack `.omo/` and restore the root build-artifact and runtime ignore
  rules that #219 had reverted (#210, #265).
- Add the repeatable backlog-audit checklist to
  `docs/agents/issue-tracker.md` (#260, #266).
- Make the `tui` view's content sanitization structural (#229): recorded
  content enters a frame only as a value of one writer (`recorded`, and the
  `labelRow` label/value row built on it), which sanitizes internally, in place
  of ~55 hand-placed `sanitizeContent` calls. The approval row and the caret
  clamp each exist once. The hostile-content test now covers the ask, gate
  kind, gate/bench error and caret branches it missed. Rendered frames are
  byte-identical.
- Point the missing-configuration error at `timesheet init` instead of the
  retired `setup` command, and bring the configuration docs in line with the
  current loader (#194): `docs/agents/collection-config.md` and the setup and
  configuration sections of `docs/agents/product-usage.md` now document the
  real `sources`/`gradion`/`judge`/`daily` keys, `init` and the env-file-only
  credential policy instead of `evidence.sources`, `storage_dir` and
  `keychain:<service>` references. `timesheet.example.json` uses neutral
  `/path/to/home` paths, a test loads it and every JSON example in the
  collection guide with the real loader, and ADR 0004 gains a dated amendment
  retiring its Keychain storage note.
- `timesheet init` now ends with `next: … run 'timesheet log'` instead of the
  internal `timesheet run`, and its env template says `init` (not `setup`)
  never reads credential values. `docs/agents/product-usage.md` leads with the
  current build, `init` and the binary-owned `log` flow, documents the real
  source statuses, and moves the retired workflow modes, `--offline`/journal
  notes and `$TS` examples under one historical banner (#194).
- Keep `serve` as an internal command (the only route to legacy migration,
  `cancel`, `check_environment` and `revoke_approval`) and pin it with in-repo
  end-to-end JSONL tests (the status handshake, `migration_preview` /
  `migration_apply`, malformed lines, close and EOF, and the binary's own
  stdin/stdout); the interface doc now lists the #262 public surface and marks
  `serve` internal, matching the README (#230).
- Align the Go and doc stamps at `0.2.0-dev.11`; protocol schema `1` and
  state schema `3` are unchanged.

## 0.2.0-dev.10

- Retire the optional pinned Pi launcher/TUI (`pi/`): its launcher, adapter,
  view and lockfile are removed, along with the adapter-pin pairing test (its
  subject, the Go and TypeScript stamp pairing, no longer exists) and the macOS
  launcher QA script. The bundled Pi/Undici advisory exposure leaves the
  repository with the lockfile, so
  [issue #137](https://github.com/huyle-gradion/pilot-timesheet/issues/137) is
  resolved by removal rather than by a patched bundle. The native `tui` verb is
  the operator surface; `serve` remains the documented protocol server and the
  negotiation stamps stay meaningful. Interactive interval authoring/editing
  forms, the classification picker, the migration forms and the in-session
  usage counters retire with the launcher; their capabilities remain available
  as headless verbs and serve ops (#225).
- Add the `tui` verb's Charm presentation layer (#225): one lipgloss style owner
  with SGR-only styles that degrade to identity under the Ascii profile
  (`NO_COLOR`, `TERM=dumb`, a pipe), per-content sanitization at render
  composition replacing the frame-wide strip (the UI's own escapes survive
  while recorded content still cannot drive the terminal), and height-only
  windowing that keeps the pinned chrome and the decision point visible with an
  honest `+N lines` indicator. No bubbles components: help would add a second
  hand-synced keymap, viewport needs the frozen scroll keys, textarea/textinput
  would reformat the byte-exact bench document, and cursor/spinner/progress need
  recurring timer effects that break the one-pending-effect rule.
- Align the Go and protocol-doc stamps at `0.2.0-dev.10`, tag `v0.2.0-dev.10`
  on `main`, and publish it as a GitHub pre-release (#226).

## 0.2.0-dev.9

- Add the `tui` verb (#221): a native Go terminal UI over the same store,
  session and derivation the CLI verbs use. The day screen renders the loop's
  own `next` action verbatim (state, step, why, judgment protocol, human-gate
  counts), `enter` runs the derived act in-process over one held store lock
  (run, record, stage, classify, save, plan), judgment authoring writes the
  loop's own answer files with `draft_digest`/`ledger_digest` copied verbatim
  (no second validation path), `stale_revision` re-runs review and reopens the
  form fresh, and approve/submit sit behind display-before-ask typed-echo gates
  that re-run `preview_plan`/`submission_status` immediately before emitting
  `approve_plan`/`submit_plan`. Quit sends `close` and discards unsaved session
  state; non-terminal stdio and `store_locked` refuse before the alt screen
  starts. No AI harness and no model calls exist on this path. The
  TTY-gated `approve`, `submit` and `resolve-write` verbs are unchanged and the
  TUI never invokes them.
- Extract shared verb entry cores (`collectDraft`, `composeDescriptionsOnly`,
  `stageAndReplay`, `confirmAndSave`, `classifyFileDay`, `classifyDefaultDay`,
  `saveDay`) so the verbs and the TUI run one pipeline; verb behavior and
  printed receipts are unchanged. `headlessSession.call` returns the op's own
  typed `*timesheet.AppError` with byte-identical printed output.
- Align Go, adapter negotiation and package/root-lock stamps at `0.2.0-dev.9`.

## 0.2.0-dev.8

- Add the separate `/submit` skill and explicit `agent-submit` review/submit
  command (#215). The agent displays the exact plan, waits for explicit chat
  confirmation, then approves and submits once. Stale plan/context bindings,
  single-use approvals and uncertain-write barriers remain enforced. Chat
  consent is agent-enforced, not proof of human presence. Legacy terminal
  commands and trusted Pi adapter capabilities are unchanged.
- Align Go, adapter negotiation and package/root-lock stamps at `0.2.0-dev.8`,
  tag `v0.2.0-dev.8` on `main`, and publish it as a GitHub pre-release (#220).

## 0.2.0-dev.7

- Capture ordinary Pi, pig and Claude timesheet usage with runtime provenance,
  refreshed growing-session totals and repository-local output. The skill now
  requires non-blocking terminal capture on ready and blocked outcomes (#213,
  #214). Token snapshots are not performance measurements.
- Follow pig's `PIG_CODING_AGENT_DIR`, `PIG_HOME`, XDG and `~/.pig`
  precedence, including tilde expansion and explicit Pi-directory sharing.
  Retain the observed legacy config location and count shared files once
  without guessing which runtime wrote them.
- Align Go, adapter negotiation and package/root-lock stamps at `0.2.0-dev.7`.
  No release tag is created.

## 0.2.0-dev.6

- Development stamp moves to `0.2.0-dev.6` for the daily-workflow and
  GitHub-collection batch, in lockstep across the Go constant, the Pi
  adapter identity, the changelog, README and the interface doc. No
  release tag, adapter-package stamp or publication is part of this work.

## 0.2.0-dev.5

- Fixed-path flag defaults: every command except `serve` now runs without
  `--config` and `--env-file`. `--config` defaults to
  `$HOME/.timesheet/config.json` (a missing default fails naming the file and
  the fix) and `--env-file` to `$HOME/.timesheet/.env.local` when that file
  exists (no file values otherwise, as with an omitted flag before). Explicit
  flags always win verbatim, so the Pi launcher, tests and alternate setups are
  unchanged — `serve` still requires both flags, and an explicit `--env-file`
  that cannot be read still fails. The skill's commands shrink accordingly.
- Merge concurrent POWs instead of splitting or dropping them (issue #183):
  work and evidence blocks whose same-day spans strictly overlap render as one
  block over the union span, carrying every contributor's topic candidates, PRs
  and sources so each POW is named, and a meeting absorbs the work and evidence
  it overlaps. A merged block's id is the union-derived `kind-start-end` in the
  draft, `record` and `stage` alike, so recording the draft after a recovery
  merge replaces that entry instead of writing a duplicate; collisions take a
  `-2`, `-3` suffix in id order and singleton blocks keep their own id. A
  meeting keeps the wording of absorbed work that has no topic candidate, and
  another cluster names a titleless contributor by its first topic candidate.
  Touching blocks stay separate and a gap is never
  bridged. `stage` derives the same merge, so one interval covers the union
  span (chunked to the four-hour policy) and every other contributor is
  reported as merged into its union block rather than dropped; `record`
  re-derives sources over the draft's pre-merge topic ownership, keeping the sources of a
  cluster that absorbed an evidence block, and a day without overlap
  renders byte-identically. Classification selections on a merged interval are
  bound to the union span and joined description, so changing any contributor
  fails the replay closed. The shortest-first split and the meeting's silent
  suppression of an overlapping work block are gone.
- Add the stateless daily planner `next` (issue #195): one typed JSON state per
  invocation — `action` (exact argv), `judgment` (model answer file),
  `stopped` (explicit blocker and remedy) or `human_gates` (read-only
  `submit-status` plus the exact approve/submit commands). It writes nothing,
  executes nothing, keeps no progress journal, and has no `--do` dispatcher or
  `--answer` flag. The regular Pi skill now drives the day by repeating
  `next`; `run`, `record`, `stage`, `classify`, `save`, `plan` and the recovery
  verbs remain directly invocable.
- Bind judgments to current content instead of timestamps: the descriptions
  answer carries `draft_digest`, the classifications answer carries
  `ledger_digest`, and a stale or unbound answer is re-judged rather than
  handed to `record`/`classify`. The classification judgment includes a
  self-contained `blocks[]` (`interval_id`, `block_id`, `start`, `end`,
  `description`), so it needs no draft or temporary artifact. Staging freshness
  compares the stored stage map with the ledger's own projection, and
  classification freshness uses the existing content digests.
- Add `setup` for once-only `~/.timesheet` creation: generated configuration is
  validated before any target effect, files are published by fsynced
  private temporary file plus no-replace hard link, and an existing config,
  env file, symlink or unsafe mode is reported and left byte-identical.
- Add the skill's `scripts/ts-env` locator (repository four levels above
  `scripts`, symlink-aware, then `$TIMESHEET_ROOT` or `PATH`) and the
  `next --help` capability check that builds once before the loop when the
  found binary predates the planner. The skill, README, Linux setup guide and
  `docs/architecture/timesheet-daily-workflow.md` document the new split
  between the regular Pi skill and the optional launcher/TUI.
- Stopped states now name the precise blocker for an unacknowledged source, an
  unselected project, a blocked plan, a finalized ledger that diverges, or a
  missing remote boundary. Source acknowledgement, attendance, identity
  resolution and plan reuse are never silently guessed; an explicitly
  user-authorized decision is still honored once where safe, and a repeated
  failed action is repaired only for an answer-file validation error, otherwise
  it stops with one line.
- Scope temporary draft and answer paths by the normalized store path and day,
  so separate stores cannot consume each other's judgments. Missing or empty
  finalized ledgers stop progression. Saved contribution revisions, including
  overnight origins, must match the ready plan before human gates are shown.
- GitHub comment collection now applies an overlapping `since` bound and stops
  after passing the requested day's end under created-ascending order. Genuine
  pagination and submitted-review search limits remain explicitly incomplete.
- GitHub pagination now honors valid terminal `Link` relations, including full
  final pages, instead of requesting a nonexistent page and reporting HTTP 422.
  Explicit next-page relations continue short pages. Genuine failures and
  collector limits remain incomplete; repository events remain bounded,
  windowed evidence rather than proof of complete history for a selected day.
- Recover classification judgments for saved days whose previews are blocked
  on unselected classifications. Answers bind to saved content and revision;
  stale answers are rejected before saved intervals change. Partial selections
  require fresh planning, and empty selections stop rather than loop.
- Add an operator-configured remote classification default and explicit
  `classify --configured-default` mode. New unselected intervals reuse the
  verified cached option without discovery queries. Explicit choices and saved
  days stay unchanged; approval and submission retain live semantic checks.
- Linux validation passed full race-enabled tests, vet and locator checks.
  `scripts/verification/regular-pi-smoke.mjs` drives the actual regular Pi runtime
  through human gates using local deterministic model/calendar/catalog fixtures.
  This does not verify live-model comprehension or production providers. macOS
  runtime was not tested.
- Development stamp remains `0.2.0-dev.5`; no version bump, release tag,
  adapter-version change or publication is part of this work.

## 0.2.0-dev.4

- Make `log today's work` one command (issue #179): `collect`, `draft`, `run`,
  `record` and `status` accept an omitted `--date`, which the binary resolves to
  today in the configured timezone instead of trusting the host clock, and Pi
  gets `.pi/prompts/timesheet.md` so `/timesheet log today work` reaches the
  skill with no Node adapter in the path. `run` writes its artifacts under the
  resolved day, `record` checks the reviewed draft against that day, and
  `status` opens it and prints the day summary. An explicit `--date` still
  wins.

- Annotate the one-day draft from an optional `work_hours` policy (issue #174):
  `days` use exactly `mon`…`sun` and one to four non-overlapping same-day
  `HH:MM` windows such as 09:00-12:00 and 13:00-18:00, validated offline so an
  invalid day, window, bound, or overlap fails `config_invalid` naming the
  value (an unknown key inside the block still fails the strict decoder).
  `draft`/`run` then set `outside_work_hours` with `work_hours_reason`
  (`day_not_in_policy`, `before_window`, `between_windows`, `after_window`, or
  `partial`); a block inside one window is untouched, nothing is rejected or
  resized, and a configuration without `work_hours` renders byte-identically.
  The draft also echoes the applied policy so each reason is checkable. The
  four-hour per-entry bound stays the remote's fixed policy, not configuration.
- Resolve every absolute provider anchor in the recorded day's timezone
  (issue #181, found while adding the annotation): meeting anchors and
  evidence-topic anchors/items stored as `07:00Z` on an `Asia/Ho_Chi_Minh` day
  now read `14:00`, so the block a reviewer reads, the topic-to-block
  attachment, the outside-work-hours claim and the entry `record` writes all
  agree with the day. No state, protocol or duration change; a re-recorded day
  corrects previously written times.
- Collect the merge-PR and comment-in-PR proof-of-work shapes (issue #175):
  a new `merged_pull_requests` search family attributes a merge only when
  `merged_by.login` names the configured user and `merged_at` falls inside the
  day's `[start, end)` window, and the events feed's `PullRequestEvent` maps to
  the same canonical `gh:{repo}:pr-merge:{number}` identity so provenance
  merges instead of double counting (a merge whose `merged_at` is outside the
  day degrades to the generic event, never to a merge attributed to the day). Issue comments are now classified from the
  documented issue document: a PR conversation comment is
  `pr_conversation_comment` grouped under the PR, otherwise it stays
  `issue_comment`; lookups are cached per run and bounded to 50, and an unknown
  answer or the ceiling marks the family incomplete with the count instead of
  guessing.
- Fix overnight staging (issue #173): a midnight-crossing ledger entry
  (23:00-01:00) now stages its recorded span onto the next local day instead
  of being read as unparsable and silently lost; zero-duration entries are
  skipped without aborting the day, and `stage --revise` reports the durable
  interval ids it replaced.
- Add the headless skill surface: `timesheet collect`, `draft`, `record` and
  `status` subcommands that construct the same session `serve` uses and drive
  its dispatcher in-process. No protocol, state-schema, store or submission
  behavior changes.
- Add the timesheet skill deliverable (`.agents/skills/timesheet/SKILL.md` +
  `workflow.md`, picked up through `.claude/skills/timesheet` and executable by
  any agent runtime with shell access, including the Pi agent): every
  deterministic step is a fixed binary subcommand; the single judgment step
  reviews only the compact one-day draft JSON. `workflow.md` states the
  ≤ 1,000 tokens/day target for that step.
- Recorded entries live in `<state dir>/entries/<date>.json`, merged
  idempotently per time block with full source references kept for checking.
- Add `scripts/tokens.py`: idempotent per-(date, runtime, session) token
  capture from Claude Code or Pi transcripts into `benchmarks/tokens.csv`.
- Development stamp `0.2.0-dev.4`; release stamping, adapter version and
  publication remain owner-gated. Existing state, entries and credential paths
  are untouched.

R6 improvements and upstream Pi dependency remediation remain tracked in
[issue #137](https://github.com/huyle-gradion/pilot-timesheet/issues/137).

## 0.1.1

- Standardize product, package, documentation and operator-facing names as Timesheet.
- Keep published history and existing tags unchanged; commits and publication use the work identity.
- Update application/adapter identity to `0.1.1`; protocol, state schemas, dependency pins and accounting behavior are unchanged.
- Existing state and credential paths remain user-controlled; naming examples do not migrate or replace durable records.
- Known upstream dependency limitations remain tracked in issue #137.

## 0.1.0

- Release the application as the main product: one compiled timesheet server and
  one pinned Pi consumer; legacy CLI and vault integration are retired.
- Promote dev.17 to `0.1.0` with no workflow, protocol, state-schema or dependency
  change. Local logging/migration/lifecycle have Linux and actual Darwin evidence;
  the owner supplied the final Darwin runner pass.
- Include the reviewed restart-durability and CLI exit-code documentation fixes.
- The owner explicitly accepted the known upstream Pi/Undici dependency risk for
  this release. The vulnerable bundle is unchanged; remediation remains open.
  This release is not claimed to be vulnerability-free or live-provider verified.
- Live credentials, model authentication and real Gradion submission remain
  unverified. Exact human approval is still required for every remote write plan.

Historical prerelease notes below retain their original evidence qualifications.

### Source cutover (`0.1.0-dev.17`)

R6 is the single-product cutover. It changes no `serve` protocol field, state
or store schema, and no local workflow behavior; `AppVersion` and the adapter
package stamp move to `0.1.0-dev.17` with dependency pins
`1.4.0`/`1.4.0`/`1.3.27` and lock integrity unchanged.

- Added `python3 scripts/verify-r6-macos.py`: a deterministic, Darwin-gated QA
  runner for the real pinned Pi local workflow and lifecycle. Uses only private
  synthetic fixtures, records screens/results/PIDs/checksums, and changes no
  application behavior, dependency pins or release identity.
- One Go target. `cmd/timesheet/main.go` is now the accepted 99-line server
  entry point, branded `timesheet`, exposing only `version` and
  `serve --config --env-file` with unchanged exit codes and credential
  loading. The temporary `cmd/timesheet` entry point was deleted; no
  alias, legacy flag, wrapper CLI or second server exists.
- Legacy retirement. 225 manifest-listed tracked files were removed from the
  working tree: the old `cmd/timesheet` subcommands and their tests, the
  sixteen legacy `internal/` package trees and tests, `cmd/fetch-git`,
  `cmd/fetch-calendar`, `cmd/benchmark`, the four `tests/*.go` files, the old
  product skill (`SKILL.md`, `workflow.md`, and the tracked
  `.claude/skills/timesheet` symlink, unlinked only), `scripts/sync-vault.sh`
  and the tracked `scripts/vaultgen` source/test fixtures. No directory was
  removed wholesale and no untracked owner file was touched.
- Module pruning. `go.mod`/`go.sum` keep only what `cmd/timesheet`,
  `internal/timesheet` and `internal/mcp` still use (`arran4/golang-ical`,
  `teambition/rrule-go`); the unused `charm.land/huh` tree was pruned by the
  installed toolchain with no version upgrade, module-directive or toolchain
  change. `go list ./...` is exactly those three packages.
- Public configuration. `cmd/timesheet/timesheet.example.json` is a minimal
  valid implementation-schema document with synthetic placeholders.
- Operator documentation. New root `README.md` documents the one build/launch
  path, fixed config/env inputs, the local `/timesheet` flow, explicit
  migration and reconciliation before writes, recovery/no-replay rules and
  honest readiness. `pi/README.md`, the application interface identity and command
  line (stale R4/dev.11 heading corrected; R1-R5 stage text marked historical),
  `docs/agents/credentials.md` (active `.env.local` policy replaces the retired
  Keychain-only guidance) and this changelog were updated.
- Documentation correction. The README no longer implies that unsaved staged
  interval edits survive a restart: staged and confirmed-but-unsaved interval
  changes are session-only, only committed state and durable ledgers reload, and
  startup never submits or replays anything on its own. Both the README and the
  implementation interface now separate top-level/`version` usage errors (exit `2`)
  from `serve` argument validation, startup and stream failures (exit `1`).
  No application, protocol, schema, dependency or release-identity change.
- Evidence. Linux, real pinned Pi 1.4.0 TUI through the unchanged
  `pi/launch.mjs` with `--binary` pointing at the built `bin/timesheet`, an
  isolated generated HOME/profile/config/env/state and synthetic loopback
  GitHub/ICS/MCP/model fixtures. `timesheet version` exits 0 with no resource
  files; extra `version` arguments and retired commands exit 2 with no remote call.
  Darwin arm64/amd64 cross-builds are compile-only. macOS runtime migration
  and local flow, live accounts/credentials and authenticated model
  readiness remain unverified prerequisites for full R6 acceptance. This
  snapshot is review-ready only: no commit, publication, tag or release.

### R5 lifecycle repair (`0.1.0-dev.16`)

Narrow lifecycle investigation/fix of the R5 consumer's pending-reload boundary,
without changing the `serve` protocol, state schema or any R1-R4 Go behavior
beyond the `AppVersion` bump:

- Root cause, established against the pinned Pi 1.4.0 source before editing.
  Pi's `AgentSession.reload` awaits `session_shutdown` handlers and then replaces
  the extension runtime. The dev.15 adapter fired `transport.shutdown()` without
  awaiting it, so the replacement runtime's `session_start` spawned a child
  while the interrupted collect's child still held the state store's exclusive
  `flock`; the new child exited `store_locked` before writing a frame, which the
  adapter reported as `transport_failed: child closed stdout (EOF)`. The
  interrupted command's failure continuation then opened a recovery custom
  screen after Pi had already reset extension UI. Reload's editor restore hid
  that screen without settling its promise, so the old command handler never
  returned. Pi's interactive loop awaits each submitted command and queues
  further input while it is pending, so the visibly entered recovery
  `/timesheet <date>` was queued in `pendingUserInputs` and never dispatched;
  a full process restart was the only escape.
- Generation ownership. Adapter state is created per factory invocation, never
  at module scope. A stale continuation cannot read or clear the replacement
  generation's guard, overwrite its day/review/collection or consume its input.
- Awaited disposal. `session_shutdown` is idempotent and returns the bounded
  transport shutdown, so Pi joins the prior writer before the replacement child
  starts. No replay, respawn or retry is added; an interrupted dispatched write
  remains an unknown outcome.
- Settled continuations. Pending input/confirm dialogs are aborted, custom
  screens are closed through their `done` callback, and every awaited UI call
  races a per-generation closed signal (the pinned `ui.custom()`/`ui.editor()`
  promises are not settled when Pi detaches a component). Disposal is the
  command's outcome: it unwinds instead of blocking Pi's input loop.
- Identity and packaging: `AppVersion` and the adapter package are
  `0.1.0-dev.16`; dependency pins stay `1.4.0`/`1.4.0`/`1.3.27` and the lock
  integrity entries are unchanged.
- Evidence: Linux, real pinned Pi 1.4.0 TUI through `pi/launch.mjs`, the compiled
  dev.16 child and generated isolated profiles with synthetic loopback
  ICS/MCP/model fixtures. A held collect across `/reload` recovered through a
  visibly entered `/timesheet <date>` in the same Pi process (replacement child
  only after the old child exited; exactly one new provider request). A pending
  classification read across reload followed by a different-date command opened
  the new review with no stale catalog screen. A pending approved write was
  applied by the loopback provider and left unacknowledged across reload; the
  replacement showed read-only submission status with the unresolved write,
  refused contradictory `not_applied` (`identity_conflict`) and resolved by
  adopting the observed entry with exactly one `log_time` call and no
  reapproval. `/new` during a pending model preparation left no orphan and the
  replacement human review opened its own day; idle reload and quit with
  pending work left no orphan. Cross-day approval selection and the
  UTF-8/byte-bound transport paths were not rerun (source unchanged since the
  dev.15 review). `durability_uncertain` UI remains source-reviewed plus
  accepted backend evidence. macOS, live accounts/credentials and authenticated
  subscription readiness remain unverified. This snapshot is review-ready only:
  no commit, publication or R6.

### R5 repair round two (`0.1.0-dev.15`)

Second boundary repair of the R5 consumer (F1/F2/F7 continuations and F9 from the
dev.14 re-review), without changing the `serve` protocol, state schema or any
R1-R4 Go behavior beyond the `AppVersion` bump:

- Envelope and identity validation (F1 continuation). The JSONL envelope is
  validated before use: a non-object frame, a non-string or mismatched `id`, a
  non-boolean `ok` and a malformed `error` object each fail the transport
  instead of becoming a fabricated `ok:false/internal` refusal. Startup
  validates the minimal status identity (application/protocol/store fields)
  before any comparison. A failed negotiation now leaves the transport in
  `failed`, stops the child and settles every startup/pending/queued caller;
  a later `ensureReady` rejects without spawning, and only an explicit reopen
  starts exactly one clean child. Malformed wire data is never presented as a
  pre-dispatch child refusal or as evidence of zero effects.
- Complete-frame byte bound (F2 continuation). The raw size bound is checked on
  each complete frame BEFORE decoding/dispatching and on an unterminated
  remainder, so a newline, trailing whitespace, chunk alignment or a pause
  cannot exempt an oversized frame. Frames are decoded with a strict UTF-8
  decoder: invalid bytes fail closed rather than being replaced silently.
  Frames exactly at the bound, split multi-byte characters and ordinary
  `write()` backpressure still succeed.
- Exact active approval selection (F7 continuation). The unkeyed
  `lastApprovalId` cache (state, initialization and writes) is removed. Submit
  resolves the current day's exact plan against one bounded, read-only
  `submission_status` read and requires an ACTIVE approval for that plan. The
  returned id is displayed before the explicit submit and is the id sent.
  Switching from a just-approved day A to an already-approved day B in the same
  Pi session now shows and sends B's approval; consumed, revoked and
  changed-plan approvals are not selectable, and no approval is silently
  reapproved, substituted, revoked or consumed.
- Display-cell paging (F9). Detail rows are wrapped with the pinned Pi
  display-cell/grapheme-aware wrapper at the detail component's real content
  width, and paging counts those physical rows instead of UTF-16 code units. The
  window budget subtracts measured chrome, the pinned Pi rows reserved outside
  the editor slot and the paging label, with no forced minimum window. Long
  ASCII/CJK/emoji/combining rows, including a sentinel at the very end, remain
  reachable via `j`/`k`/`g`/`G` at 40×24 and 60×26 and after restoring 100×36
  on both the edit picker and a full plan/detail surface; the controls stay
  usable and stored records are unchanged.
- Identity and packaging: `AppVersion` and the adapter package are
  `0.1.0-dev.15`; dependency pins stay `1.4.0`/`1.4.0`/`1.3.27` and the lock
  integrity entries are unchanged.
- Evidence: Linux, real pinned Pi 1.4.0 TUI over tmux, the compiled dev.15 child
  and generated loopback MCP/provider fixtures. Controlled fault children prove
  the malformed-identity/envelope failures, single spawn, no implicit respawn,
  explicit reopen, pending/queued settlement, the complete/chunked/unterminated
  oversized-frame rejections, the near-boundary acceptance, split UTF-8 and
  backpressure. The real TUI exercised two seeded zero-op plans/active
  approvals (A approved, then B submitted after switching days without a
  restart), cancel-before-submit, the consumed-approval boundary and long-row
  paging. macOS, live accounts/credentials and authenticated subscription
  readiness remain unverified; `durability_uncertain` UI behavior remains
  source-reviewed/accepted-backend prior evidence rather than a newly driven
  run. This snapshot is review-ready only: no commit, publication or R6.

### R5 repair round one (`0.1.0-dev.14`)

Boundary repairs to the first R5 consumer (F1-F8 of the dev.13 review) without
changing the `serve` protocol, state schema or any R1-R4 Go behavior beyond the
`AppVersion` bump:

- Transport lifecycle (F1). Readiness now means identity negotiation finished:
  concurrent callers share one startup and no ordinary operation is dispatched
  before it passes. `failChild`/shutdown settle the in-flight and queued
  callers exactly once instead of dropping them; a failure after dispatch is
  reported as an unknown outcome, never as a no-op. Stale-child frames and
  callbacks are ignored by generation, so a reopened child is never affected by
  its predecessor. `write()` returning false is treated as ordinary
  backpressure, not as a rejected write.
- UTF-8 framing (F2). Complete frames are decoded from buffered raw bytes, so a
  multi-byte character split across pipe chunks is preserved exactly; the frame
  bound is a real byte bound. Malformed, extra-id and mid-frame EOF responses
  fail closed and settle their caller.
- Evidence shapes (F3). `topics`/`meetings`/`sources` may be JSON `null` on a
  valid manual-only/empty-evidence day; the adapter and rendering treat that as
  an empty list, so those days open, review and edit normally.
- Origin identity (F4). Editing prefills from the canonical interval instead of
  an allocation's display endpoints (`23:00-01:00` no longer becomes
  `23:00-24:00`; a midnight-ending interval uses `00:00`, never the `24:00`
  label). After locked-spill navigation, removal/edit sends the origin day,
  interval id and the origin day's current revision together. Go remains
  authoritative for grammar, revision and overlap checks.
- Empty transitions (F5). Action availability follows dirty/revision state, not
  nonempty segment lists: removing the final interval keeps `Cancel staged
  changes` and `Confirm`; a confirmed-but-unsaved revision (including a
  confirmed zero-work draft) keeps `Save local`. The header and receipts
  distinguish staged dirtiness from a confirmed revision that is not yet
  durable.
- Address-bound identity (F6). `accept_observed` offers mapped AND changed
  entries and sends the actual `remote_id` with `local_id`, the observation and
  the current revision. `resolve_write` adoption lists every observed entry,
  including the already-bound identity left by an exact edit whose
  acknowledgment was lost; Go still enforces content, date and one-to-one
  checks.
- Truthful receipts (F7). The adapter consumes `WriteReceiptRow.status`
  (`confirmed`/`uncertain`/`unidentified`/`in_flight`/`stopped_unattempted`)
  and renders each reference plus the aggregate counts. A child envelope error
  during submission is shown as a proven pre-dispatch refusal; a transport or
  unexpected local failure is shown as an UNKNOWN remote outcome with read-only
  status/reconciliation next, never as "zero remote mutations".
  `persistence_failed` is non-consumption and `durability_uncertain` is
  distinct.
- Launcher scope (F8). `--extra-extension` and `--fixture-provider` and their
  forwarding/doc branches are removed from the shipped launcher; only the
  trusted adapter and the fixed isolation/tool flags remain. Acceptance
  fixtures are provisioned externally in a generated dedicated profile.
- Preparation/human ownership. The one model preparation tool and the human
  `/timesheet` form flow are mutually exclusive: a preparation call during an
  open review returns a bounded `status=busy` result and mutates no day, draft
  or collection, and the human flow refuses to start while a preparation call
  is executing.
- Identity and packaging: `AppVersion` and the adapter package are
  `0.1.0-dev.14`; dependency pins stay `1.4.0`/`1.4.0`/`1.3.27`; new
  `pi/.gitignore` keeps installed `node_modules/` out of publication without
  changing the lockfile's resolved packages or integrity pins.
- Repaired consumer behavior was exercised end to end with the real pinned TUI
  and the compiled child: shared startup/fail-closed framing probes, manual and
  empty-evidence days, final removal/cancel/confirm-empty/save/restart,
  canonical overnight and midnight-ending edits, spill-to-origin removal,
  mapped/changed/foreign identity decisions, bound-edit lost-ack recovery with
  no extra provider mutation, explicit create/edit approval and submission with
  per-reference statuses, `persistence_failed` non-consumption and an explicit
  retry, migration `no_change`, and one-request model initiation with zero
  model calls for direct workflows.

### R5 — restricted Pi launcher/adapter (`0.1.0-dev.13`)

First pinned Pi integration for the application, without changing the `serve`
protocol, schemas or R1-R4 Go behavior (only the `AppVersion` bump):

- `pi/launch.mjs` is the only daily entry point: absolute trusted paths, no
  shell, the pinned Pi CLI resolved from this package's own dependency tree,
  and a fixed isolation set (isolated workdir, dedicated profile directory,
  single trusted adapter extension, no ambient extensions/context
  files/skills/prompt templates/MCP, no built-in tools, in-memory session,
  offline automatic refresh/telemetry). Provisioning arguments are
  operator-only and path-shaped; the launcher and adapter never read the
  configuration or dotenv contents, and the child's environment is a
  whitelist of required OS variables plus explicitly named credential
  variables.
- `pi/timesheet.ts` renders the compiled child's complete local human workflow
  (typed intervals with cross-midnight locked spill and origin-day navigation,
  classification search/select, confirmation vs durable save, attendance,
  attempt-bound acknowledgment, read-only reconciliation with explicit
  identity decisions, exact plan preview → explicit approval → one-shot
  submission, submission status with address-bound `resolve_write` recovery,
  legacy migration preview/apply) and registers exactly one model tool,
  `timesheet_prepare`, bounded to one civil date with a ≤512-byte status
  result. The adapter verifies the effective tool surface at session start and
  fails closed (shutdown) on any mismatch, blocks every other model tool call,
  refuses all user shell dispatch, and keeps every operator capability out of
  the model surface.
- `pi/transport.ts` runs one session-scoped Go child over bounded, serialized
  JSONL with fail-closed identity negotiation (`AppVersion`, protocol, store
  and durable schemas); failures never respawn or replay automatically —
  reopening is explicit, and unresolved writes require explicit human
  resolution against a fresh observation. `pi/view.ts` renders untrusted text
  control-free with a bounded detail layout that can never push local
  controls off-screen.
- Accepted against the real pinned TUI with synthetic loopback sources and a
  fixture model: full local workflow with zero model calls; one natural
  language initiation produced exactly one model request declaring only
  `timesheet_prepare` with no follow-up; unknown-write uncertainty
  (`write_content_mismatch`) blocked submissions, refused a mismatched
  `adopt`, and resolved only by explicit human attestation; child termination
  surfaced `transport_failed` and replays nothing.


### Second R4 repair round (`0.1.0-dev.12`)

Boundary-scoped repairs to the first R4 repair (R4-F2 continuation, R4-F7,
R4-F8) without changing the closed R4F1/F3/F4/F5/F6 behaviors, schemas or
R1-R3 surfaces:

- A dirty session's DURABLE saved allocation footprint now participates in the
  plan dirty guard (R4F2 continuation). Confirming a final interval removal —
  including removing a prior-day spill whose allocation renders on the planned
  date — without saving emptied both session lists, so the guard previously saw
  nothing while the preview still rendered the saved work. Approval and
  consumption both re-render through the guard, so the stale saved view can no
  longer be approved or submitted; the durable state and the existing approval
  stay intact, and unrelated dirty dates stay usable.
- Zero-operation consumption reports its persistence outcome truthfully (R4F7).
  A commit failure BEFORE activation previously fell through to `all_confirmed`
  with an execution id and no durable execution. It now returns the new
  `persistence_failed` status with a `write_failed` blocker, no execution id, an
  honest `next_action`, and zero remote calls; the approval remains `active`
  because nothing was consumed, and a later explicit submit consumes it exactly
  once. Post-rename `durability_uncertain` remains sticky and unchanged.
- A zero-operation approval's legitimate EMPTY classification-choice proof is
  now durable (R4F8): the approval summary encodes `choices` explicitly as `{}`
  instead of omitting an empty map, so the established empty proof survives
  restart as a non-nil empty map. Pre-proof dev.10/dev.11 rows (absent/null)
  still reload as nil and cannot authorize anything; they are never
  reconstructed or backfilled at load. Non-empty dev.11 proofs remain valid and
  are still revalidated against the current catalog.
- New receipt status: `persistence_failed`. `version` reports `0.1.0-dev.12`;
  protocol schema remains `1` and state schema remains `3`. No state field is
  added or removed: the existing `choices` field is simply written even when
  empty (legacy rows that lack it may materialize it as `null` on a later
  update, which still reloads as missing proof and stays unauthorized). Existing
  state loads unchanged; no migration is created or implied.

### First R4 repair round (`0.1.0-dev.11`)

Boundary-scoped repairs to the R4 slice (review findings R4-F1 through R4-F6)
without changing R1-R3 behavior:

- Selected classification choices carry relevant semantic identity (R4F1):
  every planned operation binds the selected option's id/code/name/activity
  type, the plan identity and approval summary record that binding, and fresh
  authoritative results are compared per-option at BOTH approval and
  consumption. A relevant change (`classification_choice_changed`) or a choice
  that cannot be established (`classification_choice_unverifiable`) forces
  explicit reselection and a new preview/approval; unrelated catalog rows,
  queries and timestamps never invalidate an unchanged choice. Dev.10
  approvals predate the bound-choice field and fail closed for consumption
  instead of being trusted.
- Plan previews guard ALL open sessions by saved AND staged allocation
  footprint (R4F2): prior-day spill drafts, changed cross-midnight
  allocations, moved/removed contributions and unsaved confirmations touching
  the plan's affected dates block preview, approval and consumption with zero
  remote calls; unrelated dirty dates never block, and cancellation restores
  eligibility.
- Batch drift checks compare FULL entry content (R4F3): a re-read date is
  compared against the consumption baseline advanced only by this run's
  verified self-effects, so an outside change to an existing entry — including
  a previously confirmed self entry — stops later operations instead of being
  absorbed; the consumption baseline itself is never replaced.
- Edit verification requires the acknowledged identity to equal the approved
  bound one (R4F4): a different acknowledged id (`edit_identity_mismatch`)
  stays unresolved with both ids as distinct audit, never confirmed, never
  bound, never updating `LastObserved`. Write acknowledgments now parse only
  the documented `entry` resource kind.
- Unresolved-write readiness is scoped by affected allocation dates (R4F5):
  pending migrated and native writes gate a plan when their recorded date OR
  their locally resolved allocation dates touch the plan's dates, so a
  spill-day outcome whose recorded date is its origin still blocks an empty
  spill-day plan; `submission_status` surfaces the same relevance.
- Migrated adoption resolves the actual contribution (R4F6): the row's local
  identity must be confirmed durable work that timezone-resolves onto the
  recorded write date with the intended representable span, backed by the
  claimed migration crosswalk when one is named; wrong dates, missing local
  contributions, unknown crosswalks and cross-bindings all fail closed while
  the original outcome and its uncertainty stay preserved beside the explicit
  resolution.
- New stable codes: `classification_choice_changed`,
  `classification_choice_unverifiable`; new native intent error string
  `edit_identity_mismatch`.

### Native approval and verified submission (`0.1.0-dev.10`)

R4 slice: exact-plan approval, durable pre-write intent, and one verified
submission path. No R1/R2 behavior changes.

- Added the native approval ledger (state schema `3`, new `approvals`,
  `intents` and `executions` sections): `approve_plan` binds a unique
  occurrence approval (`apr-…`, never a plan alias) to the exact current
  rendered plan content after fresh precondition proof (plan identity,
  complete readable coverage, and the current authoritative catalog proving
  every selected classification). It never dispatches; `revoke_approval`
  revokes explicitly, and both records are immutable audit.
- Deriving plan status from the ledger: persisted plans no longer carry
  `approved`/`approval_available` flags; the preview result derives both from
  real approval state, while every plan operation now hashes its exact body
  including the deterministic edit audit reason.
- Added `submit_plan`: full fresh revalidation before any call, then one
  durable `in_flight` intent per operation committed BEFORE a single,
  never-retried dispatch, followed by a mandatory complete read-back whose
  content must match the exact intended body before a write is confirmed,
  bounded per-run re-checks for outside drift, and honest receipt statuses
  (`all_confirmed`, `partial_stopped`, `blocked_before_dispatch`,
  `durability_uncertain`). Zero-op plans consume their approval without
  dispatching anything.
- Added `submission_status` and `resolve_write`: every dispatched-but-unverifiable
  write is a durable unresolved reference (`wr-…`), visibly blocking further
  writing; recovery is an explicit human decision against the current complete
  observation (`adopt` an observed ID, or `not_applied` refused when a matching
  effect is visible), recorded as additive audit that never erases the original
  attempt or dispatches anything. Migrated legacy outcome rows are addressable
  through stable `mw-…` references by the same gate; rows without usable intent
  information stay visible blockers rather than resolved facts.
- Schema-2 files are converted in memory to schema `3` after their own
  validation: obsolete always-false plan authorization flags are stripped and a
  file carrying approval sections or any non-false/non-boolean claim is
  rejected (`state_invalid`). Approval authority is never read from migrated
  data.
- New stable codes: `stale_plan`, `plan_not_ready`, `unknown_approval`,
  `stale_approval`, `classification_unverified`, `unknown_write`.

### R3 continuation (`0.1.0-dev.9`)

Narrow continuation of the first R3 repair packet; no R1/R2 behavior changes:

- The inert approval audit preserves the approval binding's own historical
  `record_revision` and `remote_observed_at` beside the existing approval,
  revocation timestamps and status (F3 continuation). The values are copied
  verbatim from the legacy record and never inferred from the linked plan,
  whose revisions and observation timestamps may differ; the audit stays
  `inert: true` / `usable: false`.
- Repeat identity covers the declared legacy timezone per date (F9): matching
  per-date receipt evidence must agree on bytes AND timezone. The same bytes
  under a different declared legacy timezone are an `interpretation_conflict`,
  never an already-imported `no_change` result, while a repeated subset of a
  larger imported batch stays a valid no-change selection when both agree.
- `migration_apply` revalidates the fresh candidate before every successful
  no-change return (F9): a source or durable-target change since the preview
  is a structured `migration_stale` / `migration_conflict` failure instead of
  a false revalidation receipt, with the target untouched.

### R3 first repair packet (`0.1.0-dev.8`)

Boundary-scoped repairs to the R3 slice (findings R3-F1 through R3-F8) without
changing R1/R2 behavior:

- Plans are per-ALLOCATION (R3-F1): each date of a cross-midnight interval renders
  its own span and minutes, observes its own remote date, and binding identity is
  (origin interval, allocation date), so both allocations can bind to different
  remote IDs without stealing either mapping. An allocation without a
  representable same-day remote span blocks as `unrepresentable_projection`
  instead of being altered.
- Reconciliation surfaces occupied or orphaned identity collisions instead of
  skipping bound entries (R3-F7): an unmapped allocation overlapping an entry
  bound to another (or removed) identity blocks with `identity_conflict` /
  `orphaned_mapping`; unrelated mappings stay date-scoped and never globally
  block work.
- Foreign decisions (R3-F4) are bound to the exact current observation: they
  stop blocking equal text only, never authorize overlap, go stale on a changed
  observation, and survive identical fresh reads.
- The plan identity includes the exact readiness blockers (R3-F6), so a blocked
  preview and the ready preview after an explicit decision are distinct immutable
  records instead of one record whose state silently disagrees.
- The verified week contract validates required containers fail-closed (R3-F5):
  `days` must be a list of day objects and every day object must carry a `date`
  string and an `entries` list; a day without `entries` (or `entries: null`) is
  unreadable, never an empty day. Only `entries: []` is a valid empty result.
- Migration rejects a duplicated legacy local ID inside one source record
  (`ambiguous_local_id`) before guessing aliases or bindings, translates mapping
  and outcome references through a DATE-SCOPED crosswalk, and keeps the original
  ID/date on every audit row (R3-F2).
- Legacy outcome semantics follow the canonical predicate (R3-F8): resolved rows
  never block, only unresolved `in_flight`/`uncertain`/`unidentified` rows block,
  confirmed/failed/skipped are terminal history, and an unrecognised status is a
  `malformed_outcome` conflict rather than silently safe history.
- Migration preserves the complete bounded audit and fails loudly on excess
  (R3-F3): per-date review history with authored exclusions and attendance
  decisions, legacy plan operation contents and preconditions, approval/revocation
  timestamps, and per-category capacity bounds (`migration_capacity`) that reject
  the whole candidate instead of silently truncating.

### R3 explicit migration and read-only plans (`0.1.0-dev.7`)

- Added explicit, repeat-safe migration of confirmed legacy workflow schema-1 records. `migration_preview{dates}` validates a bounded explicit date selection and returns counts, conflicts, source hashes and a `migration_id` bound to the source bytes, dates/timezone and current durable target state. `migration_apply{migration_id}` re-reads and re-hashes every source, recomputes the candidate, rejects stale/conflicting input without mutation, and activates the whole candidate in one atomic state write through the existing durable path. Activation requires no opened day.
- Migration reads only configured, date-named regular files under `<legacy_root>/workflow/`; no recursive discovery, request-supplied paths, symlinked workflow directory/file, or writes into the legacy root, and the old store constructor is never called. The documented precondition is quiescent legacy writers; changed source bytes are detected before activation rather than pretending an unlocked multi-file read is atomic.
- Preserved through migration: confirmed work and IDs (or recorded deterministic mappings), projects/descriptions/classifications, evidence references plus non-secret provenance pointers, legacy revisions/`updated_at`, stable local/remote mappings with the binding-versus-last-observed distinction, submitted outcomes, legacy plans and approvals, and unconfirmed carry fragments. Raw collected provider documents are not imported. `uncertain`, `unidentified` and unresolved `in_flight`/`failed` outcomes remain durable blockers that R3 never replays or clears; historical approvals are inert (`usable: false`). Repeated identical source bytes are a `no_change` result with no duplicate data, while changed sources and occupied destinations are conflicts.
- Added a disabled-by-default read-only Gradion boundary over the existing MCP transport. It verifies exactly `timesheet__get_my_week` and `timesheet__get_classifications`, does not require write tools, exposes no generic call operation, and resolves the credential by NAME only (`GRADION_MCP_TOKEN` by default) with the existing dotenv/inherited precedence. Unrecognized shapes, reference envelopes, failed reads and partial coverage are explicit non-empty failures; a failed observation replaces the stored complete observation.
- Added `classifications{query}` with a bounded catalog identity and `select_classification{day,revision,interval_id,catalog_id,classification_id}` for an option the catalog actually returned. A partial/filtered catalog cannot prove an option absent; defaults and migrated hints are suggestions, never silent selections.
- Added `reconcile_remote{day}`, explicit `resolve_remote{day,revision,observation_id,remote_id,local_id,decision}` (`adopt`/`accept_observed`/`foreign`) and `preview_plan{day}`. A fresh read never binds an identity; adoption is a human one-to-one decision against a specific observed entry; changed mapped content requires explicit acceptance; stale revision/observation IDs are rejected. Plans render exact create/edit operations with stable identities and observed preconditions, omit unchanged mapped work, preserve remote-only fields on edits, never delete, never silently shorten intervals, and are content-addressed so audit timestamps alone do not change the plan. Identical fresh observations keep a stable `plan_id`; actual local/remote changes invalidate it.
- State schema `2` adds durable `migration`, `remote` and `plans` sections. A schema-1 file is validated in its own shape and upgraded in memory; schema `2` is activated only by a successful durable save/transaction. `status` reports `store_schema`/`durable_schema` and compact counts. A schema-1-only loader fails closed on a schema-2 file (`state_invalid`, file intact), and a schema-2 file claiming approval authority is rejected.
- New stable error codes: `migration_disabled`, `migration_invalid`, `migration_stale`, `migration_conflict`, `session_open`, `remote_disabled`, `remote_unavailable`, `remote_incomplete`, `remote_unreadable`, `stale_observation`, `unknown_remote`, `unknown_local`, `identity_conflict`, `unknown_classification`, `stale_catalog`, `no_confirmed_work`, `dirty_session`. `version` reports `0.1.0-dev.7`; protocol schema remains `1` with additive operations.
- R3 limits: no approval, submission, remote write, delete, retry, Pi adapter, macOS/live-account claim, or R4 execution. `preview_plan` distinguishes `ready` from `blocked`; it can never produce an approved plan.

### R2 F7 correction (`0.1.0-dev.6`)

- Submitted-review collection no longer discards reviews that were already verified when a later candidate pull request's reviews request fails. The family still reports `failed` — the failed-source gate is unchanged — but its `count` and retained topics now reflect the verified reviews, and the detail names the HTTP failure, the number retained, and states that remaining review coverage is unverified. A search-phase failure still returns no items because none have been verified at that point. `version` reports `0.1.0-dev.6`; protocol and state schemas remain `1`.

### R2 second repair (`0.1.0-dev.5`)

- Attendance decisions are now attempt-bound for every action: `decide_meeting{day,meeting_id,decision,collection_id}` requires the exact reviewed calendar collection ID. A missing ID fails `bad_request`; an ID that is not the day's current calendar attempt fails `stale_collection`; both reject without mutating evidence, so an action built from an earlier review can no longer land on a rescheduled target. This is an intentional breaking request requirement for the unreleased protocol (protocol schema remains `1`): callers must send the `collection_id` from `collect` or the review's `sources` entry. Meeting references additionally fingerprint the current target, including an overridden occurrence's rescheduled start, so a moved instance no longer shares an unchanged RECURRENCE-ID-only reference. Carrying an already recorded decision across recollection for an unchanged instance (`UID|Start`) remains separate from authorizing a new action.
- Calendar date-only correctness: all-day occurrence and boundary instants are resolved from their declared DATE values with the accepted first-real-instant day resolver, date-only recurrence sets expand on civil dates, and the exclusive end is resolved from its own date. A Santiago event declaring September 6-7, 2026 appears only on September 6 with DATE labels `2026-09-06`/`2026-09-07`, and is absent on both September 5 and the exclusive-end September 7; no `01:00` boundary is carried into the end date. Recurring, RDATE/EXDATE and overridden date-only paths use the same rule; all-day overrides with DTEND no longer fail end resolution.
- Recurrence candidate selection is span/duration-aware: the expansion look-back is derived from each series' occurrence duration (timed) or calendrical span (date-only), with exact interval overlap deciding membership. A weekly three-day all-day occurrence October 19 through exclusive October 22 is present on October 21 and absent on October 22; a monthly timed occurrence October 1 09:00Z through October 11 09:00Z is present on October 10. A real expansion bound (500 instances) still reports incomplete coverage instead of claiming the omitted instances complete.
- Historical review discovery no longer equates a resource's last-updated date with a review submission date: candidate PRs are the repository-wide `reviewed-by:` superset with no `updated:` date qualifier, and each candidate's reviews are filtered by their exact `submitted_at`. A PR reviewed on the selected day and updated later still yields that review; out-of-window reviews stay excluded. Candidate-page, candidate-count, search-total and `incomplete_results` bounds are reported as `incomplete` with honest counts.
- Commit attribution no longer falls back to a commit-author email when the response has no GitHub user document. Unverified ownership is omitted and reported as a coverage limitation in the commits family detail and observable through the source status; legitimately verified identities (user document, `user.login`, reviewer `user.login`, `actor.login`) remain supported, and no email registry or new identity subsystem was added.
- `version` reports `0.1.0-dev.5`. State schema remains `1` with no new durable fields; existing state loads unchanged. No release tag, publication, or migration is created or implied by this unreleased build.

### R2 evidence collection (`0.1.0-dev.4`)

- Added configurable source collection over HTTPS-reachable providers, driven by a new `collect{day}` protocol operation: GitHub (six REST families — commits, `author:`-filtered PR search, submitted PR reviews via `reviewed-by:` search candidates verified at `pulls/{pull_number}/reviews`, repository-wide PR review comments, issue comments, and the bounded events/discussions feed) and calendar feeds (ICS; full recurrence sets, overrides, attendance PARTSTAT). Attribution is strictly by provider-named actors verified inside provider responses (commit user document or author email, `user.login`, reviewer `user.login`, `actor.login`); a response naming another actor is dropped, and `updated_at` only selects search candidates, never attributes activity. Pagination runs to exhaustion with honest bounds — family page cap `maxGithubFamilyPages` (20), the documented search 1,000-result/`incomplete_results` cap, the submitted-review candidate cap (`maxReviewPRCandidates`), the documented events window (30 days, 300-event feed cap), and the 50-item per-topic cap (overflow is recorded on the topic as `truncated` with a dropped-item count and makes the source `incomplete`) — always reported with status-only details.
- Canonical provider identity: the same action seen through two endpoint families (issue comment, PR review comment, submitted review) carries one reference and merges provenance; distinct interactions stay distinct. Topics group by retained provider resource identity (`resource`), not rendered titles.
- Collection is verify-only: evidence proposes topics and meetings but never creates worked minutes. Explicit attendance decisions (`pending`/`attended`/`skipped`) are separate from calendar PARTSTAT and from timesheet confirmation.
- Added the incomplete-source review gate: `confirm` and `save` fail closed with `source_unacknowledged` while an enabled source's attempt for the day is not `complete` and not acknowledged — including the never-collected state, `incomplete`, `failed`, and `missing_credentials`; a never-collected source yields a structured gate error (no crash), and disabled/manual-only R1 behavior is unchanged. `acknowledge_source{day,source,collection_id}` requires and verifies the exact reviewed collection attempt ID; missing IDs fail `bad_request` and stale IDs fail a new `stale_collection` error without mutation, so a second collection cannot be silently acknowledged from a stale review action. Meeting decisions verify the meeting's current evidence identity and optionally the reviewed `collection_id`.
- Review decisions are copy-on-write at the store boundary: failed pre-activation evidence writes (`write_failed`) leave review state, disk, gate state, and absence of evidence keys exactly as they were; a failed first collection can no longer leave a null evidence entry that a later successful write persists into an unreadable state (`state_invalid` after restart). Post-rename uncertainty (`write_uncertain`) is unchanged.
- Calendar day-window correctness: all-day DTEND is exclusive (a one-day event with DTEND the next date no longer appears on that next date), and cancelled/overridden instances are selected by their replaced-or-own windows rather than being emitted on every unrelated day — while cancelled series/overrides and all-day evidence remain proposals with their provenance.
- Recurrence-set correctness with the pinned `teambition/rrule-go v1.8.2` and `arran4/golang-ical v0.3.7` (dependencies unchanged from the R2 base commit; `go.mod`/`go.sum` untouched): instances are RRULE ∪ RDATE − EXDATE with per-property TZID/value-form handling and de-duplication, for timed and all-day series; an unresolvable or form-mismatched RDATE/EXDATE value records an incomplete-coverage issue instead of silently claiming complete evidence. Wall-clock recurrence across DST transitions is unchanged.
- State schema remains `1`; the optional `Evidence` map gains only additive optional fields (verified acknowledgment identity receipts, item `resource`, topic truncation counts, decision `collection_id`). A pre-R2 binary still fails closed (`state_invalid`) on a file this build writes; pre-R2 files load unchanged.
- New protocol receipts and errors: `collect`, `acknowledge_source` (now with verified `collection_id`), `decide_meeting`; new error code `stale_collection`. Error details carry HTTP status only; feed URLs, tokens, and raw provider documents are never echoed.
- Configuration gains an optional `sources` block (GitHub `user`/`repositories`/`api_base`, calendar feed user, `required_env` extension); a config without `sources` behaves exactly like R1 (`no_sources` collection, no gate).
- The development entry point's usage text now describes the implemented collection scope; the `serve` protocol in [docs/agents/timesheet-interface.md](docs/agents/timesheet-interface.md) is the authoritative interface.
- R2 limits unchanged from R1 beyond the above: no migration, Pi adapter, remote reconciliation, submission approval, or remote write; evidence never creates worked minutes.

### R1 timezone continuation (`0.1.0-dev.3`)

- Fixed local day-boundary resolution when a transition skips midnight: `time.Date` normalizing a nonexistent `00:00` into a neighboring date is no longer treated as proof that the civil date is absent. An existing date now starts at its first real instant, so ordinary work on such a date is accepted and allocated correctly (for example 09:00-10:00 on 2026-09-06 in `America/Santiago` and 2026-03-08 in `America/Havana`, 60 minutes each). A wholly skipped civil date is still not reported as work. A cross-midnight interval spills with real local continuation labels: Santiago 2026-09-05 23:00-01:30 totals 90 minutes, split 60 on September 5 and 30 on September 6 labeled 01:00-01:30, and totals agree between save and restart views. Wall times in the missing midnight hour still fail `nonexistent_time`; no zone or date special cases and no new dependencies were added.

### R1 repair (`0.1.0-dev.2`)

- Save validation now uses the candidate durable store: persisted state for other dates plus the requested day's confirmed intervals. Unsaved confirmations on other open days can be planned and reviewed in-session but are never saved implicitly and cannot make a save appear valid. A save receipt describes exactly what restart will load for that day. A dependent save that would still overlap fails with `overlap` and an actionable conflict message.
- Fixed explicit removal of a day's final interval. `confirm` accepts an empty staged draft when the day already had confirmed work, records a zero-interval revision and history entry, and `save` persists it. Cancelling a staged removal still restores the confirmed interval.
- Interval validity, elapsed duration, overlap, and midnight allocation now use each day's recorded IANA timezone. Spring-forward intervals use elapsed time (for example 60 minutes for 01:30-03:30 in `Europe/Amsterdam`); fall-back intervals include the repeated hour when unambiguous. Nonexistent local times fail with `nonexistent_time`; repeated-hour endpoints fail with `ambiguous_time` rather than guessing. Existing saved days keep their recorded timezone when configuration changes.
- Added a test-only, narrowly scoped fault-injection seam and split save failures at the rename boundary. Pre-rename failures return `write_failed`, preserve the previous file, and roll memory back for a safe retry. Post-rename directory-open/sync/close failures return `write_uncertain`, keep the activated in-memory snapshot, block blind retries, and surface `durability_uncertain` in `status` until restart.
- State schema remains `1`; the build adds stricter timezone validation. Unreleased `0.1.0-dev.1` state containing a saved nonexistent or ambiguous wall time now fails `state_invalid`; no automatic migration is provided.
- R1 limits are unchanged: no GitHub/calendar collection, migration, Pi adapter, remote reconciliation, submission approval, or remote write.

### Local daily workflow (R1, `0.1.0-dev.1`)

- Added the development entry point `cmd/timesheet` and `internal/timesheet` for the local, review-first daily workflow. This is not a release and not a second production CLI.
- Added a persistent line-delimited JSON `serve` session with strict request schemas, echoed request IDs, expected revisions, stable error codes, and protocol schema `1`. `version` reports the development build identifier and loads no credentials.
- Added manual typed intervals (`DATE HH:MM-HH:MM PROJECT[/CLASSIFICATION] DESCRIPTION`), staged add/edit/remove, review with accurate totals, explicit confirmation, atomic local save, revision, cancellation, status, and restart recovery.
- Rejected zero/negative/invalid intervals and unresolved overlaps without changing confirmed work. Multiple evidence references add no minutes. Cross-midnight work is allocated explicitly to both local dates without wrapping or double-counting.
- Added binary-only JSON configuration and environment-file loading with inherited environment precedence, documented dotenv syntax, lazy required-variable checks, and value-free diagnostics. Local save requires no remote credential.
- Added single-writer state locking, atomic `0600` state replacement, and failed-write rollback so cancellation or a failed write cannot report success or erase the last confirmed state.
- Documented the R1 public interface in [docs/agents/timesheet-interface.md](docs/agents/timesheet-interface.md) and added placeholder-only environment examples.
- R1 limits: no GitHub/calendar collection, migration, Pi adapter, remote reconciliation, submission approval, or remote write. No legacy store is read, rewritten, or deleted.

### Project kickoff

- Accepted the [timesheet update specification](docs/timesheet-spec.md) and prepared an owner-mediated implementation plan and first worker packet.
- Targeted a persistent compiled Go workflow with local Pi review, GitHub/calendar evidence, user-supplied working intervals, explicit migration, and exact-plan submission approval.
- Replaced the proposed Keychain-first credential flow with binary-only `.env.local` loading and process-environment overrides.
- Added Git exclusions for `.env.local` and environment-specific local credential files. Credential contents were not inspected.

### Release policy

- Initial update target: `0.1.0`, with development/alpha/beta/rc identifiers as documented in the [execution plan](.omo/plans/timesheet.md).
- No update application implementation, release artifact, tag, or production readiness is claimed by this kickoff.
- Public-interface compatibility covers CLI/input grammar, Pi protocol, configuration names/precedence, and state migration. Stable `1.0.0` requires accepted end-to-end behavior and an owner release decision.
