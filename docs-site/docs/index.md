---
title: Documentation
sidebar_label: Overview
sidebar_position: 0
---

# msgbrowse Documentation

msgbrowse is a self-hosted, local-only browser, search engine, and (upcoming)
AI-editorialized journal over your personal Signal, iMessage, and WhatsApp
archives. It is a single pure-Go binary (`go install`, no C toolchain) that
renders a clean local UI over the output of three upstream exporters
([`signal-export`](https://github.com/carderne/signal-export),
[`imessage-exporter`](https://github.com/ReagentX/imessage-exporter), and
[`whatsapp-chat-exporter`](https://github.com/KnugiHK/WhatsApp-Chat-Exporter)),
adds FTS5 keyword search and semantic search, and exposes an MCP server so AI
assistants can answer questions over your history. Nothing leaves your machine
except calls to the one OpenAI-compatible LLM endpoint you configure.

## Where to start

- **[Getting Started](getting-started/what-is-msgbrowse.md)** — what msgbrowse
  is, [installation](getting-started/installation.md),
  [exporting your archives](getting-started/exporting-your-archives.md), and
  your [first import](getting-started/first-import.md).
- **Features** — [browsing](features/browsing.md),
  [search](features/search.md), the [media gallery](features/media-gallery.md),
  [status & backups](features/status-and-backups.md),
  [AI features](features/ai-features.md), and the
  [MCP server](features/mcp-server.md).
- **Reference** — the [CLI](reference/cli.md),
  [configuration](reference/configuration.md), the
  [security model](reference/security-model.md), and
  [troubleshooting](reference/troubleshooting.md).
- **Development** — [local development](development/local-development.md)
  (toolchain, make targets, the no-Node CSS pipeline, desktop-shell builds)
  and the [macOS signing & notarization](development/release-signing.md)
  owner runbook.
- **[Architecture](/architecture)** — the generated ADR and specification
  section (also reachable from the **ADRs** and **Specifications** tabs above).
