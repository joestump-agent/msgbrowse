# ADR-0010: Security & privacy posture

- **Status:** Accepted
- **Date:** 2026-06-27
- **Relates to:** [ADR-0006](0006-web-stack-htmx.md) (CSP), [ADR-0007](0007-frontend-styling-tailwind-daisyui.md) (no CDN), [ADR-0009](0009-config-cli-cobra-viper.md) (env-only secrets)

## Context

msgbrowse holds the most sensitive data a person owns — their entire message
history — and adds an LLM feature set (embeddings, RAG, journal digests) that
*could* exfiltrate it. The adversaries are other software on the same machine, a
**malicious archive** (crafted message content), accidental data leakage to a
hosted LLM, and network attackers if the UI is ever exposed beyond loopback. This
ADR records the posture; SECURITY.md is the living document.

## Decision

**Local-first, least-privilege, read-only with respect to the archive, with
exactly one configurable network egress and no telemetry.**

1. **Loopback-only by default, warn otherwise.** `listen_addr` defaults to
   `127.0.0.1:8787`. `Run` (`internal/web/server.go`) logs a warning on a
   non-loopback bind ("the UI has no authentication") via `isLoopback`. The UI has
   no auth, so any wider exposure is the operator's deliberate, flagged choice.
2. **Strict CSP, self-only assets.** `securityHeaders` sets
   `Content-Security-Policy: default-src 'none'` with `script-src 'self'`,
   `style-src 'self'`, `img-src 'self' data:`, `connect-src 'self'`,
   `font-src 'self'`, `base-uri 'none'`, `form-action 'self'`,
   `frame-ancestors 'none'`, plus `X-Content-Type-Options: nosniff`,
   `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`. Every asset is
   same-origin and self-hosted — vendored htmx ([ADR-0006](0006-web-stack-htmx.md)),
   the built stylesheet and inline Hero Icons ([ADR-0007](0007-frontend-styling-tailwind-daisyui.md)),
   `theme.js` — so the CSP holds with no CDN.
3. **Untrusted content is always escaped.** Message bodies (a crafted-archive
   attack surface) flow through `html/template` auto-escaping; `renderBody`
   re-escapes text runs and linkifies URLs with `rel="noopener noreferrer
   nofollow"`; `highlightSnippet` escapes before applying `<mark>` and strips
   stray control-character sentinels.
4. **User-supplied archives are read-only and never written.** User-pointed
   archive roots mount `:ro` in Docker and msgbrowse only opens their files for
   reading; all writes go to `data_dir`, which must be outside any user-supplied
   archive. Since [ADR-0020](0020-bundled-exporters-guided-setup.md) the desktop
   app additionally owns **managed archive roots inside `data_dir`**
   (`<data_dir>/archives/<source>`, `internal/setup/layout.go`); those are
   written only by the app-spawned bundled exporters and — on replica nodes —
   by the bundled Syncthing daemon ([ADR-0021](0021-syncthing-sync-engine.md)),
   never by msgbrowse's own importer, which still only reads them. Media serving
   is path-traversal-contained:
   `containWithin` (`internal/web/media.go`) anchors and cleans the relative path
   and verifies it stays inside the per-source base. SVG attachments are forced to
   download, never inlined (they can carry script).
5. **Encrypted `.snapshots` are listed, never opened.** The SQLCipher
   `.snapshots/*.tar` backups are inventoried by **filename and size only**;
   msgbrowse never opens, decrypts, or reads them, and never touches the macOS
   Keychain.
6. **One egress, local by default.** The single configurable `llm.base_url`
   (`internal/llm`) is the **only** outbound connection off the local network;
   it defaults to a local LiteLLM proxy (`http://127.0.0.1:4000/v1`) routing to
   a local model, so out of the box no message content leaves the device. There
   is **no telemetry or analytics**. The LLM API key is supplied via env
   (`MSGBROWSE_LLM_API_KEY`, [ADR-0009](0009-config-cli-cobra-viper.md)) or, for
   a desktop user, stored in the mode-`0600` config file through the Settings →
   LLM tab (ADR-0009 *Amendment*); an env-provided key is never written to disk,
   and the key is never baked into the image or committed. Device sync
   ([ADR-0018](0018-device-pairing-archive-sync.md), superseded by
   [ADR-0021](0021-syncthing-sync-engine.md)) later added a second, **opt-in**
   network surface: a bundled Syncthing daemon with a non-loopback sync
   listener (`device_sync.listen_addr`, default `:8788`; `device_sync.enabled`
   defaults to `false`) that does LAN-local discovery and archive transfers
   only — global discovery, relays, and NAT traversal are all disabled
   (`internal/syncthing/configgen.go`), so enabling it adds no cloud egress.
