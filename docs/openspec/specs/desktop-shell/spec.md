---
status: draft
date: 2026-07-03
implements: [ADR-0017]
requires: [SPEC-0004]
---

# SPEC-0010: Desktop shell

## Overview

msgbrowse becomes a first-class desktop application: a Wails v2 shell
([ADR-0017](../../../adr/0017-desktop-shell-wails.md)) that opens the system
webview onto the existing web UI ([SPEC-0004](../web-ui/spec.md)), served by
the same `internal/web` server embedded in the desktop process and bound to a
loopback ephemeral port. Browser mode (`msgbrowse serve`) is unchanged and
remains the path for servers and Docker.

The shell adds no second frontend. Its one new page — Connect/Settings —
belongs to the web app itself, served identically in browser and desktop
modes: MCP connection details plus a server-rendered QR code whose payload is
defined by [SPEC-0011 (device sync)](../device-sync/spec.md). This spec owns
rendering that QR, not its contents.

The core build stays pure Go: the desktop target is the only cgo code in the
repository, isolated behind its own build tags so `CGO_ENABLED=0` builds of
the server, CLI, and MCP core keep succeeding
([ADR-0013](../../../adr/0013-pure-go-sqlite-driver.md)).

## Requirements

### Requirement: Isolated cgo build target

The desktop shell MUST live in its own build target (`cmd/msgbrowse-desktop`)
gated by build tags. Packages outside that target MUST NOT import Wails or any
cgo-requiring dependency, and the core build and test paths (`make check`,
release builds of `cmd/msgbrowse`) MUST continue to run with `CGO_ENABLED=0`.
The desktop target MUST NOT be a prerequisite of any core Makefile target.

#### Scenario: Core stays pure-Go

- **WHEN** `CGO_ENABLED=0 go build ./...` runs at the repository root
- **THEN** the build succeeds, with the tag-gated desktop command excluded by its build constraints and every other package compiling without cgo.

### Requirement: Embedded server on a loopback ephemeral port

The desktop app MUST start the existing `internal/web` server in-process,
bound to `127.0.0.1` on an ephemeral port (port 0, discovered from the
listener), and MUST point its webview window at the resulting URL. The
embedded server MUST serve the same handlers, templates, middleware, and
security headers as `msgbrowse serve` — zero handler divergence between
desktop and browser modes.

#### Scenario: The window shows the real app

- **WHEN** the desktop app launches against a populated `data_dir`
- **THEN** the window renders the same conversation list a browser sees at `msgbrowse serve`'s address, produced by the same handlers, and the server is listening only on a loopback ephemeral port.

#### Scenario: No port collision with a running serve

- **WHEN** the desktop app launches while `msgbrowse serve` already holds `127.0.0.1:8787`
- **THEN** the desktop app binds its own ephemeral port and both keep working.

### Requirement: Native shell affordances

The desktop app MUST present a native application menu with standard quit
semantics (Cmd+Q on macOS, and the platform's conventional equivalents), MUST
appear in the platform's application switcher (macOS Dock presence with the
app icon), and MUST set a meaningful window title. Open-at-login registration
MAY be provided.

#### Scenario: Quit from the menu

- **WHEN** the user invokes Quit from the application menu (or Cmd+Q on macOS)
- **THEN** the window closes and the process exits after the embedded server shuts down cleanly.

### Requirement: Menubar residency

The desktop app MUST live in the platform menubar/tray: a status item with the
app icon MUST be present whenever the app runs, and closing the main window
MUST hide it rather than quit — the embedded server keeps running and the app
stays one click away. Quitting MUST be explicit (the tray menu's Quit, the
application menu, or Cmd+Q). The app SHOULD support launching hidden
(menubar-only, no window) as its default mode.

#### Scenario: Close-to-tray

- **WHEN** the user closes the main window
- **THEN** the app stays resident in the menubar with the embedded server running, and choosing "View Messages" from the menubar restores the window.

