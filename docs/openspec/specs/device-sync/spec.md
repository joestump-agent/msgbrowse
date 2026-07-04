---
status: deprecated
date: 2026-07-03
implements: [ADR-0018]
requires: [SPEC-0001]
---

# SPEC-0011: Device sync

## Overview

Device sync makes one msgbrowse install's archives browsable on other machines
on the LAN by synchronizing the **archives, not the database**
([ADR-0018](../../../adr/0018-device-pairing-archive-sync.md)). The node that
runs the exporters for a source is that source's **importer**; paired
**replicas** receive the archive files — messages *and* media — verify them,
adopt them atomically, and run their own local
[SPEC-0001](../ingestion/spec.md) ingest. The SQLite database is derived state
and never crosses the wire.

This is the first msgbrowse feature that listens beyond loopback. Trust is
established once, physically, via a QR/manual pairing code — displayed on the
web app's `/settings` Connect page, whose rendering
[SPEC-0010 (desktop shell)](../desktop-shell/spec.md) owns while this spec
owns the payload and pairing semantics — and enforced thereafter with mutual
TLS on pinned self-signed certificates. The sync listener is disabled by
default; the loopback web UI posture
([ADR-0010](../../../adr/0010-security-privacy-posture.md)) is unchanged.
Exact config key, flag, CLI command, and route spellings are provisional —
see design.md Open Questions.

## Requirements

### Requirement: Pairing Initiation

An importer with device sync enabled MUST be able to open a **pairing window**
from the settings page (loopback web UI) and from a CLI command. Opening the
window MUST generate a single-use pairing token whose TTL MUST NOT exceed 10
minutes, enforced against the issuing node's own clock. The pairing payload
`{protocol version, listener endpoint, token, SHA-256 fingerprint of the
listener's TLS certificate}` MUST be presented both as a server-generated QR
code and as a copyable manual code carrying the same fields. The token MUST be
invalidated on first use, on TTL expiry, on explicit window close, and when
device sync is disabled. Pairing attempts MUST be rate-limited: after at most
5 consecutive failed attempts the pairing window MUST close and the failure
MUST be logged and surfaced in the settings UI.

#### Scenario: Token is single-use

- **WHEN** a replica completes pairing with a valid token and a second device presents the same token
- **THEN** the second attempt is rejected, and pairing another device requires opening a new pairing window.

#### Scenario: Expired token

- **WHEN** a replica presents a token after its TTL has elapsed on the importer's clock
- **THEN** pairing is rejected with a clear error and the window is closed.

#### Scenario: Brute force closes the window

- **WHEN** 5 consecutive pairing attempts fail token validation
- **THEN** the pairing window closes, further `/v1/pair` requests are refused until a new window is opened, and the event is logged and shown in settings.

### Requirement: Pairing Acceptance and Mutual Certificate Pinning

Each node MUST generate a long-lived self-signed TLS keypair when device sync
is first enabled. A pairing replica MUST verify the importer's server
certificate against the fingerprint from the QR/manual payload — never against
the WebPKI — and MUST abort before transmitting the token if the fingerprint
does not match. On a valid token, the importer MUST pin the replica's client
certificate fingerprint and persist the peer record (device name, role,
replica listener address); the replica MUST pin the importer's fingerprint
symmetrically. After pairing, every sync connection in either direction MUST
require mutual TLS (TLS 1.3 minimum) with an exact pinned-fingerprint match,
and connections presenting unknown certificates MUST be rejected.

#### Scenario: Fingerprint mismatch aborts before token disclosure

- **WHEN** a replica connects to the pairing endpoint and the presented server certificate does not match the QR fingerprint
- **THEN** the replica aborts the handshake without sending the token and reports a possible wrong-device or man-in-the-middle condition.

#### Scenario: Unknown certificate rejected after pairing

- **WHEN** a client presents a certificate whose fingerprint is not pinned on any non-pairing sync endpoint
- **THEN** the connection is rejected at the TLS layer and the rejection is logged with the presented fingerprint.

### Requirement: Sync Listener Posture