7. **Per-thread privacy denylist.** `journal.exclude_conversations` is a denylist
   of conversations whose content is **never** sent to any LLM, for any feature —
   the right control regardless of source ([ADR-0003](0003-dual-source-archive.md)).
   Note that vision/audio journal features send raw media bytes, a heavier egress
   that only applies if LiteLLM is pointed at a hosted provider.
8. **Hardened container.** The app container runs **non-root**, with a
   **read-only root filesystem** (`/tmp` tmpfs, `/data` the only writable volume),
   **all Linux capabilities dropped**, and `no-new-privileges`. The web port is
   published to host **loopback only**; LiteLLM is not published to the host. The
   in-container `0.0.0.0` bind is safe because the host mapping is loopback-only.
9. **MCP is read-only on stderr-only logs.** The MCP server exposes read-only
   tools and keeps logs on stderr so they never corrupt the stdio JSON-RPC stream
   ([ADR-0008](0008-structured-logging-charmbracelet.md)).

## Why these choices

- **Loopback default over auth:** a single-user local tool shouldn't ship a login
  system; binding to loopback removes the network attacker entirely, and a loud
  warning makes any wider bind a conscious decision.
- **`default-src 'none'` + self-only assets:** the archive is hostile input, so
  the UI is built ([ADR-0006](0006-web-stack-htmx.md)/[ADR-0007](0007-frontend-styling-tailwind-daisyui.md))
  to need nothing off-origin — letting the CSP be maximally strict instead of
  carving CDN exceptions.
- **Read-only archive + contained media paths:** the archive is the irreplaceable
  asset; never writing it and lexically containing every served path closes both
  corruption and traversal.
- **Snapshots listed, not opened:** their value is knowing a backup exists;
  decrypting them would pull plaintext and the Keychain into scope for no feature
  gain.
- **One egress, local default, no telemetry:** makes the data-leaves-the-box
  boundary a single, auditable line of config, off by default.

## Consequences

### Positive

- Out of the box, nothing leaves the machine; the one egress is explicit,
  single, and local-by-default. The only other network surface — the Syncthing
  sync listener ([ADR-0021](0021-syncthing-sync-engine.md)) — is off by
  default, LAN-only, and never touches a cloud relay.
- A malicious archive cannot execute script in the UI (escaping + strict CSP) or
  escape its directory (path containment), nor force a script-capable SVG inline.
- Container blast radius is minimized (non-root, read-only rootfs, cap-drop,
  loopback-only publish).

### Negative

- No built-in authentication — exposing the UI beyond loopback is entirely the
  operator's responsibility (the warning is the only guardrail).
- The strict no-CDN CSP forces vendoring/building every asset
  ([ADR-0007](0007-frontend-styling-tailwind-daisyui.md)'s dev-time toolchain).
- Routing LiteLLM to a hosted provider (and enabling vision/audio) silently
  widens egress to raw media bytes; the denylist and the local default are the
  mitigations, but the operator can override them.

### Operational

- Keep the default local LiteLLM route; routing to a hosted model must be a
  deliberate `litellm.config.yaml` edit and is documented as off-device.
- Supply the LLM key via `MSGBROWSE_LLM_API_KEY` (recommended for server/Docker)
  or the Settings → LLM tab (desktop, `0600` config file; an env key always wins
  and is never persisted). Never commit it.
- `data_dir` must live outside any user-supplied (read-only) archive root
  (app-managed roots live *inside* `data_dir` by design,
  [ADR-0020](0020-bundled-exporters-guided-setup.md)).

## Alternatives considered

- **Add UI authentication / TLS.** Deferred: out of scope for a loopback,
  single-user tool; loopback-by-default plus operator-owned access control is the
  pragmatic posture.
- **Index/search inside the encrypted snapshots.** Rejected: would require
  decryption and Keychain access, dragging plaintext backups into the threat model
  for no browsing benefit.
- **Send full archives to a hosted LLM by default for richer features.** Rejected:
  inverts the privacy default; the local route + per-thread denylist keep the user
  in control of every byte that leaves.

## References

- `internal/web/server.go` (`securityHeaders` CSP, loopback default + warning)
- `internal/web/media.go` (`containWithin`, SVG-not-inlined), `render.go` (escaping)
- `internal/config/config.go` (`llm.base_url`, env-only key, `journal.exclude_conversations`)
- `internal/llm/` (single egress client), `internal/ingest/snapshots.go` (list-only)
- `SECURITY.md` (threat model, container hardening, egress table)
