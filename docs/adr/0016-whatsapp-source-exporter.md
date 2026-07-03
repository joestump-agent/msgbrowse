# ADR-0016: WhatsApp as a third source via WhatsApp-Chat-Exporter JSON

- **Status:** Accepted
- **Date:** 2026-07-03
- **Deciders:** Joe Stump
- **Related:** [ADR-0003 (dual-source archive)](0003-dual-source-archive.md), [ADR-0005 (iMessage txt parser)](0005-imessage-txt-parser.md), [ADR-0015 (onboarding: doctor/export/sync)](0015-onboarding-doctor-export-sync.md)

## Context and Problem Statement

msgbrowse ingests Signal (signal-export Markdown) and iMessage
(imessage-exporter txt) into one source-tagged store. The owner wants WhatsApp
history browsable alongside them (issue #86). WhatsApp offers no sanctioned
bulk exporter; the ecosystem options are (a) the app's built-in per-chat
"Export chat" text files, (b) community tools that parse the message database
out of a phone backup, or (c) reading live app data — which our read-only,
offline-archive posture (ADR-0010) rules out entirely.

## Decision Drivers

- Complete archives in one operation, not thousands of manual per-chat exports.
- Structured, stable input: the iMessage/Signal text formats cost us real bugs
  (locale timestamp strings, trailer parsing); typed fields beat regex.
- Reactions parity with the other two sources (reactions table since v6).
- Fits the existing exporter contract: a CLI msgbrowse can shell out to from
  `export`/`sync`, never auto-installed, archive treated read-only (ADR-0015).

## Considered Options

1. **WhatsApp-Chat-Exporter (KnugiHK) JSON output** — pip/pipx-installable CLI
   parsing iOS/iPadOS backups and Android crypt12/14/15 databases into
   structured JSON + copied media; reactions supported; incremental merge.
2. Native per-chat "Export chat" `.txt` — manual per conversation, no
   reactions, locale-dependent timestamp strings, media caps.
3. Bespoke backup parsing inside msgbrowse — reimplements (1) in Go, owning
   backup decryption (crypt15 keys, iOS backup formats) forever.

## Decision Outcome

**Option 1: consume WhatsApp-Chat-Exporter's JSON export** as the canonical
WhatsApp input, mirroring how sigexport and imessage-exporter are wrapped: a
configurable `whatsapp_archive_root` holding the tool's JSON + media output,
a new `internal/whatsapp` parser emitting the shared message shape with
timestamps canonicalized to `signal.TimestampLayout` at ingest, and
`export`/`sync`/`doctor` integration. Native `.txt` ingestion is a non-goal
for the first slice (revisit only if a real archive can't use the backup
route). Live database/backup parsing stays out of msgbrowse.

### Consequences

- Good: complete archive + reactions + typed timestamps in one pass; the
  parser is a JSON mapping, not a text grammar.
- Good: no schema change — source tagging, reactions, contact identity
  merging (phone-keyed) all exist.
- Bad: requires a phone backup on the Mac (iOS: local Finder backup; Android:
  backup + 64-digit key) — a heavier prerequisite than the other exporters;
  `doctor` must explain it well.
- Bad: upstream JSON schema is not a stable public contract; fixtures from a
  real export pin our expectations and CI catches drift.
- Neutral: voice notes (.opus) and stickers render as file chips initially,
  like other non-web media.