The device-sync listener MUST be disabled by default and MUST only start when
explicitly enabled in configuration. When enabled it MUST bind the configured
LAN address on a dedicated port distinct from the web UI port, and the web UI
bind MUST remain unchanged (loopback default per ADR-0010). Disabling device
sync MUST stop the listener and invalidate any open pairing window. Listener
startup MUST log the bound address and the number of paired peers.

#### Scenario: Default config exposes nothing new

- **WHEN** msgbrowse runs with device sync absent from configuration
- **THEN** the only listening socket is the loopback web UI and no sync endpoint exists.

#### Scenario: Opt-in starts the listener

- **WHEN** the operator enables device sync and restarts (or reloads) the node
- **THEN** the sync listener binds its configured LAN address on its own port and logs the bind, while the web UI continues to serve loopback only.

### Requirement: Archive Manifest and Diff

The importer MUST maintain a per-source manifest: an inventory of every file
under each configured archive root as `{relative path, size, SHA-256}` plus a
monotonically increasing generation number. Manifests MUST be recomputed after
each completed ingest pass (incremental hashing keyed on size/mtime MAY be
used). Manifests MUST NOT include the database, WAL/SHM files, or anything
under `data_dir`, and SHOULD exclude the encrypted `.snapshots` backups
(device-local backups, not browsing data — ADR-0010). Replicas MUST diff the
fetched manifest against their persisted sync state and fetch only missing or
changed files. File transfers MUST be resumable via byte ranges, and every
fetched file MUST be verified against its manifest hash before adoption.

#### Scenario: Unchanged archive transfers nothing

- **WHEN** a replica completes a sync round against an importer whose manifest generation has not changed since the replica's last round
- **THEN** no file bytes are transferred and the round completes as a cheap no-op.

#### Scenario: Interrupted transfer resumes

- **WHEN** a file transfer is interrupted mid-stream and the replica retries in the next round
- **THEN** the fetch resumes from the persisted byte offset rather than restarting, and the completed file still hash-verifies before adoption.

### Requirement: Bootstrap Transfer

The first sync round after pairing MUST transfer the full manifest contents
for every source the importer serves. Bootstrap progress (per-file completion,
bytes transferred) MUST be persisted so an interrupted bootstrap resumes
across process restarts without refetching files that already verified.
Bootstrap progress MUST be observable in the CLI status output and the
settings page.

#### Scenario: Bootstrap survives a restart

- **WHEN** a replica is restarted midway through its initial bootstrap transfer
- **THEN** on restart the bootstrap continues from persisted progress, previously verified files are not refetched, and progress reporting reflects the true remaining work.

### Requirement: Persistent Synchronization

After completing an ingest pass, the importer MUST send an advisory
notification to each paired replica announcing the new manifest generation.
Notifications MUST carry no archive content and MUST be idempotent — a
duplicate or replayed notification triggers at most a redundant manifest
check. Replicas MUST also poll the importer's manifest on a configurable
interval as a fallback, so a replica that was offline for a notification
converges on its next poll. Notification delivery failures MUST be logged and
MUST NOT fail the importer's ingest pass.

#### Scenario: Offline replica converges by polling

- **WHEN** an importer's post-ingest notification cannot be delivered because the replica is offline
- **THEN** the importer's ingest pass still succeeds, and the replica fetches the new generation on its next scheduled poll.

### Requirement: Importer and Replica Roles

