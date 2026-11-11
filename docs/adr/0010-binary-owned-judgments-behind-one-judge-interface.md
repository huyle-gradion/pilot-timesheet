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

# Binary-owned judgments behind one judge interface

The binary will answer its two judgments, descriptions and classifications,
itself instead of leaving them to an interactive coding-agent session (issue
#237, epic #246). Recent sessions spend about 41k to 552k tokens per day, almost
all of it on the agent's own system prompt, tool definitions, skill text and
extra turns. The judgment itself is a small, closed question. When the binary
builds the prompt and owns the context, a day costs what the judgment costs.

## Decisions

1. **Transport: two backends behind one judge interface, chosen in
   configuration.** The binary builds the prompt and validates the answer the
   same way for both:

   - **Headless agent command (the default).** The binary runs one
     owner-configured command per judgment, writes the prompt to its stdin,
     and reads one answer document from its stdout. The harness runs with no
     tools and with the binary's prompt as the whole task. This rides on
     whatever subscription or login the owner's harness already has. Its cost
     is the harness's own setup overhead and inexact token counts.
   - **OpenAI-compatible HTTP endpoint.** The binary sends one chat-completions
     request to a configured base URL and model. This covers hosted providers
     that offer the compatible API, gateways (one endpoint for many models)
     and local runners, which need no key. Only the binary's prompt is sent,
     and the response reports exact usage. It needs a key and per-token
     billing unless the endpoint is local.

   The interface lets the owner start on the subscription and move to a key
   or a local model by changing configuration, not by writing a new ADR. A
   provider-specific SDK was rejected for the core: it would tie the binary to
   one provider. Another backend can be added behind the same interface
   later.

2. **Model and harness agnostic.** The binary names no provider, model or
   harness. For the headless backend, the configuration holds the command as
   an argv (for example a coding-agent CLI in print or exec mode, or any
   wrapper script), and the model is whatever that command runs. For the HTTP
   backend, it holds the base URL and the model name. The prompt
   asks for one JSON document in the existing answer schema. The binary
   extracts and validates it exactly as it validates an agent-written answer
   file today, so no harness-specific output parsing enters the core. Example
   commands in documentation are illustrative, never pinned.

   The daily budget is enforced on what the binary controls. The binary
   bounds every prompt it builds, and a configurable per-day cap (default
   10,000 tokens, estimated from prompt and answer size) stops further judge
   calls for the day. The HTTP backend always records the endpoint's reported
   usage. The headless backend records real usage when the harness reports it;
   otherwise the record is marked as an estimate. Issue #242 reports
   tokens per logged day against the epic's target of 1,000.

3. **Credentials follow the existing policy.** For the headless backend, the
   harness owns its own login, and the binary never reads, stores or passes
   it. For the HTTP backend, the key is one more name in the owner-authored
   `.env.local`, read only by the binary (`docs/agents/credentials.md`). The
   configuration holds only the variable's name (as `gradion.token_env` does
   today), no name means no key (for a local endpoint), and the value is
   never echoed. Both work the same way on Linux and macOS. The judge
   command's environment is the binary's own minus every credential name the
   policy reserves (`GITHUB_TOKEN`, `GCAL_ICS_URL`, the Gradion token name and
   the judge key name), so pipeline secrets never reach a harness.

4. **Data boundary: the day's evidence may be sent.** The owner accepts that
   the prompt carries the same evidence an agent session sees today: PR
   titles, commit subjects, meeting titles, recorded descriptions and the
   prefetched catalog option names. Credentials, env-file contents,
   configuration and store paths are never part of a prompt.

5. **One thin front end.** After #242, the project keeps one runner-agnostic
   skill that only runs `next --do`, relays `ask_human` questions and reports
   (#244). Per-agent integrations are not maintained in parallel.
   *(Amended 2026-11-11 by #291: since #262 the skill runs `timesheet log`,
   not `next --do`. See [Amendments](#amendments).)*

## Failure, absence and the human gates

- **No judge configured** is the default. `next` reports the judgment exactly
  as today, and the agent-driven flow works unchanged.
- **The judge is unreachable or fails:** the command is missing, exits
  nonzero or times out; the endpoint is unreachable or returns an error; no
  valid answer comes back; or the daily cap is reached. The
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

Issue #241 implements the judge interface and both backends (headless command
and OpenAI-compatible HTTP), the bounded prompts, response caching per judged
digest and the usage record. On the default headless backend, token
accounting is exact only when the harness reports usage. That trade is
accepted in exchange for running on the owner's subscription; the HTTP
backend is the exact and lowest-overhead path when a key or a local model is
available.

## Amendments

### 2026-11-11 — the skill runs `log`, not `next --do` (#291)

Decision 5 names the command the thin front end runs as `next --do`. Since the
public command surface landed (#262), the public verb is `log`, and the skill
runs `timesheet log` (`.agents/skills/timesheet/SKILL.md`). `log` always runs
the day: it is `next --do`, answering judgments in-process with `--auto` when a
judge is configured, and it rejects `--do` and `--auto` as flags. `next` remains
a public, read-only command that prints the one action it derives. The decision
itself stands: one runner-agnostic skill that runs the day, relays `ask_human`
questions and reports, with no per-agent integrations maintained in parallel.
