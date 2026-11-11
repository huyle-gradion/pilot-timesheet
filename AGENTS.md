# Agent Instructions

## Publication and versioning

- Use GitHub account `huyle-gradion` and repository-local Git identity `Huy Le <huy.leminh@gradion.com>`. Verify author, committer and authenticated account before publication; on this host use `GH_CONFIG_DIR=$HOME/.config/gh` because the agent runtime's XDG config selects a different account. Confirm `gh api user --jq .login` is `huyle-gradion`. Keep global Git settings unchanged.
- Use **Pilot Timesheet** as the public product name and neutral product terminology in source, documentation and release metadata.
- Every shipped application changeset updates SemVer consistently across Go, package/root-lock stamps, changelog and release tag.
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
`.agents/skills/timesheet/SKILL.md` exactly. The skill finds the binary with
its `scripts/ts-env` helper and runs `timesheet log`: the binary runs the
day and answers its own judgments (ADR 0010). The agent acts on the one
`state` it reports. At `human_gates` it reports the ready day and its usage.
At `ask_human` it relays the question verbatim and writes only the user's own
answer; it never answers it. At `stopped` it reports the reason and stops. It
never runs the terminal-gated `submit` or `submit resolve`. Without a judge
configured, `log` stops at a `judgment` and the agent follows the skill's
`fallback.md`, under the binary's repair-once policy.

## Chat-confirmed submission

On `/submit` or a natural-language request to submit, follow
`.agents/skills/submit/SKILL.md`. Review the exact day and all operations first,
then wait for explicit confirmation in chat. Only that confirmation authorizes
one `submit --chat` invocation with the exact reviewed plan and review IDs.
This is agent-enforced consent, not proof of human presence. Logging or status
never authorizes writes. The optional trusted Pi adapter gains no write tools.
