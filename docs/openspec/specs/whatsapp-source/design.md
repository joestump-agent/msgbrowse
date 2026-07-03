# SPEC-0009 Design: WhatsApp source

- **Capability:** whatsapp-source
- **Related ADRs:** [ADR-0016](../../../adr/0016-whatsapp-source-exporter.md), [ADR-0003](../../../adr/0003-dual-source-archive.md), [ADR-0010](../../../adr/0010-security-privacy-posture.md), [ADR-0015](../../../adr/0015-onboarding-doctor-export-sync.md)

## Architecture

WhatsApp is the third tenant of the multi-source machinery — every box below
except `internal/whatsapp` already exists and gains only a `case`:

```
msgbrowse export ──▶ whatsapp-chat-exporter (pipx tool, ADR-0016)
                        │  reads iOS backup / Android crypt DB (outside msgbrowse)
                        ▼
        <whatsapp_archive_root>/          ← read-only to msgbrowse
          result.json (or per-chat JSON)  ← exact layout pinned vs fixtures
          <media dirs copied by the tool>
                        │
msgbrowse import ──▶ internal/whatsapp.Run(store, Options)   (mirrors imessage.Run)
                        │  JSON → signal.Message stream:
                        │    TimestampRaw = epoch → signal.TimestampLayout (REQ-0009-004)
                        │    Reactions    → []signal.Reaction (REQ-0009-005)
                        │    media refs   → Attachments (root-relative RelPaths)
                        ▼
                unified store (source='whatsapp'; NO schema change)
                        │
web/media: archivepath.Resolve(source, roots…) gains the whatsapp branch
web/UI:   sourceSlug → 'src-whatsapp' (presence dot + pill; input.css tokens)
contacts: phone-keyed contact_identifiers merge — free (existing machinery)
```

## Key decisions

- **JSON over text** (ADR-0016): the parser is a field mapping, not a grammar.
  The upstream schema is unversioned, so the contract is defended by
  **committed fixtures from a real sanitized export**; the concrete field
  table lives here (below) and is filled in by the foundation story when the
  first fixture lands. The parser ignores unknown fields and skip-logs
  malformed entries (ParseError parity).
- **Timestamps**: the export carries epoch timestamps; `TimestampRaw` is
  `time.Unix(...).Format(signal.TimestampLayout)` from day one. The render
  fallback added for legacy iMessage rows (#81) must never be needed for
  WhatsApp rows — a test asserts canonical output on fixtures.
- **Hashing/identity**: message hash inputs (conversation, ts_raw, sender,
  body, seq) are unchanged; WhatsApp rows are new so there is no re-key
  concern. Conversation naming follows the export's chat naming (phone number
  or group subject); phone-named chats merge onto contacts exactly like
  iMessage numbers, and the `initials()`/`humanName()` phone handling from
  #81 applies unchanged.
- **Media**: the tool copies media beneath its output dir; RelPaths are stored
  root-relative and served through the existing `/media/{conv}/{path}` route,
  so traversal containment and HEIC/TIFF transcoding come for free. Voice
  notes (`.opus`) and stickers (`.webp` animated) render as file chips/images
  respectively in slice one — no transcription, no special casing.
- **Platform**: iOS vs Android changes only the *backup prerequisite* the user
  performs before running the exporter. Doctor prints both remediation paths;
  docs describe both; parsing is identical. (Owner's platform is an open
  question on the epic — it decides which doc path gets written first, not
  the architecture.)

## Field mapping (pinned during implementation)

| store field | exporter JSON source | notes |
|---|---|---|
| conversation name | chat key / group subject | groups keep subject; 1:1 keep number |
| sender | per-message sender name/number | owner detection → `Me` mapping TBD vs fixture |
| ts_unix / TimestampRaw | epoch field | canonical format at ingest |
| body | text field | media-only messages: empty body + attachment |
| Attachments.RelPath | media path relative to root | must survive archivepath.Contain |
| Reactions | reactions array (emoji, actor) | aggregate per emoji at render (existing) |

*This table is intentionally shape-level until fixtures exist; the foundation
story fills exact key names and commits them alongside the fixtures.*

## Non-goals (this spec)

- Native per-chat "Export chat" `.txt` parsing (manual, reaction-less,
  locale-string timestamps — ADR-0016 option 2). Revisit only if a real
  archive cannot use the backup route.
- Reading live WhatsApp databases or decrypting backups inside msgbrowse.
- Voice-note transcription (existing `llm.transcribe` machinery could adopt
  it later, separate spec).

## Testing & verification

- Parser: fixture-driven golden tests (messages, groups, reactions, media
  refs, malformed-entry skip, canonical timestamps); property: re-parse
  idempotence.
- Ingest: incremental no-op on unchanged root; changed-chat replacement
  without duplication; reactions rebuilt on re-ingest.
- Media: traversal rejection under the new root; renderability checks for
  webp/jpeg; transcode path exercised for HEIC-in-WhatsApp (rare but real).
- Doctor: table-driven checks for missing root / missing JSON / missing media
  / missing exporter, each with its remediation string.
- Web: source pill + presence dot for `src-whatsapp` (CSS assertions per the
  #84 pattern), identifier chips show merged handles.
