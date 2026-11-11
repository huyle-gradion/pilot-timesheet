# ADR 0004: Interactive Pre-flight Onboarding, Semantic Time Clumping, and Gradion MCP Discovery

## Status
Superseded by [ADR 0005](0005-review-first-timesheet-architecture.md) on 2026-10-09. Retained as historical context.

## Context
As the project moves from isolated fetchers into integrated workflows, five essential operational and user-experience requirements were established:

1. **Zero-Assumption Credential Onboarding (Calendar)**:
   The calendar collector must never fail with a cryptic error or assume `ICAL_URL` is configured. If missing, it must intercept execution and provide clear, step-by-step instructions on extracting the secret iCal address from Google Calendar (`calendar.google.com` -> Settings & Sharing -> Integrate calendar -> Secret address in iCal format).
2. **Zero-Assumption Credential Onboarding (GitHub)**:
   The GitHub collector must never assume a PAT exists or that `gh` CLI is logged in. It must guide the user to either run `gh auth login` or generate a token with minimal scopes (`repo`, `read:user`) at `github.com/settings/tokens`.
3. **Universal Pre-flight Invariant**:
   Every pluggable collector and downstream module must implement a `Preflight` check that tests configuration, provides actionable resolution guides, and is validated by unit tests before integration into the AI Skill or standalone CLI.
4. **Gradion Workspace MCP & App Resources Integration**:
   The timesheet sync module can leverage Gradion Workspace's installed app MCP endpoints (`https://workspace.gradion.com/api/me/apps/mcp-resources` and `/api/me/apps/mcp`) in addition to direct REST endpoints (`ts-prod.gradion.com`), enabling MCP tool invocation (`/api/me/apps/timesheet/tools/log_hours/call`) and dataset export (`/api/me/apps/timesheet/export/entries`).
5. **Work Hours Baseline & Semantic Micro-Block Clumping (< 30 min)**:
   - **Standard Core Hours**: `09:00 - 12:00` (Morning) and `13:00 - 18:00` (Afternoon), with lunch `12:00 - 13:00`.
   - **Off-hours Flexibility**: Commits or activity outside core hours (early morning, evening) are valid and must be captured.
   - **Semantic Clumping Rule**: If an activity/commit span is shorter than 30 minutes, it must not create a fragmented micro-log. If an adjacent or related task shares a similar action/intent (e.g. coding, fixing, implementing, maintaining), they must be consolidated into a single coherent time block. This reduces visual noise and streamlines supervisor reviews.

---

## Decisions

### 1. Pre-flight Check Contract (`internal/preflight`)
Every module exposes a pre-flight probe:
```go
type PreflightStatus struct {
    Ready       bool     `json:"ready"`
    MissingKeys []string `json:"missing_keys"`
    Guide       string   `json:"guide"`
}

type PreflightChecker interface {
    CheckPreflight(ctx context.Context) PreflightStatus
}
```
- In interactive CLI mode: Prompts user to input missing values and optionally stores them in macOS Keychain or `.env`. *(Amended 2026-11-11 by #194: credentials are env-file only; no Keychain storage. See [Amendments](#amendments).)*
- In headless/CI mode: Emits structured error with the resolution guide and exit code `EXIT_USAGE (64)`.
- In AI Skill (`workflow.md`): Step 0 runs pre-flight checks and provides human-readable guidance before starting collection.

### 2. Time-Clustering Normalization Rules (`internal/timesheet`)
The clustering algorithm in Milestone M2 adheres to:
1. **Advisory Work Hours Baseline**: The standard core hours `09:00 - 12:00` and `13:00 - 18:00` represent informational baseline indicators of typical work patterns. There is no rule preventing employees from committing work logs during lunch (`12:00 - 13:00`), before `09:00`, or after `18:00`.
2. **First-Class Lunch and Off-Hours Activity**: Commits or activities during lunch (`12:00 - 13:00`) or off-hours are fully valid work. Activity during lunch is clustered into standard focus blocks without artificial splits or truncation. Normal inter-commit pauses (> 30 to 60 minutes) separate morning and afternoon blocks naturally.
3. **Semantic Micro-Block Clumping**:
   - Threshold: Span duration < 30 minutes.
   - Similarity Match: Actions matching the same category (`development`: coding, implementing, refactoring; `maintenance`: fixing, bugfix, deps, lint; `research_docs`: docs, design, adr).
   - Action: Merge into the nearest adjacent or topic-matched focus block within 60 minutes, updating start/end times and appending bullet descriptions.
4. **Existing Timesheet Updates (M2-02, M2-03, M3-01)**:
   - Users can update old or existing daily timesheets.
   - Storage supports atomic upserts, incrementing record revision versions and stamping update timestamps.
   - The review CLI allows loading past dates, modifying time blocks, adding or deleting entries, and persisting changes.

### 3. Gradion Workspace MCP Discovery Support
The timesheet client in Milestone M3 will support dual reconciliation paths:
- **Direct REST Client**: Direct calls to `ts-prod.gradion.com/api/entries`.
- **Gradion Workspace MCP Client**: Calling `/api/me/apps/timesheet/tools/log_hours/call` via Bearer PAT and `X-Gradion-Skill-Bundle-Version: 1.9.0` discovered from `https://workspace.gradion.com/api/me/apps/mcp-resources`.

---

## Consequences
- Eliminates onboarding hurdles and confusing crashes for new users.
- Produces clean, readable timesheets with zero micro-block clutter for supervisor review.
- Establishes a standard contract for all future plugins (Jira, Slack, etc.).

## Amendments

### 2026-11-11 — credentials are env-file only (#194)

The Keychain half of the interactive-prompt decision above no longer holds.
Credential values live only in the owner-authored env file
(`~/.timesheet/.env.local` by default, or `--env-file`), which only the compiled
binary reads; the Keychain resolver was retired and no Keychain storage or
fallback exists on any platform. `timesheet init` creates that file with
commented variable names and no values, and nothing in the product prompts for,
stores or writes a credential value. The active policy is
[credentials](../agents/credentials.md). The rest of this ADR stands as
written, as superseded historical context.
