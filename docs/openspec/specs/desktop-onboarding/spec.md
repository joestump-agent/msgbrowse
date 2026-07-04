---
status: draft
date: 2026-07-04
implements: [ADR-0020]
extends: [SPEC-0010]
requires: [SPEC-0001]
---

# SPEC-0013: Desktop guided setup

## Overview

The desktop app becomes a consumer product: download, double-click, and an
in-app setup detects which messaging apps you use and enables imports with a
click — no terminal, no visible data directory, no manual tool installs
([ADR-0020](../../../adr/0020-bundled-exporters-guided-setup.md)). This spec
extends the desktop shell ([SPEC-0010](../desktop-shell/spec.md)): the same
Wails v2 window over the same embedded `internal/web` server on a loopback
ephemeral port, now with a first-run Setup surface served by the web app.

The three upstream exporters (`sigexport`, `imessage-exporter`, `wtsexporter`)
are **bundled** inside the `.app` under `Contents/Resources` — a relocatable
Python runtime plus a prebuilt venv (`signal-export`, `whatsapp-chat-exporter`)
and the native `imessage-exporter` binary. The app resolves those bundled paths
directly; it never reads PATH and never asks the user to install anything. The
detection, export-orchestration, and incremental-import building blocks already
exist ([SPEC-0007 onboarding](../onboarding/spec.md), reused here; ingestion per
[SPEC-0001](../ingestion/spec.md)); this spec refactors the detection logic out
of `internal/cli/doctor.go` into a shared package both the CLI and the UI call.

OS consent gates (macOS Full Disk Access, Signal Keychain access, WhatsApp
container access) are detect-and-guide only: the app detects a missing grant
and deep-links the user to the exact System Settings pane, then re-checks — it
never bypasses OS consent.

## Requirements

### Requirement: Bundled toolchain resolution

The desktop app MUST resolve every exporter and the Python runtime from paths
inside the app bundle (`Contents/Resources`), and MUST NOT consult `PATH`, the
user's Homebrew, or any system Python when running exports in desktop mode. The
bundled venv's `sigexport` and `wtsexporter` MUST be invoked through the bundled
relocatable Python, and `imessage-exporter` from its bundled native binary. The
resolved tools MUST run fully offline — no network egress is added by
detection, export, or import.

#### Scenario: Fresh Mac with no Homebrew or Python still exports

- **WHEN** the app runs on a Mac that has no Homebrew, no system Python, and no network connection, and the user clicks Enable on a detected source
- **THEN** the app resolves the exporter and Python from `Contents/Resources`, runs the export offline, and imports the result — with no PATH lookup and no install prompt.

### Requirement: Bundled tool integrity and version check

On startup (or before first use), the app MUST verify the bundled toolchain: it
MUST confirm each expected exporter and the Python runtime is present at its
bundled path and executable, and MUST record the pinned upstream version of each
tool for display in the About/Advanced view. If a bundled tool is missing or not
executable, the app MUST surface a clear error state for the affected source
rather than silently falling back to PATH.

#### Scenario: Missing bundled binary is a clear error, not a PATH fallback

- **WHEN** the app starts and the bundled `imessage-exporter` binary is absent or not executable
- **THEN** the iMessage Setup card reports a bundled-tool error with the tool name and version expected, and the app does NOT attempt to resolve `imessage-exporter` from PATH.

### Requirement: App-owned, hidden data and archive roots

The desktop app MUST provision and use a managed layout without the user
specifying any path: the `data_dir` anchored to `os.UserConfigDir()/msgbrowse`
(per [SPEC-0010](../desktop-shell/spec.md)), and managed archive roots at
`<data_dir>/archives/signal`, `<data_dir>/archives/imessage`, and
`<data_dir>/archives/whatsapp`. The exporters MUST write only into the managed
root for their source. The layout MUST be discoverable in an About/Advanced view
(the resolved paths shown, openable in the file manager) but MUST NOT be required
input at any point in Setup.

#### Scenario: First launch creates the managed layout with no prompts

- **WHEN** the desktop app launches for the first time against an empty store
- **THEN** it creates `<data_dir>` and the three `<data_dir>/archives/<source>` roots itself, asks the user for no path, and the resolved locations are visible only in the About/Advanced view.

### Requirement: Source detection

