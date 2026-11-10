---
status: accepted
date: 2026-11-10
deciders:
  - Huy Le Minh
related:
  - 0006
  - 0008
  - 0009
---

# Binary-owned judgments through a headless harness

The binary will answer its two judgments, descriptions and classifications,
itself instead of leaving them to an interactive coding-agent session (issue
#237, epic #246). Recent sessions spend about 41k to 552k tokens per day, almost
all of it on the agent's own system prompt, tool definitions, skill text and
extra turns. The judgment itself is a small, closed question. When the binary
builds the prompt and owns the context, a day costs what the judgment costs.

## Decisions

1. **Transport: a headless agent CLI, run by the binary.** The binary runs one
   owner-configured command per judgment, writes the prompt it built to the
   command's stdin, and reads one answer document from its stdout. The
   harness runs with no tools and with the binary's prompt as the whole
   task. This rides on whatever subscription or login the owner's harness
   already has. A direct provider API was rejected for now: it needs a
   separate key and billing, and it ties the binary to one provider.

2. **Model and harness agnostic.** The binary names no provider, model or
   harness. The configuration holds the command as an argv (for example a
   coding-agent CLI in print or exec mode, a local-model runner, or any
   wrapper script), and the model is whatever that command runs. The prompt
   asks for one JSON document in the existing answer schema. The binary
   extracts and validates it exactly as it validates an agent-written answer
   file today, so no harness-specific output parsing enters the core. Example
   commands in documentation are illustrative, never pinned.

   The daily budget is enforced on what the binary controls. The binary
   bounds every prompt it builds, and a configurable per-day cap (default
   10,000 tokens, estimated from prompt and answer size) stops further judge
   calls for the day. When the harness reports real usage, the binary records
   that. Otherwise the record is marked as an estimate. Issue #242 reports
   tokens per logged day against the epic's target of 1,000.

3. **Credentials: the harness owns its own.** The binary never reads, stores
   or passes a model credential. The harness authenticates through its own
   login on Linux and macOS alike. The existing `.env.local` policy
   (`docs/agents/credentials.md`) is unchanged for the pipeline credentials.
   The judge command's environment is the binary's own, minus the variable
   names that policy reserves (`GITHUB_TOKEN`, `GCAL_ICS_URL` and the Gradion
   token name), so pipeline secrets never reach the harness.

4. **Data boundary: the day's evidence may be sent.** The owner accepts that
   the prompt carries the same evidence an agent session sees today: PR
   titles, commit subjects, meeting titles, recorded descriptions and the
   prefetched catalog option names. Credentials, env-file contents,
   configuration and store paths are never part of a prompt.

5. **One thin front end.** After #242, the project keeps one runner-agnostic
   skill that only runs `next --do`, relays `ask_human` questions and reports
   (#244). Per-agent integrations are not maintained in parallel.

## Failure, absence and the human gates

- **No judge configured** is the default. `next` reports the judgment exactly
  as today, and the agent-driven flow works unchanged.
- **The judge is unreachable or fails:** the command is missing, exits
  nonzero, times out, prints no valid answer, or the daily cap is reached. The
  run then stops at the same digest-bound judgment it would report without a
  judge, with a one-line reason. Nothing is guessed, and the day resumes from
  either an agent-written answer or a later judge run. An answer the binary
  rejects follows the repair-once policy (#240).
- **No remote write or approval depends on model output alone.** A judge
  answer only feeds the existing local acts (`record`, `classify`). Planning
  previews the result, and approval and submission keep their human gates
  (ADR 0008, ADR 0009). The judge never answers an `ask_human` question
  (#239): those stay with the human.
- Every judge prompt and model change is scored against the golden-day eval
  (#236) before it becomes a default.

## Consequences

Issue #241 implements a judge interface with one headless-command backend,
the bounded prompts, response caching per judged digest and the usage
record. A direct-API backend can be added later behind the same interface
without changing these decisions. Token accounting is exact only when the
harness reports usage. That trade is accepted in exchange for running on
any harness and model.
