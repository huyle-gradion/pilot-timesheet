# Contributing to Pilot Timesheet

Thanks for your interest in contributing. This is a single-compiled-binary Go
CLI that owns all accounting, validation and persistence for a pilot timesheet
workflow; a small surface, with strong conventions the code already encodes.

## Issues first

Start with an issue. Open one before a non-trivial change, describe the
behavior you want to change and why, and let it be triaged. Existing issues:
the GitHub issue tracker. Small, obvious fixes may reference an existing issue
in the PR instead.

## Pull requests

- Work on a topic branch named for the change (for example
  `docs/oss-standards`), not a long-lived personal branch.
- One topic per PR. Keep the diff focused; unrelated changes get their own PR.
- The PR body must follow the established format (see
  `.github/PULL_REQUEST_TEMPLATE.md`): **Summary**, **Measured**,
  **FS / Acceptance criteria**, **Known limits**, **Tests**,
  **Definition of Done**. A PR without it is not reviewable here.

## Local checks (required, in every PR)

Run all three locally and report the results in the PR's **Tests** section:

```bash
go build -trimpath -o ./bin-timesheet ./cmd/timesheet
go test -race -shuffle=on ./...
go vet ./...
```

On hosts where `go` is not on `PATH`, prefix with `mise exec --` (mise is
environment troubleshooting, not a project dependency).

## Conventions the code encodes

These are enforced by the binary and the repository, not just preferred:

- **Never run or automate the TTY-gated human verbs.** `submit` (terminal
  mode) and `submit resolve` refuse non-TTY stdin/stdout by design; they exist so a
  human pushes the final button. A pty wrapper does not make them agent-safe
  — it defeats exactly the check they implement. Run `submit status`
  (read-only) and stop there unless the user explicitly requests the separate
  chat-confirmed workflow below.
- **Chat-confirmed submission is a separate opt-in workflow.** Follow
  `.agents/skills/submit/SKILL.md`: show the exact current plan, wait for fresh
  explicit confirmation in chat, then invoke `submit --chat` once with the
  reviewed identifiers. This does not authorize the legacy TTY verbs or
  `submit resolve`. Chat consent is agent-enforced policy, not proof of human
  presence; stale reviews and uncertain outcomes require a stop, never an
  automatic retry.
  This workflow works in regular Pi and pig sessions.
- **Never echo or copy credentials.** The `.env.local` env file holds tokens
  (for example `GITHUB_TOKEN`, `GCAL_ICS_URL`, `GRADION_MCP_TOKEN`); only the
  compiled binary reads it, and no credential value ever appears in
  diagnostics, errors, receipts, state or reports. Do not cat, print, copy or
  store credential values in tools, tests or logs. See
  `docs/agents/credentials.md`.
- **No live-model calls outside the documented plumbing.** Do not add
  code paths that call a live model; the documented plumbing is the only
  sanctioned route.
- **Keep the judgment token budget honest in docs.** The editorial judgment
  lane has a target of 1,000 tokens/day. Record usage truthfully — real
  numbers or explicit "unknown", never a fabricated zero or a rounded-down
  figure.

## Go style

- Modern standard library first; reach for a dependency only with a reason.
- gofumpt-style formatting (`gofumpt`-clean); no dogmatic line length, but
  keep lines readable.
- Typed errors; wrap with `%w` and let callers pick apart errors with
  `errors.Is`/`errors.As`, not string matching.
- Tests pin behavior, not implementation: assert what a user of the code
  observes, so internals can change without breaking the test.

## Licensing

By contributing, you agree that your contributions are licensed under the
[MIT License](LICENSE) that covers the repository. Your work is subject to
the same license; there is no separate contributor license agreement.
