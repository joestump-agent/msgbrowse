# sigbrowse

> Self-hosted, local-only browser, search engine, and MCP server over a
> [`signal-export`](https://github.com/carderne/signal-export) archive.
> Think *Backrest-for-Restic*, but for your Signal Desktop history.

sigbrowse renders a clean local UI over an existing on-disk Signal backup tree,
adds fast keyword + semantic search, and exposes an **MCP server** so Claude can
answer natural-language questions over your message history. It runs entirely on
your machine via Docker. **Nothing leaves the box** except calls to the one
OpenAI-compatible LLM endpoint you configure (default: a local LiteLLM proxy).

> **Status:** under active construction, built in vertical slices. See the
> project TODO and [`ARCHITECTURE.md`](ARCHITECTURE.md). This README is filled in
> as features land; the security model lives in [`SECURITY.md`](SECURITY.md).

## Features (target)

1. **Browse backups** — inventory the encrypted `.snapshots/*.tar` raw-DB backups
   with GFS retention tiers and total footprint (listed, never decrypted).
2. **Browse chat history by person** — sidebar of every conversation; transcript
   view with inline media.
3. **Search** — SQLite FTS5 keyword search with filters and live HTMX results.
4. **Semantic search + RAG over MCP** — a vector index plus an MCP server with
   citation-faithful retrieval tools.
5. **Browse by media type** — image gallery, file list, and deduplicated links.
6. **Journal view** — browse the archive like a date-navigable diary, with
   "on this day" and LLM digests.

## Engineering principles

- **Go only** for all application code (Go 1.23+), standard project layout.
- **HTMX** + server-rendered `html/template`; no SPA, no npm build step.
- **Cobra + Viper** for the CLI and configuration (file + env + flags).
- **Tests and docs are mandatory.** See `make check`.
- **Security by default** — loopback-only, read-only archive, least privilege.

## Quickstart

```sh
# Build and test locally
make build
make test

# Or run the whole stack (LiteLLM + sigbrowse) with Docker
make up
make ingest
# open http://127.0.0.1:8787
```

Full configuration reference, the MCP client config snippet, the Docker compose
setup, and the **"Setting up the backup pipeline in Claude Cowork"** prompt are
documented as those slices land.

## License

[MIT](LICENSE) © Joe Stump
