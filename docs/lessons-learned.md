# Lessons learned

Durable lessons from this project. Each line is one lesson, one or two
sentences. Keep it short and hand-maintainable, and add a lesson only when it
has already been paid for at least once.

- Unknown is never zero. An unavailable measurement is recorded honestly as
  unknown and must never be read as 0.
- A blocked case must name a reason. A generic failure is not a refusal; every
  block carries a specific, stable reason.
- Evidence never automatically becomes confirmed working time. Gradion is
  authoritative for what is currently submitted.
- Time overlap is not an identity key. Two overlapping items are reconciled,
  never merged, and identity comes only from an explicit mapping.
- Do not implementation another session's commit. An amendment to shared history is
  drift even when the intent is to correct it.
- A stale sentence in an ADR is drift. Supersede it in place with a dated note
  rather than leaving it to describe a rule the code no longer implements.
- A check cannot catch a wrong generator, because the same generator produces
  both sides. Golden fixtures are required, not optional.
- A hand-maintained manifest and a hand-maintained ledger can disagree. Keep the
  machine-readable status in one place, and let the ledger remain the record.