#### Scenario: Explicit quit from the tray

- **WHEN** the user chooses Quit from the menubar menu
- **THEN** the embedded server shuts down cleanly and the process exits.

### Requirement: Menubar quick menu

The menubar status item MUST open a menu containing, at minimum: **View
Messages** (opens or focuses the main window on the transcript UI);
**Transfer / Pair Device…** (opens the main window deep-linked to the settings
pairing section showing the SPEC-0011 QR code); an **MCP status line** showing
the MCP endpoint and live server state (running + port at minimum; a degraded
state when the embedded server is unhealthy) whose activation copies the MCP
endpoint URL to the system clipboard; **Copy MCP config** (copies the full
JSON client-configuration block); and **Quit**. Clipboard actions MUST use the
native clipboard and MUST work while the main window is closed.

#### Scenario: Copy MCP endpoint from the tray

- **WHEN** the user activates the MCP status item while the main window is closed
- **THEN** the MCP endpoint URL lands on the system clipboard and the item briefly acknowledges the copy (checkmark or "Copied" retitle).

#### Scenario: Pairing from the tray

- **WHEN** the user chooses "Transfer / Pair Device…"
- **THEN** the main window opens directly on the settings pairing section with the QR code visible.

### Requirement: Connect/Settings page in the web app

A Connect/Settings page at `/settings` MUST be served by the normal web app —
not desktop-only — and MUST show: the MCP endpoint URL, a copy-paste JSON
client configuration block, and the equivalent `claude mcp add` command line
for the existing MCP server command. Copy affordances SHOULD be provided for
each block. The page MUST render identically (same template, same data) in
browser and desktop modes.

#### Scenario: Browser-mode parity

- **WHEN** `/settings` is requested from a plain browser against `msgbrowse serve`
- **THEN** the page renders with the same MCP endpoint URL, JSON config, and `claude mcp add` line the desktop window shows, with no desktop-only gating.

### Requirement: Server-rendered QR code

The `/settings` page MUST include a QR code rendered **server-side** as a PNG
`data:` URI embedded in the page (the existing CSP already allows
`img-src 'self' data:`, ADR-0010) — no client-side QR generation and no CSP
change. A pure-Go QR library (e.g. `skip2/go-qrcode`) is an acceptable new
dependency and MUST NOT introduce cgo. The QR payload format is defined by
SPEC-0011 (device sync); this spec only renders the payload it is given.

#### Scenario: QR renders under the strict CSP

- **WHEN** `/settings` renders with a pairing payload available
- **THEN** the QR appears as an `<img>` with a PNG `data:` URI source, the response carries the unchanged `Content-Security-Policy`, and the browser reports no CSP violations.

### Requirement: Release packaging via CI matrix

Desktop artifacts MUST be produced by a CI build matrix, because webview
shells cannot be cross-compiled (ADR-0017): a macOS runner producing a `.app`
bundle (and dmg), an Ubuntu runner with WebKit2GTK producing the Linux build,
and a Windows runner producing an `.exe`. Matrix jobs MUST NOT gate the core
CI check for non-desktop changes. Artifacts are unsigned in v1; macOS signing
and notarization are deferred (ADR-0017).

#### Scenario: Release produces per-OS artifacts

- **WHEN** a release build runs in CI
- **THEN** the matrix publishes a macOS `.app`/dmg, a Linux build, and a Windows `.exe` as release artifacts, while the `CGO_ENABLED=0` core check passes independently of the matrix.

### Requirement: Graceful shutdown

Closing the desktop window (or quitting the app) MUST stop the embedded
server cleanly: in-flight requests drained via the server's graceful shutdown,
the store closed, and no orphaned listener or process left behind. Abnormal
webview termination MUST NOT leave the server running headless.

#### Scenario: Window close stops the server

- **WHEN** the user closes the desktop window
- **THEN** the embedded server completes in-flight requests, closes the store, releases its loopback port, and the process exits.

