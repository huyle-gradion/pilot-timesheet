---
status: accepted
date: 2026-11-11
deciders:
  - Huy Le Minh
related:
  - 0005
  - 0008
  - 0010
  - 0011
---

# Retire the local project

Every interval had to carry a project, but the remote has no project. The
Gradion write tools take `date`, `startTime`, `endTime`, `description` and
`classificationId` (plus `entryId` and `reason` for an edit), and the week read
returns no project either. The project existed only in this binary, and its one
effect was a local classification label that never reached the remote. The
remote classification came from the reviewed selection, which is either the
configured `gradion.default_classification` or the ADR 0010 classification
judgment. Even so, a configuration with two projects made `stage` refuse and
made `next`/`log` stop with a `project` question that only the human could
answer (issue #185). The owner retired the project on 2026-11-11 because the
remote does not use it. No earlier ADR requires a project, so this one records
the change.

## Decisions

1. **A new interval carries no project.** The staged interval text grammar is
   `DATE HH:MM-HH:MM DESCRIPTION`. Neither the project token nor the optional
   `/CLASSIFICATION` suffix exists any more. `stage`, `classify`, `save` and
   `stage --revise` derive intervals from the entries ledger alone.

2. **An existing interval keeps its project as an inert legacy label.** A
   confirmed interval, a history snapshot or a migrated carry fragment that
   already holds a `project` (and the local `classification` label the
   project once defaulted) keeps it. Records are never rewritten. Nothing reads
   the label for a decision: the label is not checked against the
   configuration, `plan` never compares it, and the ledger-derivation proof
   never compared it. A saved day therefore stays current and keeps its plan.
   The saved-classification digest still covers the label, so an answer bound
   to a legacy day stays valid. Only an explicit human revision
   (`stage --revise`) replaces those intervals, and the new ones have no label.

3. **Classification is decided without a project.** The remote classification
   is the reviewed selection, as before. When `gradion.default_classification`
   is configured it applies. Otherwise the ADR 0010 classification judgment
   decides, answered by the judge or by an agent-written answer file. No day
   needs a classification typed by hand, and no remote write body changes: a
   test pins the exact bytes of a day's write bodies from before the change.

4. **State schema 4, with its own upgrade receipt.** Schema 4 makes the
   interval's `project` optional (`omitempty`). A schema 1, 2 or 3 file is
   still validated in its own shape, where a confirmed interval needs a
   project. It is then upgraded in memory and written as schema 4 only by the
   next successful durable write, never at startup. The upgrade adds one
   receipt to `migration.schema_upgrades`: the schema it came from and the
   schema it went to, the build version, the number of days and confirmed
   intervals, and how many of them keep a legacy project label. The receipt
   becomes durable in the same atomic write that activates schema 4.
   `applied_at` is stamped at that write. A store that never had a file gets
   no receipt. A build that only reads schema 3 fails closed on a schema 4
   file with `state_invalid`.

5. **The surface drops the project.** `--project` is gone from `init`, `log`,
   `next`, `tui`, `judge`, `stage`, `classify` and `save`. Passing it fails
   with one line that says to drop the flag, in the same way as a removed
   command (`retiredCommands`). The typed `ask_human` `project` question is
   gone too. An `ask-project.json` answer left in a day folder is ignored.
   `init` needs no project, and `--seed-config` copies the seed's
   `required_env` and `sources`.

6. **Old configurations keep loading.** The configuration keys `projects` and
   `migration.project_map` are retired. They are still accepted and ignored,
   so an existing `config.json` and the daily run keep working. `init` no
   longer writes them. Every other unknown key still fails closed.

7. **Legacy migration stops mapping projects.** `migrate preview|apply` no
   longer requires a legacy project to name a configured project, so an
   unrepresentable project is no longer a migration conflict. The imported
   interval carries no project, and the receipt's legacy interval audit keeps
   the original value. A migrated carry fragment keeps the legacy value
   verbatim. The `unknown_project` error code is retired.

## Alternatives rejected

- **Make the project optional but keep it on new intervals.** The field would
  still need a value, a flag and a question, while nothing would use it.
- **Rewrite existing intervals without the project.** That would rewrite
  saved and submitted records for a field nobody reads. The owner chose an
  inert label.
- **Fail closed on a configuration that still lists `projects`.** That would
  stop every existing scheduled run until the owner edits the file, for a key
  that no longer does anything. It is listed as an owner question below.
- **Keep schema 3 with an optional project.** A schema 3 build would then
  reject the new files with a misleading "interval is incomplete" error
  instead of an unsupported schema. The bump keeps the old build's failure
  honest.

## Consequences

`next` loses an input and a question. The observation no longer carries a
project, every derived argv drops `--project`, and a multi-project
configuration no longer reaches `ask_human` (12-factor factor 7: contact the
human only for a decision the human must make). Classification stays
deterministic first, through the configured default, and judged second, so
`next` remains a stateless reducer over the store and the day folder (factor
12). A script or skill that still passes `--project` fails with a one-line
instruction instead of being silently ignored.

## Open questions for the owner

- Should a configuration that still lists `projects` or
  `migration.project_map` fail closed with a one-line instruction instead of
  being ignored?
- Should `status` show the schema-4 upgrade receipt, or is the state file
  enough?
