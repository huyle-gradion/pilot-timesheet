# Agent-answered judgments (no judge configured)

Use this only when `log` reports a `judgment`: the configuration has no `judge` section, so the answer is yours. Write only `judgment.answer_file`, copy its digest verbatim, then run `"$TS_BINARY" log` again with the same `--date`. A `last_error` means your last answer for this same content was rejected; fix exactly what it names. `judgment_failed` is a stop.

**`block_descriptions`**: judge only `judgment.draft.blocks`. Write `{"draft_digest":"<judgment.draft_digest>","notes":"<string>","descriptions":{"<block_id>":"<prose>"}}`, with one grouped-topic entry per block and no times, block fields or `PRs:` list (the binary appends it). `outside_work_hours` is informational only.

**`classifications`**: use only `judgment.blocks[]` and `judgment.candidates[]`. For each interval pick at most one option from the set its block's `candidates` names, or omit it. Write `{"ledger_digest":"<judgment.ledger_digest>","selections":{"<interval_id>":{"catalog_id":"<set catalog_id>","classification_id":"<option id>"}}}`; `{"selections":{}}` is the honest no-op. A saved-day recovery judgment carries no candidates: query `"$TS_BINARY" classifications --query "<topic>"` and copy the receipt's ids. Never invent an id.