## Security Requirements

The desktop shell inherits the
[ADR-0010](../../../adr/0010-security-privacy-posture.md) posture —
loopback-only, single-user, no auth layer — and MUST NOT weaken it. Deviations
from web-facing defaults are stated explicitly below with their justification.

- **Bind surface.** The embedded server MUST bind `127.0.0.1` on an ephemeral
  port and nothing else. The desktop shell MUST NOT widen the bind, expose a
  non-loopback listener, or add any listener beyond the embedded server. The
  device-sync listener is the one deliberate exception to the no-auth posture
  and it gets real authentication via mutual TLS — that listener is specified
  by SPEC-0011, not here.
- **Authentication.** No authentication is added (deviation from web-facing
  defaults): the app has no auth layer by design — loopback single-user trust
  per ADR-0010, where binding to loopback removes the network attacker. The
  desktop shell keeps that boundary intact by construction (ephemeral loopback
  bind).
- **Endpoints.** All routes served by the embedded server carry the same
  posture as browser mode. New route introduced by this spec:

  | Endpoint | Method | Auth | Justification |
  |---|---|---|---|
  | `/settings` | GET | Public | No auth layer exists — loopback single-user trust per ADR-0010; the page is reachable only by local processes. It reveals MCP connection details and the SPEC-0011 pairing QR, which are intended precisely for the local operator; pairing-material sensitivity and rotation are SPEC-0011's contract. |

- **Rate limiting.** None (deviation, justified): a single-user loopback
  server has no unauthenticated remote surface to throttle; QR/PNG generation
  is cheap and per-request bounded.
- **Security headers.** `/settings` MUST be served through the existing
  `securityHeaders` middleware unchanged — strict CSP (`default-src 'none'`,
  `img-src 'self' data:` already permits the QR data URI), `nosniff`,
  `no-referrer`, frame denial. The desktop webview loads over loopback HTTP
  and MUST receive the same headers.
- **Request body size limits.** `/settings` is GET-only and accepts no request
  body; this spec introduces no new body-accepting endpoint.
- **CSRF protection.** No state-changing endpoint is introduced; the existing
  `form-action 'self'` posture stands. If a future settings mutation is added
  it MUST re-visit CSRF explicitly.
- **Redirect validation.** `/settings` performs no redirects; this spec
  introduces no redirect targets, so no open-redirect surface is added.

## Accessibility Requirements

The Connect/Settings page and any shell chrome MUST meet WCAG 2.1 AA.

- **Landmarks.** `/settings` MUST render inside the existing shell's ARIA
  landmarks (`main` content region, navigation), with a proper heading
  hierarchy starting at a single page `h1`.
- **Icon-only controls.** Copy buttons for the endpoint URL, JSON config, and
  `claude mcp add` line MUST carry `aria-label`s describing what they copy.
- **Dynamic feedback.** "Copied" confirmations MUST be announced via an
  `aria-live="polite"` region, not conveyed by visual change alone.
- **QR alternative.** The QR `<img>` MUST have alt text stating its purpose,
  and the QR MUST NOT be the only path to its information — the same
  connection details are present as selectable, copyable text on the page.
- **Keyboard navigation.** Every interactive element on `/settings` MUST be
  reachable and operable by keyboard with a visible focus indicator; copy
  actions MUST work via keyboard activation.
- **Focus management.** HTMX boosted navigation to `/settings` MUST follow the
  app's existing focus behavior for swapped content; a focus trap MUST NOT be
  introduced.
- **Shell chrome.** Native menus, the dock entry, and window controls use the
  OS accessibility layer (VoiceOver et al.) that Wails' native widgets
  provide; custom in-window chrome, if any, MUST follow the same WCAG 2.1 AA
  rules as the rest of the UI. Color and contrast MUST follow the
  [ADR-0012](../../../adr/0012-slate-redesign-design-system.md) token system
  already audited for AA.