The in-app Setup MUST detect each supported source by probing its well-known
local location: Signal Desktop at `~/Library/Application Support/Signal`,
Messages at `~/Library/Messages/chat.db`, and the WhatsApp app at
`~/Library/Group Containers/*WhatsApp*/ChatStorage.sqlite`. It MUST present one
Setup card per source with an explicit state of **Ready**, **Needs-permission**,
**Not-detected**, or **Enabled**. The detection logic MUST be a reusable package
that both the CLI `doctor` and the desktop UI call — the detection currently in
`internal/cli/doctor.go` MUST be refactored into a shared package
(`internal/setup`, or equivalent) rather than duplicated.

#### Scenario: Signal + iMessage present, no WhatsApp

- **WHEN** Setup runs on a machine with Signal Desktop and Messages present but no WhatsApp app installed
- **THEN** it shows an actionable Signal card and an actionable iMessage card (Ready or Needs-permission), and a WhatsApp card in the Not-detected state, and the CLI `doctor` reports the same three detections from the same shared code.

### Requirement: Permission detection and guidance

Setup MUST detect missing OS consent grants — macOS Full Disk Access for the
iMessage `chat.db`, and Signal Desktop Keychain access — and, when a grant is
missing, MUST render step-by-step guidance with a deep link to the specific
System Settings pane. It MUST provide a **Recheck** action that re-runs the
permission probe on the user's return and updates the card state. The app MUST
NOT bypass, spoof, or attempt to work around any OS consent gate.

#### Scenario: iMessage enabled without Full Disk Access

- **WHEN** the user tries to enable iMessage but the app lacks Full Disk Access to `~/Library/Messages/chat.db`
- **THEN** the card enters Needs-permission, shows the Full Disk Access guidance with a deep link to the exact System Settings pane and a Recheck action, and the export never silently fails or produces an empty archive.

### Requirement: One-click enable and import per source

Each Setup card MUST offer a single **Enable** action that, for a source in the
Ready state, configures the managed archive root, runs the bundled exporter as a
cancellable background job with live progress, then imports the result
incrementally ([SPEC-0001](../ingestion/spec.md)), after which the source
appears in the transcript sidebar and the card enters the Enabled state. The
export job MUST reuse the existing `export` orchestration
([SPEC-0007](../onboarding/spec.md)) and the import MUST reuse the existing
incremental ingest — neither is reimplemented in the UI layer.

#### Scenario: Enable iMessage end to end

- **WHEN** Full Disk Access is granted and the user clicks Enable on the iMessage card
- **THEN** the app runs the bundled `imessage-exporter` in copy mode into `<data_dir>/archives/imessage` with a live progress indicator, imports the messages, the card enters Enabled, and the iMessage conversations appear in the sidebar.

### Requirement: Error Handling Standards

The export and import background jobs MUST propagate the caller's `context` for
cancellation and MUST honor a user Cancel action promptly. Exporter and importer
errors MUST be captured as structured results and surfaced in the UI — the exact
failing step and a human-readable message — and MUST NOT be silently swallowed
or reduced to a generic failure. A failed or cancelled job MUST NOT corrupt the
store or leave a partially-written archive that a later import treats as
complete: exports MUST write to a staging location and be promoted to the managed
root only on success (or otherwise be import-idempotent so a re-run repairs the
state). Progress and error state MUST be observable from the UI throughout the
job's lifetime.

#### Scenario: Cancel mid-export leaves no partial archive

- **WHEN** the user cancels an in-flight export
- **THEN** the background job's context is cancelled, the exporter subprocess is terminated, no partial output is promoted into the managed archive root, the store is unchanged, and the card returns to its pre-Enable state with a clear "cancelled" message.

#### Scenario: Exporter failure surfaces the real error

- **WHEN** a bundled exporter exits non-zero (e.g. a locked source database)
- **THEN** the UI shows the failing source, the step, and the exporter's error text, the job is marked failed (not silently dropped), and the store and managed archive are left in a consistent, re-runnable state.

### Requirement: Concurrency Safety

The export/import jobs are long-running concurrent workers with progress and
cancellation. Each source's Enable/Refresh MUST run as a supervised worker with a
defined lifecycle (start, progress, terminal state) tied to a cancellable
context. The app MUST NOT run two mutating jobs for the same source concurrently
(a second Enable/Refresh while one is in flight MUST be rejected or coalesced),
and MUST NOT leave orphaned exporter subprocesses when the job ends, is
cancelled, or the app quits. Shared state (job registry, per-source status) MUST
be accessed under synchronization, and worker shutdown MUST be part of the
app's graceful shutdown so no subprocess outlives the window.

