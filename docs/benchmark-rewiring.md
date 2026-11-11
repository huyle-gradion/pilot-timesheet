# Benchmark rewiring: one pipeline instrument, one harness instrument

> **Historical.** The `internal/run` and `internal/benchmark` code this design note
> cites was removed in the CLI cut-over (f2c80880); the current flow is
> `timesheet log` (see the [README](../README.md)).

**Purpose.** Settle, before any code, what `internal/benchmark` is allowed to measure now that
`internal/metrics` measures the harness session — and retire the pieces that answer the wrong
question. Design only: nothing here is implemented. Every anchor was re-read at `main` `63f3018`,
and the stack between `63f3018` and this branch touched only the personal workflow state file
since moved out-of-tree (hash-verified in review); where an anchor from #69/#73 had moved or changed status, the
correction is stated inline. The independent review rounds are recorded in the change's review
record, not here.

## Two instruments, one boundary

| | pipeline cost — `internal/benchmark` | harness session cost — `internal/metrics` |
|---|---|---|
| question | what does running the `timesheet` CLI cost per day? | what did the agent's session cost? |
| unit | one day's CSV row | one run record + one day's aggregate |
| measures | wall clock + peak RSS of a `timesheet` invocation; the `[AI]` editorial child call's tokens; entries | per-call input/output/cache-read/cache-write/reasoning, durations, attributed steps |
| store | `benchmarks/metrics.csv` (`csv_store.go:15`; personal data, untracked in the repository) | `~/.timesheet/metrics` (`metrics.Store`) |
| never answers | harness tokens, cache-hit rate, model price | wall clock, RSS, editorial spend (day-granular, copied from the record) |

Both feed the 3.6 proof; neither substitutes for the other. ADR-0006:24–27 keeps both deliberately.

