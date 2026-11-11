# Classification selection

How the timesheet chooses a Gradion classification, why it is always explicit, and
how the catalog, cache, and approval gates behave. Private catalog content and
credentials never appear here: names/codes/ids below are placeholders.

## 1. Verified catalog contract

Read-only discovery against `gradion-workspace` 1.0.0 (protocol `2025-06-18`)
established:

- `timesheet__get_classifications` takes `search` and `activityTypeId` and returns
  **at most 20** rows.
- Its result carries **two** representations:
  - `structuredContent = {"resources":[{"kind","id","label"}]}` — authoritative for
    the classification **id** and **name**.
  - a `content[type=text]` block whose lines are
    `- <Name> (<Code>) [<Activity Type>] · <uuid>` — the **only** source of the
    **Code** and the **Activity Type**.
  - a truncated result's text begins like
    `20+ classifications found (showing 20 — refine search to narrow)`.
- `timesheet__log_time` / `timesheet__edit_time` accept **no** activity-type field.
  They receive only a `classificationId`.

### Derived activity type

There is **no standalone activity-type catalog tool**. The server **derives** the
activity type from the classification. Activity type is therefore **display and
validation only**: it is never a payload field, and the local record's
`activity_type` never reaches the server.

### Parsing rules (fail closed)

- Structured envelope → id + name.
- Text block → code + activity type, joined to the envelope **by uuid**.
- A text line that does not match the pattern is skipped.
- A truncation header, or a result that reaches the 20-row cap, marks the catalog
  **partial** (`Complete=false`).
- A text block that is present but wholly unparseable fails closed with a typed
  error (`GRADION_ERR_CATALOG_UNPARSED`) rather than yielding an empty catalog.
- A **structured envelope with no text block** is unverifiable (the text block is
  the only source of Code + Activity Type) and is therefore also marked **partial**
  (`Complete=false`) with empty Code/ActivityType — it is never reported complete.

## 2. Selection semantics (explicit only)

- A plan's classification comes **exclusively** from the confirmed interval's
  explicit `classification_id`.
- An **empty** classification **blocks** the plan with
  `unresolved_classification:<interval>`.
- There is no project→classification map any more: the local project is
  retired ([ADR 0012](../adr/0012-retire-the-local-project.md)). The only
  configured default is `gradion.default_classification`, an exact reviewed
  catalog option that `classify --configured-default` applies; otherwise the
  classification judgment decides (ADR 0010).
- Repository mappings, calendar text, AI, and config defaults never assign a
  classification silently.
- Selection requires an **EXACT unique** catalog match (by id, then by code, then by
  an **unambiguous id-prefix** — the same short prefix `classify list` prints).
  Unknown or ambiguous tokens are refused; no id is ever synthesized or
  fuzzy-matched.

### Bulk selection

`timesheet classify select --all` applies to **every** confirmed interval of the
day and requires the explicit `--all` flag. The exact change list is printed
**before** applying. Without `--all`, only `--block-id <ID>` is honored.

The guided review's **Bulk actions** step (see
[product usage](product-usage.md)) also offers a **Bulk-assign a classification**
action: the user picks one classification through the same searchable catalog UI,
the step prints the **exact affected intervals and counts** (and any already-
classified intervals it skips), and an explicit consent is required. It only
**stages** decisions on the same atomic review — an abort applies nothing. A bulk
classification never overwrites an interval that already carries a classification.

Within a single guided run the per-block classification step **does not re-prompt**
an interval the bulk action already staged: those intervals are disclosed as
bulk-classified and skipped. Per-block override is therefore guaranteed at the
**data level** (bulk staging is add-if-absent, and a per-block classification
decision staged for the same interval replaces it), and the documented way to
change a bulk-assigned interval in the UI is the printed
`timesheet classify select --date D --block-id <ID> --classification <CODE|ID>`
follow-up.

### First-run and recent choices

- First-run suggestion: activity type `internal project`, classification
  `INTGRADI2610 – Gradion Intern Academy 2026`. It is resolved against the
  **actual** fetched catalog; if it is absent, ambiguous, or incompatible it is
  explained and the user is asked for another selection. Nothing is auto-selected.
  It is surfaced (labelled UNAPPLIED) by both `classify list` and the guided
  classification step **only when there is no recent selection**.
- The most recent choices (`~/.timesheet/preferences.json`, non-secret, capped at
  20) are surfaced as **confirmation hints** (up to the 3 most recent); they are
  **never** auto-applied. A recent selection takes precedence over the first-run
  suggestion. The surfacing cap is display-only: it never changes the stored list.

## 3. Cache and offline behavior

- `classify list` / `classify select` fetch **live** by default; `--cache` reads the
  cache (`~/.timesheet/catalog-cache.json`: `{ObservedAt, Query, Complete, Source,
  Options}`).
