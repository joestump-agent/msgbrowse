# msgbrowse

> Self-hosted, local-only browser, search engine, and **AI-editorialized
> journal** over your personal message archives — **Signal and Apple iMessage** —
> built on the two upstream Markdown exporters
> [`signal-export`](https://github.com/carderne/signal-export) and
> [`imessage-exporter`](https://github.com/ReagentX/imessage-exporter).
> Think *Backrest-for-Restic*, but for your chat history.

msgbrowse renders a clean local UI over on-disk Markdown exports, adds fast
keyword + semantic search, and exposes an **MCP server** so Claude can answer
natural-language questions over your message history. The headline feature is the
**editorialized journal** — Daylio-style daily cards the LLM writes from your
chats and the media you received.

It runs entirely on your machine — install it with a single `go install` (the
SQLite driver is pure Go, so there's no C toolchain to set up) or run it in
Docker. **Nothing leaves the box** except calls to the one OpenAI-compatible LLM
endpoint you configure (default: a local LiteLLM → Ollama route). See
[`SECURITY.md`](SECURITY.md) for the exact egress and data-handling model.

> **Status:** active construction, built in vertical slices. Working today:
> Signal **and** iMessage import, browse, transcript, FTS search, media/links
> gallery, embeddings + semantic search, and the MCP server. In progress: the
> editorialized journal and the contacts page (cross-source identity merging).
> See [`ARCHITECTURE.md`](ARCHITECTURE.md).
>
> **iMessage support targets `imessage-exporter` 4.2.0 `-f txt` output** and was
> built against that format + a synthetic fixture; validate it against your first
> real export and open an issue if a line looks misparsed.

## Contents

- [Quickstart (`go install`)](#quickstart-go-install)
- [Alternative: Docker](#alternative-docker)
- [The data layout it reads](#the-data-layout-it-reads)
- [Commands](#commands)
- [Connecting Claude (MCP)](#connecting-claude-mcp)
- [Configuration reference](#configuration-reference)
- [Security](#security)
- [Setting up the backup pipeline in Claude Cowork](#setting-up-the-backup-pipeline-in-claude-cowork)
- [Development](#development)

## Quickstart (`go install`)

Requires **Go 1.25+** — and nothing else. The SQLite driver is pure Go
(FTS5 built in), so there is **no C toolchain and no build tag** to deal with.

```sh
go install github.com/joestump/msgbrowse/cmd/msgbrowse@latest
# msgbrowse lands in $(go env GOBIN) (or $(go env GOPATH)/bin) — put that on $PATH

msgbrowse --archive-root ~/"Managed Files/Signal-Archive" --data-dir ./data signal-import
msgbrowse --imessage-archive-root ~/"Managed Files/iMessage-Archive" --data-dir ./data imessage-import
msgbrowse --data-dir ./data embed          # optional; needs an LLM endpoint
msgbrowse --data-dir ./data serve          # auto-opens http://127.0.0.1:8787 (use --open=false for headless)
```

Both archive roots can also live in config/env, in which case the all-in-one
`msgbrowse --data-dir ./data import` imports every configured source in one go.
Import whichever sources you have — `signal-import`, `imessage-import`, `import`,
or both individually; they share one database and are incremental and idempotent,
so re-run after each new export.
`embed` computes vectors for semantic search and needs an LLM endpoint (see
below); browsing and keyword search work without one.

> **LLM endpoint.** msgbrowse only talks to **your own** OpenAI-compatible
> endpoint — set `MSGBROWSE_LLM_BASE_URL` (…/v1) and `MSGBROWSE_LLM_API_KEY`
> (env, `config.yaml`, or flags; see [Configuration](#configuration-reference)).
> Until one is reachable, `embed` and the journal fail, but everything else
> works. Don't have a proxy? The Docker path below can run a bundled local
> LiteLLM → Ollama for you.

### Use it from Claude (MCP over stdio)

`msgbrowse mcp` is an MCP server that speaks stdio, so any MCP client (Claude
Desktop, Claude Code, …) can launch the installed binary directly. Run
`msgbrowse embed` first so semantic search has vectors, then add:

```json
{
  "mcpServers": {
    "msgbrowse": {
      "command": "msgbrowse",
      "args": ["--data-dir", "/absolute/path/to/data", "mcp"]
    }
  }
}
```

Use the absolute path to the binary (e.g. `/home/you/go/bin/msgbrowse`) if it
isn't on the client's `PATH`. See [Connecting Claude (MCP)](#connecting-claude-mcp)
for the available tools and a Docker-based stdio variant.

## Alternative: Docker

Prefer containers? msgbrowse ships a Dockerfile and a compose stack — the image
is a fully static binary on a distroless base.

```sh
cp .env.example .env
# edit .env:
#   MSGBROWSE_ARCHIVE_HOST  → your archive's absolute path
#   MSGBROWSE_LLM_BASE_URL  → your LiteLLM proxy (…/v1), MSGBROWSE_LLM_API_KEY → its key

make up            # build + start msgbrowse (points at your external LiteLLM)
make signal-import # import the signal-export archive into the local DB
make embed         # compute embeddings for semantic search (optional)
# open http://127.0.0.1:8787
```

`make logs` tails the server; `make down` stops the stack. The archive is mounted
read-only, app data lives in a named volume, and the UI is published to host
loopback only.

> **LLM endpoint.** By default msgbrowse talks to **your own** OpenAI-compatible
> LiteLLM proxy via `MSGBROWSE_LLM_BASE_URL` / `MSGBROWSE_LLM_API_KEY` in `.env`.
> If LiteLLM runs on the same host as Docker, use
> `http://host.docker.internal:4000/v1`.
>
> Don't have one? Run the **bundled** LiteLLM (fully local via Ollama) with
> `make up-bundled` and set `MSGBROWSE_LLM_BASE_URL=http://litellm:4000/v1`; then
> uncomment the `ollama` service in `docker-compose.yml` and
> `docker compose exec ollama ollama pull nomic-embed-text` / `… pull llama3.1`.
>
> Until an endpoint is reachable, `make embed` and the journal fail; browsing and
> keyword search work without any LLM.

## The data layout it reads

msgbrowse treats the archive as **strictly read-only**. The signal-export layout:

```
Signal-Archive/
├── export/                      # the browsable, decrypted corpus
│   └── <ChatName>/
│       ├── chat.md              # the conversation, plaintext markdown
│       └── media/               # attachments for this conversation
├── journal/                     # day-by-day Markdown journal (msgbrowse owns this)
│   └── <YYYY>/<YYYY-MM-DD>.md
└── .snapshots/                  # timestamped RAW encrypted DB backups
    └── db-YYYYMMDD-HHMMSS.tar   # SQLCipher-encrypted; LISTED, never decrypted
```

`chat.md` messages start with a bracketed local timestamp, e.g.
`[2021-12-30 02:58:19] MJ: hey are we still on for tomorrow?`. `Me` is you;
`No-Sender` marks system/timeline events. Bodies may span multiple lines and
carry Markdown image/file/link syntax, which msgbrowse extracts into structured
attachment and link records. The `.snapshots/*.tar` files are encrypted
disaster-recovery backups — msgbrowse inventories them (size, GFS tier) but
never opens or decrypts them.

## Commands

| Command | What it does |
| --- | --- |
| `msgbrowse import` | **All-in-one**: import every configured archive (Signal + iMessage) into one DB. Unset sources are skipped. |
| `msgbrowse signal-import` | Import/refresh a signal-export archive (incremental, idempotent). `ingest` is a deprecated alias. |
| `msgbrowse imessage-import` | Import/refresh an imessage-exporter archive (`-f txt`, 4.2.0). Uses `imessage_archive_root`. |
| `msgbrowse doctor` | Read-only setup diagnostic: checks data dir/DB, archive roots, attachment health (catches non-copy-mode iMessage exports), converter, embeddings, exporters. `--check-llm` adds a TCP reachability probe. Exits non-zero only on a ✗. |
| `msgbrowse embed` | Compute embeddings for new messages (semantic search). `--prune` reclaims orphans. |
| `msgbrowse facts` | Extract AI facts about each contact (incremental, cited; shown on the conversation page). `--reset` rebuilds. |
| `msgbrowse media` | Transcode non-web images (HEIC/TIFF) to cached JPEGs so the gallery can show them. Incremental; `import` runs it automatically. |
| `msgbrowse serve` | Run the local HTMX web UI. `--port`/`--host` (or `--listen-addr`) to bind elsewhere; `--open=false` for headless. Default `127.0.0.1:8787`. |
| `msgbrowse mcp` | Run the MCP server (stdio by default; `--http` for streamable HTTP). |
| `msgbrowse journal` | Rebuild the journal + LLM digests *(Slice 6)*. |
| `msgbrowse watch` | Re-ingest when the archive changes *(planned)*. |
| `msgbrowse version` | Print version. |

## Connecting Claude (MCP)

msgbrowse exposes citation-faithful retrieval tools (`search_messages` [hybrid
keyword+vector], `semantic_search`, `get_conversation`, `list_conversations`,
`get_context`, `list_media`, `list_links`). Run `msgbrowse embed` first so
semantic search has vectors.

**Claude Desktop / Claude Code** — add to your MCP config (`claude_desktop_config.json`
or the Claude Code MCP settings):

Local binary (stdio) — the `go install` path. Use `msgbrowse` if it's on the
client's `PATH`, otherwise the absolute path (e.g. `/home/you/go/bin/msgbrowse`):

```json
{
  "mcpServers": {
    "msgbrowse": {
      "command": "msgbrowse",
      "args": ["--data-dir", "/absolute/path/to/data", "mcp"]
    }
  }
}
```

Via Docker (stdio; reuses the compose data volume):

```json
{
  "mcpServers": {
    "msgbrowse": {
      "command": "docker",
      "args": ["compose", "-f", "/absolute/path/to/msgbrowse/docker-compose.yml",
               "run", "--rm", "-T", "msgbrowse", "mcp"]
    }
  }
}
```

Then ask Claude things like *"what did MJ say about the lease?"* or *"summarize my
thread with Harper about the trip."* Every answer can be traced to source
messages (conversation, sender, timestamp, message id).

## Configuration reference

Resolved low→high: built-in defaults < `config.yaml` < `MSGBROWSE_*` env <
flags. See [`config.example.yaml`](config.example.yaml).

| Key | Env | Default | Notes |
| --- | --- | --- | --- |
| `archive_root` | `MSGBROWSE_ARCHIVE_ROOT` | — | read-only signal-export archive |
| `imessage_archive_root` | `MSGBROWSE_IMESSAGE_ARCHIVE_ROOT` | — | read-only imessage-exporter archive |
| `data_dir` | `MSGBROWSE_DATA_DIR` | `./data` | writable DB/embeddings dir |
| `listen_addr` | `MSGBROWSE_LISTEN_ADDR` | `127.0.0.1:8787` | loopback by default |
| `llm.base_url` | `MSGBROWSE_LLM_BASE_URL` | `http://127.0.0.1:4000/v1` | the only egress |
| `llm.api_key` | `MSGBROWSE_LLM_API_KEY` | — | env/secret only; never commit |
| `llm.chat_model` | `MSGBROWSE_LLM_CHAT_MODEL` | `local-chat` | RAG + digests |
| `llm.embed_model` | `MSGBROWSE_LLM_EMBED_MODEL` | `local-embed` | embeddings |
| `vector_backend` | `MSGBROWSE_VECTOR_BACKEND` | `sqlite-vec` | brute-force today (ADR-0002) |
| `journal.exclude_conversations` | — | `[]` | never sent to the LLM |
| `log_level` | `MSGBROWSE_LOG_LEVEL` | `info` | debug/info/warn/error |

## Security

Loopback-only by default, archive mounted read-only, container runs non-root
with a read-only root filesystem and all capabilities dropped, the encrypted
`.snapshots` are never opened, and the **only** outbound network call is to your
configured `llm.base_url`. Read [`SECURITY.md`](SECURITY.md) for the full threat
model and the data-sent-to-the-LLM boundary (including the heavier egress that
image captioning / audio transcription imply if you point LiteLLM at a hosted
model).

## Setting up the backup pipeline in Claude Cowork

msgbrowse reads an archive produced by an upstream exporter. To create that
archive on your Mac, paste the following prompt into Claude Cowork. **Signal:**

```
Set up a recurring daily job on my Mac that runs `signal-export` to dump my Signal Desktop
history into ~/Managed Files/Signal-Archive, building a searchable, ever-growing archive.

Do it as a careful, in-the-loop setup — propose a plan and wait for my approval before
installing anything or changing system state. Requirements:

1. Discovery first (read-only): confirm Signal Desktop is installed/linked, locate
   ~/Library/Application Support/Signal (sql/db.sqlite + config.json), and check that the
   config has an `encryptedKey` (v10 / Electron safeStorage, macOS Keychain-wrapped). Confirm
   Python 3.11+ is available (Homebrew). Flag the known breakage point: recent Signal Desktop
   encrypts the SQLCipher key via the macOS Keychain ("Signal Safe Storage"), so the export
   needs a one-time "Always Allow" on a Keychain prompt; after that, unattended runs are silent.

2. Install `signal-export` in an isolated venv (not system pip): a dedicated
   ~/Managed Files/Signal-Archive/.venv. Markdown output only (--no-html --no-json), keep
   attachments.

3. Write a wrapper script that, each run:
   - copies config.json + sql/db.sqlite{,-wal,-shm} to a private same-volume work dir (avoids
     the live DB lock / "I/O disk error" without quitting Signal), and SYMLINKS the large,
     immutable media dirs (attachments.noindex, avatars.noindex, stickers.noindex,
     badges.noindex) so attachments export without copying gigabytes;
   - persists a timestamped RAW DB snapshot as an uncompressed .tar under .snapshots/ — note
     the SQLCipher DB is encrypted and therefore incompressible, so compression is skipped and
     footprint is controlled by GFS retention instead;
   - runs `sigexport --source <copy> --old <archive> --no-html --no-json <staging>` so messages
     that roll past Signal's ~45-day linked-device window are merged in and never lost;
   - atomically swaps the new export into place;
   - prunes snapshots with GFS compaction: keep all dailies ≤14d, then 1 per month (≤~13mo),
     1 per quarter (≤~3y), 1 per year forever — the oldest snapshot in each period is its
     anchor (~37 snapshots / ~12 GB steady state at a ~350 MB DB).

4. Schedule it with a macOS launchd LaunchAgent (~/Library/LaunchAgents/) running DAILY at
   09:00 in my user session (so it can reach the Keychain), runs without Cowork open.

5. Do a one-time interactive test run so I can click "Always Allow" on the Keychain prompt,
   confirm the markdown export + a snapshot landed, then bootstrap the LaunchAgent and verify
   with `launchctl list`. Lock down perms (archive dir 700, snapshots 600); my disk is already
   FileVault-encrypted, which covers the plaintext export at rest.
```

**iMessage** (companion source; the importer lands in a later slice):

```
Set up a recurring daily job on my Mac that runs `imessage-exporter` to dump my iMessage
history into ~/Managed Files/iMessage-Archive in Markdown, building a searchable archive that
msgbrowse can import alongside my Signal export.

Do it as a careful, in-the-loop setup — propose a plan and wait for my approval before
installing anything or changing system state. Requirements:

1. Discovery first (read-only): confirm ~/Library/Messages/chat.db exists and is readable.
   Note that reading it requires the terminal/job to have Full Disk Access in System Settings →
   Privacy & Security; flag that as the one-time manual grant I must approve.

2. Install `imessage-exporter` via Homebrew (or cargo). Pin the version.

3. Write a wrapper script that, each run, exports to a staging dir with Markdown output
   (`imessage-exporter -f txt -c full -o <staging>`), keeps attachments/media, then atomically
   swaps the result into ~/Managed Files/iMessage-Archive. Do NOT modify chat.db.

4. Schedule it with a macOS launchd LaunchAgent running DAILY at 09:15 in my user session.

5. Do a one-time interactive test run so I can grant Full Disk Access, confirm the Markdown
   export + attachments landed, then bootstrap the LaunchAgent and verify with `launchctl list`.
   Lock down perms (archive dir 700); FileVault covers the plaintext export at rest.
```

## Development

```sh
make build      # build ./bin/msgbrowse (pure Go, no cgo, no build tag)
make install    # go install into $GOBIN / $GOPATH/bin
make test       # run the test suite
make check      # gofmt + go vet + tests (the CI gate)
make cover      # coverage summary
make css        # rebuild internal/web/static/app.css (Tailwind + daisyUI)
```

**UI styling.** The web UI uses Tailwind CSS + daisyUI (dim/winter themes, with a
header light/dark toggle) and vendored Hero Icons. The built `app.css` is
committed and `go:embed`-served, so the **runtime needs no toolchain** (no Node,
no CDN — keeps the strict CSP intact). When you change template classes, run
`make css` to regenerate it: that fetches the Tailwind **standalone CLI** + the
daisyUI package into a gitignored `.tools/` (no npm) and rebuilds. CI fails if
`app.css` is stale.

Architecture decisions live in [`docs/adr/`](docs/adr/). Contributions should
keep `make check` green and add tests for new ingest/search/MCP behavior.

## License

[MIT](LICENSE) © Joe Stump