The `store` row above describes where each lane's DATA lives, which is still true. *(Superseded
2026-10-21 by #126 for the CHART only: `cmd/benchmark -chart` no longer charts the CSV's token
columns — it reads the metrics store's day aggregates. The CSV row remains the store for
`-record` and `-report`. See [Reversal, 2026-10-21](#reversal-2026-10-21-126-the-charts-source-moves).)*

### Reversal, 2026-10-21 (#126): the chart's source moves

The pipeline lane keeps its wall-clock/RSS CSV; the chart stops reading it. `cmd/benchmark
-chart` now reads the metrics store's day aggregates and draws each day's
`session_total_tokens` — the same number `metrics report` prints — and prints its source on
the output as `source: <dir> day aggregates (harness session tokens, reasoning excluded; day
totals may include synthesized lower bounds; see: timesheet metrics sessions --format json ->
total_source)`. Costs, stated:

- The chart no longer answers the pipeline question "what did the `timesheet` CLI cost per
  day?" for tokens; it answers the harness question. `-report` still answers the pipeline
  question from the CSV, so the instrument as a whole loses nothing — the split is now
  explicit in one flag's help text instead of implied.
- The ceiling tick on the chart becomes a REFERENCE, not a budget, and the chart SAYS so:
  after the chart it prints `reference: 1,000/day is the editorial-child soft target;
  [PASS]/[OVER] compares harness session tokens against it — it is not a harness
  budget. (use -ceiling to change)`. The verdict is `[PASS]` when every charted day with a
  KNOWN total is at or below the ceiling and `[OVER]` when any is above it. When no charted
  day carries a known total the comparison clause is replaced by `[n/a] — no charted day
  carries a known total. This is not a harness budget. (use -ceiling to change)`, so an
  unmeasured chart is never called a pass. 1,000 still
  has one home and still governs the editorial-child lane as a soft target, and no harness
  budget is claimed (see §2's supersession note and OWNER-DECISION-7).
- A day's figure can include a synthesized lower bound (`metrics import --declare-total`), so
  the source line names that caveat rather than leaving it to the aggregate JSON; the
  per-session label is `total_source` in `metrics sessions --format json`.
- The chart requires a metrics store to draw anything. With no store it prints one line and
  no axis; with a corrupt aggregate it exits 1 naming the date. The day axis therefore
  reflects imported sessions, not pipeline rows.
- The `-metrics-dir` flag is new and names the store; it is distinct from `-storage-dir`,
  which keeps its workflow-store meaning.

*(2026-10-22, #128.)* The ceiling bullet above is **superseded**: the chart's fixed
1,000 — and every budget word around it (`[PASS]`/`[OVER]`/`[n/a]`, `[░] Available
budget`, `[!] Ceiling overage`, the `reference: … editorial-child soft target …`
line) — is gone. The chart's ceiling is now DERIVED: the largest charted day with a
known total, snapped UP to the next multiple of 1,000 (4,757,111 → 4,758,000), with
bars scaled proportionally, the peak day as the full 40-char bar (a marker, not a
proportion — the title carries the real ceiling), and a single line
`ceiling: largest charted day rounded up to the next 1,000 — not a harness budget.`
A range with no known total states `(Ceiling: unknown)` and draws no bars. `-ceiling`
remains an explicit FIXED override (a day over it is a full bar, never an overage
marker); `-report`, the CSV and the `source:` line from #126 are unchanged.

### Corrections to anchors as filed

- **`internal/benchmark/parser.go:16-40` is stale.** The invented union is `rawTokenRecord` at
  **parser.go:19–52**; `ParseTranscript` is **:55–97**; the summing function is `extractTokens`
  at **:120–175**. `:16` is `ErrEmptyTranscript`.
- **ADR-0002 no longer carries the harness-agnostic requirement.** ADR-0002:4 records it
  *superseded by ADR-0005 on 2026-10-09*; ADR-0005 confirms the supersession and does **not**
  restate harness-agnosticism. The only surviving statement is ADR-0002:8, as history. The
  binding text is therefore **ADR-0006:31–36** (“Provider adapters, fail closed”). #73’s record
  already flags this (state.md:2371 item 7).
- **`internal/benchmark/models.go:10-12` is unchanged** (`DefaultTokenCeiling int64 = 600`), and
  ADR-0006:139–141 already calls it stale — BUT the same lines record a decision to LEAVE IT
  UNCHANGED (“is not changed”), and ADR-0006:119–120 records the parallel refusal for the legacy
  code (“the legacy code is left as-is”). **This design contradicts both recorded refusals and
  therefore requires an amendment to ADR-0006 — it is not “consistent with a recorded position”.**
  That amendment joins the owner-decision list (OWNER-DECISION-1).

## 1. Parser disposition: retire it — do not reuse it, do not share it

A note on the choice itself, because the issue names two options and this is neither. #69 frames
the disposition as “rewire its token accounting onto the metrics adapters, or retire it”; the
design takes a **third option**: retire the *parsing* (no transcript is read by the pipeline lane
again) and source the token columns from the day’s workflow record. It satisfies the issue’s
intent more strongly than either named option — the pipeline lane stops touching harness data
entirely, so “no harness shape in a shared code path” holds by construction — and the reason is
ADR-0006:92–97: the editorial child call is day-granular and copied from the record, so
re-parsing a harness transcript to produce the editorial lane recreates exactly the conflation
this design removes. The divergence from the issue’s two named options is surfaced to the owner
below (Owner visibility, not a decision).

**The disposition:** `internal/benchmark` stops parsing transcripts entirely. Delete
`rawTokenRecord`, `extractTokens`, `ParseTranscript`, `ParseTranscriptFile`,
`ErrEmptyTranscript`, `TokenUsage`, and the `cmd/benchmark` `-transcript` flag
(`cmd/benchmark/main.go:75`, used at :124–134). The CSV's token columns are filled instead from
the day's workflow record (mechanism in §3). `internal/benchmark` gains **no internal import**;
`internal/metrics` is untouched; no new leaf package.

*Rejected — benchmark imports metrics.* It would re-parse a harness transcript to produce the
editorial lane — the conflation in ADR-0006:92–97 — and ADR-0006:119–120 deliberately kept the
accounting surface from depending on the chart path.

*Rejected — a shared leaf parser both import.* It would move pi/claude decoding
(`adapter_pi.go`, `adapter_claude.go`) out of a tested, accepted surface to serve a need that
disappears once the record is the source. Refactor with no consumer.

The parser is not merely ugly, it is wrong, three ways: `extractTokens` adds counters from up to
four nesting locations **on the same line** (parser.go:120–175, called once per line at :82), so
a line carrying both top-level and nested usage is double-counted — the defect ADR-0006:16–17
already names; `TotalTokens` is then summed as input+output+cache (:95), assuming the three
components are disjoint, which no adapter guarantees; and `rawTokenRecord.CacheCreationInputToks`
(parser.go:31) is decoded and **never read**, so Claude’s cache-write is silently dropped from
both `CacheTokens` and the total — the sharpest live defect, and it needs no exotic input shape.

Cross-package consequence: `editorial.OrchestrationUsageFromBenchmark` (usage.go:24–29) consumes
`benchmark.TokenUsage` and has **no production caller** — only `usage_test.go:65`. It goes with the
type; the run package already feeds `editorial.Usage` from the record.

Harness-agnosticism holds better after this: the pipeline lane never reads a transcript at all,
and the harness lane is vendor-agnostic only through the adapter registry (adapter.go:46–62).

## 2. One cost ceiling, one number

**The number survives, its home moves.** The only surviving number is the owner-recorded **1000**
(`editorial/model.go:34–37`, the override of the specification's 600; ADR-0006:139–140). Both
`internal/editorial` and `internal/metrics` already import `internal/workflow`, while `metrics`
deliberately imports nothing else of this repository (models.go:5–8) — so the constant is defined
**once in `internal/workflow`** and `editorial.SoftDailyTokenTarget` and
`metrics.BudgetTargetTokens` become named aliases of it. No mirror, no lockstep test, and no
`package metrics` test importing `editorial` against the documented package boundary. The
owner-facing names and the number are unchanged. An amendment to ADR-0006:139–141 (which records
“is not changed”) and to `docs/timesheet-redesign.md:147` (“left unchanged deliberately”) is
required for this — OWNER-DECISION-1.

**Retired:** `benchmark.DefaultTokenCeiling = 600` (models.go:11) is **deleted, not re-pointed** —
a formatter package should state no budget.

**The flag finally reaches both renderers.** `cmd/benchmark` takes `-ceiling`
(main.go:82) and passes it into **both** renderers. That fixes a live defect: today `-ceiling`
reaches only the chart (main.go:170 → chart.go:15–20); `RenderReport` hardcodes the constant at
**report.go:118 and :127**, so the flag is inert for the compliance line. The flag's default
expression was the deleted `benchmark.DefaultTokenCeiling`; its default becomes the shared
`workflow` constant, so the untouched invocation keeps the owner's 1000.

**Import shapes after the slice:** `internal/benchmark` is stdlib-only (R3); the record mapping
lives in `cmd/benchmark`, which imports `internal/workflow`; and `editorial` KEEPS its
`benchmark` import — not for the parser, which §1 deletes, but for
`DefaultMeasureRSS` → `benchmark.CurrentProcessRSSMB` (usage.go:31–32, called at
synthesize.go:121, the `Options.MeasureRSS` contract). No cycle anywhere: it holds because
`internal/benchmark` imports nothing internal (R3), not because the old
`cmd/benchmark → editorial → benchmark` path survives (it is no longer needed by the constant).

Semantics: the ceiling governs the **editorial-child token lane only** — precisely the lane the
CSV's token columns carry — and it NEVER governs harness-session tokens, which have no ceiling →
OWNER-DECISION-7. The chart's own nil-total path is stated in §7, the same unknown rule applied
to the bar. The same
number is already applied at synthesize time (`internal/run/synthesize.go:41`,
`editorial/synthesize.go:103`, `editorial/config.go:55–60`), so chart and budget compare
one lane to one number.

*(Superseded 2026-10-21 by #126: the ceiling's home, value and editorial-child meaning stand,
but the CHART no longer compares that lane. It compares a day's `session_total_tokens` against
the same 1,000 as a REFERENCE line. No harness budget is claimed and no number is invented for
harness tokens — the reversal is in the label and in the ADR, not in a second constant. See
[Reversal, 2026-10-21](#reversal-2026-10-21-126-the-charts-source-moves) and
OWNER-DECISION-7.)*

## 3. Record-fed columns: the mechanism

The token columns are computed, not parsed. The day's record supplies them through the interface
that already exists for exactly this shape: `metrics.RecordSource`
(**internal/metrics/aggregate.go:15–18** — `GetDay(ctx, date) (*workflow.Record, error)`),
satisfied by `workflow.JSONFileStore` (**internal/workflow/file_store.go:75**), wired in the CLI
by the precedent at **cmd/timesheet/metrics.go:744–750** (`workflow.NewJSONFileStore(resolveStorageDir(dir))`).
`EditorialInput/Output/Cache` are the fields the record already accumulates at synthesis time
(**internal/run/synthesize.go:104–115**), reading the same `rec.Usage` shape
(`reconstructSpent`, :75–83).

Constraints and traps, named:

- **The mapping lives in `cmd/benchmark`, not `internal/benchmark`** — R3 holds the package to a
  stdlib-only import set, so the record reading is a `main` package wiring.
- **`-storage-dir` is the wrong store today.** `cmd/benchmark`'s existing flag
  (main.go:136–143) reads the LEGACY store (`internal/storage` `NewJSONFileStore` →
  `GetDay` on `storage.TimesheetRecord`), while `EditorialUsage` lives in the workflow store
  (`internal/workflow/file_store.go:46` is the `<root>/workflow` join). Reusing the flag
  untouched would read the wrong file.
  The flag is re-pointed to the workflow store (same operator meaning: “where the records live”).
- **A date with no record** (or no store file) writes **empty cells** for the token columns —
  never zeros, per §7 — while wall clock and RSS are produced by the run itself and do not depend
  on the store. `entries` comes from the record's entry count; a record-read error makes the
  `entries` cell EMPTY with a warning on stderr, never a silently-swallowed zero (the current
  code swallows the error, main.go:138–142).

## 4. The explicit-date gate: nothing lands in a day lane without `--date` (#73b)

The recorded constraint, verbatim: **“nothing flows into a day's lane without --date.”** The
gate below is the mechanism that implements it.

Today a day can be attributed with no operator-stated day, three ways: `metrics import --date`
defaults to empty (metrics.go:95); `Import` then accepts a date derived from the transcript’s
first timestamp in **UTC** (**adapter.go:291–293**); and `BuildSession` may set `LoggedDate` from
the calls’ local step date (`AssociationLocalStepDate`, **session.go:189–212**).

**Where the gate lives:** `metrics.Import` (**adapter.go:81–124**), the single funnel both
frontends use. `opts.Date` becomes required and validated (`workflow.ValidateDate`, as the CLI
already does at metrics.go:110–115); the `startedAt` derivation is removed.

**The error:** a typed `ErrDateRequired` —
`metrics: import requires an explicit --date YYYY-MM-DD; no day is inferred from the transcript or the clock`
— surfacing through the existing parse-error path at exit 1 (metrics.go:130–133,
`ExitError = 1` at main.go:41).

**Prospective, not retrospective.** The gate governs NEW imports. Sessions stored before it
land keep their derived/absent associations, so legacy rows can still flow into a day's lane
without an operator-stated date until they age out or are re-imported — stated here where the
rule is, not only in Deferred.

**Consequence:** with `opts.Date` set, both `rec.Date` and session `LoggedDate` take that value
with `Association = import_flag` (metrics.go:156–161). `AssociationLocalStepDate` becomes
unreachable for new imports and survives only for stored legacy sessions; the aggregate is
unchanged (aggregate.go:284–301 still excludes `LoggedDate == ""`).

**Call sites that violate the rule and must change in the same packet:**
`.agents/skills/timesheet/SKILL.md:209` and `.agents/skills/timesheet/workflow.md:537` both invoke
`metrics import --transcript "$PI_SESSION"` with no `--date`; and the CLI's own help text breaks
with the gate — `cmd/timesheet/metrics.go:29` shows `metrics import --transcript FILE` and `:95`
describes `--date` as overriding the transcript-derived date, which no longer exists. Both
strings change in the same packet, or `--help` lies.

**ADR conflict — amend, do not silently contradict.** ADR-0006:35–36 requires failure only when
the date is “neither derivable from the transcript nor supplied with `--date`”; it *blesses*
transcript derivation. This gate is stricter. → OWNER-DECISION-3.

**Explicitly in-scope-adjacent but out of this slice's scope:** `metrics run`
(metrics.go:674–677) writes only a `RunRecord`, no session lane, and its day is already an
explicitly validated input; `metrics aggregate` (:461–463) reads a day, it does not attribute
one. → OWNER-DECISION-4 decides whether `run` is gated too.

## 5. Refuse, never write an empty record

**Owner-backed guard first.** `state.md:2370(1)` and :2401 record the owner decision: **REFUSE,
NOT LABEL, for a forced mismatched provider.** Guard 1 implements that decision.

**Guard 1 — mismatch-refusal (owner-decided), scoped so readable imports cannot be refused.**
Detect was built for `auto` on a single first line (pi needs `type=="session"`,
adapter_pi.go:22–27; claude needs `sessionId`/`summary`+`leafUuid`, adapter_claude.go:40–43);
using it as a one-line validator would refuse legitimate imports whose first line is not the
probe line (resumed files, leading queue/snapshot lines). Refusal therefore triggers only where
the harm exists:

- After `Parse`, if the record has **no calls and no observations**, the transcript was not
  readable by the requested adapter. THEN diagnose: if another registered adapter's `Detect`
  matches within a scan window of the first **32** non-empty lines (an implementation constant,
  the recognition per adapter unchanged), refuse with `ErrProviderMismatch` naming the requested
  provider and the detected one; if nothing matches, refuse with `ErrUnreadableTranscript`.
- If the parse is NON-empty, nothing refuses — behavior for every previously-readable import is
  byte-for-byte unchanged, including resumed files.

This reproduces the SUBSTANCE of the #73 defect as a refusal: `--provider pi` against an
antigravity transcript parses to zero calls (`adapter_pi.go:109–164` switches only on
`session`/`model_change`/`message`), `buildRun` assigns `Usage = UnknownUsage()` and no closing
turn (adapter.go:275–276, :286–290) → `RunCancelled` (duration.go:129–131); today `Import`
writes it and the CLI prints `0 call(s), state=cancelled` (metrics.go:146–147) — a phantom row.
After the guard both arms refuse before any write. The DIAGNOSIS NAMING is slice-ordered: in
the #69 slice no antigravity adapter exists to consult, so this case takes the
`ErrUnreadableTranscript` arm (the phantom row is still prevented — that is the owner decision's
substance) and is UPGRADED to `ErrProviderMismatch` naming antigravity when #73 registers the
adapter (R6 carries the split).

**Guard 2 — the unreadable arm is broader than the recorded decision.** Refusing
unreadable transcripts for EVERY provider (not only forced mismatches) is a NEW refusal class:
#73's own "Needed" list offered "either be refused or labelled unreadable", and the recorded
decision covers only the forced mismatch. The `state=unreadable` label is rejected here — it
needs a `RunState` schema change to encode an operator mistake, and a labelled phantom row still
corrupts the instrument — but that is an argument, not a grant. → **OWNER-DECISION-6**.

`--provider auto` keeps today's behaviour — no `Detect` match → `ErrUnknownProvider`
(adapter.go:58–59). No guessing anywhere.

## 6. The antigravity adapter: the shape it will take (#73 owns the code)

- `internal/metrics/adapter_antigravity.go`; `Name() == "antigravity"`; `Provenance() ==
  ProvenanceObserved` (host-verified first-hand, unlike claude’s declared schema,
  adapter_claude.go:13–21), so the `[declared schema]` marker is not printed
  (metrics/report.go:121–122).
- **Detect:** the five-scalar discriminator recorded at state.md:2372 — `step_index` numeric,
  `created_at` non-empty, `source ∈ {MODEL, USER_EXPLICIT}`, `type ∈ {GENERIC, PLANNER_RESPONSE,
  USER_INPUT}`, `sessionId` absent. Note there is **no `type:"session"` line**, so pi’s Detect
  can never match, and claude’s `sessionId` probe can never match: the three are mutually
  exclusive by construction, so `auto` order is immaterial.
- **Decoded:** `type`, `source`, `step_index`, `created_at`, the three counters, and
  `truncated_fields` **only as a trust marker**. **Never** `content`, `thinking`, tool-call names,
  args or results, `status`, `error` (ADR-0006:37–39).
- **Truncation:** the line is counted; any counter named by `truncated_fields` is downgraded to
  UNKNOWN. The marker names only text fields (state.md:2371(6)), so this is a guard, not an
  expected path.
- **Calls:** each usage-bearing line → `CallRecord` with `StepUnknown` + `CategoryUnassigned`,
  because `tool_calls[].args` carries no shell command (state.md:2371(5)). Steps empty, model empty,
  price unknown, and — with §4 — the session is day-associated only by `--date`.
- **Session id:** absent → `RunID = "sha256:"+ContentHash` (adapter.go:103–109); the session record
  falls back to `RunID` (session.go:72).
- **Registration:** one `case "antigravity"` plus one entry in the `auto` slice of `SelectAdapter`
  (adapter.go:46–62). One edit point; no dispatch table.
- **Shape provenance is the host’s, not the legacy parser’s.** The real counters happen to sit
  top-level, which the legacy top-level branch reads by coincidence (parser.go:122–132); that is
  not evidence the legacy parser was right — it sums four locations (:134–173) and cannot tell a
  harness line from anything else.
- **Fixtures:** new `internal/metrics/testdata/antigravity_synthetic.jsonl` with **invented
  values and the real field names**, plus a `transcript_full.jsonl` variant (same shape, no
  `truncated_fields`; state.md:2371(9)). The owner’s real transcript is never copied, committed,
  or referenced.
- **Known limitation owned by #73's ADR amendment:** `total_tokens` and `cache_hit_pct` remain
  `unknown` for this harness (the counter semantics are the subsection directly below; the
  owner’s NO-MODEL-CHANGE decision, state.md:2370(2), :2401, governs).

### Counters: an honest unknown, never a zero (the #73a fold, stated exactly)

The owner's handover records the constraint word-for-word: **“the Antigravity counters cannot
produce a token total or a cache-hit rate.”** Measured shape (#73): `input_tokens`,
`output_tokens`, `cache_read_tokens` only; **no cache-write counter**; **no model identifier**;
`thinking` is thinking **text**, not a counter (state.md:2371 items 1–4). Owner decision:
**NO MODEL CHANGE** (state.md:2370(2), :2401).

Design: `Usage{Input, Output, CacheRead` known from the counters; `CacheWrite` and `Reasoning`
**Unknown**`}`. The existing contracts then do the right thing with no new type:
`ConsumedTokens()` returns Unknown because one component is unknown (**metrics/models.go:208–218**),
and `CacheHitPercent()` returns nil (**session.go:19–31**). Model `""` ⇒ price unknown ⇒ `Cost` nil
(session.go:127–136, :170–182).

Precedent to cite, not invent: the claude adapter’s `ContextLength: workflow.UnknownMeasurement()`
with its “no totalTokens field, so it is UNKNOWN” comment (**adapter_claude.go:135–137**). The
report already prints `unknown` for a nil measurement (cmd/timesheet/metrics.go:166–169).

**Forbidden dodge:** emitting a total from input+output+cache_read. ADR-0006:29–30 is explicit
(“an unavailable value is UNKNOWN, never zero”), and such a total is a lower bound presented as a
total. The limitation is stated in the report and the ADR amendment, as the owner accepted.

## 7. The CSV store: one definition of the daily measure

Keep the file, the header order, and the reader (`csv_store.go:15`, pinned by
`tests/e2e_test.go:637`). Define each column once:

| column | definition |
|---|---|
| `date` | the operator’s day. Required; the `today` default at **main.go:70,74** goes, mirroring §4 |
| `wall_clock_ms` | wall clock of **one** measured `timesheet` invocation (measurer.go:47–81, exec + `time.Since`) |
| `peak_rss_mb` | on the `-run` path, `ru_maxrss` of the **measured child process** (measurer.go:59–68). When `SysUsage` is unavailable the sampler falls back to the CLI's own current RSS (measurer.go:66–68), and on a `-record`-without-`-run` run main.go:145–148 measures the CLI itself — so the honest definition is: the child's `ru_maxrss` where a child exists, the CLI's own RSS otherwise, and the report should not imply the `ru_maxrss` ideal was met |
| `entries` | the day's entry count from the record (see §3: from the workflow record; a record read error yields an EMPTY cell plus a stderr warning, never a swallowed zero) |
| `input_tokens`/`output_tokens`/`cache_tokens` | the `[AI]` **editorial-child** lane, from `EditorialInput/Output/Cache` (mechanism §3). Names kept (renaming buys no reader); scope documented as deprecated-in-meaning |
| `total_tokens` | `EditorialUsage.Editorial` — the soft-budget total *including cache and retries* (workflow/models.go:355). Never re-derived from the three components |
| unknown | a day with no editor or no record writes an **empty cell**, never `0`; token fields become `*int64`, the nil-vs-zero idiom already used at metrics/models.go:220–230 |

**One row per date — and the reader must change before the writer can.** `AppendMetric` appends
unconditionally (csv_store.go:20–69), `ReadMetrics` never dedupes (:99–120), and
`ComputeSummary` counts **rows** as days (report.go:71–73, `TotalDays: len(metrics)`) — so a
second record for a date silently inflates “Total days tracked”. The upsert mechanism, stated:

- **The reader changes first.** Today `ReadMetrics` is bounded (`MaxCSVRowsCount = 1000`,
  models.go:8) and **silently drops rows it cannot parse** (csv_store.go:113–115 → `continue`),
  while `parseCSVRow` **errors on an empty token cell** (`csv_store.go:138–141`; `record[3]` is
  the first token cell). A reader built
  today would truncate history past 1000 rows and delete every unknown-token row §7 introduces.
  The update capability therefore requires: a update path not bounded by `MaxCSVRowsCount`;
  `parseCSVRow` tolerant of empty token cells; and the update failing LOUDLY on any malformed
  row rather than skipping it.
- **The upsert** is temp-file + atomic rename, never append-on-top-of-a-duplicate.
- R2 (below) gains a truncation assertion, because the naive upsert would not fail it.

**The chart honors the same unknown rule.** `chart.go:36–40` divides `TotalTokens` by the
ceiling and compares it; with a nil total the chart takes the nil path, not a dereference and
not a zero-fill: the bar is OMITTED and the day's status prints `unknown` — never a
`0 ≤ ceiling` tick for a day nobody measured. R1 asserts this for the chart as well as the
report, because neither R1's store/report legs nor R4's flag plumbing alone proves it.

*(Superseded 2026-10-21 by #126: the chart's input is now the metrics store's day aggregates,
not the CSV's `TotalTokens`. The UNKNOWN RULE above is unchanged and now governs
`session_total_tokens`: an unknown session total omits the bar and the day's status prints
`unknown`, never `0`. The recorded rows and the CSV reader keep their nil-token behavior. See
[Reversal, 2026-10-21](#reversal-2026-10-21-126-the-charts-source-moves).)*

**The three historical rows** (`benchmarks/metrics.csv:2-4`, archived out-of-tree since the CSV is
untracked personal data; 2026-10-06/07/08; totals 180/295/287;
`cache_tokens` 0) are consistent with this scope: a few hundred **editorial-child** tokens per day
is what one `[AI]` call plus a retry looks like, and `cache_tokens 0` means the editor reported no
cache-read that day — not that the harness used none. The provenance is an inference from
consistent arithmetic, not a recorded measurement; the rows are **frozen as recorded**, and the
instruments do not disagree: they measure different things.

## 8. Regression coverage

Pipeline instrument (post-retirement):

- **R1** CSV round-trip: append → read → identical row; an unknown token cell reads back as nil
  and the report prints `unknown`, not `0`, and the CHART takes its stated nil path (§7: bar
  omitted, status `unknown`, never `0 ≤ ceiling`); an empty-token-cell row survives the reader
  (parseCSVRow no longer rejects it).
- **R2** upsert: two records for one date yield one row, last value wins, “days tracked” counts
  distinct dates; AND a update of a file with more than `MaxCSVRowsCount` rows preserves all of
  them (no silent truncation).
- **R3** no-parser invariant: a package-boundary test asserting `internal/benchmark`’s import set
  is stdlib-only and that no symbol in it names a provider.
- **R4** single-source ceiling: `-ceiling` reaches **both** chart and report (fixes
  report.go:118/:127); the constant is defined once in `workflow` and both public names alias it.
- **R5** gate, fail-before: `import` with no `--date` fails `ErrDateRequired` **even when the
  transcript carries timestamps** — today it succeeds.
- **R6** refusal, fail-before, provable entirely inside the #69 slice with two
  ALREADY-REGISTERED adapters: `--provider pi` against
  `internal/metrics/testdata/claude_declared.jsonl` (claude's first line carries `sessionId`, so
  `adapter_claude.go:40–43` matches; the invocation SUPPLIES `--date`, so the pre-fix failure is
  the phantom row and not today's date gate) yields `ErrProviderMismatch` and **no file** in the
  store — today it writes a cancelled run. The antigravity-shaped arm is asserted in #69 as
  `ErrUnreadableTranscript` — CONTINGENT on OWNER-DECISION-6 confirming refuse-all; if the
  forced-case-only alternative is chosen, this arm moves entirely to #73 — reading from an
  INLINE literal (a few invented lines carrying the real field names; privacy-safe by
  construction, since the fixture file itself is #73's), with the assertion
  UPGRADED to `ErrProviderMismatch` naming antigravity when #73 registers the adapter. And
  the no-false-refusal companion: a readable import whose first line is not the probe line
  (resumed or leading-queue-line file) parses and writes exactly as before.
- **R7** the three recorded rows still read back (no migration breakage).
- **R8** the e2e scenario that drives `-transcript` (`tests/e2e_test.go:591–618`) and asserts
  `benchmark.DefaultTokenCeiling` (:644–650) is rewritten against the record-fed columns; the
  OTHER e2e consumer too — `TestE2E_FullHeadlessPipeline` step 7
  (`tests/e2e_test.go:456–487`) writes a transcript and asserts `TotalTokens != 295` through the
  old parser path.
- **R9** the five parser unit tests and the nil-assert tests
  (`internal/benchmark/benchmark_test.go:13–101`, `:314–316`) and the CLI transcript tests
  (`cmd/benchmark/main_test.go:74–102`, `:136–148`) are deleted WITH the parser, each listed in
  the implementation packet so the deletion is provable.

Harness instrument (#73 owns): synthetic-fixture parse; `Detect` exclusivity across all three
adapters; `CacheWrite` unknown ⇒ `ConsumedTokens` unknown ⇒ nil cache-hit; empty step/model;
`truncated_fields` downgrade; provenance observed.

## Owner decisions required

1. **Amendment + ceiling number, ONE decision.** The design CONTRADICTS two recorded refusals —
   ADR-0006:119–120 (“the legacy code is left as-is”) and :139–141, together with
   `docs/timesheet-redesign.md:147` — so the work needs an ADR-0006 amendment (the gate pattern
   this design already follows for §5 per the owner's REFUSE decision), AND confirmation that
   **1000** is the only surviving number with 600 deleted rather than re-pointed. No recorded row
   changes verdict: 287 < 600 < 1000.
2. **CSV retirement.** Recommended: keep the file with the §7 definitions (it is the only
   wall-clock/RSS store). Alternative: retire it and let the record own the token lane.
3. **ADR-0006 amendment** for the §4 gate. The accepted ADR currently blesses transcript-derived
   dates (:35–36); the gate is stricter and cannot be introduced silently.
4. **`metrics run` / `metrics aggregate`** keep their `today` default? They write no session lane.
5. **Legacy fixtures.** The recorded state (state.md:2372) is LEAVE UNTOUCHED (“do not mutate;
   record as a misnamed asset”), with the owner to decide naming later. Three options, presented
   unranked by this design's ID but with the design's preference stated: (a) leave untouched
   — the recorded default; (b) rename; (c) retire both parser fixtures
   (`internal/benchmark/testdata/sample_antigravity.jsonl:1-4`, consumed only by
   `benchmark_test.go:39–63`, and `sample_claude.jsonl`) because §1 leaves them with no
   consumer — a fixture with no consumer is a false contract. RETIRES the recorded
   default if chosen; the choice is the owner's.
6. **The unreadable arm of Guard 1.** Refusing unreadable transcripts for EVERY provider (not
   only forced mismatches) is a new refusal class; #73 offered “refused or labelled unreadable”
   and the recorded decision covers only the forced mismatch. Refuse-all is this design's
   recommendation (reasons in §5); the alternative is reserving refusal for the forced case
   and letting `auto` on garbage fail via `ErrUnknownProvider` only.
7. **Ceiling scope.** The 1000 governs the editorial-child lane only (§2). Confirm that
   harness-session tokens remain ceiling-free — no number is invented for them.

   *(REVERSED 2026-10-21 by #126, in part: harness-session tokens remain free of any BUDGET —
   no number governs them and none is invented. But the CHART now draws a day's
   `session_total_tokens` against the same 1,000 as a REFERENCE line, labelled as such. The
   decision's substance (no invented harness ceiling) stands; its "ceiling-free" wording does
   not describe the chart any more. See
   [Reversal, 2026-10-21](#reversal-2026-10-21-126-the-charts-source-moves).)*

## Owner visibility (not a decision — stated so nothing arrives unannounced)

- The disposition is a THIRD option beyond the two #69 names (“rewire onto the metrics adapters,
  or retire it”): retire the parsing, source the columns from the workflow record. Reasons in §1.
- The two #73 points are folded as binding constraints — #73a quoted verbatim in §6's counters
  subsection, #73b quoted verbatim at §4's opening.

## Deferred

- Renaming the CSV token columns (no reader gains; churn only).
- Retiring the `AssociationLocalStepDate` legacy read path and stored unassociated sessions
  (the gate is prospective; §4).
- Per-invocation rows instead of one-per-day: needs an owner definition of “daily cost” when a
  day has several pipeline runs (R2 assumes last-wins).
- Stating “limited value on antigravity” in the report/README — lands with #73’s ADR amendment.
- `.tickets/M3-02` (:4, :26) still documents the retired parser and the stale `antigravity-cli`
  path; tickets are not maintained in this repo’s flow, so it is recorded, not fixed.

## Slice boundaries

- **#69 — pipeline instrument + shared ingest guards (this design IS #69's decision, and its
  implementation lands here).** §1 retirement (parser, `TokenUsage`, `-transcript`,
  `OrchestrationUsageFromBenchmark`); §2 constant; §3 record-reading mechanism; §4 gate; §5
  guards; §7 CSV definitions, unknown cells and upsert; **adapter-registry seam only** — verify
  `SelectAdapter` stays a one-line-plus-one-case edit, add no provider; R1–R9; fixture
  disposition per OWNER-DECISION-5; and the ADR amendments its decisions 1 and 3 name, as
  deliverables of this packet. One pull request; no issue touched beyond #69 itself
  closing through it.
- **#73 — the antigravity adapter itself.** `adapter_antigravity.go`, its synthetic fixtures, the
  antigravity-specific tests, the ADR-0006 amendment for its counter semantics, and the skill
  *reading* guidance (`$PI_SESSION` → the conversation-id path under
  `~/.gemini/antigravity/brain/...`; `--date` added at SKILL.md:209 / workflow.md:537; CLI help
  strings per §4). Also: the R6 upgrade — #69's antigravity-shaped
  `ErrUnreadableTranscript` assertion becomes `ErrProviderMismatch` naming antigravity once
  this adapter's `Detect` is registered.
- **#24 — skill packaging only.** Ships #73’s guidance text into the packaged skill. No Go code,
  no schema, no data.
- **Order:** #69 first (it changes `Import`’s contract, which #73’s adapter is tested against),
  then #73, then #24.
- **ADR-0006 amendment sequencing, so no amendment drops:** ONE ADR, three edits, each assigned —
  the recorded-refusals amendment (OWNER-DECISION-1) and the §4 gate amendment (OWNER-DECISION-3)
  are #69 packet deliverables; the counter-semantics amendment rides #73.
- **Non-overlaps:** #69 adds no provider and no counter semantics; #73 touches neither benchmark,
  the ceiling, the CSV, nor `Import`’s guards; #24 changes no code.
