# Interactive workflow (`timesheet tui` and terminal `submit`)

This note is for a human working a day at a terminal. The binary owns the day:
`timesheet log` runs every deterministic step and, when a `judge` section is
configured, answers its own judgments ([ADR 0010](../adr/0010-binary-owned-judgments-behind-one-judge-interface.md)).
A human only steps in at the gates the binary reports. `timesheet help` lists
the public commands.

The old guided `run --interactive` / `run --plain` review, its `approve --plan`
follow-up and the `internal/run` package were removed in the CLI cut-over
(f2c80880). This file keeps its path so existing links still resolve.

## The flow

1. **Run the day.** `bin/timesheet log --date <YYYY-MM-DD>` (default: today in
   the configured timezone). It reports one typed `state`:
   - `human_gates`: the day is saved and planned. Approve and submit in the
     TUI (step 2) or with terminal `submit` (step 3).
   - `ask_human`: a decision only the owner makes. Answer it in a terminal
     with `bin/timesheet answer --date <YYYY-MM-DD> [--kind <kind>] <answer>`,
     in the TUI's answer form, or through the agent skill, then run `log`
     again.
   - `judgment`: no judge is configured. Answer it in the TUI or through the
     agent skill's fallback, then run `log` again.
   - `stopped`: read the reason; nothing was submitted.
2. **Look at or finish the day in the TUI (optional).**
   `bin/timesheet tui --date <YYYY-MM-DD>` shows the same derived state that
   `log` and `next` report. `enter` runs the step the state allows, `r`
   re-derives it, and `q` exits. At `human_gates`, `enter` opens the approve
   gate and `s` opens the submit gate. Saving is the only thing that makes work
   eligible for a plan. See [Native TUI operator flow](../../README.md#native-tui-operator-flow)
   for the full key and form list.
3. **Or approve and submit with terminal `submit`.**
   `bin/timesheet submit --date <YYYY-MM-DD>` shows the exact plan, asks you to
   type its `plan_id` to approve, then asks you to type the `approval_id` to
   submit. If you stop between the two, the approval is kept; resume with
   `bin/timesheet submit --date <YYYY-MM-DD> --approval-id <id>`.
4. **Check or resolve.** `bin/timesheet submit status --date <YYYY-MM-DD>`
   reports outcomes and unresolved writes. An unresolved write is settled only
   by a human with `bin/timesheet submit resolve --file <decision.json>`.

`log`, `status` and `next` never approve or submit. The TUI and terminal
`submit` do, each behind a typed `plan_id` (approve) and `approval_id`
(submit) echo. An agent never
runs `submit` or `submit resolve`; chat-confirmed submission (`submit --chat`)
follows [ADR 0009](../adr/0009-chat-confirmed-submission.md) and the `/submit`
skill.

## Binary provenance

Build once with `go build -o ./bin/timesheet ./cmd/timesheet` and reuse the
binary for a session. `go run ./cmd/timesheet` rebuilds on every call, which
adds build time to every step and does not change what the commands do.

## Terminal requirements

`tui` and terminal `submit` refuse to start unless both stdin and stdout are
real terminals. They never fall back to a non-interactive mode. Use `log` and
`status` for headless or piped use.
