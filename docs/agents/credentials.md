# Credentials and environment (active policy)

Owner policy (superseded the 2026-10-09 Keychain-only note): pipeline
credentials live in a local `.env.local` file and are read **only** by the
compiled `bin/timesheet` binary. A model session never reads the file;
callers pass its path.

## Active rules

- **One owner-authored file.** `.env.local` at the repository root is
  git-ignored and owner-authored. Nothing in this repository creates, implementations
  or logs it.
- **Binary-owned.** The `timesheet` binary (`--env-file <path>`, defaulting
  to `~/.timesheet/.env.local`) is the only reader. The path is an operator
  input, never a request field and never a model argument.
- **Precedence.** An inherited process-environment value overrides the file
  value for the same name, including an explicitly empty value. A file-only
  value is reported present. Validation is lazy: `timesheet status` reports
  required and missing **names** under `environment`; local review and `save` never require a credential.
- **Config holds names only.** A config file may reserve credential variable
  **names** (`required_env`, and `gradion.token_env` for the remote endpoint).
  It never contains a secret value.
- **Never echoed.** No credential value, dotenv line, authenticated URL or
  config dump appears in diagnostics, errors, receipts, state or reports. Only
  variable names are ever reported.
- **Linux and macOS use the same path.** macOS-first is a platform-
  verification requirement, not permission to revive the retired Keychain
  resolver. The current build links no Keychain code and has no fallback to
  one.

## Names used by the current build

| Variable | Used by | Notes |
| --- | --- | --- |
| `GITHUB_TOKEN` | `sources.github` collection | Bearer credential for the configured `api_base`. |
| `GCAL_ICS_URL` | `sources.calendar` collection | Private feed URL; never stored in config or state. |
| `GRADION_MCP_TOKEN` | `gradion` read/submit boundary | Variable name configurable via `gradion.token_env`; default shown. |
| (owner-named) | `judge` OpenAI-compatible backend (ADR 0010) | Optional: named in `judge.api_key_env`, no default; leave it empty for a local endpoint. The `command` backend uses the harness's own login and no variable. |

`required_env` in the config document is a name-only reservation. Declaring a
name never makes a local operation fail and never reaches the network by
itself.

## File syntax

Read by the compiled binary as data:

- blank lines and `#` comment lines are ignored;
- `KEY=VALUE`, keys matching `[A-Za-z_][A-Za-z0-9_]*`;
- unquoted values are trimmed and end at a whitespace-prefixed `#`;
- single-quoted values are literal;
- double-quoted values support `\\`, `\"`, `\n`, `\t`.

Not supported: `export` prefixes, command substitution, variable expansion,
sourcing, or other dotenv extensions. Invalid syntax reports only a line number
and a reason, never the raw line or value.

`cmd/timesheet/timesheet.example.json` and `.env.example` list synthetic
placeholder names and values only.

## Safety

- Never echo, log or commit a credential, and never pass one on a command line
  (process arguments are visible to `ps`).
- Never read, copy or modify `.env.local`, an owner profile, a real config or
  a real record outside an explicitly authorized task.
- The remote boundary performs no write without an exact-plan approval and a
  verified read-back; a credential never authorizes a write by itself.

## Historical record (retired)

Owner decision of 2026-10-09 stored the three credentials in the macOS
Keychain and the then-legacy pipeline read them with
`internal/keychain` (`security find-generic-password`, service names
`gradion.workspace.ApiKey`, `gh_auth_token`, `calendar_feed_url`). That
resolver, its package and the legacy CLI call sites were retired in R6 and are
**not** part of the current product. Do not reintroduce Keychain-only
resolution or add a Keychain fallback; the active policy above is the
supported path. The owner remains the only party who creates or changes
credential values.
