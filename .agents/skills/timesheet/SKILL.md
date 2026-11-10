---
name: timesheet
description: Log today's work into the personal timesheet from real git, pull-request and calendar evidence. Run when the user asks to "log today's work" or "log the timesheet", or a past working day.
---

# Pilot Timesheet

The binary runs the whole day and answers its own judgments. You start it, relay its questions and report its result.

1. **Setup.** `TS_BINARY="$("$SKILL_DIR/scripts/ts-env")"`, where `SKILL_DIR` is this skill's directory. It finds or rebuilds the binary; if it fails, report its error and stop. Never read the env file.
2. **Run** `"$TS_BINARY" next --do --auto`, adding `--date YYYY-MM-DD` for a past day and keeping that date on every later run. Act on its one `state`:
   - `human_gates`: report that the day is ready, with `usage` and `human.status` (unresolved writes, next safe action). Approval and submission are the user's: they run `human.approve_command` in a terminal or ask for `/submit`. Never run approve, submit or resolve-write.
   - `ask_human`: relay `ask.question`, `ask.options` and any `ask.last_error` verbatim, then wait. Write only the user's own answer to `ask.answer_file` as `ask.protocol` shows, with `"surface":"chat"`, and run step 2 again. Never answer it yourself.
   - `stopped`: print `reason` verbatim and stop. Never retry or work around it.
3. **No judge configured** (the run says `--auto needs a judge section`): follow [fallback.md](fallback.md) instead.
