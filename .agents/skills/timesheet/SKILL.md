---
name: timesheet
description: Log today's work into the personal timesheet from real git, pull-request and calendar evidence. Run when the user asks to "log today's work" or "log the timesheet", or a past working day.
---

# Pilot Timesheet

The binary runs the whole day and answers its own judgments. You start it, relay its questions and report its result.

1. **Setup.** `TS_BINARY="$("$SKILL_DIR/scripts/ts-env")"`, where `SKILL_DIR` is this skill's directory. It finds or rebuilds the binary; if it fails, report its error and stop. Never read the env file.
2. **Run** `"$TS_BINARY" log` (add `--date YYYY-MM-DD` for a past day). Pass the first report's `day` as `--date` on every later run, including after an `ask_human` answer, so a run that crosses midnight stays on its day. Act on its one `state`:
   - `human_gates`: report that the day is ready, with `usage` when present and `human.status` (unresolved writes, next action); if `human.status` is missing, say the submission status is unavailable rather than clear. Approval and submission are the user's: they run `human.approve_command` (`timesheet submit`) in a terminal or ask for `/submit`. Never run `submit` or `submit resolve` yourself.
   - `ask_human`: relay `ask.question`, `ask.options` and any `ask.last_error` verbatim, then wait. Write only the user's own answer to `ask.answer_file` as `ask.protocol` shows, with `"surface":"chat"`, and run step 2 again. Never answer it yourself.
   - `stopped`: print `reason` verbatim and stop. Never retry or work around it. The one exception: a `judgment_failed` reason that mentions "the agent path" (the judge failed, reached its daily token cap, or cannot answer a saved-day classification) means follow [fallback.md](fallback.md) once for that judgment, without rerunning the judge.
3. **`judgment`, or no usable judge** (`log` exits because the judge's key variable is not set): follow [fallback.md](fallback.md).
