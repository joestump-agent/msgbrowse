# Security

msgbrowse handles sensitive personal data (your entire message history). It is
designed local-first, least-privilege, and read-only with respect to the
archive. This document describes the threat model and the mitigations.

## Threat model

- **Adversary:** other software on the same machine, a malicious archive (crafted
  message content), accidental data exfiltration to a hosted LLM, and network
  attackers if the UI is ever exposed beyond loopback.
- **Assets:** the plaintext message archive, the derived SQLite database +
  embeddings, and the encrypted `.snapshots` backups.
- **Out of scope:** the internal security of the upstream exporter codebases
  (msgbrowse now *executes* them — that boundary is covered under
  [Exporter execution](#exporter-execution) — but does not audit their code),
  the macOS Keychain, and disk-at-rest encryption (FileVault is assumed for the
  plaintext export).

## What stays on the machine

Everything, except calls to the single configured `llm.base_url` — and, if you
opt in, LAN-only [device-sync](#device-sync-opt-in) traffic to devices you
explicitly pair. There is **no telemetry, no analytics, and no other outbound
connection.** The default `llm.base_url` is a local LiteLLM proxy routing to a
local model (Ollama), so out of the box message content never leaves the
device — and device-sync traffic, when enabled, never leaves your LAN.

## The data-sent-to-the-LLM boundary

This is the one place data leaves the box, and only if you point LiteLLM at a
hosted provider. What is sent, and when:

| Feature | What is sent to `llm.base_url` |
| --- | --- |
| `embed` | Message text (per message), to compute embeddings. |
| MCP `semantic_search` / hybrid `search_messages` | Your **query** text (to embed it). |
| Journal digests *(Slice 6)* | A day's message text, to write the digest. |
| Journal image captions *(Slice 6, opt-in)* | **Image bytes** of received photos. |
| Journal audio transcripts *(Slice 6, opt-in)* | **Audio bytes** of voice messages. |

Image and audio bytes are a **much heavier and more sensitive egress** than text.
If — and only if — you route LiteLLM to a hosted model, enabling vision/audio
sends raw media off-device. The default local route keeps it on the machine.

**Privacy controls:**
- `journal.exclude_conversations` is a denylist of conversations whose content is
  **never** sent to any LLM, for any feature.
- Keep the default local LiteLLM route. Routing to a hosted provider must be a
  deliberate edit to `litellm.config.yaml` and is documented as off-device.
- The API key is read from `MSGBROWSE_LLM_API_KEY` (env/secret) only and is never
  baked into the image or expected in a committed file.

## Archive integrity

- The archive is mounted **read-only** in Docker (`:ro`) and msgbrowse only ever
  opens files for reading. Imports write exclusively to `data_dir`, which must be
  outside the archive.
- The encrypted `.snapshots/*.tar` (SQLCipher raw-DB backups) are **inventoried by
  filename and size only** — msgbrowse never opens, decrypts, or reads their
  contents, and never touches the macOS Keychain.

## Exporter execution

msgbrowse originally only *read* archives you produced yourself; it now also
**runs the upstream exporters as subprocesses**: `msgbrowse export`,
`msgbrowse sync`, and the one-click Enable/Refresh flow on the Providers page
all spawn `sigexport`, `imessage-exporter`, and `wtsexporter` (ADR-0020). That
is a materially wider permission surface than reading an on-disk export, so
the boundary is explicit:

- **What runs, and from where.** In browser/CLI mode the binaries are resolved
  from your `$PATH` or your explicit `--signal-export-bin` /
  `--imessage-exporter-bin` / `--whatsapp-exporter-bin` (config `*_bin`)
  overrides — you choose exactly what executes. The macOS `.app` resolves
  *only* its bundled, version-pinned, ad-hoc-signed copies under
  `Contents/Resources/tools` and never consults `$PATH`.
- **What they read.** The exporters read the sensitive upstream stores
  directly: Signal Desktop's SQLCipher database (whose key `sigexport` unwraps
  via the macOS Keychain "Signal Safe Storage" entry) and
  `~/Library/Messages/chat.db` (which requires Full Disk Access granted to the
  hosting process). msgbrowse itself still never touches the Keychain or
  `chat.db` — only the exporter subprocesses it spawns do, with permissions
  *you* grant.
- **What stays out of scope:** the internal security of the exporter codebases
  themselves. msgbrowse pins and bundles known versions but does not audit
  them; treat granting them Keychain/Full Disk Access with the same care as
  any other tool you give those permissions.

## Web UI hardening

- Binds to **loopback by default**. A non-loopback bind logs a warning; the UI has
  no authentication, so only expose it behind your own access control.
- Strict `Content-Security-Policy: default-src 'none'` (plus `script-src 'self'`,
  `img-src 'self' data:`), `X-Content-Type-Options: nosniff`,
  `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`, `frame-ancestors 'none'`.
- Everything the UI loads is **same-origin and self-hosted** — the stylesheet
  (Tailwind + daisyUI, built at dev time and committed), htmx, the theme-toggle
  script, and Hero Icons (inline SVG). No CDN, no external fonts/scripts, so the
  strict CSP holds and nothing about your browsing is fetched off-device.
- All message content is untrusted and **HTML-escaped** via `html/template`;
  attachment markdown is stripped, URLs are linkified with
  `rel="noopener noreferrer nofollow"`, and search snippets are escaped before
  highlight markers are applied.
- Media is served with correct `Content-Type` and `Content-Disposition`; SVGs are
  forced to download (never inlined); **path traversal is contained** (cleaned,
  leading-slash-anchored, and verified to stay within the conversation directory).
- The MCP server is read-only and its stdio transport keeps logs on stderr so they
  never corrupt the JSON-RPC stream.

## Device sync (opt-in)

`device_sync.enabled` is **false by default** — with it off, none of the
following exists and the loopback-only posture above is the whole story. When
enabled (ADR-0021 / SPEC-0014), msgbrowse supervises a Syncthing process as the
archive-sync transfer engine:

- **A second listener, beyond loopback.** Syncthing binds
  `device_sync.listen_addr` (default `:8788`, all interfaces) so paired devices
  on your LAN can reach it. It carries only Syncthing's mutually authenticated
  sync protocol — the web UI itself stays loopback-only; the sync listener
  never serves the UI.
- **LAN-only by construction.** msgbrowse generates the Syncthing configuration
  itself: global discovery **off**, relaying **off**, NAT traversal **off**,
  local (LAN) announce **on**. No connection is ever made to Syncthing's public
  discovery or relay infrastructure, and sync traffic does not cross the
  internet.
- **Pairing is explicit and pinned.** Devices pair by exchanging Syncthing
  device IDs (QR code or manual code on the Settings page). A device ID is
  public — it is the hash of the device's TLS certificate, not a secret — and
  connections are mutual TLS pinned to the paired IDs; **both ends must accept**
  before any data moves. `msgbrowse devices unpair` revokes a peer.
- **What replicates, and to whom.** The importer role shares its managed
  archives — plaintext message exports and media — to its paired replicas,
  which import the copy into their own local database. A paired replica
  therefore holds your full archive: pair only devices you trust exactly as
  much as the source machine, and note FileVault/disk encryption is *its*
  responsibility too.

## Container hardening

The app container runs as a **non-root** user with a **read-only root
filesystem** (`/tmp` is tmpfs, `/data` is the only writable volume), **all Linux
capabilities dropped**, and **no-new-privileges**. The web port is published to
host **loopback only**; LiteLLM is not published to the host at all. The
in-container `0.0.0.0` bind is safe because the container network is isolated and
the host mapping is loopback-only — this is the standard Docker pattern, not a
public exposure.

## Reporting

This is a personal project. If you find a vulnerability, open an issue (without
sensitive data) at <https://github.com/joestump/msgbrowse/issues>.
