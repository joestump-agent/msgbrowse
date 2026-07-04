# ADR-0020: Self-contained desktop onboarding — bundled exporter toolchain + in-app guided setup

- **Status:** Accepted
- **Date:** 2026-07-04
- **Deciders:** Joe Stump
- **Related:**
  - [ADR-0015 (onboarding: doctor/export/sync)](0015-onboarding-doctor-export-sync.md) — this ADR evolves the CLI doctor/export/sync into a consumer surface.
  - [ADR-0017 (desktop shell via Wails v2)](0017-desktop-shell-wails.md) — **amends** its "signing/notarization deferred" stance: signing + notarization become required.
  - [ADR-0010 (security & privacy posture)](0010-security-privacy-posture.md) — **amends** the "archives are read-only external inputs" framing: archives become app-managed local outputs of a bundled local tool.
  - [ADR-0005 (iMessage txt parser)](0005-imessage-txt-parser.md) — the `imessage-exporter` binary bundled here.
  - [ADR-0016 (WhatsApp exporter)](0016-whatsapp-source-exporter.md) — the `whatsapp-chat-exporter` (`wtsexporter`) tool bundled here.
  - [ADR-0003 (dual/multi-source archive)](0003-dual-source-archive.md) — the three-source model the setup enables.

## Context and Problem Statement

msgbrowse today is a power-user tool. The three upstream exporters
(`sigexport`, `imessage-exporter`, `wtsexporter`) are bring-your-own: the user
installs them via pipx/Homebrew, sets `archive_root` / `imessage_archive_root`
/ `whatsapp_archive_root` and a `data_dir` by hand, and runs `doctor` /
`export` / `import` from a terminal ([ADR-0015](0015-onboarding-doctor-export-sync.md)).
The desktop shell ([ADR-0017](0017-desktop-shell-wails.md)) wraps the same web
UI in a native window, but it inherits that BYO-exporter, hand-configured
posture — a `.app` that still needs Homebrew, Python, and a terminal is not a
consumer product.

The owner wants the DESKTOP app to be a consumer product: download,
double-click, and an in-app setup detects which messaging apps you use and
enables imports with a click — no terminal, no visible data directory, no
manual tool installs. The crux decision is how to provision the three
exporters so they "just work," offline, on a machine that may have neither
Homebrew nor Python.

## Decision Drivers

- **Zero-terminal onboarding.** A non-technical user reaches a populated,
  browsable archive without ever opening a shell.
- **The app fully owns and hides its state.** The `data_dir` is already
  anchored to `os.UserConfigDir()/msgbrowse` for the desktop build
  (`cmd/msgbrowse-desktop/internal/embedded`); the app should own the managed
  archive roots the same way, and never make the user name a path.
- **Offline and reproducible.** Onboarding must not depend on the user's
  Homebrew tap state, a working Python, or network reachability at setup time.
- **The exporters must "just work."** Detection, permission guidance, export,
  and import are one click per source, with no version skew the user can see.
- **The `CGO_ENABLED=0` core stays sacred** ([ADR-0013](0013-pure-go-sqlite-driver.md));
  bundling changes packaging, not the pure-Go server/CLI/MCP core.

## Considered Options

The provisioning of the three exporters is the crux; four options span the
trade space from "guide the user to install" to "ship everything in the box."

### (a) Detect + guide manual installs (status quo-ish)

Keep BYO exporters; the in-app setup detects whether each tool is on PATH and,
if missing, links the user to install instructions. Rejected: not
zero-terminal — the user still runs `pipx install …` / `brew install …` in a
shell, which is exactly the friction the desktop product exists to remove.

### (b) Managed install via pipx/Homebrew on demand

The app runs the package managers itself when a tool is missing (spawns
`pipx install signal-export`, `brew install imessage-exporter`). Rejected:
requires a network connection AND a working package manager on the user's
machine at setup time; both are fragile (proxies, offline installs, no
Homebrew, a broken Python), and the app would own driving third-party
installers it cannot fully control.

### (c) Bundle the native binary + managed-install the Python tools

Ship `imessage-exporter` (a self-contained native binary) in the bundle, but
still install `signal-export` and `wtsexporter` (Python) on demand. Rejected:
partial — iMessage would work offline but Signal and WhatsApp would still need
network + a working Python, so onboarding is only one-third self-contained and
the setup UX has two different failure modes.

### (d) FULLY BUNDLED — embed a relocatable Python + a prebuilt venv + the native binary (CHOSEN)

The `.app` embeds, under `Contents/Resources`:

- a **relocatable Python runtime** (e.g. python-build-standalone) — no
  dependence on any system Python;
- a **prebuilt venv** with `signal-export` and `whatsapp-chat-exporter`
  (`wtsexporter`) installed into it;
- the **`imessage-exporter` native binary**.

The app resolves these BUNDLED tool paths from `Contents/Resources` — never
PATH, never asking the user to pipx/brew install anything. All three sources
work immediately, offline, on a machine with no Homebrew and no Python.

