---
sidebar_position: 1
title: Configuration
---

# Configuration

msgbrowse is local-only by design: every setting below controls software that runs on your machine, and the only network egress any of them enable is the single OpenAI-compatible endpoint at `llm.base_url` (a local proxy by default). Configuration resolves from four layers, lowest to highest precedence:

1. Built-in defaults
2. A YAML config file
3. `MSGBROWSE_*` environment variables
4. Command-line flags

Only flags you actually set override file and environment values.

## Config file search order

Pass an explicit file with the global `--config` flag, or let msgbrowse search for a file named `config.yaml` in these directories, in order:

1. `.` (the current directory)
2. `~/.config/msgbrowse/`
3. `/etc/msgbrowse/`

```bash
msgbrowse --config /path/to/config.yaml serve
```

A missing config file is fine — defaults, environment variables, and flags still apply. Start from [`config.example.yaml`](https://github.com/joestump/msgbrowse/blob/main/config.example.yaml) in the repo root.

## Environment variables

Every key maps to an environment variable with the `MSGBROWSE_` prefix; dots in nested keys become underscores:

| Key | Environment variable |
| --- | --- |
| `data_dir` | `MSGBROWSE_DATA_DIR` |
| `whatsapp_archive_root` | `MSGBROWSE_WHATSAPP_ARCHIVE_ROOT` |
| `llm.base_url` | `MSGBROWSE_LLM_BASE_URL` |
| `llm.api_key` | `MSGBROWSE_LLM_API_KEY` |
| `journal.max_days_per_run` | `MSGBROWSE_JOURNAL_MAX_DAYS_PER_RUN` |

:::tip
Never put `llm.api_key` in a committed file. Leave it empty in `config.yaml` and set `MSGBROWSE_LLM_API_KEY` in the environment instead. The default local Ollama/LiteLLM route needs no key at all.
:::

List-valued keys (such as `journal.exclude_conversations`) are easiest to set in the config file rather than the environment.

## Data dir vs. archive roots

These path settings have very different contracts:

- **`data_dir` is the only place msgbrowse writes.** The SQLite database (`msgbrowse.sqlite`), embeddings, and transcoded-image caches live here. It must be outside the archive.
- **`archive_root`, `imessage_archive_root`, and `whatsapp_archive_root` are strictly read-only.** msgbrowse only ever opens files inside them for reading; imports never modify them, and in Docker they are mounted `:ro`. The encrypted `.snapshots/*.tar` backups inside the Signal archive are listed by name and size but never opened or decrypted.

## Key reference

### Top-level keys

| Key | Type | Default | Purpose |
| --- | --- | --- | --- |
| `archive_root` | string | `""` | Path to the signal-export archive — the folder that **contains** `export/`, not `export/` itself. Read-only. Leave empty if you only import iMessage. |
| `imessage_archive_root` | string | `""` | Path to the imessage-exporter output (a flat directory of `<ChatName>.txt` files plus attachments). Read-only. Leave empty if you only import Signal. |
| `whatsapp_archive_root` | string | `""` | Path to the whatsapp-chat-exporter output (`result.json` plus any media directories the tool copied). Read-only. Leave empty to skip the WhatsApp source. |
| `data_dir` | string | `./data` | Writable directory for the SQLite database, vector index, and caches. Must not be empty; must be outside the archive. |
| `signal_export_bin` | string | `""` | Path to the Signal exporter binary used by `msgbrowse export`. Empty means look up `sigexport` on `PATH`. Set it to pin a specific binary (e.g. one inside a pipx venv). |
| `imessage_exporter_bin` | string | `""` | Path to the `imessage-exporter` binary used by `msgbrowse export`. Empty means look it up on `PATH`. |
| `whatsapp_exporter_bin` | string | `""` | Path to the `wtsexporter` binary used by `msgbrowse export` (the pipx package is `whatsapp-chat-exporter`). Empty means look it up on `PATH`. |
| `listen_addr` | string | `127.0.0.1:8787` | Web UI bind address. Loopback by default; the UI has no authentication, so binding to a non-loopback interface is an explicit, deliberate choice. |
| `vector_backend` | string | `sqlite-vec` | Vector store for semantic search: `sqlite-vec` or `qdrant`. Any other value fails validation. |
| `ingest_on_start` | bool | `false` | Run an import pass automatically when `serve` boots. |
| `watch` | bool | `false` | Enable the fsnotify archive watcher inside `serve` (equivalent to running `msgbrowse watch` alongside the server). |
| `log_level` | string | `info` | One of `debug`, `info`, `warn`, `error`. Any other value fails validation. |

:::warning
`listen_addr` defaults to loopback for a reason: the web UI is unauthenticated. If you bind to `0.0.0.0` or another non-loopback address, put your own access control (VPN, reverse proxy with auth) in front of it.
:::

### `llm.*` — the single egress

The `llm` block configures the one OpenAI-compatible endpoint msgbrowse ever talks to. It is used for embeddings, contact-facts extraction, and journal digests. The default points at a local LiteLLM proxy, so out of the box nothing leaves the machine.

| Key | Type | Default | Purpose |
| --- | --- | --- | --- |
| `llm.base_url` | string | `http://127.0.0.1:4000/v1` | Base URL of the OpenAI-compatible API. **The only network egress msgbrowse performs.** |
| `llm.api_key` | string | `""` | API key for the endpoint. Prefer `MSGBROWSE_LLM_API_KEY`; a local route usually needs none. |
| `llm.chat_model` | string | `local-chat` | Model used for RAG synthesis, contact facts, and journal digests. The default is a LiteLLM route alias meant to resolve to a local model. |
| `llm.embed_model` | string | `local-embed` | Model used for message embeddings (semantic search). |
| `llm.max_concurrency` | int | `4` | Maximum concurrent LLM requests. |
| `llm.timeout` | duration | `60s` | Per-request timeout, as a Go duration string (`30s`, `2m`, ...). |

### `journal.*`

| Key | Type | Default | Purpose |
| --- | --- | --- | --- |
| `journal.digest_enabled` | bool | `true` | Turn the LLM digest pass on or off. The mechanical (non-LLM) journal is always written regardless. |
| `journal.digest_prompt` | string | built-in prompt | The instruction prompt for the digest pass. Changing it bumps the effective prompt version and invalidates the digest cache. |
| `journal.exclude_conversations` | string list | `[]` | Privacy denylist: conversation folder names whose content is **never** sent to the LLM, for any feature (digests, facts). |
| `journal.max_days_per_run` | int | `0` | Cap on how many days a single digest run processes. `0` means unbounded. |

### `device_sync.*`

Multi-device archive sync ([Device sync](../features/device-sync.md)). Strictly opt-in: with `enabled: false` (the default) no sync engine process runs and msgbrowse keeps its loopback-only posture. When enabled, msgbrowse supervises a bundled (desktop app) or bring-your-own (`serve`) Syncthing as the transfer engine, configured LAN-only — global discovery and relaying off.

| Key | Type | Default | Purpose |
| --- | --- | --- | --- |
| `device_sync.enabled` | bool | `false` | Turn device sync on. Off means no engine process and no P2P listener. |
| `device_sync.listen_addr` | string | `:8788` | The engine's P2P sync listener bind (host:port). Must use a port distinct from `listen_addr`; it is the one socket beyond loopback, protected by device-ID mutual TLS. |
| `device_sync.device_name` | string | `""` | This device's friendly name shown on paired peers. Empty derives the hostname. |
| `device_sync.syncthing_bin` | string | `""` | Path to the Syncthing binary for `msgbrowse serve` (bring-your-own). Empty looks up `syncthing` on `PATH`. The desktop app ignores this and always runs its bundled, version-pinned copy. |

## Flag overrides

Persistent flags, available on every subcommand, bind directly onto config keys:

| Flag | Overrides |
| --- | --- |
| `--config` | which config file is loaded |
| `--archive-root` | `archive_root` |
| `--imessage-archive-root` | `imessage_archive_root` |
| `--whatsapp-archive-root` | `whatsapp_archive_root` |
| `--data-dir` | `data_dir` |
| `--log-level` | `log_level` |

Some subcommands add their own overrides — for example `serve --listen-addr` (or `--host`/`--port`) overrides `listen_addr`, and `export --signal-export-bin`/`--imessage-exporter-bin` override `signal_export_bin`/`imessage_exporter_bin`. See the [CLI reference](./cli.md) for the full per-command flag list.

```bash
# Flags win over env, which wins over the file, which wins over defaults.
MSGBROWSE_DATA_DIR=/srv/msgbrowse/data \
  msgbrowse --log-level debug serve --port 8888 --open=false
```
