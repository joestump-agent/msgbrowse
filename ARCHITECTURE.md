# Architecture

msgbrowse is a Go application that imports on-disk message-archive exports into
one SQLite database and serves four faces over it: an HTMX web UI, an MCP
server, a journal generator, and a native desktop shell
(`cmd/msgbrowse-desktop` — a Wails window plus menubar tray over the exact same
embedded web server, ADR-0017). It is local-first and read-only with respect to
the archive.

## Layering

```
cmd/msgbrowse            thin main(): delegates to internal/cli
cmd/msgbrowse-desktop    Wails v2 desktop shell + systray, embedding the same web server (ADR-0017)
└── internal/cli         Cobra commands + Viper config wiring
    ├── internal/config  config model (defaults < file < MSGBROWSE_* env < flags)
    ├── internal/signal  signal-export chat.md parser → []signal.Message (shared model)
    ├── internal/imessage imessage-exporter txt parser + flat-layout importer
    ├── internal/whatsapp WhatsApp-Chat-Exporter result.json parser + importer (ADR-0016)
    ├── internal/source  canonical source names (signal, imessage, whatsapp)
    ├── internal/ingest  scan signal archive, incremental idempotent import, snapshots
    ├── internal/store   SQLite: schema/migrations, relational + FTS5 + vectors
    ├── internal/llm     OpenAI-compatible client (the only internet egress)
    ├── internal/embed   batch embedding orchestration
    ├── internal/facts   incremental, cited contact-fact extraction (LLM)
    ├── internal/imageconv  transcode HEIC/TIFF → cached JPEG (external converter, ADR-0014)
    ├── internal/archivepath shared, traversal-safe attachment path resolution
    ├── internal/setup   source detection + permission probes for guided setup
    ├── internal/onboard(+svc) exporter execution: one-click Enable/Refresh runs (ADR-0020)
    ├── internal/devices device identity: Syncthing device-ID parsing + pairing payloads
    ├── internal/devsync device pairing flow + sync-completion → re-ingest worker
    ├── internal/syncthing supervised Syncthing sync engine: config gen + REST (ADR-0021)
    ├── internal/mcp     Model Context Protocol server (tools over the store)
    └── internal/web     net/http + html/template + HTMX UI
```

Dependencies point inward toward `store` and `signal`; `mcp` and `web` are sibling
presentation layers that share the same `store` methods, so keyword/semantic/media
behavior cannot drift between the model-facing and human-facing surfaces. The
desktop shell embeds the identical server (same pages, same handlers), so browser
mode and the `.app` cannot drift either; `internal/onboardsvc` and
`internal/devsync` are pure-Go seams importable by both `msgbrowse serve` and the
desktop's embedded server.

## Data model (SQLite, one file in `data_dir`)

- `conversations(id, source, name, contact_id, is_group)` — `UNIQUE(source, name)`.
- `messages(id, hash, conversation_id, source, ts, ts_unix, sender, body, is_system, seq)`
  — `hash` is the stable content key for idempotent re-import; `id` is the FTS/cursor rowid.
- `attachments`, `links` — cascade-delete with their message.
- `reactions(conversation_id, message_hash, source, emoji, actor)` — emoji
  reaction badges, keyed to the message by stable hash
  (`UNIQUE(message_hash, emoji, actor)`), cascade-delete with the conversation.
- `contacts`, `contact_identifiers(contact_id, source, identifier)` — the unified
  identity layer; one canonical person spans Signal + iMessage handles
  (reconciled manually; see ADR-0003).
- `embeddings(message_hash, model, dim, vec)` — `PRIMARY KEY (message_hash, model)`,
  no FK (keyed by stable hash so re-import doesn't wipe vectors; multiple models
  coexist).
- `contact_facts(contact_id, fact, category, fact_hash, source, source_message_hash, …)`
  — AI-extracted, cited facts deduped per contact (`UNIQUE(contact_id, fact_hash)`),
  no FK to messages (provenance by stable hash; see ADR-0011). `fact_state` is the
  per-conversation incremental cursor (last message hash + model).
- `snapshots`, `ingest_state`, `ingest_runs` — backup inventory + incremental
  bookkeeping + per-run summaries.
- `paired_devices`, `sync_state` — device sync (ADR-0021): the explicitly
  paired Syncthing peers (device-ID fingerprint, per-source importer/replica
  roles) and per-folder re-ingest bookkeeping (folder↔source mapping + last
  re-ingest time).
- `messages_fts` — FTS5 external-content table kept in sync by triggers.

Schema changes go through the versioned migration runner (`internal/store/migrations.go`),
which applies each version in its own transaction and records `PRAGMA user_version`.

## Import pipeline

`signal-import` walks `export/<conv>/chat.md`, skipping unchanged files via
`(mtime, size)` then content hash recorded in `ingest_state` (`--full` forces a
rescan). Each changed conversation is parsed (streaming, malformed lines logged
and skipped — never fatal) and its messages atomically replaced in one
transaction. Re-import is idempotent; messages are keyed by a content hash with a
sequence disambiguator for byte-identical lines. The `.snapshots/*.tar`
inventory is refreshed by filename/size only.

