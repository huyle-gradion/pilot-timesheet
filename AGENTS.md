# Agent Instructions

## Publication and versioning

- Use GitHub account `huyle-gradion` and repository-local Git identity `Huy Le <huy.leminh@gradion.com>`. Verify author, committer and authenticated account before publication; on this host use `GH_CONFIG_DIR=$HOME/.config/gh` because the agent runtime's XDG config selects a different account. Confirm `gh api user --jq .login` is `huyle-gradion`. Keep global Git settings unchanged.
- Use **Pilot Timesheet** as the public product name and neutral product terminology in source, documentation and release metadata.
- Every shipped application changeset updates SemVer consistently across Go, adapter negotiation, package/root-lock stamps, changelog and release tag.
- Preserve published commits and tags unless the owner explicitly authorizes changing history.
- Any Go toolchain matching `go.mod` builds the repository; a mise-managed Go path is local environment troubleshooting, not a project dependency.

## Agent skills

### Orchestration

Personal workflow and orchestration notes — coordination authority, worker
handoffs, review gates and staged redesign plans — are kept out of the public
tree; they live in a private archive outside the repository. A plan is not
permission to implement or submit work.

### Issue tracker

GitHub issues via `gh` CLI. See `docs/agents/issue-tracker.md`.

### Triage labels

Canonical 5-role vocabulary (`needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`). See `docs/agents/triage-labels.md`.

### Domain docs

Single-context repo (`GLOSSARY.md` + `docs/adr/`). See `docs/agents/domain.md`.

### Keychain credentials

Owner wants all three pipeline credentials (Gradion Workspace MCP, GitHub token,
iCal URL) stored in the macOS Keychain, with code reading from there. See
`docs/agents/credentials.md`.

## Knowledge base integration

- **Vault**: the owner's private Obsidian vault, local to the owner's machine (paths intentionally not recorded here)
- **Project Note**: the project note in the private vault's Projects folder
- **Architecture Decisions**: ADRs mirrored on the private vault
- **Daily Logs**: the vault's daily-log folder

### Knowledge base operating rules

1. **ADR & Architecture checks**: Consult `Project - assignment-1.md` and related ADRs before making major architectural changes. Record formal decisions as ADRs in `05 - Architecture Decisions/`.
2. **Project tracking**: Keep `Project - assignment-1.md` updated with completed milestones and active tasks formatted as `- [ ] Task [[@Assignee]] [due:: YYYY-MM-DD]`.
3. **Daily impact logging**: When completing significant features or closing sessions, log standup summary and quantified accomplishment bullets ("Accomplished [X] as measured by [Y] by doing [Z]") into today's log in `02 - Daily & Logs/`.

## Timesheet daily log (skill)

When asked to "log today's work" or "log the timesheet" for a day, follow
`.agents/skills/timesheet/SKILL.md` exactly. The skill resolves `TS_BINARY`
through its own `scripts/ts-env` helper, checks `next --help`, and builds once
if the found binary predates the planner; it then drives the day with one `next`
JSON loop: each invocation prints exactly one `action` (run its `action.cmd`
argv verbatim, never through a shell), `judgment` (write the model answer file
it names, copying the report's `draft_digest`/`ledger_digest` verbatim),
`stopped` (report the precise blocker and stop) or `human_gates` (run
read-only `submit-status`, report the ready plan, stop).
The descriptions judgment reads only the draft the protocol names; the
classification judgment is self-contained in `judgment.blocks[]` and needs no
draft or `$TMP` artifact. The binary enforces repair-once: a rejected
answer is re-asked once with `last_error`, then stops as `judgment_failed`. Stopped
states are never silently acknowledged, guessed or resolved; an explicitly
user-authorized decision is performed exactly once where safe. It is
runner-agnostic (Pi or any agent with shell access). It never runs the
TTY-gated `approve`, `submit` or `resolve-write` — those need an interactive
terminal and the exact typed `plan_id`/`approval_id`/`write_ref`, and
attempting them is a contract violation. Submission is never auto-retried, and
unknown outcomes stay unresolved until a human resolves them. Run the skill's
mandatory terminal metrics helper on success and blocked exits. Capture errors
are reported without blocking the day.

## Chat-confirmed submission

On `/submit` or a natural-language request to submit, follow
`.agents/skills/submit/SKILL.md`. Review the exact day and all operations first,
then wait for explicit confirmation in chat. Only that confirmation authorizes
one `agent-submit` invocation with the exact reviewed plan and review IDs.
This is agent-enforced consent, not proof of human presence. Logging or status
never authorizes writes. The optional trusted Pi adapter gains no write tools.
