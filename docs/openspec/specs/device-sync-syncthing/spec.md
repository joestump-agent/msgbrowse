---
status: draft
date: 2026-07-04
implements: [ADR-0021]
supersedes: [SPEC-0011]
requires: [SPEC-0001]
---

# SPEC-0014: Syncthing-based device sync

## Overview

Device sync makes one msgbrowse install's archives browsable on other machines
by synchronizing the **archives, not the database**
([ADR-0021](../../../adr/0021-syncthing-sync-engine.md)), using a **bundled,
supervised Syncthing** as the transfer engine. The node that runs the exporters
for a source is that source's **importer**; paired **replicas** receive the
archive files — messages *and* media — and run their own local
[SPEC-0001](../ingestion/spec.md) ingest. The SQLite database is derived state
and never enters a synced folder.

This supersedes [SPEC-0011](../device-sync/spec.md), which specified a bespoke
transfer engine (per-source hash manifests, resumable byte-range transfer,
staging with atomic adoption, notify/poll convergence, mDNS discovery) built on
msgbrowse-issued self-signed certificates pinned at pairing. Syncthing now
provides the transport, the mutual-TLS trust, and the discovery; msgbrowse owns
config generation, the pairing UX, the folder-watch → re-ingest trigger, and
doctor/status. Trust is Syncthing's **device ID** (the SHA-256 of a device's TLS
certificate), pinned by mutual TLS and accepted explicitly on both peers —
replacing SPEC-0011's single-use TTL tokens and msgbrowse-issued certificates.

Syncthing runs a network listener; that listener is the beyond-loopback surface,
but Syncthing **owns** the TLS and authentication for it. msgbrowse's own
control surface — the REST/GUI API it uses to drive Syncthing, and the
`/settings` pairing page — stays loopback. Device sync is disabled by default;
the loopback web UI posture
([ADR-0010](../../../adr/0010-security-privacy-posture.md)) is unchanged. Exact
config keys, flags, CLI verbs, and route spellings are provisional — see
design.md Open Questions.

## Requirements

### Requirement: Bundled Syncthing Runtime

The desktop `.app` MUST bundle the Syncthing binary under `Contents/Resources`,
mirroring the exporter-toolchain resolution pattern of
[ADR-0020](../../../adr/0020-bundled-exporters-guided-setup.md). msgbrowse MUST
resolve the Syncthing binary from the bundle and MUST NOT resolve it from
`$PATH` or any system-installed copy. The bundled binary MUST be version-pinned
and integrity-checked (its expected hash recorded at build time and verified
before first launch); a hash mismatch MUST fail device-sync startup with a clear
error rather than launching an unverified binary. Device sync MUST NOT require
the user to install Syncthing.

#### Scenario: Fresh Mac with no Syncthing installed still syncs

- **WHEN** device sync is enabled on a Mac that has never had Syncthing installed and has no Syncthing on `$PATH`
- **THEN** msgbrowse resolves and launches the bundled Syncthing from `Contents/Resources`, verifies its pinned hash, and sync proceeds without any user install step.

#### Scenario: Tampered bundled binary refuses to launch

- **WHEN** the bundled Syncthing binary's hash does not match the pinned build-time value
- **THEN** device-sync startup fails with a structured error naming the integrity check, and no Syncthing process is started.

### Requirement: Supervised Daemon Lifecycle

msgbrowse MUST run Syncthing as a managed child process, started only when
device sync is ENABLED. Device sync MUST be OFF by default
([ADR-0010](../../../adr/0010-security-privacy-posture.md) posture), so that a
default install runs no Syncthing process and opens no P2P listener. When
started, Syncthing's REST and GUI API MUST bind loopback with a
msgbrowse-generated API key. msgbrowse MUST stop the Syncthing child cleanly on
app quit and on device-sync disable, leaving no orphaned process, and MUST
restart it with backoff if it exits unexpectedly while sync remains enabled.

#### Scenario: Device sync disabled means no Syncthing process

- **WHEN** msgbrowse runs with device sync disabled (the default)
- **THEN** no Syncthing child process exists, no Syncthing REST/GUI API is bound, and no P2P sync listener is open.

#### Scenario: App quit stops the daemon

