---
sidebar_position: 2
title: CLI reference
---

# CLI reference

Every msgbrowse command runs locally against your read-only archives and the writable `data_dir`. No command performs network egress except `embed`, `facts`, `mcp` (semantic search), and `doctor --check-llm` — and all of those talk only to the single configured `llm.base_url` (a local endpoint by default).

## Global flags

These persistent flags work on every subcommand and override the corresponding config keys (see [Configuration](./configuration.md)):

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--config` | string | search order | Config file (default: `./config.yaml`, `~/.config/msgbrowse/config.yaml`, or `/etc/msgbrowse/config.yaml`). |
| `--archive-root` | string | from config | Path to the signal-export archive (read-only). |
| `--imessage-archive-root` | string | from config | Path to the imessage-exporter archive (read-only). |
| `--whatsapp-archive-root` | string | from config | Path to the whatsapp-chat-exporter output (read-only). |
| `--data-dir` | string | from config | Writable directory for the database and caches. |
| `--log-level` | string | from config | Log level: `debug`, `info`, `warn`, `error`. |

---

## `msgbrowse export`

Runs the upstream exporters msgbrowse reads from, so a fresh install can populate its archives in one step: `sigexport` into `<archive_root>/export/` (per-conversation `chat.md` + media), `imessage-exporter -f txt -c clone -o <imessage_archive_root>` for iMessage, and `wtsexporter` with JSON output into `<whatsapp_archive_root>` for WhatsApp (which additionally needs a database to read — see [the platform prerequisites](../getting-started/exporting-your-archives.md#whatsapp-platform-prerequisites), passed via `--whatsapp-exporter-args`). A source whose archive root is unset is skipped. msgbrowse never auto-installs the tools, stores no secrets, and reads no Keychain — the invoked exporters do, with your consent, and their output streams to your terminal.

```bash
msgbrowse export
msgbrowse export --skip-on-error
msgbrowse export --imessage-exporter-args="--start-date" --imessage-exporter-args="2024-01-01"
msgbrowse export -- --some-shared-flag   # trailing args go to BOTH tools
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--signal-export-bin` | string | `sigexport` on `PATH` | Path to the Signal exporter (or set the `signal_export_bin` config key). |
| `--imessage-exporter-bin` | string | `imessage-exporter` on `PATH` | Path to imessage-exporter (or set the `imessage_exporter_bin` config key). |
| `--whatsapp-exporter-bin` | string | `wtsexporter` on `PATH` | Path to the WhatsApp exporter (or set the `whatsapp_exporter_bin` config key). |
| `--signal-export-args` | string array | none | Extra arg for `sigexport` only; repeatable. |
| `--imessage-exporter-args` | string array | none | Extra arg for `imessage-exporter` only; repeatable. |
| `--whatsapp-exporter-args` | string array | none | Extra arg for `wtsexporter` only; repeatable (use it to pass the database/backup flags). |
| `--skip-on-error` | bool | `false` | Log and skip a failing or missing source instead of aborting; the run still exits non-zero if any configured source failed. |

:::warning
iMessage export **always** runs in copy mode (`-c clone`) so attachments are copied into the archive. Without copy mode, the export records absolute `~/Library/...` paths that msgbrowse cannot render — the exact trap `msgbrowse doctor` diagnoses. The exporter also needs Full Disk Access to read `~/Library/Messages/chat.db`.
:::

:::tip
The exporters' console commands differ from their package names: the Signal exporter's command is `sigexport` (pip package `signal-export`, `pipx install signal-export`) and the WhatsApp exporter's command is `wtsexporter` (pip package `whatsapp-chat-exporter`, `pipx install whatsapp-chat-exporter`). Install imessage-exporter with `brew install imessage-exporter`.
:::

## `msgbrowse import`

The all-in-one importer: runs the Signal, iMessage, and WhatsApp importers for whichever archive roots are configured (`archive_root`, `imessage_archive_root`, and/or `whatsapp_archive_root`) into one database, then transcodes non-web images (HEIC/TIFF) as a best-effort final step. A source whose root is unset is skipped; a source whose root is set but missing is an error. It does **not** embed — run `msgbrowse embed` separately, since that step needs the LLM endpoint.

```bash
msgbrowse import
msgbrowse import --full
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--full` | bool | `false` | Ignore incremental state and re-scan every conversation. Required once after upgrades that change how messages are parsed (e.g. reaction handling). |

## `msgbrowse signal-import`

Imports (or refreshes) a signal-export archive: scans the read-only tree, parses each changed conversation's `chat.md` into the unified SQLite store, and refreshes the encrypted-snapshot inventory. Incremental and idempotent — unchanged conversations are skipped, so re-running is cheap. Every row is tagged `source="signal"`.

```bash
msgbrowse signal-import
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--full` | bool | `false` | Ignore incremental state and re-scan every conversation. |

`msgbrowse ingest` still works as a hidden, deprecated alias for `signal-import`.

## `msgbrowse imessage-import`

Imports (or refreshes) an imessage-exporter archive — the flat directory of `<ChatName>.txt` files produced by `imessage-exporter -f txt` (targets the 4.2.0 txt format). Incremental and idempotent; every row is tagged `source="imessage"`. The path comes from `imessage_archive_root` / `MSGBROWSE_IMESSAGE_ARCHIVE_ROOT` / `--imessage-archive-root`.

```bash
msgbrowse imessage-import
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--full` | bool | `false` | Ignore incremental state and re-scan every conversation. |

## `msgbrowse sync`

The one-command refresh: chains every step that turns a fresh install into a populated, browsable archive, reusing the other commands end to end — **export → import → media → embed → facts**. The database is opened once and shared by every stage; a source whose archive root is unset is skipped, so single-source (or any subset) setups just work.

Error policy: export/import/media failures abort the run unless `--skip-on-error`. The `embed` and `facts` stages need the LLM endpoint and **always** warn-and-continue on failure, so a fully local run with no reachable LLM still completes export/import/media and exits successfully.

```bash
msgbrowse sync
msgbrowse sync --no-export          # re-import existing archives only
msgbrowse sync --no-embed --no-facts
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--no-export` | bool | `false` | Skip the export stage (don't re-run the upstream exporters). |
| `--no-media` | bool | `false` | Skip the media transcode stage. |
| `--no-embed` | bool | `false` | Skip the embed stage. |
| `--no-facts` | bool | `false` | Skip the facts stage. |
| `--skip-on-error` | bool | `false` | Log and continue past a failing export/import/media stage instead of aborting (run still exits non-zero). |
| `--signal-export-bin` | string | `sigexport` on `PATH` | Path to the Signal exporter (as in `export`). |
| `--imessage-exporter-bin` | string | on `PATH` | Path to imessage-exporter (as in `export`). |
| `--whatsapp-exporter-bin` | string | `wtsexporter` on `PATH` | Path to the WhatsApp exporter (as in `export`). |
| `--signal-export-args` | string array | none | `sigexport`-only extra arg, repeatable. |
| `--imessage-exporter-args` | string array | none | `imessage-exporter`-only extra arg, repeatable. |
| `--whatsapp-exporter-args` | string array | none | `wtsexporter`-only extra arg, repeatable. |

Trailing `-- <args>` are passed through to **every** upstream exporter.

## `msgbrowse serve`

Runs the server-rendered HTMX web UI. Binds to loopback by default and opens your browser once the listener is up.

```bash
msgbrowse serve
msgbrowse serve --port 8888 --open=false
msgbrowse serve --listen-addr 127.0.0.1:9000
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--listen-addr` | string | `listen_addr` config (`127.0.0.1:8787`) | Full `host:port` listen address; overrides `--host`/`--port` and config. |
| `--host` | string | configured host | Bind host (e.g. `127.0.0.1` or `0.0.0.0`); default keeps the configured host. |
| `--port` | int | configured port | Bind port (1–65535); default keeps the configured port. |
| `--open` | bool | `true` | Open the UI in your default browser on start; `--open=false` for headless runs. |

:::warning
The UI has no authentication. Only expose it on a non-loopback address behind your own access control.
:::

## `msgbrowse doctor`

Read-only setup diagnostic. Runs checks over your configuration, data directory, archives, and imported attachment rows, printing one line per check (`✓` pass, `⚠` warn, `✗` problem) and a summary. Exits non-zero only if a check fails, so it is safe in scripts. Its headline check catches iMessage exports done without copy mode. See [Troubleshooting](./troubleshooting.md) for every check and its remediation.

doctor makes no off-machine network calls except an optional TCP-connect reachability probe (no data sent) of the configured `llm.base_url`, behind `--check-llm`. With [device sync](../features/device-sync.md) enabled it also reads the local sync engine's loopback REST API (engine running?, peers connected?, folder completion) — that traffic never leaves the machine.

```bash
msgbrowse doctor
msgbrowse doctor --check-llm
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--check-llm` | bool | `false` | Additionally TCP-probe the configured `llm.base_url` for reachability (the single configured egress; no data is sent). |

## `msgbrowse devices`

Manages [device sync](../features/device-sync.md) peers (Syncthing-based archive sync between your own machines). Pairing itself happens in the web UI's Settings page via device-ID QR codes; the CLI covers the registry and the operational verbs:

```bash
msgbrowse devices list                # paired peers, shared folders, roles
msgbrowse devices status              # engine state, peer connections, folder completion
msgbrowse devices unpair XW4UY46      # sever a peer (full device ID or unique prefix)
```

`unpair` removes the device from this node's registry and — when the sync engine is running — removes its device entry and folder shares live, so sync to it stops immediately. Archives already synced to this machine stay on disk; only future synchronization stops. `status` and `unpair` reach the supervised engine over its loopback REST API only; nothing leaves the machine.

## `msgbrowse facts`

Extracts AI facts about each contact from their messages using the configured chat model, storing atomic, cited facts (e.g. "Has a dog named Biscuit") that appear on the conversation page. Incremental: a per-conversation cursor means re-runs only analyze new messages, and facts are deduplicated per contact. Conversations on `journal.exclude_conversations` are never sent to the LLM. This command performs network egress to `llm.base_url`.

```bash
msgbrowse facts
msgbrowse facts --conversation 42
msgbrowse facts --reset
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--reset` | bool | `false` | Wipe all stored facts and cursors before running. |
| `--batch-size` | int | `60` | Messages per extraction call. |
| `--concurrency` | int | `4` | Conversations processed in parallel. |
| `--conversation` | int64 | `0` | Limit extraction to a single conversation id. |

## `msgbrowse embed`

Sends message text to the configured embedding model and stores the vectors for semantic search. Incremental: only messages without an embedding for the current model are processed. This command performs network egress to `llm.base_url` — point it at a local endpoint (the default) to keep message content on the machine.

```bash
msgbrowse embed
msgbrowse embed --prune
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--prune` | bool | `false` | Remove embeddings whose message no longer exists before embedding. |

## `msgbrowse media`

Converts image attachments browsers can't render (Apple HEIC/HEIF, TIFF) into JPEG derivatives cached under `<data_dir>/derived`, so the gallery and transcript display them. Incremental, and uses whatever converter is on `PATH` (`sips`, `magick`, `convert`, `heif-convert`); with none installed it is a no-op and the UI shows placeholders. `import` runs this step automatically.

```bash
msgbrowse media
msgbrowse media --force
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--force` | bool | `false` | Re-convert even if a derivative already exists. |
| `--concurrency` | int | `6` | Number of images to convert in parallel. |

## `msgbrowse mcp`

Runs the Model Context Protocol server so an MCP client (Claude Desktop / Claude Code) can query your archive with citation-faithful retrieval tools. Serves over stdio by default; logs go to stderr so they never corrupt the JSON-RPC stream. Semantic search embeds the query via `llm.base_url` — run `msgbrowse embed` first so message embeddings exist.

```bash
msgbrowse mcp
msgbrowse mcp --http --listen-addr 127.0.0.1:8788
```

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--http` | bool | `false` | Serve over streamable HTTP instead of stdio. |
| `--listen-addr` | string | `127.0.0.1:8788` | HTTP listen address when `--http` is set. |

## `msgbrowse journal`

Rebuilds the day-by-day journal and optional LLM digests. **Not implemented yet** — the command, flags, and config surface are in place; the behavior lands in an upcoming slice.

| Flag | Type | Default | Description |
| --- | --- | --- | --- |
| `--since` | string | none | Only process days on or after this date (`YYYY-MM-DD`). |
| `--backfill` | bool | `false` | Process all days that lack a current digest. |
| `--regenerate` | bool | `false` | Regenerate digests even if cached. |
| `--dry-run` | bool | `false` | Print day count and cost estimate; make no LLM calls. |

## `msgbrowse watch`

Re-ingests automatically when the archive changes (fsnotify). **Not implemented yet.** The `watch: true` config key wires the same behavior into `serve` once it lands. No flags.

## `msgbrowse version`

Prints version information: version, commit, build date, and Go runtime.

```bash
msgbrowse version
```

No flags.