## Search

- **Keyword**: SQLite FTS5 (`bm25` ranking), filterable by conversation, source,
  sender, date, has-attachment/has-link. Snippets are highlighted safely (the
  store emits control-char markers; the web layer escapes then swaps them for
  `<mark>`).
- **Semantic**: per-message embeddings via the LLM, searched with a brute-force
  cosine scan over the filtered candidate set (ADR-0002). The MCP
  `search_messages` tool fuses keyword + semantic with reciprocal-rank fusion and
  degrades to keyword-only when no LLM/embeddings are available.

## LLM access

One `llm.Client` interface (`Embed`, `Chat`, `Transcribe`, `Vision`) backed by an
OpenAI-compatible HTTP client, pointed by default at a local LiteLLM proxy. This
package is the sole *internet* egress. `Transcribe`/`Vision` exist for the
media-first journal (Slice 6).

Two other network/process paths exist beyond it, both local: opt-in device
sync (`device_sync.enabled`, off by default) has `internal/syncthing` supervise
a Syncthing process whose msgbrowse-generated config is LAN-only — global
discovery, relaying, and NAT traversal all disabled, local (LAN) announce on —
connecting only to explicitly paired devices (ADR-0021); and
`msgbrowse export` / the Providers Enable flow spawn the upstream exporter
subprocesses, which do their own local I/O against the source message stores
(ADR-0020).

## Web UI

`net/http` with Go 1.22 pattern routing, `html/template` (auto-escaping all
untrusted message content), HTMX for partials (live search, infinite scroll). No
SPA, no Node; vendored htmx pinned by SHA.

Styling is **Tailwind CSS + daisyUI** (drawer/navbar layout, `chat` bubbles for
transcripts, `card`/`menu`/`tabs`/`stat` components) with a bespoke **slate**
(dark, default) / **slate-light** custom-theme toggle (ADR-0012;
`internal/web/static/theme.js`, self-hosted, persists to `localStorage`). Icons are vendored **Hero Icons** inline SVG. The stylesheet
is built by the Tailwind **standalone CLI + daisyUI** at dev time (`make css`,
no Node) and the resulting `app.css` is committed and `go:embed`-served, so the
runtime and Docker image need no toolchain and stay CDN-free.

A strict `Content-Security-Policy` (`default-src 'none'`, `script-src 'self'`,
`style-src 'self'`, `img-src 'self' data:`) plus `nosniff`, `no-referrer`, and
frame denial harden every response; media is served with correct
`Content-Type`/`Content-Disposition` and path-traversal containment. Everything
the UI loads — CSS, htmx, the theme script, icons — is same-origin.

## Key decisions (ADRs)

- [ADR-0001](docs/adr/0001-sqlite-driver-mattn-cgo.md) — SQLite driver: originally mattn + cgo + `sqlite_fts5` (superseded by ADR-0013).
- [ADR-0013](docs/adr/0013-pure-go-sqlite-driver.md) — SQLite driver: pure-Go `modernc.org/sqlite` (FTS5 built in; toolchain-free `go install`).
- [ADR-0002](docs/adr/0002-vector-backend.md) — vector backend: brute-force default, sqlite-vec optional.
- [ADR-0003](docs/adr/0003-dual-source-archive.md) — dual-source unified schema + manual contact reconciliation.
- [ADR-0004](docs/adr/0004-mcp-sdk-and-rag.md) — official MCP SDK + citation-faithful hybrid RAG.
- [ADR-0014](docs/adr/0014-image-transcoding-external-converter.md) — HEIC/TIFF transcoding via an external converter, cached JPEGs.
- [ADR-0016](docs/adr/0016-whatsapp-source-exporter.md) — WhatsApp as a third source via WhatsApp-Chat-Exporter JSON.
- [ADR-0017](docs/adr/0017-desktop-shell-wails.md) — desktop shell: Wails v2 window over the embedded web server.
- [ADR-0020](docs/adr/0020-bundled-exporters-guided-setup.md) — bundled exporter toolchain in the `.app` + guided setup.
- [ADR-0021](docs/adr/0021-syncthing-sync-engine.md) — device sync: supervised Syncthing engine (supersedes ADR-0018).

The full set (ADR-0001–0021) lives in [`docs/adr/`](docs/adr/).

## Containerization

Multi-stage `Dockerfile`: static (`CGO_ENABLED=0`) build on `golang:1.25-bookworm`,
runtime on `distroless/static-debian12` (`nonroot`, no libc, no shell) — fully
static since the SQLite driver is pure Go (ADR-0013). `docker-compose.yml` wires msgbrowse to a
LiteLLM proxy (optional Ollama behind it), bind-mounts the archive read-only,
keeps app data in a named volume, publishes the UI to host loopback only, and
hardens the app container (read-only rootfs, dropped capabilities, no privilege
escalation).