- **WHEN** the desktop app quits while device sync is enabled and Syncthing is running
- **THEN** msgbrowse signals the Syncthing child, waits for it to exit, and leaves no orphaned Syncthing process.

### Requirement: msgbrowse-Owned Config Generation

msgbrowse MUST generate and own Syncthing's configuration through its REST API:
folders MUST be the managed archive roots under
`<data_dir>/archives/<source>` (folder type and ignore patterns set so the DB,
WAL/SHM, and `data_dir` state never enter a synced folder), and devices MUST be
the paired peers. The user MUST NOT be required to edit Syncthing's config or
open its GUI. Enabling sync for a source MUST add exactly that source's archive
root as a Syncthing folder; enabling sync for a source MUST NOT add any folder
outside the managed archive roots.

#### Scenario: Enabling a source's sync adds exactly that archive folder

- **WHEN** the operator enables device sync for the Signal source
- **THEN** msgbrowse adds a Syncthing folder for `<data_dir>/archives/signal` and no other folder, via the REST API, without the user editing Syncthing config.

#### Scenario: The database is never in a synced folder

- **WHEN** msgbrowse generates the Syncthing folder configuration for any source
- **THEN** no configured folder path resolves inside `data_dir` (including the SQLite database and its WAL/SHM files), and ignore patterns exclude any non-archive state that shares a root.

### Requirement: Pairing via Device ID and QR

msgbrowse MUST present a pairing affordance that encodes **this node's Syncthing
device ID**, the archive folder id(s) to introduce, and a friendly device
introduction — as a QR code and as a copyable manual code carrying the same
fields. The payload MUST NOT contain a secret token; a Syncthing device ID is a
public identifier, and possessing it does not grant sync. On the pairing node,
msgbrowse MUST add the scanned peer as a Syncthing device and share the relevant
folders with it via the REST API. Syncthing's mutual-TLS device-ID trust MUST
govern the resulting connection, and both peers MUST have accepted the other's
device ID before any archive data flows.

#### Scenario: Scanning the QR on a second Mac pairs the devices

- **WHEN** the operator scans the pairing QR (or enters the manual code) on a second Mac
- **THEN** msgbrowse adds the first node's Syncthing device ID as a peer, shares the introduced archive folders, and once both peers have accepted each other the archives begin syncing.

#### Scenario: A device ID alone does not grant sync

- **WHEN** a third device obtains this node's device ID but is never accepted as a peer on this node
- **THEN** no folder is shared with it and no archive data is transferred to it, because reachability plus a public device ID is not acceptance.

### Requirement: Archive Sync Not Database Replication

Syncthing MUST synchronize only the archive **files** between nodes. Each node
MUST derive its database exclusively from its own local
[SPEC-0001](../ingestion/spec.md) ingest of its local archive tree, and the
database MUST NEVER be placed in a synced folder or transferred between nodes.
On replicas, synced archive roots MUST be treated as read-only by every
subsystem other than Syncthing's writer, preserving SPEC-0001's read-only
archive guarantee.

#### Scenario: Replica DB equals a fresh local ingest of the synced archives

- **WHEN** a replica finishes syncing an importer's archives and running its own ingest, and the same archive tree is independently ingested into a fresh database
- **THEN** both databases contain identical conversations, messages, attachments, links, and reactions for the synced sources, and no database file was ever copied between nodes.

#### Scenario: No database file enters a synced folder

- **WHEN** msgbrowse configures any Syncthing folder
- **THEN** the SQLite database, its WAL/SHM files, and everything under `data_dir` are excluded, so they cannot be synchronized to a peer.

### Requirement: Re-ingest Trigger

msgbrowse MUST detect when a synced archive folder has received new or changed
content — via Syncthing's REST/events API folder-completion signals, or via
`fsnotify` on the synced folder — and MUST run its existing incremental import
(`internal/onboardsvc`) so new messages appear without manual action. The
re-ingest MUST be incremental (only the delta), MUST be idempotent per
SPEC-0001, and MUST NOT run against a folder that Syncthing reports as
mid-transfer (partial state).

#### Scenario: Importer adds messages and the replica imports only the delta

