---
status: accepted
date: 2026-10-11
deciders:
  - Huy Le Minh
related:
  - 0005
vault_mirror: ADR-007
---

# Journal evidence source

Mirrored in the ai-database vault as
`05 - Architecture Decisions/ADR-007 - Optional read-only journal evidence.md`
(verified 2026-10-11).

## Context

ai-database (the owner's Obsidian vault) was previously **export-only**: the
workflow wrote a managed timesheet section into a daily note but never read it
back. The owner wants the daily note's narrative journal to act as an OPTIONAL,
local, read-only **evidence** source so that manually recorded work can support a
timesheet entry. The redesign ([0005](0005-review-first-timesheet-architecture.md),
[the specification](../timesheet-redesign.md)) keeps evidence, proposals,
confirmations, approvals, and submissions distinct, and states that
**ai-database is optional and not the primary workflow store**.

A narrative note is the riskiest evidence source: it is free-form, it contains
private text, it can contain task checkboxes and exported timesheet sections, and
it never states a working duration. Treating it carelessly would either
(a) fabricate working time from prose, (b) leak private text to an external AI
service, or (c) feed an exported timesheet back into itself.

## Decision

Add one compile-time evidence plugin, `journal` (`internal/evidence/plugins/journal.go`),
registered explicitly in the CLI registry. It is a **supplementary, read-only**
evidence source: the vault is never written by it, and ai-database is never the
primary store. *(Amended 2026-11-11 by #270: the source was removed in the
CLI cut-over and no journal source exists. See [Amendments](#amendments).)*

- **Explicit, confined configuration.** A journal source requires `vault` (the
  vault root) and `daily_dir` (a RELATIVE directory, e.g. `02 - Daily & Logs`).
  An absolute or `..`-escaping `daily_dir`, or a resolved path that leaves the
  vault through a symlink, is refused. Only `<vault>/<daily_dir>/<date>.md` is
  ever read. `fixture` is an optional contract-mode test input.
- **Narrative, UNTIMED evidence.** Every emitted item is `SourceJournal` with a
  zero `Start`/`End` and an UNKNOWN `Duration`. A note never establishes working
  time; no classification, activity type, or duration is inferred.
- **Recognized sections only.** Items come from recognized narrative headings
  (`Work journal`, `quick capture`, `Daily impact` / `Daily impact and brag
  bullets (for performance review)`), including nested descendants with their
  full heading ancestry. Checkbox tasks (even checked), `priorities`/`standup`/
  `todo`/`agent session` sections, fenced code, the exact `internal/export`
  managed markers, and legacy `## Clustered timesheet` / generated `## Timesheet`
  sections are skipped. The generated/legacy timesheet skip matches ONLY precise
  shapes (the exact `clustered timesheet` heading or the exact generated
  `Timesheet — <date>` heading), and skip phrases apply to SECTION headings
  (level ≥ 2), never to a level-1 note title: an arbitrary heading that merely
  contains `timesheet`/`todo` never silently empties a note.
  *(Amended 2026-10-21 by #85.)* The skip-token set is a CONTAINS match on the
  normalized heading text, and `agent session` is its fourth token. See
  [Amendments](#amendments). A malformed (unterminated) managed region fails
  closed so exported text can never feed back.
- **Narrow, fixed exclusions.** A small fixed set of whole-item authority/safety
  boilerplate strings and metrics-only entries are excluded, with a count
  warning. A metrics-only entry contains a number and only a BOUNDED unit/filler
  vocabulary (`tokens`, `ms`/`s`/`min`/`m`/`h`, `calls`, `packages`, `tests`,
  `runs`, `files`, `%`, `$`, plus connectors) with numeric separators; any other
  word (e.g. `reviewed`, `benchmark`, `shipped`) makes the item substantive and it
  is retained. Substantive agent-authored docs/review/research/benchmark work is
  therefore never excluded.
- **Stable identity.** `JournalID` distinguishes an explicit identity
  (`sourceID`/`date`/`X` from a bounded trailing `<!-- evidence_id: X -->`,
  independent of section) from an implicit one (`sourceID`/`date`/NORMALIZED FULL
  heading ancestry/normalized content hash). Reordering is stable; two different
  headings that merely share a recognized phrase, and nested sections, stay
  distinct; editing an implicit item changes its identity. Duplicate explicit ids
  and any remaining duplicate IMPLICIT ids fail visibly rather than being silently
  upsert-collapsed. The note path is NOT identity. A cosmetic heading edit
  (decoration/case that normalizes identically) keeps identity; a real heading
  text change changes the implicit identity, so the item detaches as history.
- **Additive provenance.** `SourceRef` gains relative note path, full heading
  ancestry, section, inclusive line range, stated date, clock, and inline
  project links. Project hints union frontmatter `focus_projects` and inline
  wikilinks. A quoted YAML date scalar is normalized (surrounding quotes
  stripped) before comparison so a quoted frontmatter date never raises a
  spurious mismatch; the raw value is never rewritten in the note. The stated
  date is retained unchanged and a mismatch with the target date is warned,
  never rewritten.
- **Active/detached history.** `Evidence.JournalState` is additive (`""` =
  active). On a successful COMPLETE authoritative journal collection, prior
  journal ids for the same source/date that are absent from the latest set are
  marked detached (history retained, never deleted, never re-proposed); present
  ids are upserted active. A failed or partial collection never detaches prior
  history, and no positional "superseded" guess is made. A detached item is
  surfaced as advisory with human-readable provenance (title, relative note
  path, heading, line range, stated date, reason) in the proposal's excluded
  entries, in `Proposal.Changed`, and in the CLI review output — never only an
  opaque id or a bare count.
- **Narrative proposals.** `proposal.Proposal` gains additive
  `Narratives []NarrativeCandidate` (id/section/date/title/description/evidence
  ids/project hints/unknown duration, NO time fields). Detached items are
  visible as excluded history.
- **Manual linkage.** A manual interval may cite several supporting source
  records via `Evidence.LinkedEvidenceIDs` (`ManualDecision.EvidenceIDs`,
  `review.ManualInterval.EvidenceIDs`). Linked ids must exist and must not form
  a cycle. Linked evidence adds NO minutes: the manual interval's own span is
  authoritative, and it propagates to the proposed manual block and every
  four-hour split fragment without losing provenance.
- **Privacy.** Private journal text is never forwarded to an external editorial
  service, including a manually linked journal description: the editorial request
  carries no journal or manual facts, and a source-aware guard enforces it.

## Alternatives rejected

- **Parse the vault with a general Obsidian/markdown library**: rejected. The
  journal source is a narrow, fixed-shape reader; a general parser would add a
  dependency and broaden the blast radius for a single note shape.
- **Make ai-database the primary store or write the vault**: rejected; the
  redesign keeps the vault optional and export-only except for this read.
- **Infer duration from note prose or timestamps**: rejected; a stated clock is
  not a working duration.
- **Delete or implementation superseded journal items**: rejected; history is retained
  and only marked detached.

## Consequences

The CLI and the agent skill keep one shared core; the journal source adds no new
subcommand. The exact authority/safety boilerplate strings must be confirmed by
the owner against the real vault (the current set is deliberately small). This
record is mirrored in the vault as ADR-007. No UI or downstream (B) dependency is
added.

## Amendments

### 2026-10-21 — the `agent session` skip token (#85)

The skip-token set gained a fourth token, `agent session` (#85, merged as PR
#91), so an agent's activity is never read as the owner's working time. The rest
of this ADR stands as written: the collected headings, the timesheet shapes, the
exclusions, identity, provenance and privacy clauses are unchanged.

- **A fourth skip token, matched by containment.** The tokens are `priorities`,
  `standup`, `todo` and `agent session`, and each is a CONTAINS match on the
  NORMALIZED heading text (lowercased, markdown and emoji decoration stripped),
  rather than an exact heading match. A heading such as `### Agent session log` is
  skipped, and because skip applies to any heading in a bullet's ancestry the
  whole subtree contributes nothing — including when it nests inside a recognized
  `## Work journal` section, where the deepest recognized heading would otherwise
  supply the family and the agent's leg would be collected as work time. It also
  skips the subtree when an `agent session` heading sits above recognized
  headings.
- **The level-1 exception leaves one residual, stated rather than hidden.** Skip
  phrases still never apply to a level-1 note title, so a note is never emptied by
  an arbitrary title. For this token that leaves a residual: a note whose H1
  contains `agent session` still collects a recognized section beneath it, so the
  split has to hold at section level.
- **The hyphen spelling is not covered.** Normalization preserves hyphens but
  turns underscores into spaces, so `Agent_session log` matches the token and
  `Agent-session log` does not. That asymmetry is inherited from the shared
  normalization and applies to every token, `To-do` included; a note that
  hyphenates the heading will not be skipped.

The authoritative, code-grounded list of collected and skipped headings is
[the journal collection note](../journal-collection.md).

### 2026-11-11 — the journal source was removed (#270)

The `journal` source no longer exists. The CLI cut-over (f2c80880, #137)
removed `internal/evidence`, including the `journal` plugin and its
`timesheet.journal.example.json` template, and the current configuration
accepts only `sources.github` and `sources.calendar`
([collection configuration](../agents/collection-config.md)). Nothing in the
product reads the vault. The decision above stays as the accepted record of
how the source behaved while it existed; the operator note
[journal source](../agents/journal-source.md) is retired to a historical stub.
Reintroducing a narrative source would need a new decision.
