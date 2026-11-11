# Pilot Timesheet implementation specification

Status: product design accepted by the owner on 2026-10-22. Implementation kickoff requested; execution remains owner-mediated (personal orchestration records live out-of-tree, not in this repository). This document supersedes conflicting earlier product requirements for the application, not historical records or source ownership.

## Objective

Replace the existing application with a review-first timesheet workflow that does not require an agent to discover files, read records, or orchestrate deterministic steps. One compiled Go process owns the daily workflow. Pi provides local forms and optional conversation/editorial assistance.

The supplied Antigravity transcript is the baseline: 30 tool calls before the first daily review question, and a whole-file reported token-component sum of 2,490,890. That sum is not a verified provider bill. See [research and evidence](research-timesheet-token-efficiency.md).

## Authority and exclusions

- This kickoff permits requirements, execution planning, a bounded worker packet, and excluding credential files from Git. No worker has been dispatched.
- Source implementation follows the named owner-mediated packet. Commit, publication, issue/PR mutation, release tags, credential retrieval, live collection, and production writes are not authorized by this kickoff.
- No ai-database/vault updates. Remove that integration from the target product and operating workflow. Existing owner files and historical records remain untouched.
- Tests are optional, not a delivery gate. Observable execution must support behavioral claims.
- Effective Go is the minimum coding baseline: <https://go.dev/doc/effective_go>. TigerStyle and `docs/engineering/go-rules.md` are not mandatory for this implementation.

## Daily workflow

1. `/timesheet <date>` starts or reuses one compiled process for the uninterrupted daily Pi session.
2. The binary loads non-secret configuration and credentials internally, collects GitHub and calendar evidence, and groups related activity.
3. Pi renders the review locally. Scrolling, detail inspection, typed changes, source acknowledgment, local confirmation, and submission approval do not require model calls.
4. The user supplies working intervals and explicitly confirms attendance for proposed calendar intervals. GitHub activity timestamps never establish working duration.
5. The user chooses save-local, revise, discard, or review-submission. Save-local persists reviewed work without remote writes. Discarding (leaving without saving) drops unapplied review changes and preserves prior confirmed work.
6. Review-submission shows the exact remote creates/edits and total. Approval authorizes only that exact Submission Plan. Approval may then lead to execution without asking the model to rediscover another command.
7. The binary reports verified outcomes. Uncertain writes remain unresolved and block blind replay.

Confirmation establishes recorded work. Approval authorizes a Submission Plan. Acknowledging failed collection establishes neither. Use these terms consistently with `GLOSSARY.md`.

## Evidence sources

Only GitHub and calendar are evidence plugins. Gradion is the submission destination, not a third evidence plugin.

### GitHub

Collect the configured user's attributable activity within explicitly configured repositories across all selected scopes:

- Commits and authored pull requests.
- Submitted PR reviews.
- Issue activity/comments, discussions, and other attributable repository interactions.

Preserve provider identity, event time, repository, resource links, and provenance. Group related evidence without counting it as additional time. Do not mistake a resource's most recent update by another actor for the user's activity. Exhaust pagination where supported; declare incomplete coverage for inaccessible activity families, unavailable historical detail, permission failures, bounds, or partial enumeration. Never claim complete coverage merely because a feed returned some events. The collector's implementation must document its verified API coverage.

No local repository scanning, local-git collector, agent-session collector, or journal collector.

### Calendar

Collect configured calendar evidence with original event identity and timing, recurrence/exception handling, timezone-aware day selection, and attendance/invitation status where available. Calendar intervals are proposals, not confirmed work. Cancelled or declined events must not silently become work.

### Source failures

Name each failed/incomplete source and the missing coverage. Permit explicit human acknowledgment and manual completion. Keep incompleteness visible and persisted even after acknowledgment. Do not retry writes or fetch unrelated files to compensate. No model-controlled acknowledgment field.

## Time and classification

- Show GitHub Topic Candidates and observed activity bounds with unknown worked duration.
- Parse user-supplied structured intervals in Go. No model is needed to interpret the supported typed grammar.
- Reject invalid spans and unresolved accounting collisions. Multiple evidence links add no minutes.
- Calendar attendance, working time, classification, and description remain reviewable by the user. An interval has no project: the remote has none, so classification comes from the configured default or the classification judgment ([ADR 0012](adr/0012-retire-the-local-project.md)).
- Classification suggestions are not applied silently. Persist explicit selections; verify destination identifiers before submission.
- Optional AI changes titles/descriptions only, never time, attendance, classification, evidence links, confirmation, or approval.
- Use configured timezone and calendrical day windows. Surface cross-midnight work explicitly rather than losing or duplicating minutes.
- An overnight entry with more than 240 minutes after midnight keeps the part up to midnight plus up to 240 minutes after it. The remainder is carried into the next date's timesheet as a derived record, shown at that day's review and submitted only through its own approval. It never silently revises a saved day ([ADR 0011](adr/0011-overnight-carry-into-the-next-day.md)).

The concrete typed grammar and error codes are part of the public interface and must be documented when implemented.

## Configuration and credentials

Credentials are stored in `.env.local`. This replaces the earlier Keychain-first proposal. No new Keychain dependency is required.