- **WHEN** the importer node exports new messages, its archive files change, and Syncthing brings the replica's folder to completion
- **THEN** msgbrowse on the replica runs an incremental import that ingests only the new messages, and they appear in the replica's UI without any manual action.

#### Scenario: No re-ingest during an in-flight transfer

- **WHEN** Syncthing reports a synced folder as still transferring (incomplete)
- **THEN** msgbrowse does not trigger a re-ingest until the folder reaches completion, avoiding an import against a partial archive.

### Requirement: Importer and Replica Roles

Role MUST be per source: a node MUST act as importer only for sources whose
exporter-produced archive root it holds and can refresh from live data, and
there MUST be exactly one importer per source across a paired set. A replica
MUST receive and ingest a source's archives but MUST NOT run the exporters for
that source. An attempt to register a second importer for an already-claimed
source MUST fail with a clear error naming the existing importer.

#### Scenario: Single importer per source is enforced

- **WHEN** a peer attempts to become the importer for a source that already has a different registered importer
- **THEN** the registration fails with an error identifying the existing importer, and the peer remains a replica for that source.

### Requirement: Unpair and Revoke

Either node MUST be able to unpair a peer from the CLI and from the settings
page. Unpairing MUST remove the peer's Syncthing device and unshare the archive
folders from it via the REST API, so archive data stops flowing to that peer
immediately and locally, without requiring the peer to be reachable or
cooperative. Unpairing MUST NOT delete already-synced local archive files or the
local database; it severs only future synchronization.

#### Scenario: Unpair stops sync to that device immediately

- **WHEN** an operator unpairs a peer
- **THEN** msgbrowse removes that peer's Syncthing device and unshares the folders locally, archive data stops syncing to it at once, and the already-synced local archives and database remain intact.

### Requirement: Status and Doctor Surfacing

msgbrowse MUST surface Syncthing's state — connected peers, per-folder
completion percentage, and errors — from Syncthing's REST API into the
Settings/Logs/Status views and into `doctor`. The user MUST NOT need to open
Syncthing's own GUI to see sync health. `doctor` MUST report device-sync
condition, including: the supervised daemon is running when sync is enabled,
each paired peer's connection state, folder completion and staleness, and any
Syncthing-reported folder errors (paused, out-of-sync, permission).

#### Scenario: A paused or errored sync shows in msgbrowse's status

- **WHEN** Syncthing reports a folder as paused or in an error state
- **THEN** msgbrowse surfaces that condition in its own Settings/Status and flags it in `doctor` with a remediation hint, and the user never has to open Syncthing's GUI to discover it.

### Requirement: Migration from SPEC-0011