#### Scenario: A second Enable while one is running is rejected

- **WHEN** an iMessage Enable is already running and the user triggers Enable (or Refresh) on iMessage again
- **THEN** the second request is rejected or coalesced onto the running job — never a second concurrent exporter for the same source — and the UI reflects the single in-flight job.

#### Scenario: Quitting the app tears down running jobs

- **WHEN** the user quits the app while an export/import job is running
- **THEN** the job's context is cancelled as part of graceful shutdown, the exporter subprocess is terminated, and no orphaned process or listener is left behind.

### Requirement: Refresh

Setup MUST provide a per-source **Refresh** and an **all-sources Refresh** that
re-run export + incremental import for the enabled source(s), adding only the
delta ([SPEC-0001](../ingestion/spec.md) incremental semantics). Refresh MUST
reuse the same background-job, progress, cancellation, and concurrency machinery
as Enable. Scheduled or background auto-refresh is a **NON-GOAL for v1** (a
launchd background agent is a follow-on); Refresh in v1 is user-initiated only.

#### Scenario: Refresh adds only the delta

- **WHEN** new messages have arrived since the last import and the user clicks Refresh on an enabled source
- **THEN** the app re-exports and runs an incremental import that adds only the new messages, without duplicating existing rows, and reports the number of new conversations/messages.

### Requirement: First-run wizard versus returning launch

On launch, the app MUST route based on store state: an empty store (no imported
conversations) MUST land on the Setup surface as a first-run wizard; a configured
store (at least one enabled source with imported data) MUST land on the
transcript UI, with Setup reachable from the app navigation. Setup MUST remain
reachable at all times so a returning user can enable an additional source or
Refresh.

#### Scenario: First launch lands on Setup

- **WHEN** the app launches against an empty store
- **THEN** the window opens on the Setup wizard showing the per-source cards, not the empty transcript UI.

#### Scenario: Returning launch lands on the transcript

- **WHEN** the app launches against a store that already has imported conversations
- **THEN** the window opens on the transcript UI and Setup is reachable from the navigation without re-running the wizard.

### Requirement: Signing and notarization for bundled binaries

