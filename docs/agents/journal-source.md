# Journal evidence source (retired)

**Historical.** The optional, read-only `journal` source that read the owner's
Obsidian daily note was removed in the CLI cut-over (f2c80880), together with
`internal/evidence` and its `timesheet.journal.example.json` template. The
current configuration accepts only `sources.github` and `sources.calendar`; see
[collection configuration](collection-config.md).

The decision that introduced the source stays on record as
[ADR 0007](../adr/0007-journal-evidence-source.md), whose 2026-11-11 amendment
records the removal. The heading rules it applied are kept as history in
[the journal collection note](../journal-collection.md). The full text of this
note before retirement is in the git history of this path.

This file keeps its path so existing links still resolve.
