# Journal collection: which headings are read and which are skipped

> **Historical.** The journal source and the collector code this note cites were
> removed in the CLI cut-over (f2c80880); see the 2026-11-11 amendment to
> [ADR 0007](adr/0007-journal-evidence-source.md). The note below is kept
> unchanged as the record of what that collector did.

This note records what the journal evidence source actually does, grounded in the
collector code rather than in intention: `internal/evidence/plugins/journal.go`,
with the tests in `journal_test.go`. It is the repository's list of collected and
skipped headings. Nothing here authorizes a code change.

## Collected: the narrative heading phrases

A bullet is collected only when a heading in its ancestry contains one of three
narrative phrases, matched on the NORMALIZED heading text (lowercased, markdown
and emoji decoration stripped):

- `work journal`
- `quick capture`
- `daily impact`

The DEEPEST recognized heading supplies the section family, and the item keeps its
full heading ancestry, so two sections that merely share a phrase stay distinct
identities.

## Skipped: headings

Skip wins over recognition, and applies to ANY heading in the bullet's ancestry,
not only its immediate parent:

- any SECTION heading (level 2 or deeper) whose normalized text CONTAINS one of the
  skip tokens: `priorities`, `standup`, `todo`, `agent session`;
- the heading whose normalized text is exactly `clustered timesheet`, so case and
  decoration variants skip too;
- the exact generated timesheet heading shape that `internal/export` writes,
  `Timesheet <dash> <date>` (an em dash, en dash or hyphen before a
  `YYYY-MM-DD` date).

Three limits follow from the code and are worth stating because they look like
exceptions:

- **Skip phrases never apply to a level-1 note title.** A note titled with a skip
  word is not emptied; the rule is for section headings. For `agent session` this
  leaves one residual: a note whose H1 contains the token still collects a
  recognized section beneath it, so the split has to hold at section level.
- **The two timesheet skips match exact shapes**, not the word `timesheet`. An
  arbitrary heading that merely contains `timesheet` is never skipped, which is
  why an owner's own title cannot silently suppress the whole note.
- **Normalization preserves hyphens but turns underscores into spaces**, so
  `Agent_session log` matches the token and `Agent-session log` does not. That
  asymmetry is inherited from the shared normalization and applies to every
  token, `To-do` included; a note that hyphenates the heading will not be skipped.

The `agent session` token exists so an agent's activity cannot be read as the
owner's working time. Note where the exposure actually is: a TOP-LEVEL
`## Agent session log` already collected nothing before this token, because an
unrecognized heading yields no narrative family. The token matters when the
heading NESTS, for example `## Work journal` containing `### Agent session log`,
where the deepest recognized heading would otherwise be `Work journal` and an
agent's leg would be collected as work time. It also skips the whole subtree when
an `agent session` heading sits ABOVE recognized headings.

## Content-level rules, which are not heading rules

These apply to content rather than to headings, and are listed so they are not
mistaken for part of the heading list above:

- checkbox tasks, `- [ ]` and `- [x]`, are skipped even when checked;
- fenced code blocks are skipped;
- the exact `internal/export` managed markers delimit a region that is skipped,
  and an unterminated managed region fails the source closed so exported text can
  never feed back;
- an indented continuation line joins the bullet above it rather than starting a
  new item.

## Relationship to the vault recording standard

The vault's own recording standard, `06 - Knowledge & Wiki/Timesheet time
recording standard.md`, requires the collector to skip a heading containing
`agent session`, and says in its `Two kinds of bullet, and the split between them`
section that the collector did not yet cover the token. That standard lives in the
owner's vault and OUTSIDE this repository, so its wording cannot be verified from
here and its line numbers drift; at the time of writing the requirement sat around
line 92 of that note. With the token in the skip list the requirement and the
collector agree, and this note is the repository-side record of the agreement.

## Older lists in this repository

Three earlier documents described the skip list as it stood before the
`agent session` token: `docs/adr/0007-journal-evidence-source.md` (the
recognized-sections bullet), `docs/agents/journal-source.md` (the always-skipped
list) and `docs/timesheet-redesign.md` (the narrative-collection paragraph). All
three were brought into line with this note on 2026-10-21 (#92) — the ADR through
an amendment, the other two in place — so each now names the four tokens. Their
statements remain accurate about everything else, and this file stays the
authoritative current list.
