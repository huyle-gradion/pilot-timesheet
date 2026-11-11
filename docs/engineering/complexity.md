---
version: 1
date: 2026-10-11
commit: 340f7c2
---

# Complexity and space analysis

> **Historical.** A dated snapshot at commit 340f7c2. The `internal/run` and
> `internal/evidence` paths it cites were removed in the CLI cut-over (f2c80880);
> the current flow is `timesheet log` (see the [README](../../README.md)).

## Changelog

| version | date | commit | change |
| --- | --- | --- | --- |
| 1 | 2026-10-11 | 340f7c2 | Initial analysis slice (METRICS-1). Adds the metrics-collect and metrics-aggregate stages; aligns the soft editorial target to 1,000 tokens/day. |

## Scope and method

This document records **analysis**, not measurement. Every time and space entry is
an asymptotic claim derived by reading the code, not a wall-clock or RSS
observation. Where a concrete number would require a run that was **not**
performed in this slice, the cell is marked `unavailable` rather than estimated.

- Method: static reading of the referenced functions. No profiler, no benchmark
  run, no live service.
- Units: time is in the workload variable named in the notation; space is bytes
  (`B_bytes`) or element counts.
- Platform for any future measurement: `darwin/amd64`, Go 1.27.1 (recorded here so
  a later RSS number is comparable; this slice reports `unavailable`).
- Scope: the deterministic workflow core (`internal/run`, `internal/evidence`,
  `internal/proposal`, `internal/workflow`, `internal/export`) and the additive
  metrics slice (`internal/metrics`). The legacy `internal/benchmark` extractor is
  out of scope and explicitly unchanged.

## Notation

| symbol | meaning |
| --- | --- |
| `n_commits` | local git commits considered for a day |
| `n_prs` | remote pull-request items considered for a day |
| `n_evidence` | evidence items collected for a day |
| `n_blocks` | proposed/confirmed time blocks for a day |
| `n_calls` | assistant turns with usage in a transcript |
| `n_runs` | imported runs (sessions) for a day |
| `n_days` | days in a requested range |
| `B_bytes` | serialized bytes of a record, note, or transcript |

## Per-stage complexity

### Collection

| algorithm | time | in-memory space | durable space | code ref | cap | note |
| --- | --- | --- | --- | --- | --- | --- |
| Per-source fetch and merge into the day record; local git scan + GitHub pagination | `O(n_commits + n_prs)` per repo | `O(n_evidence)` buffered per source | `O(n_evidence)` appended to the record | `internal/run/collect.go:27`, `internal/evidence/registry.go:227` | `MaxCollectedEvidenceCount=2000` stored items; `MaxCommitsCount=100` returned git entries | The returned-entry cap does NOT bound the git-log scan or remote pagination (see callout). |

### Dedup

| algorithm | time | in-memory space | durable space | code ref | cap | note |
| --- | --- | --- | --- | --- | --- | --- |
| Content-addressed `StableID` + map-based dedup/merge | `O(n_evidence)` | `O(n_evidence)` id map | `O(n_evidence)` merged into the record | `internal/evidence/identity.go:32`, `internal/workflow/merge.go` | `MaxCollectedEvidenceCount=2000` | Dedup is linear in collected items, not in fetched bytes. |

### Proposal

| algorithm | time | in-memory space | durable space | code ref | cap | note |
| --- | --- | --- | --- | --- | --- | --- |
| Topic clustering (repo+PR, else repo+branch) and adjacency-gap grouping | `O(n_evidence log n_evidence)` | `O(n_blocks)` | `O(n_blocks)` | `internal/run/propose.go:18`, `internal/proposal/` | none on block count (bounded by evidence count) | Commits are activity points; duration stays unknown. |

### Reconciliation

| algorithm | time | in-memory space | durable space | code ref | cap | note |
| --- | --- | --- | --- | --- | --- | --- |
| Match local operations to freshly observed remote state by stable remote id | `O(n_blocks)` | `O(n_blocks)` | `O(n_blocks)` outcomes | `internal/run/submit.go:133` | `MaxOutcomesCount=1000` | One remote read per reconcile, PLUS one bounded stable-ID read-back (one listing scan) per identity-only (reference-envelope) write to verify content; uncertain/unverified writes stop the run. |

### Persistence

