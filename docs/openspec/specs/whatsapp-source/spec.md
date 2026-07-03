# SPEC-0009: WhatsApp source

- **Status:** Accepted
- **Date:** 2026-07-03
- **Capability:** whatsapp-source
- **Source packages:** `internal/whatsapp` (new), `internal/source`, `internal/archivepath`, `internal/ingest`, `internal/cli` (`export.go`, `sync.go`, `import.go`, `doctor.go`), `internal/config`, `internal/web` (source styling), `docs-site`
- **Related ADRs:** [ADR-0016 (WhatsApp exporter choice)](../../../adr/0016-whatsapp-source-exporter.md), [ADR-0003 (dual-source archive)](../../../adr/0003-dual-source-archive.md), [ADR-0010 (security & privacy posture)](../../../adr/0010-security-privacy-posture.md), [ADR-0015 (doctor/export/sync)](../../../adr/0015-onboarding-doctor-export-sync.md)

## Overview

WhatsApp becomes the third message source, ingested from
[WhatsApp-Chat-Exporter](https://github.com/KnugiHK/WhatsApp-Chat-Exporter)'s
JSON + media output (ADR-0016) under a new read-only archive root. The unified
schema needs no change: conversations are source-tagged, reactions land in the
v6 reactions table, and phone-keyed contact identity merging unifies WhatsApp
threads with existing Signal/iMessage contacts. Timestamps are canonicalized
at ingest — the iMessage locale-string lesson (SPEC-0008 REQ-0008-002, PR #81)
is a design input here, not a bug to rediscover.

Both phone platforms are supported by the upstream tool (iOS/iPadOS backups;
Android crypt12/14/15); msgbrowse only sees the tool's output directory, so
the platform choice affects documentation and doctor hints, not parsing.

## Requirements

### REQ-0009-001: WhatsApp archive root

A `whatsapp_archive_root` config key (flag `--whatsapp-archive-root`, env
`MSGBROWSE_WHATSAPP_ARCHIVE_ROOT`) MUST point at the WhatsApp-Chat-Exporter
output directory. It MUST be treated strictly read-only, and an unset root
MUST skip the source exactly as the other two roots do.

#### Scenario: Source is optional
- **Given** a config with only `archive_root` and `imessage_archive_root`
- **When** `msgbrowse import` runs
- **Then** WhatsApp is skipped with an info log and no error.

### REQ-0009-002: Exporter integration

`msgbrowse export` (and `sync`) MUST run WhatsApp-Chat-Exporter into the
configured root when the root is set — with a bin override
(`whatsapp_exporter_bin` / `--whatsapp-exporter-bin`), repeatable
tool-specific extra args, and the shared trailing-`--` passthrough — and MUST
NOT auto-install it (install hint: `pipx install whatsapp-chat-exporter`).
Export MUST fail with a clear message when the tool is missing, consistent
with the sigexport/imessage-exporter wrappers.

#### Scenario: Wrapper parity
- **Given** `whatsapp_archive_root` set and the exporter on PATH
- **When** `msgbrowse export` runs
- **Then** the WhatsApp exporter is invoked with JSON output directed into the root, alongside the Signal and iMessage exports.

### REQ-0009-003: JSON-first parsing

A new `internal/whatsapp` parser MUST consume the exporter's JSON export and
emit the shared message shape (`signal.Message`) — sender, body, per-message
media references, links — one conversation per WhatsApp chat (groups
included). Unknown JSON fields MUST be ignored; malformed entries MUST be
skipped and surfaced through the ParseError/skip-logging contract rather than
aborting the conversation. Fixtures derived from a real (sanitized) export
MUST be committed, and the exact field mapping MUST be pinned in design.md
during implementation against those fixtures.

#### Scenario: Malformed entry tolerance
- **Given** a chat JSON containing one entry with a missing timestamp
- **When** the conversation is parsed
- **Then** the entry is skipped and logged, and every other message imports.

### REQ-0009-004: Canonical timestamps at ingest

Message `TimestampRaw` MUST be formatted as `signal.TimestampLayout`
(`YYYY-MM-DD HH:MM:SS`) derived from the export's epoch/typed timestamp field,
never from locale-formatted strings. No render-side fallback layer may be
required for WhatsApp rows.

#### Scenario: Gutter renders canonically on day one
- **Given** an imported WhatsApp conversation
- **When** its transcript renders
- **Then** time gutters, day separators, and header date ranges behave identically to Signal conversations with zero render-side format tolerance in play.

### REQ-0009-005: Reactions parity

Exporter-provided reactions MUST be imported into the `reactions` table keyed
by the stable message hash, aggregated and rendered as badges exactly like
Signal/iMessage reactions. Re-ingest MUST remain idempotent
(`ReplaceConversationMessages` clears and rebuilds them).

#### Scenario: Reaction badge
- **Given** a WhatsApp message with a 👍 reaction in the export
- **When** the transcript renders
- **Then** the message shows a 👍 badge with the reactor in the tooltip, and no reaction text appears in any message body.

### REQ-0009-006: Contained media resolution

WhatsApp attachments MUST resolve under the WhatsApp root via
`archivepath.Resolve` with the same traversal containment guarantees as the
other sources (a cleaned path escaping the root is rejected), and image
formats outside browser support MUST flow through the existing `imageconv`
transcode path. Voice notes and stickers MAY render as file chips initially.

#### Scenario: Traversal rejected
- **Given** a crafted attachment path containing `../`
- **When** `/media/{conv}/{path}` is requested
- **Then** the request is rejected exactly as for Signal/iMessage roots.

### REQ-0009-007: Source identity and contact merging

WhatsApp conversations MUST be tagged with a distinct source
(`source.WhatsApp`), MUST get their own presence-dot/source-pill styling
(CSP-safe, no inline styles), and phone-number identifiers MUST merge onto
existing contacts through the `contact_identifiers` machinery so a person's
Signal, iMessage, and WhatsApp threads share identity.

#### Scenario: Cross-source identity
- **Given** an existing contact with phone +1555… from iMessage
- **When** a WhatsApp chat with the same number imports
- **Then** the conversation's header identifier chips include the shared contact's other handles.

### REQ-0009-008: Incremental, idempotent ingest

WhatsApp ingest MUST follow SPEC-0001 semantics: re-running over an unchanged
export is a cheap no-op; a changed chat replaces that conversation's rows
without duplication; `import --full` re-scans everything.

#### Scenario: Unchanged export
- **Given** a previously imported WhatsApp root with no changes
- **When** `msgbrowse import` runs again
- **Then** the WhatsApp source reports 0 changed conversations.

### REQ-0009-009: Doctor coverage

`msgbrowse doctor` MUST validate the WhatsApp setup when the root is set:
root exists and contains the expected JSON export, a sample of media
references resolves inside the root, and the exporter binary is discoverable
for `export` users — with platform-aware remediation hints (iOS: local
Finder/iTunes backup required; Android: backup plus the 64-digit key).

#### Scenario: Missing media
- **Given** a JSON export whose media directory was not copied
- **When** doctor runs
- **Then** the WhatsApp media check fails with a hint to re-run the exporter with media enabled.

### REQ-0009-010: Documentation

The docs site MUST gain WhatsApp coverage: exporting-your-archives gains the
backup prerequisites + exporter invocation for both platforms, configuration
and CLI references gain the new keys/flags, and the features/what-is pages
mention the third source.

#### Scenario: A new user succeeds from docs alone
- **Given** the published docs site
- **When** a user follows Getting Started for WhatsApp
- **Then** every command and config key they need appears with real names, including the backup prerequisite for their platform.
