# ADR-0017: Desktop shell via Wails v2 wrapping the embedded server

- **Status:** Accepted
- **Date:** 2026-07-03
- **Deciders:** Joe Stump
- **Related:** [ADR-0006 (web stack)](0006-web-stack-htmx.md), [ADR-0010 (security & privacy posture)](0010-security-privacy-posture.md), [ADR-0013 (pure-Go SQLite driver)](0013-pure-go-sqlite-driver.md), [ADR-0018 (device pairing & archive sync)](0018-device-pairing-archive-sync.md), [SPEC-0008 (web performance)](../openspec/specs/web-performance/spec.md), [SPEC-0010 (desktop shell)](../openspec/specs/desktop-shell/spec.md)

## Context and Problem Statement

msgbrowse is a server-rendered web app you start from a terminal; `serve
--open` launches the default browser as a convenience (`internal/cli/serve.go`).
After the performance epic (SPEC-0008: < 300 ms TTFB, 19 ms boosted
navigations) the UI *feels* like a native app, but it doesn't *live* like one:
no dock icon, no app menu, no lifecycle (quit the terminal, kill the app), and
a terminal is required at all. The owner wants msgbrowse installable and
launchable as a normal desktop application — and specifically asked whether
Proton Native was the way to do it. The core constraint is ADR-0013: the
server and CLI build with `CGO_ENABLED=0`, and that stays sacred.

## Decision Drivers

- Reuse the existing HTMX web UI byte-for-byte — no second frontend, no
  divergence between browser mode and desktop mode.
- Preserve the pure-Go `CGO_ENABLED=0` core (ADR-0013); any cgo must be
  quarantined, not smeared across the module.
- Lightweight ethos: a single Go binary today (~tens of MB); bundling a
  browser engine is antithetical to it.
- Native lifecycle: dock/taskbar presence, app menu, quit semantics, window
  close cleanly stopping the server.
- Loopback-only posture unchanged (ADR-0010): a desktop shell must not widen
  the network surface.
- Cross-platform packaging (macOS first) without maintaining three UI stacks.

## Considered Options

1. **Wails v2** — Go-native desktop framework wrapping the OS system webview
   (WKWebView / WebView2 / WebKit2GTK); Go bindings, menus, tray, dock, and
   packaging (`.app`/dmg, `.exe`, Linux) built in; requires cgo for the
   webview bindings.
2. **Electron** — bundle Chromium + Node per platform; ~150+ MB artifacts, a
   second runtime and JS toolchain to babysit, for a UI that is already
   server-rendered HTML.
3. **Tauri** — Rust shell over the same system webviews; the Go server would
   run as a sidecar process the Rust core spawns and supervises, adding a Rust
   toolchain and cross-language process management to a pure-Go project.
4. **webview/webview_go** — minimal cgo bindings that open one system-webview
   window at a URL; same engine choice as Wails with far fewer batteries: no
   menus, no tray, no dock integration, no packaging tooling.
5. **Proton Native** — the owner asked about it by name: a React-to-native
   widgets project (libui/Qt) that has been unmaintained for years and targets
   JavaScript/React component trees. msgbrowse has no React and no JS build;
   adopting it would mean rewriting the entire UI on an abandoned runtime.
6. **Status quo: `serve --open`** — keep the browser convenience as the only
   entry point: no dock presence, no lifecycle management, terminal required.

## Decision Outcome

**Option 1: Wails v2**, wrapping the system webview around the embedded Go
server. The desktop app starts the existing `internal/web` server in-process
on a loopback ephemeral port and points its window at it, so browser mode and
desktop mode serve the identical app
([SPEC-0010 (desktop shell)](../openspec/specs/desktop-shell/spec.md)
specifies the shell;
[SPEC-0011 (device sync)](../openspec/specs/device-sync/spec.md) — see
[ADR-0018](0018-device-pairing-archive-sync.md) — specifies device pairing,
whose QR the shared Connect page renders).

The key consequence is that webview bindings **require cgo**. This is
resolved by isolation, not by surrender: the shell lives in its own build
target (`cmd/msgbrowse-desktop`, gated by its own build tags — and, as
implemented, its own nested Go module, because `go mod tidy` is
build-tag-agnostic and tags alone would have pulled Wails' dependency tree
into the core `go.mod`/`go.sum`; see the
[SPEC-0010 design](../openspec/specs/desktop-shell/design.md) "Build
isolation" decision) so `CGO_ENABLED=0 go build ./...` keeps succeeding for
the server, CLI, and MCP core. Because webview shells cannot be cross-compiled, desktop binaries are
built in a GitHub Actions matrix — macOS runner → `.app`/dmg, Ubuntu runner
with WebKit2GTK → Linux build, Windows runner → `.exe`.

Options 2, 3, and 5 are rejected outright (Chromium bundle; Rust +
sidecar supervision; unmaintained wrong-stack). Option 6 is rejected as the
end state but remains the default for servers and Docker. Option 4 remains
the documented minimal fallback if Wails friction ever outweighs its
batteries — the architecture (webview → loopback HTTP) is identical.

### Consequences

- Good: the whole existing UI, CSP, and performance work carry over
  unchanged; the desktop app is a window onto the same served bytes.
- Good: system webview keeps artifacts small and Wails supplies menus, dock,
  tray, and packaging that webview_go would make us hand-roll.
- Bad: cgo enters the repo — contained to `cmd/msgbrowse-desktop` and CI
  matrix builds; local `make check` and releases of the core stay
  `CGO_ENABLED=0`, and the desktop target must never become a build
  prerequisite for the core.
- Bad: macOS signing and notarization are a real cost (Apple Developer ID,
  notary submission in CI) — known, and explicitly deferred past v1;
  unsigned artifacts are acceptable for the owner-operator initially.
- Neutral: the rendering engine now differs per OS (WebKit vs WebView2 vs
  WebKit2GTK); the UI already targets evergreen browsers, so this is a test
  matrix, not a rewrite.
- Neutral: Wails v3 is not stable yet; v2 is the supported line today and a
  v3 migration is revisited when it ships stable.