- Only the compiled binary loads `.env.local` and non-secret configuration. A model session never reads either file.
- The operator passes fixed configuration/environment-file paths as command-line flags. The model cannot supply arbitrary paths or executable names.
- Inherited process environment values take precedence over matching `.env.local` assignments, including explicitly empty values; the binary validates the resulting required values.
- Parse documented dotenv assignments as data. Support ordinary unquoted/single-quoted/double-quoted values; do not execute shell expressions, source the file, expand commands, or infer undocumented dotenv features.
- Missing required values produce variable-name-only diagnostics. Do not echo values, authenticated URLs, source payloads, or raw parser lines.
- Pass only necessary environment variables to the child. Do not forward the chat provider's credentials unnecessarily.
- Never read `.env.local` with agent tools, paste it, stage it, include it in snapshots, or print it. Provide only placeholder examples if needed.
- Configuration is never attached to a model request or exported report. User-facing setup and credential checks operate locally through the binary.
- macOS is the first supported target. Environment-based credentials avoid requiring Keychain on fallback environments; Linux support is not claimed without runtime evidence.

## Pi integration and token contract

Use a supported chat model through an available authenticated Pi provider. Luna is not required; subscription compatibility must be verified for the selected provider. Do not add classifier calls without a demonstrated task.

- Direct `/timesheet` commands and local review use no model calls.
- Optional natural-language initiation uses one narrowly scoped preparation tool, not shell commands or file reads.
- Model-visible results contain bounded status and next action only. Full review data is rendered locally, not inserted into model context. Local UI state should hold identifiers rather than duplicating complete records in Pi logs.
- Do not let an automatic model follow-up restate successful deterministic output.
- Expose no model tools for filesystem read/write/search, shell, arbitrary MCP calls, source acknowledgment, work confirmation, or approval/submission.
- Explicitly load the trusted adapter in a dedicated daily profile, without unrelated project context, skills, or extensions.
- The adapter has transport/UI responsibilities only. Accounting, validation, persistence, rendering data, export formatting, and submission decisions belong in Go.
- Use one session-scoped child over private stdin/stdout messages, launched without a shell. A daemon or persistence across Pi reloads is not required.
- Treat model text, evidence text, and terminal output as untrusted data. Tool removal is capability restriction, not an OS sandbox.
- Token reduction is best effort. Record actual model calls and available input/cache/output counters without inventing missing totals or costs. Do not import a growing transcript to answer a usage question.

## Durable state, migration, and remote writes

Preserve confirmed intervals, classifications, provenance, revisions, stable remote identities/mappings, and submission history through an explicit migration. Historical provenance from retired sources may remain without enabling those collectors.

Migration must leave original stores intact, validate before activation, be repeat-safe, and never submit. Old approvals are historical data only and cannot become active authorization. Preserve unresolved/in-flight outcomes as blockers, not permission to replay.

Before creating entries, reconcile current remote entries and migrated mappings. Ambiguous matches require human resolution. Never infer a safe create merely because a new store is empty.

Approval is bound to the exact daily record revision, Submission Plan content, and relevant remote observation. Changed plans require a new preview and approval. Persist remote-write intent before mutation. Verify outcome content and identity; a response that merely acknowledges an ID is not proof of success. Do not delete remote entries or blindly retry uncertain writes.

Local state is necessary even though ai-database is removed. Local saved records are the initial local output; no replacement note system or extra export format is required. Full records must not be model-readable.

## SemVer and release contract

Follow [Semantic Versioning 2.0.0](https://semver.org/). The observed local baseline has snapshot tags but no SemVer release tag; this does not prove no external release exists. Start the documented implementation line at `0.1.0` unless release inventory later establishes an existing published version that must be respected.

Public compatibility includes CLI flags/exit behavior, typed review input, the Pi-to-binary protocol, configuration and environment-variable names/precedence, and supported durable-state migration. Internal Go implementation details are not public contracts.

- In-development builds identify as `0.1.0-dev.<n>`; this kickoff does not create a binary version or a release.
- Release candidates progress through `0.1.0-alpha.<n>`, `0.1.0-beta.<n>`, and `0.1.0-rc.<n>` only when their documented scope runs. Do not label scaffolding as a release.
- Before 1.0, increment the minor version for incompatible public-interface changes and the patch version for compatible fixes; document changes and required migrations.
- `1.0.0` declares the complete accepted public interface stable. After that, breaking public changes increment MAJOR, compatible additions MINOR, and compatible fixes PATCH.
- Tags use `v` plus the version. Released artifacts/tags are immutable. Release creation/publication requires separate owner authority.
- The binary identifies its application version (`timesheet version`). The separate integer protocol schema it once negotiated retired with `serve` (issue #272): no process speaks a wire protocol to the binary, and the state schema still fails closed on mismatch.
- Maintain `CHANGELOG.md`. An Unreleased entry is not proof of implementation or a published release.

## Completion evidence

the application is complete only when the real executable and Pi UI demonstrate GitHub/calendar collection, manual interval entry, explicit attendance and incomplete-source decisions, local save/revise/discard, restart-safe durable state, repeat-safe migration, remote reconciliation, exact-plan approval, verified submission outcomes, and restricted model tools. Use hermetic sources for ordinary development; live proof requires separately authorized credentials and service scope.

Show the actual review surface and receipt, not only unit results. Measure the daily model-visible workflow against the transcript baseline without claiming an unmeasured reduction. The macOS runtime and chosen model/subscription path must be exercised before support is claimed.

Independent review of the exact implementation snapshot remains separate from optional tests. Do not claim production readiness from documentation, a build, simulated services, or source acceptance alone.

## References

- Execution plan: `../.omo/plans/timesheet.md`
- Personal orchestration records live out-of-tree; they are no longer tracked in this repository
- Research is provenance, not required daily-agent context.