- Provenance and freshness (`live`/`cache`, observed-at, fresh/`STALE`) are
  **always** disclosed by `classify list`, `classify select`, and `submit`.
- A `--cache` catalog older than 24h is **STALE**. `classify select` **refuses** a
  stale cache unless the explicit `--force` flag is given (a refusal applies no
  selection); `submit` never uses a stale cache.
- Offline collection and review remain fully usable.
- An **unresolved remote choice blocks submission** — it never becomes a default.
  Before any write, **every** planned selection must be re-proven against a
  **fresh LIVE** remote result: a fresh **cache is never proof of identity**, even
  when it already contains the row. When the catalog used at submit is itself a
  fresh **live, unfiltered, complete** fetch it is authoritative and reused;
  otherwise one bounded **targeted read per unique recorded code** (code
  preferred, else name) is issued and must return a **live, complete** page
  carrying the exact selected stable id with a nonempty name, code, and derived
  activity type, the recorded code/name matching, and the confirmed interval's
  derived type matching. A missing, errored, ambiguous/duplicate, changed,
  incomplete/capped, or unrelated result blocks the whole run with
  `unverified_classification` and **zero writes**; absence from a full catalog is
  `unknown_classification`. Operations that share an id but disagree on metadata
  or type block as a group. A stale or absent cache blocks submission rather than
  defaulting.
- A **legacy** confirmed interval that carries **no** recorded code/name cannot be
  re-proven from a targeted read. It fails closed with an explicit
  `reselect_classification` diagnostic and **zero writes**, unless it is proven
  from a fresh live full complete catalog whose returned row meets the safety
  checks above. Existing bytes/history are preserved (no silent migration).
- When the catalog used is partial and a token is not found, the error says so and
  suggests refining with `timesheet classify list --search <term>`.

### Catalog validation is mandatory with a remote

`run.SubmitOptions.Catalog` is **mandatory whenever the runner has a remote**
(`deps.Remote != nil`). The CLI path is unconditional: when a Gradion endpoint is
configured it always loads (live, else a fresh cache) and validates the catalog
before any write. `Catalog` is nil only for an offline runner, which blocks earlier
with `remote_unavailable`.

## 4. Approval-invalidation rules

A selection change alters the plan-relevant local state, so it invalidates any
existing approval:

- `classification_id` and `activity_type` both feed `LocalFingerprint`. Changing
  either makes a **stored** approval stale: `ValidateApproval` reports `stale_local`
  and the run refuses. (The plan content hash, `PlanHash`, is only recomputed when a
  plan is rebuilt — e.g. at the next `preview` — so it is the *rebuilt* plan whose
  hash differs; the stored approval is invalidated via `stale_local`, not by a
  retroactive hash implementation.)
- The optional retrieval metadata `classification_code`/`classification_name` is
  **deliberately excluded** from `LocalFingerprint` and `PlanHash` (and never sent
  to the remote): adding it to an existing record does **not** stale an approval.
- `timesheet classify select` also explicitly invalidates any not-yet-invalidated
  approval for the day (reason `classification_selection`).

### Release note (upgrade behavior)

`ConfirmedInterval.activity_type` is an **additive** field that feeds
`LocalFingerprint`. On upgrade, a pre-existing approval recorded before this field
existed hashes without `activity_type`, while the recomputed fingerprint now
includes it — so any **in-flight (not-yet-executed) approval** is treated as stale
(`stale_local`) and must be re-approved. This is **fail-closed and safe**:
approvals are single-use and short-lived, and no already-submitted work is affected.
No migration is required.

## 5. CLI and skill contract (one contract)

| Step | Command |
| --- | --- |
| Present choices | `timesheet classify list [--search Q] [--json] [--cache]` |
| Apply explicit choice | `timesheet classify select --date D --classification <code\|id\|id-prefix> [--block-id ID \| --all] [--cache] [--force]` |

The `[Script]` skill step is identical: fetch the catalog, **ask the user**, then
apply the explicit choice. `--all` requires explicit consent. This is the single
contract shared by the CLI and the skill; the skill reimplements no accounting or
submission logic.

## 6. Non-goals and data handling

- No activity-type payload field is ever sent.
- No automatic classification from any source.
- No **credential** ever reaches records, exports, logs, or this document.
- The **local catalog cache** (`~/.timesheet/catalog-cache.json`) and the recent
  preferences (`~/.timesheet/preferences.json`) are **local-only files**. The code
  never copies their rows into workflow records, exports, or logs, and they never
  contain credentials. The cache is bounded and 24h-stale-gated; both files are
  created mode `0600`.
- An **explicit selection** stores retrieval metadata on the confirmed interval:
  `classification_id`, the derived `activity_type`, and the selected option's
  `code`/`name`. This metadata is retrieval-only: it looks the choice back up at
  submit (see §3) and never reaches a remote payload, the editorial agent, or the
  managed export as catalog content. Catalog rows the user did not select are never
  stored.