| algorithm | time | in-memory space | durable space | code ref | cap | note |
| --- | --- | --- | --- | --- | --- | --- |
| Atomic temp-file + rename of the day JSON | `O(B_bytes)` serialize | `O(B_bytes)` | `O(B_bytes)` per record | `internal/workflow/file_store.go:75`, `internal/workflow/file_store.go:506` | `MaxRecordSizeBytes=10MiB`, `MaxEvidenceCount=500`, `MaxEditorialCount=200` | `checkCapacity` bounds both counts and total bytes; a corrupt record fails closed. |

### Export

| algorithm | time | in-memory space | durable space | code ref | cap | note |
| --- | --- | --- | --- | --- | --- | --- |
| Marker-delimited implementation of the managed section | `O(B_note)` | `O(B_note)` | `O(B_note)` | `internal/run/submit.go:108`, `internal/export/` | none explicit | Non-destructive: only the `timesheet:begin/end` block changes. |

### Metrics-collect

| algorithm | time | in-memory space | durable space | code ref | cap | note |
| --- | --- | --- | --- | --- | --- | --- |
| Single-pass JSONL scan extracting only counters, ids, timestamps, and the `timesheet` subcommand token | `O(B_bytes)` | `O(B_bytes)` (whole transcript read once for hashing) + `O(n_calls)` | `O(n_calls)` one run file | `internal/metrics/adapter_pi.go:69`, `internal/metrics/adapter_claude.go:78`, `internal/metrics/adapter.go:79` | `MaxTranscriptBytes=64MiB` | Never decodes message text, tool arguments, or tool results into a persisted field. |

### Metrics-aggregate

| algorithm | time | in-memory space | durable space | code ref | cap | note |
| --- | --- | --- | --- | --- | --- | --- |
| Fold per-call usage across the day's runs + read one workflow record for editorial usage. Listing runs READS EVERY run file in the store (bounded by `MaxRunFilesCount`), then filters by date IN MEMORY; it is NOT day-scoped | `O(n_runs_total x n_calls)` over all stored runs + `O(n_runs_day)` to build | `O(n_runs_total)` (all run files are read to filter by date) | `O(1)` one aggregate file per day | `internal/metrics/aggregate.go:33`, `internal/metrics/store.go:105` | `MaxRunFilesCount=5000` | A PURE function of its inputs: re-aggregation is byte-identical (no stored timestamp). |

## Callout: output caps do not bound upstream scanning or remote work

A cap on what is **returned or stored** is not a cap on the work **performed** to
produce it:

- `MaxCommitsCount=100` (`internal/git/github.go:23`, applied at
  `internal/git/local.go:273` and `internal/git/github.go:603`) caps the number of
  commits **admitted per day**. It does NOT bound the `git log` scan or GitHub
  pagination that precedes admission.
- `MaxCollectedEvidenceCount=2000` caps the number of evidence items **stored** per
  day. It does NOT bound fetch volume from a source.
- `MaxEditorialCount=200` caps editorial **suggestions** per day. It does NOT bound
  the number of agent calls; the agent call budget is a separate
  `MaxAttempts`-per-target bound.

Any statement of cost must therefore cite the cap that applies to the stage it
bounds, not an upstream cap that happens to be nearby.

## Space

In-memory and durable space are reported **separately**. A per-stage peak RSS is a
property of one stage's execution and is NEVER summed across stages (stages do not
run concurrently in the CLI, and summing peaks double-counts the allocator's
reuse).

| artifact | in-memory | durable | note |
| --- | --- | --- | --- |
| Workflow day record | `O(B_bytes)` | `O(B_bytes)` per day, capped at 10 MiB | `internal/workflow/file_store.go` |
| Metrics run file | `O(n_calls)` | `O(n_calls)` per run | `internal/metrics/store.go` |
| Metrics daily aggregate | `O(n_runs)` | `O(1)` per day | `internal/metrics/store.go` |

- Per-stage peak RSS: `unavailable` for this slice (no benchmark run was
  performed; see Scope and method).
- Workload counts for a concrete day: `unavailable` (no live collection was run).
- Platform/units/scope: `darwin/amd64`, Go 1.27.1, bytes and element counts, static
  analysis only.

## Alignment to the soft editorial target

The soft daily editorial target is **1,000 tokens/day** (owner override of the
specification's 600; `internal/editorial/model.go:37`). The metrics slice exists to
prove movement toward it: `DailyAggregate.EditorialInputToStep` reports the
editorial **input** sent to the step (the §3.5 optimization metric), while
`DailyAggregate.Budget.Spent` reports the editorial total including cache and
retries. A day with zero editorial calls reports `EditorialInputToStep = 0` as a
genuine zero (zero calls), never as "zero total tokens".