Role is per source: a node MUST act as importer only for sources whose
exporter-produced archive root it holds, and there MUST be exactly one
importer per source across a paired set. A replica MUST refuse manifest
entries for a source from any peer other than that source's registered
importer, and an attempt to register a second importer for an
already-claimed source MUST fail with a clear error naming the existing
importer. On replicas, synced archive roots MUST be written only by the sync
engine and MUST be treated as strictly read-only by every other subsystem
(preserving SPEC-0001's read-only archive guarantee); replicas MUST ingest
adopted archives locally.

#### Scenario: Second importer claim rejected

- **WHEN** a peer attempts to register as importer for a source that already has a different registered importer
- **THEN** the registration fails with an error identifying the existing importer, and no manifest entries from the new claimant are adopted for that source.

### Requirement: Unpairing and Revocation

Either node MUST be able to unpair a peer from the CLI and from the settings
page. Unpairing MUST remove the peer's pinned certificate and peer record,
after which connections presenting that certificate MUST be rejected at the
TLS layer. Revocation MUST take effect locally and immediately, without
requiring the revoked peer to be reachable or cooperative. Unpairing MUST NOT
delete already-synced archive files or the replica's database; it only severs
future synchronization.

#### Scenario: Revoked replica is refused

- **WHEN** an importer unpairs a replica and that replica later attempts a manifest fetch with its previously pinned certificate
- **THEN** the connection is rejected at the TLS layer and the attempt is logged.

### Requirement: Status Surfacing

A CLI status command MUST report, per peer: role, last successful sync time,
manifest generation lag, and in-flight transfer progress. The settings page
MUST show a devices section with the same information plus pairing controls.
`doctor` MUST gain device-sync checks: listener posture matches configuration,
each paired peer is reachable over pinned mTLS, pinned certificates are valid
and not near expiry, replica manifest staleness beyond a threshold is flagged,
and leftover staging files are reported.

#### Scenario: Doctor flags an unreachable peer

- **WHEN** `doctor` runs on a node with a paired peer that does not answer a pinned-mTLS ping
- **THEN** the device-sync check fails for that peer with a remediation hint (peer offline, address changed, or unpaired remotely).

### Requirement: Database Is Never Transferred

The sync protocol MUST NOT expose, list, or transfer the SQLite database or
any file under `data_dir`; only archive files appear in manifests and file
responses. Each node's database MUST be derived exclusively from its own local
ingest of its local archive tree, so a replica's database is definitionally a
fresh SPEC-0001 ingest of the synced archives.

#### Scenario: Replica DB equals its own ingest of synced archives

- **WHEN** a replica completes a sync round and its local ingest, and the same archive tree is independently ingested into a fresh database
- **THEN** both databases contain identical conversations, messages, attachments, links, and reactions for the synced sources.

#### Scenario: data_dir never appears on the wire

- **WHEN** any manifest or file request is served by the sync listener
- **THEN** no returned path or served file resolves inside `data_dir`, including the SQLite database and its WAL/SHM files.

### Requirement: Failure Tolerance and Atomic Adoption

A partial or failed transfer MUST NOT corrupt an archive. Fetched files MUST
be streamed to a staging area on the same filesystem as the archive root,
hash-verified, fsynced, and moved into place with an atomic rename; a file at
its final archive path MUST always be either the complete old version or the
complete new version. A crash at any point mid-round MUST leave the archive
ingestable and the round resumable. Hash-verification failures (e.g. a file
mutated on the importer mid-transfer) MUST discard the staged file and requeue
it for the next round rather than adopting or aborting. Replica ingest MUST be
triggered only after a round's verified adoptions complete.

#### Scenario: Kill mid-transfer leaves no partial file

- **WHEN** the replica process is killed while streaming a large attachment
- **THEN** the archive root contains no partial file (the partial exists only in staging), the next round resumes the transfer, and an ingest pass at any point succeeds against a consistent tree.

### Requirement: Error Handling Standards

All error-producing device-sync operations MUST follow structured error
handling:

- Errors MUST be wrapped with contextual information at each layer boundary
  (e.g. "sync round failed: fetch signal/media/cabin.jpg: hash mismatch").
- Sentinel errors MUST be defined for failure modes callers distinguish
  programmatically (token expired, token consumed, unknown peer certificate,
  hash mismatch, importer conflict).
- Silent error swallowing MUST NOT occur — every error MUST be returned,
  logged with sufficient context, or explicitly handled with a documented
  reason for suppression.
- Structured logging MUST be used for error reporting (key-value pairs, not
  string interpolation).

#### Scenario: Hash mismatch is attributable

- **WHEN** a fetched file fails hash verification
- **THEN** the log entry carries the source, relative path, expected and actual hashes, and peer identity, and the round continues with the file requeued.

### Requirement: Concurrency Safety

All concurrent device-sync operations MUST follow safe concurrency patterns:

- Context propagation MUST be used for cancellation and timeout signaling
  across the listener, transfer workers, pollers, and notification senders.
- Worker lifecycle MUST be explicitly managed — the listener, poll loop, and
  transfer workers MUST have clean startup and graceful shutdown sequences.
- Shared mutable state (peer registry, pairing window, transfer cursors) MUST
  be protected by appropriate synchronization primitives or eliminated via
  message passing.
- Concurrent tests MUST run with race detection enabled in CI.

#### Scenario: Graceful shutdown mid-round

- **WHEN** the process receives a shutdown signal during an active sync round
- **THEN** in-flight transfers cancel via context, staging state is left resumable, the listener drains, and the process exits without panics or leaked workers.

### Requirement: Database Operation Standards

Device sync introduces node-local sync-state tables (peer registry with pinned
fingerprints, manifest cache and generations, transfer cursors). These tables
are operational state for this node only and MUST NOT be synchronized. All
sync-state database operations MUST follow structured data access patterns:

- Transactions MUST be used for multi-step mutations that require atomicity
  (e.g. adopting a round: cursor advance plus generation update).
- Connection lifecycle MUST be explicitly managed, with timeouts configured.
- Query parameters MUST use parameterized queries — string interpolation in
  SQL MUST NOT occur.

#### Scenario: Round adoption is atomic in sync state

- **WHEN** a replica finishes a round and persists the new manifest generation and per-file cursors
- **THEN** both are committed in a single transaction, and a crash between them cannot record the generation as complete with stale cursors.

## Security Requirements

This capability deliberately **amends**
[ADR-0010](../../../adr/0010-security-privacy-posture.md)'s loopback-only
posture (per [ADR-0018](../../../adr/0018-device-pairing-archive-sync.md)):
the loopback web UI keeps ADR-0010's loopback-trust model unchanged, while the
sync listener — the first socket beyond loopback — gets real authentication
via mutual TLS on pairing-pinned certificates.

### Authentication

All sync listener endpoints MUST require mutual TLS with an exact
pinned-certificate fingerprint match, except the single pairing endpoint,
which is token-gated because mutual trust cannot exist before pairing
completes. Sync listener endpoints (paths provisional):

| Method | Path | Auth | Justification / Description |
|--------|------|------|-----------------------------|
| POST | `/v1/pair` | Public (token-gated) | The only pre-trust endpoint: gated by a single-use token with TTL ≤ 10 min, rate-limited to 5 failures, served only while a pairing window is open, over TLS whose server cert the client verifies against the QR fingerprint. |
| GET | `/v1/manifest` | Required (mTLS, pinned peer) | Per-source manifest + generation. |
| GET | `/v1/file/{source}/{path}` | Required (mTLS, pinned peer) | Archive file bytes; path containment enforced as in [SPEC-0004](../web-ui/spec.md)'s traversal-safe media serving (REQ-0004-008). |
| POST | `/v1/notify` | Required (mTLS, pinned peer) | Advisory ingest-complete announcement; idempotent. |
| GET | `/v1/ping` | Required (mTLS, pinned peer) | Reachability probe for `doctor` and status. |
| POST | `/v1/unpair` | Required (mTLS, pinned peer) | Cooperative unpair; local revocation works without it. |

The pairing UI lives on the existing **loopback** web server, never on the
sync listener (routes provisional):

| Method | Path | Auth | Justification |
|--------|------|------|---------------|
| GET | `/settings` (devices section, QR display) | Public (loopback trust) | The web UI has no auth layer by design — loopback-only bind, single local user, per ADR-0010. The QR payload contains a pairing secret, which is exactly why this page MUST NOT be served beyond loopback. |
| POST | `/settings/devices/pairing/open` | Public (loopback trust) | Same ADR-0010 loopback posture; opening a window grants nothing without physical access to the displayed QR/manual code, and the window self-expires. |
| POST | `/settings/devices/pairing/close` | Public (loopback trust) | Same posture; closing is fail-safe (invalidates the token). |
| POST | `/settings/devices/{id}/unpair` | Public (loopback trust) | Same posture; destructive only to future sync, never to data. |

### Rate Limiting

The pairing endpoint MUST enforce the 5-consecutive-failure window closure and
token single-use/TTL rules from the Pairing Initiation requirement — this is
the only endpoint reachable without a pinned certificate, so its budget is
deliberately tiny and stateful. mTLS-authenticated endpoints have no
per-request rate limits in v1: only pinned LAN peers can reach them, and file
fetches are naturally bounded by disk and network. This deferral MUST be
revisited if bandwidth caps land (see design.md Open Questions).

### Security Headers

The sync protocol is a non-browser mTLS API (JSON and file bytes); browser
security headers — CSP, `X-Frame-Options`, `Referrer-Policy` — do not apply
and are intentionally not specified for it. Sync responses MUST still set
accurate `Content-Type` and `X-Content-Type-Options: nosniff` (cheap defense
in depth). The pairing page rides the existing loopback web UI and MUST
conform to ADR-0010's strict CSP: the QR image MUST be generated locally and
served same-origin (`img-src 'self' data:`), with no external QR service and
no new CSP carve-outs.

### Request Body Size Limits

The sync protocol has no archive upload endpoints — transfers are
replica-initiated GETs — so no unbounded request body exists by construction.
All request bodies (`/v1/pair`, `/v1/notify`, `/v1/unpair`) are small JSON and
MUST be bounded server-side (e.g. `http.MaxBytesReader`) at 64 KiB. Archive
file *responses* MUST be validated against the manifest-declared size and
hash on the receiving side, so an importer cannot stream unbounded bytes into
a replica's disk unnoticed.

### CSRF Protection

Not applicable to the sync API, stated explicitly: it uses no cookies and no
ambient credentials, and a browser cannot complete a mutual-TLS handshake with
a pinned client certificate, so cross-site request forgery has no vehicle.
The loopback settings forms (pairing open/close, unpair) are state-changing
and follow the existing web UI posture: same-origin `form-action` under
ADR-0010's CSP, loopback-only reachability as the trust boundary.

### Redirect Validation

The sync API MUST NOT emit redirects, and sync clients MUST treat any 3xx
response as a protocol error and abort the request. No endpoint accepts a
user-supplied redirect target, so no open-redirect surface exists.

### Replay Resistance

Pairing tokens MUST be consumed atomically on first presentation and compared
in constant time; a captured token is useless after the legitimate pairing
completes, and TTL bounds the exposure of an unused one. Post-pairing traffic
MUST use TLS 1.3, whose handshake freshness prevents replay of captured
sessions. The only non-idempotent unauthenticated action is pairing itself
(single-use token); `/v1/notify` is idempotent by requirement, so a replayed
notification is harmless.

## Accessibility Requirements

The pairing and status UI in settings is user-facing; the requirements below
are normative under WCAG 2.1 AA.

### WCAG 2.1 AA Compliance

All device-sync UI (devices section, pairing display, status region) MUST meet
WCAG 2.1 Level AA conformance.

### QR Code and Manual Code Fallback

The manual pairing code **is** the accessibility affordance: a QR scan MUST
NOT be the only pairing path. The manual code MUST be rendered as
selectable, copyable text with a copy control, and the QR image MUST carry
alt text that identifies it as a pairing code and directs users to the manual
code alternative (e.g. `alt="Device pairing QR code — a text pairing code is
provided below"`).

### ARIA Landmarks

The devices section MUST live within the settings page's existing landmark
structure (`role="main"` content area, `role="navigation"` for nav,
`role="banner"` header) so screen-reader users can navigate to it by landmark.

### Icon-Only Controls

All icon-only controls (copy code, regenerate/close pairing window, unpair)
MUST include an `aria-label` describing the action and its target device
(e.g. `aria-label="Unpair kitchen-server"`).

### Dynamic Content Regions

Sync status updates (pairing countdown, transfer progress, last-sync results)
MUST use `aria-live="polite"` regions; a pairing failure or window closure
MAY use `aria-live="assertive"`. The pairing countdown MUST NOT announce
every tick — coarse updates only (e.g. minute boundaries and expiry).

### Keyboard Navigation

All pairing and device controls MUST be keyboard-operable: logical tab order,
Enter/Space activation, and Escape to dismiss the pairing display if it is
presented as a dialog.

### Focus Management

If the pairing display is a modal dialog, focus MUST move into it on open, be
trapped while open, and return to the triggering control on close.