The shipped desktop app MUST be code-signed with an Apple Developer ID and
notarized, and every embedded executable (the relocatable Python runtime, the
venv's compiled extensions, and the `imessage-exporter` binary) MUST be signed,
so macOS Gatekeeper allows the app and its bundled subprocesses to run. This is a
release-pipeline requirement that amends
[ADR-0017](../../../adr/0017-desktop-shell-wails.md)'s deferred-signing stance;
it extends the desktop CI matrix and release packaging of
[SPEC-0010](../desktop-shell/spec.md) and the release-publishing pipeline
([SPEC-0012](../release-publishing/spec.md)).

#### Scenario: A downloaded release runs without Gatekeeper killing the exporters

- **WHEN** a user downloads the signed, notarized release, opens the app, and clicks Enable on a source
- **THEN** Gatekeeper allows the app to launch and allows it to spawn the bundled exporter and Python runtime, and no embedded executable is blocked as an unsigned/unnotarized binary.

## Security Requirements

The desktop app inherits the
[ADR-0010](../../../adr/0010-security-privacy-posture.md) posture —
loopback-only, single-user, strict CSP, no auth layer — and the
[SPEC-0010](../desktop-shell/spec.md) bind surface (the embedded server binds
`127.0.0.1` on an ephemeral port and nothing else). This spec adds Setup routes
that trigger **privileged local actions** (spawning subprocesses that read
personal databases, writing archives under the managed roots), so it tightens
same-origin protection beyond the read-only `/settings` page.

### Endpoint table

All routes are served by the embedded `internal/web` server, loopback-only.
`GET` routes are safe; the `POST` routes trigger privileged local actions and
carry same-origin protection.

| Endpoint | Method | Auth | Justification |
|---|---|---|---|
| `/setup` | GET | Public, loopback-only | No auth layer exists — loopback single-user trust per ADR-0010; reachable only by local processes. Renders the source-detection cards; performs detection (read-only probes), no mutation. |
| `/setup/enable` | POST | Public, loopback-only + same-origin | Loopback single-user trust per ADR-0010, PLUS same-origin protection (below): this route spawns a bundled exporter that reads a personal database and writes the managed archive, then imports — a privileged local action that MUST NOT be triggerable cross-origin. |
| `/setup/refresh` | POST | Public, loopback-only + same-origin | Same as `/setup/enable` — re-runs export + incremental import for an enabled source. Same-origin required. |
| `/setup/recheck` | POST | Public, loopback-only + same-origin | Re-runs the OS-permission probe for a source. Read-only in effect, but state-changing in the UI (updates card state) and same-origin protected for consistency with the other POSTs. |

### Same-origin protection for privileged POSTs

- The app is loopback-only with a strict CSP (`form-action 'self'`,
  `frame-ancestors 'none'` per ADR-0010), but a POST that launches an exporter
  MUST NOT be triggerable cross-origin. Each state-changing Setup POST
  (`/setup/enable`, `/setup/refresh`, `/setup/recheck`) MUST be rejected unless
  it is same-origin: the handler MUST verify the `Origin` (or, absent it, the
  `Sec-Fetch-Site: same-origin` / `Referer`) header against the embedded
  server's own loopback origin, and MUST require a per-session token minted at
  page render and submitted with the POST (a CSRF-style token) — enforced even
  under loopback, because another local process or a malicious page loaded in a
  browser must not be able to drive the exporter.
- A POST failing the same-origin/token check MUST be rejected with `403` and
  MUST NOT start any subprocess.

### No arbitrary paths — managed roots only

- No Setup route accepts a filesystem path as input. The managed archive roots
  are computed by the app (`<data_dir>/archives/<source>`); the request body
  carries only a source identifier from a fixed enum (`signal`, `imessage`,
  `whatsapp`). This closes path-injection into exporter arguments: the exporter
  command line is assembled from app-owned constants and the bundled tool path,
  never from a client-supplied path.

### Request body size limits

- The Setup POST bodies are tiny (a source identifier plus the session token).
  Each POST handler MUST enforce a small `http.MaxBytesReader` body cap
  (kilobytes, not megabytes) so a malformed or oversized body is rejected
  before processing.

### Subprocess argument safety

- Exporter subprocesses MUST be spawned with an explicit argv (never a shell
  string), from the bundled tool path and app-owned constant flags plus the
  computed managed root — no client input reaches the command line. The bundled
  Python runtime is invoked by absolute bundled path so no PATH/interpreter
  hijack is possible.

### Security headers

- All Setup routes MUST be served through the existing `securityHeaders`
  middleware unchanged — strict CSP (`default-src 'none'`, `img-src 'self'
  data:`), `nosniff`, `no-referrer`, `X-Frame-Options: DENY` — so the webview
  and any browser-mode access receive identical hardening.

## Accessibility Requirements

The Setup wizard is the most accessibility-sensitive surface in the app: it
combines async progress, permission-guidance modals, and per-source actions. It
MUST meet WCAG 2.1 AA.

- **Landmarks and headings.** Setup MUST render inside the app's ARIA
  landmarks (`main` content region, `nav`), with a single page `h1` and a
  correct heading hierarchy; each source card SHOULD be a labelled region or
  list item so assistive tech can enumerate the sources.
- **Async status via `aria-live`.** Export/import progress and terminal state
  (running, imported N, failed, cancelled) MUST be announced through an
  `aria-live="polite"` region (or `role="status"`), not conveyed by visual
  spinner/progress-bar change alone. A hard error MAY use
  `aria-live="assertive"`.
- **Focus management for guidance modals.** When a permission-guidance modal or
  dialog opens, focus MUST move into it, be trapped within it while open, and
  return to the triggering control (the card's Enable/Recheck button) on close;
  `Esc` MUST close it.
- **Keyboard operability.** Every Setup control — Enable, Recheck, Refresh,
  Cancel, the all-sources Refresh, and the System Settings deep link — MUST be
  reachable and operable by keyboard with a visible focus indicator, and MUST
  activate via `Enter`/`Space` as appropriate.
- **Icon-only controls and card icons.** The per-source card icons (Signal,
  Messages, WhatsApp) and any icon-only controls MUST carry `aria-label`s (or
  equivalent accessible names) stating the source and the action; state
  (Ready / Needs-permission / Not-detected / Enabled) MUST be conveyed as text
  or an accessible name, not by color alone.
- **Color and contrast.** Card states and progress MUST meet AA contrast using
  the [ADR-0012](../../../adr/0012-slate-redesign-design-system.md) token
  system already audited for AA; state MUST never be signalled by color alone.
</content>
