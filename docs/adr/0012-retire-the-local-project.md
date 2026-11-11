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
   Neither label is shown: review, day and segment output, history snapshots
   included, leave both out (owner, 2026-11-11). They stay in state only.

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
   no receipt.

   The upgrade is one-way. Before the activating write, the binary copies the
   older file once to `<state>.schema<N>.bak` (for example
   `state.json.schema3.bak`, mode 0600, never overwritten). If the copy fails,
   the write fails and nothing changes. When the write activates, it prints
   one stderr line that names the backup and says the upgrade is one-way. A
   write that fails before activation leaves the older file as it was and the
   receipt unstamped; the backup may remain. A 0.2.0 build reading a schema 4
   file fails closed with `state_invalid: local state file is not valid JSON
   for the supported schema`, because its strict decoder rejects the unknown
   `migration.schema_upgrades` before it checks the schema number. To
   downgrade, restore the backup and put `projects` back in the
   configuration. The receipt records `app_version` `0.2.0` until the 0.3.0
   release that follows this change.

5. **The surface drops the project.** `--project` is gone from `init`, `log`,
   `next`, `tui`, `judge`, `stage`, `classify` and `save`. Passing it fails
   with one line that says to drop the flag, in the same way as a removed
   command (`retiredCommands`). The typed `ask_human` `project` question is
   gone too. An `ask-project.json` answer left in a day folder is ignored.
   `init` needs no project, and `--seed-config` copies the seed's
   `required_env` and `sources`.

6. **Retired configuration keys are removed, or else ignored.** The keys
   `projects` and `migration.project_map` are retired, and a configuration
   that lists them still loads (owner, 2026-11-11). On the first load in a run,
   every command except `init` removes them from the file: a token-level edit
   that keeps every other key, its order, values and formatting, and that is
   checked to change nothing else. The original is kept once as
   `<config>.bak` (0600, never overwritten). The new bytes replace the file
   atomically (temporary file in the same directory, the original mode,
   rename), and one stderr line names what was removed and the backup. If the
   file cannot be rewritten safely (read-only file or directory, a symlink,
   or a shape the edit cannot prove), nothing changes and one stderr line
   says the keys are ignored and can be removed by hand. The env file is never
   touched. `init` neither writes the keys nor edits a seed. Every other
   unknown key still fails closed.

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
  that no longer does anything. The owner chose removal, with ignoring as the
  fallback.
- **Keep schema 3 with an optional project.** A schema 3 build would then
  load a file it cannot fully read and reject only some intervals, as
  "incomplete". With the bump, the old build rejects the whole file before
  touching it (with the generic `state_invalid` message quoted in decision
  4), and the backup gives the way back.

## Consequences

`next` loses an input and a question. The observation no longer carries a
project, every derived argv drops `--project`, and a multi-project
configuration no longer reaches `ask_human` (12-factor factor 7: contact the
human only for a decision the human must make). Classification stays
deterministic first, through the configured default, and judged second, so
`next` remains a stateless reducer over the store and the day folder (factor
12). A script or skill that still passes `--project` fails with a one-line
instruction instead of being silently ignored.

## Owner decisions on the open questions (2026-11-11)

- **Retired configuration keys:** remove them from the file; when that cannot
  be done safely, ignore them and say so (decision 6).
- **The upgrade receipt in `status`:** no. `status` stays as it is, and the
  receipt lives in the state file only. Closed.
- **The legacy local classification label:** removed from review, day and
  segment output, like the project label. It stays inert in state
  (decision 2).