The bespoke pairing and transport code merged for
[SPEC-0011](../device-sync/spec.md) MUST be retired or repurposed as follows.
The pairing-token windows, the msgbrowse-issued self-signed identity, the
versioned pairing-token payload, and the mutual-TLS LAN listener
(`internal/devices` token/identity/pairing crypto and `internal/devices/listener`;
#104/#105) MUST be removed, because Syncthing owns identity, trust, transport,
and discovery. The `paired_devices` and `sync_state` schema tables MUST be
repurposed to store Syncthing device IDs and folder-to-source mappings rather
than pinned msgbrowse certificate fingerprints and byte-range transfer cursors.
The QR/manual pairing UX shape and the `/settings/devices` route surface MAY be
retained, with the payload changed from a token to a device ID. A migration MUST
leave a node that had SPEC-0011 sync-state rows in a coherent state (either
migrated to the Syncthing schema or cleared), with no dangling pinned
certificates.

#### Scenario: SPEC-0011 crypto and listener are removed

- **WHEN** SPEC-0014 is implemented
- **THEN** the msgbrowse-issued self-signed identity, the single-use token pairing windows, and the mTLS LAN listener no longer exist in the build, and no msgbrowse-issued certificate is generated or pinned for device sync.

#### Scenario: Schema tables carry Syncthing identifiers

- **WHEN** a peer is paired under SPEC-0014
- **THEN** its `paired_devices` row stores the peer's Syncthing device ID and the shared folder-to-source mapping, not a pinned msgbrowse certificate fingerprint.

### Requirement: Error Handling Standards

All error-producing device-sync operations MUST follow structured error
handling:

- Errors MUST be wrapped with contextual information at each layer boundary
  (e.g. `device sync start failed: verify bundled syncthing: hash mismatch`;
  `share folder signal with peer <id>: syncthing REST 409`).
- Sentinel errors MUST be defined for failure modes callers distinguish
  programmatically (Syncthing binary missing from bundle, integrity mismatch,
  daemon not running, REST client auth failure, unknown peer, importer
  conflict).
- Silent error swallowing MUST NOT occur — every error from the supervisor, the
  REST client, and the folder-watch → re-ingest worker MUST be returned, logged
  with sufficient context, or explicitly handled with a documented reason for
  suppression.
- Structured logging MUST be used for error reporting (key-value pairs, not
  string interpolation), and Syncthing's own stderr/stdout MUST be captured into
  msgbrowse's structured logs rather than discarded.

#### Scenario: REST failure is attributable and surfaced

- **WHEN** a Syncthing REST call to share a folder fails
- **THEN** the error is wrapped with the operation, folder, and peer, logged as structured key-value pairs, surfaced in Settings/Logs, and never silently dropped.

### Requirement: Concurrency Safety

The supervised Syncthing daemon and the folder-watch → re-ingest worker MUST
follow safe concurrency patterns:

- Context propagation MUST be used for cancellation and timeout across the
  daemon supervisor, the REST/events client, and the re-ingest worker.
- Worker lifecycle MUST be explicitly managed — the supervisor, the
  event/watch loop, and the re-ingest worker MUST have clean startup and
  graceful shutdown sequences, and app quit MUST terminate the Syncthing child
  with no orphan process and no leaked goroutine.
- Concurrent re-ingest for the same source MUST be serialized so two overlapping
  folder-completion events cannot run two imports against the same archive root
  at once.
- Shared mutable state (peer registry, folder mappings, daemon status cache)
  MUST be protected by synchronization primitives or eliminated via message
  passing, and concurrent tests MUST run with race detection enabled in CI.

#### Scenario: Graceful shutdown terminates the child

- **WHEN** the process receives a shutdown signal while Syncthing is running and a re-ingest is in flight
- **THEN** the re-ingest cancels via context, the supervisor terminates the Syncthing child and waits for its exit, and the process exits without panics, orphaned Syncthing processes, or leaked workers.

#### Scenario: Overlapping folder events do not double-import

- **WHEN** two folder-completion events for the same source arrive in quick succession
- **THEN** the re-ingest worker serializes them so only one import runs against that archive root at a time, and the second is coalesced or queued rather than run concurrently.

## Security Requirements

This capability adds a P2P network listener, but Syncthing **owns** its TLS and
authentication. This **amends**
[ADR-0010](../../../adr/0010-security-privacy-posture.md)'s loopback-only
posture the same way [ADR-0018](../../../adr/0018-device-pairing-archive-sync.md)
did — no more: the loopback web UI keeps its loopback-trust model, and the new
beyond-loopback surface is Syncthing's, LAN-scoped, off by default, and adds no
cloud egress.

### Trust Model

Peer authentication MUST be Syncthing's **device ID = mutual TLS**: each device
has a long-lived TLS certificate whose SHA-256 is its device ID, every
connection is mutual TLS with the peer's device ID pinned, and a peer MUST be
explicitly accepted on both ends before any folder is shared. msgbrowse MUST NOT
issue or pin its own certificates for device sync; the SPEC-0011 self-signed
identity and token model are removed (see Migration from SPEC-0011). The pairing
QR/manual code MUST carry a **device ID and folder introduction, not a secret
token** — a device ID is public, and reachability plus a device ID grants
nothing; the peer must still be accepted. Losing or sharing a device ID does not
compromise archives, because acceptance, not knowledge of the ID, gates sync.

### Authentication

Syncthing's REST and GUI API — msgbrowse's only control channel to the daemon —
MUST bind loopback and MUST require a msgbrowse-generated API key. msgbrowse's
own pairing routes MUST stay loopback per
[ADR-0010](../../../adr/0010-security-privacy-posture.md). The auth-by-default
table (routes and control endpoints provisional):

| Surface | Bind | Auth | Justification / Description |
|---------|------|------|-----------------------------|
| Syncthing P2P sync listener | LAN (Syncthing-owned) | Mutual TLS, device-ID pinned, both-ends acceptance | Syncthing owns this listener's TLS and authentication; a peer must be accepted before any folder is shared. |
| Syncthing REST/GUI API | Loopback | msgbrowse-generated API key | The only channel msgbrowse uses to configure and query Syncthing; never bound beyond loopback, never exposed to the user's browser. |
| `GET /settings` (devices section, QR display) | Loopback | Public (loopback trust) | The web UI has no auth layer by design ([ADR-0010](../../../adr/0010-security-privacy-posture.md)); the QR carries a public device ID, not a secret, but the page still stays loopback-only. |
| `POST /settings/devices/pair` | Loopback | Public (loopback trust) | Adds a peer device + shares folders via the REST API; loopback reachability is the trust boundary. |
| `POST /settings/devices/{id}/unpair` | Loopback | Public (loopback trust) | Removes the peer's Syncthing device + unshares folders; destructive only to future sync, never to data. |

### Relay and Discovery Posture

Syncthing's global relay and global discovery reach the public internet, which
touches egress against
[ADR-0010](../../../adr/0010-security-privacy-posture.md). msgbrowse MUST
configure Syncthing to default to **LAN + local discovery only**, with global
discovery and relaying **OFF**, so no archive metadata or bytes leave the LAN by
default. Enabling global discovery or relaying (for cross-network sync) MUST be
an explicit, owner-gated opt-in, documented as internet egress, never the
default.

#### Scenario: Default posture stays on the LAN

- **WHEN** device sync is enabled with default settings
- **THEN** Syncthing is configured with global discovery and relaying disabled, and no sync-related connection is made to any Syncthing relay or global discovery server.

### Transfer Bounds

Archive transfer sizing (large media, incremental block transfer, resumption)
MUST be handled by Syncthing, which is designed for exactly this; msgbrowse MUST
NOT re-implement byte-range transfer or unbounded upload endpoints. msgbrowse's
own REST calls to Syncthing carry only small JSON control payloads and MUST be
bounded on the msgbrowse side.

### CSRF and Redirect

The loopback settings forms (pair, unpair) are state-changing and follow the
existing web UI posture: same-origin `form-action` under
[ADR-0010](../../../adr/0010-security-privacy-posture.md)'s CSP, loopback-only
reachability as the trust boundary. msgbrowse's Syncthing REST client MUST treat
unexpected redirects from the daemon's API as protocol errors.

## Accessibility Requirements

The pairing and sync-status UI in settings is user-facing; the requirements
below are normative under WCAG 2.1 AA.

### WCAG 2.1 AA Compliance

All device-sync UI (devices section, pairing display, status region) MUST meet
WCAG 2.1 Level AA conformance and MUST live within the settings page's existing
landmark structure (`role="main"` content, `role="navigation"`, `role="banner"`)
so screen-reader users can reach it by landmark.

### QR Code and Manual Device-ID Fallback

The manual device-ID code **is** the accessibility affordance: a QR scan MUST
NOT be the only pairing path. The device ID MUST be rendered as selectable,
copyable text with a copy control, and the QR image MUST carry alt text that
identifies it as a device-pairing code and directs users to the manual code
alternative (e.g. `alt="Device pairing QR code — a text device-ID code is
provided below"`).

### Dynamic Content Regions

Sync-progress and status updates (folder completion, peer connection state,
last-sync results) MUST use `aria-live="polite"` regions; a pairing failure or a
folder error MAY use `aria-live="assertive"`. Progress announcements MUST be
coarse (not every percent tick) to avoid flooding assistive technology.

### Icon-Only Controls and Keyboard Navigation

All icon-only controls (copy device ID, unpair) MUST include an `aria-label`
describing the action and its target device (e.g. `aria-label="Unpair
kitchen-server"`). All pairing and device controls MUST be keyboard-operable
with logical tab order and Enter/Space activation; if the pairing display is a
modal dialog, focus MUST move into it on open, be trapped while open, and return
to the triggering control on close.
