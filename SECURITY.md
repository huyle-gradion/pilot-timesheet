# Security Policy

## Reporting a vulnerability

Report vulnerabilities privately through a **GitHub private security
advisory** on this repository (Security → Report a vulnerability). State it
plainly: there is no `security@` email address for this project; the private
advisory channel is the only reporting route. Do not open a public issue for
a security report.

Please include what you found, how to reproduce it, the affected version or
commit, and what you believe the impact is. Reports are reviewed promptly and
fairly.

## What counts

Anything on this list is in scope:

- **Binaries.** The compiled `timesheet` binary — argument handling, exit
  codes, store/state handling, and any path that spills data
  outside the documented surface.
- **Command interface.** The JSON the commands read and print (decision and
  answer files, receipts) — schema confusion, unvalidated input,
  digest-binding or replay issues.
- **Credential handling.** How the binary reads the `.env.local` env file;
  any path where a credential value could be echoed into diagnostics,
  errors, receipts, state, reports or logs.
- **Submission audit chain.** The integrity of the human-gated terminal
  states — `submit` TTY gating (approve and submit confirmations), plan/approval identity binding,
  audit history — up to and including the remote submission itself.

## Supported versions

The unreleased development line on `main` is supported. There are no
published releases under this policy yet (the historical `0.1.1` stamp
predates it); fixes land on `main` first.

## Where data lives

- `~/.timesheet` — the state directory. Entries live under
  `~/.timesheet/entries/<date>.json`; that directory is the human ledger and
  staging source.
- The env file pointed at by `--env-file` (default `~/.timesheet/.env.local`)
  holds tokens. Only the compiled binary reads it — never a model or an agent
  harness, which receive the path, not the contents.
- Credentials are never echoed: no secret value appears in diagnostics,
  errors, receipts, state or reports; only variable names are reported.

## Fail-closed stance

The project is deliberately fail-closed:

- `submit` (terminal mode) and `submit resolve` require a real TTY on stdin and
  stdout plus the exact typed plan/approval identifier. A pty wrapper
  defeats the check, so it is treated as a bypass, not a workaround.
- A failed action is never retried as-is; unknown submission outcomes stay
  unresolved until a human resolves them.
- The separate opt-in `submit --chat` mode accepts an exact reviewed plan
  and context binding with an explicit chat-confirmation assertion. The
  [submit skill](.agents/skills/submit/SKILL.md) enforces review and fresh human
  consent in chat. Flags do not prove consent or human presence. This path
  deliberately trusts the agent to honor that policy; it does not claim the
  terminal barrier applies to all writes. No model gains write capability.
  Stale plans/context, single-use approvals and durable
  uncertain-write barriers remain enforced by the binary.
- Validation is lazy, but remote submission never proceeds without an exact
  human-approved plan.

Bypassing a legacy terminal gate or a plan/context/replay invariant, or making
a secret appear where only names belong, is a high-priority report. A scripted
chat-confirmation assertion is a disclosed trust limit, not human-presence proof.