Trade: **+80–120 MB** app size, and **code-signing + notarization of the app
AND every embedded executable becomes REQUIRED** (macOS Gatekeeper kills
unsigned embedded binaries), which AMENDS [ADR-0017](0017-desktop-shell-wails.md)'s
"signing/notarization deferred." In exchange we get a true "download and go"
experience that works offline and reproducibly.

## Decision Outcome

**Chosen: option (d), fully bundled, zero dependencies.** The desktop `.app`
ships a relocatable Python runtime, a prebuilt venv containing `signal-export`
and `wtsexporter`, and the `imessage-exporter` binary — all under
`Contents/Resources` — and resolves those bundled paths directly. The CLI keeps
its BYO-exporter path ([ADR-0015](0015-onboarding-doctor-export-sync.md)) for
advanced users; only the desktop app bundles.

Alongside the provisioning choice, this ADR settles five coupled decisions:

1. **The app owns the data dir AND the managed archive roots.** The desktop
   app provisions and uses managed roots under `<data_dir>/archives/{signal,imessage,whatsapp}`,
   with `data_dir` already anchored to `os.UserConfigDir()/msgbrowse`. The CLI
   still accepts `--data-dir` and the three `*_archive_root` keys, but the
   desktop app never surfaces any of them: the location is discoverable in an
   About/Advanced view, never required. The user picks WHICH sources to enable,
   never WHERE anything lives.

2. **OS consent gates are DETECT-AND-GUIDE only; the app never bypasses OS
   consent.** macOS Full Disk Access for the iMessage `chat.db`, Signal
   Desktop's Keychain "Always Allow" prompt, and WhatsApp container access are
   granted by the OS, not by msgbrowse. The app detects a missing grant and
   deep-links the user to the exact System Settings pane, then re-checks on
   return. It never attempts to defeat, spoof, or work around a consent gate.

3. **macOS-first.** The bundling mechanics are `.app`-specific (relocatable
   Python layout, `Contents/Resources`, Gatekeeper). Linux/Windows bundling
   (AppImage/MSI, and their equivalent runtime embedding) is an explicit open
   question, deferred.

4. **No new network egress.** The exporters run locally against local
   databases; [ADR-0010](0010-security-privacy-posture.md)'s
   single-egress-to-the-LLM posture holds — setup adds no outbound connection.
   But the app now WRITES archives (previously read-only external inputs) and
   RUNS bundled subprocesses that read the user's message databases. This
   **amends [ADR-0010](0010-security-privacy-posture.md)**: archives become
   app-managed local OUTPUTS of a local tool, not just read-only external
   inputs. The read-only-at-ingest guarantee still holds for msgbrowse's own
   importer — only the bundled exporter writes the managed roots, and only
   under the managed layout.

5. **Signing/notarization now REQUIRED — amends [ADR-0017](0017-desktop-shell-wails.md).**
   Gatekeeper refuses to launch unsigned embedded executables (the Python
   runtime, the venv's compiled extensions, and `imessage-exporter`), so the
   shipped app and every embedded binary MUST be signed with a Developer ID and
   the app MUST be notarized. This retires ADR-0017's "unsigned artifacts are
   acceptable initially" for the desktop release.

Requirements: [SPEC-0013 (desktop guided setup)](../openspec/specs/desktop-onboarding/spec.md).

### Consequences

#### Good

- **True consumer onboarding.** Download, double-click, click Enable per
  source — no terminal, no visible data dir, no manual installs.
- **Offline.** Every tool is in the box; setup works on a fresh Mac with no
  Homebrew and no Python, with no network at setup time.
- **Reproducible.** The exporter versions are pinned in the bundle, not
  whatever the user's package manager happens to resolve, so behavior is
  identical machine-to-machine and the support surface is knowable.

#### Bad

- **Large app.** +80–120 MB from the Python runtime, the venv, and the native
  binary — a real jump from today's tens-of-MB single binary.
- **Signing/notarization pipeline is now mandatory.** An Apple Developer ID
  (cost + provisioning), a CI notary submission step, and signing of every
  embedded executable become release prerequisites — new pipeline surface
  ([ADR-0019](0019-gitea-primary-github-publishing.md)'s release story gains a
  signing/notarization stage).
- **Upstream version tracking becomes our job.** We must monitor the three
  exporters for security fixes and re-bundle + re-notarize to ship them; the
  BYO path let the user's package manager carry that.
- **Bundled Python is attack-surface + supply-chain.** An embedded interpreter
  and its dependency tree widen what ships inside the signed app; the venv must
  be produced from pinned, hash-verified sources in CI.

#### Neutral

- **Linux/Windows bundling is deferred.** Those platforms keep browser mode and
  the CLI BYO-exporter path until their bundling story is designed.
- **The CLI is unchanged.** It keeps resolving exporters from PATH / overrides
  ([ADR-0015](0015-onboarding-doctor-export-sync.md)); the bundle is a
  desktop-only provisioning layer, so advanced users lose nothing.
</content>
</invoke>
