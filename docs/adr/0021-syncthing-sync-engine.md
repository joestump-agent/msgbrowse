# ADR-0021: Adopt Syncthing as the device-sync transfer engine (supersedes ADR-0018)

- **Status:** Accepted
- **Date:** 2026-07-04
- **Deciders:** Joe Stump
- **Related:**
  - **Supersedes** [ADR-0018 (multi-device via QR pairing and archive sync)](0018-device-pairing-archive-sync.md) — this ADR reverses ADR-0018's "build bespoke pairing + mTLS + resumable transfer" decision while preserving its two structural invariants (archive-not-DB, importer/replica roles).
  - [ADR-0020 (bundled exporters + guided setup)](0020-bundled-exporters-guided-setup.md) — the bundling precedent: Syncthing ships in `Contents/Resources` exactly like the exporter toolchain, and inherits the same signing/notarization obligation.
  - [ADR-0010 (security & privacy posture)](0010-security-privacy-posture.md) — **amends** the loopback-only framing the same way ADR-0018 did: a P2P listener appears, but it is Syncthing's, LAN-scoped, off by default, and adds no cloud egress.
  - [ADR-0017 (desktop shell via Wails v2)](0017-desktop-shell-wails.md) — the `.app` that bundles and supervises Syncthing, and whose signing pipeline now covers a third-party daemon.

## Context and Problem Statement

[ADR-0018](0018-device-pairing-archive-sync.md) decided to **build** msgbrowse's
own multi-device transfer engine: QR/manual pairing with single-use TTL tokens,
long-lived self-signed certificates pinned at pairing, mutual TLS on every
connection, per-source hash manifests, resumable byte-range file transfer,
staging with atomic adoption, mDNS discovery, and a poll/notify convergence
loop. [SPEC-0011](../openspec/specs/device-sync/spec.md) specified all of it.

Two of the early, self-contained pieces shipped and merged: the pairing and
trust core (`internal/devices` — token windows, self-signed identity, the
versioned pairing payload, the pairing exchange; #104) and an mTLS LAN listener
that mounts the pairing handler on a real socket (`internal/devices/listener`;
#105). What had **not** been built was the hard, bug-prone majority of the
feature: the manifest/diff engine, resumable transfer with staging and atomic
adoption, bootstrap resume across restarts, the notify/poll convergence loop,
mDNS discovery, and certificate rotation (#106/#107). Those are precisely the
subsystems where a hand-rolled local-first sync engine accrues years of
edge-case bugs — partial transfers, NAT/VLAN reachability, conflict handling,
large media, clock skew, cert expiry.

Before committing to build #106/#107, the owner asked to reconsider a mature,
audited, local-first P2P sync engine — Syncthing — as the transfer layer, with
msgbrowse keeping only the parts that are genuinely its own: the pairing UX, the
archive layout, the re-ingest trigger, and doctor/status. ADR-0018 had
considered Syncthing and rejected it as the *externally-installed, unintegrated*
option; this ADR reconsiders it as a *bundled, supervised, msgbrowse-driven*
component — the same posture ADR-0020 established for the exporters.

## Decision Drivers

- **Do not reinvent a mature local-first P2P sync engine.** Resumable transfer,
  LAN and NAT reachability, conflict handling, and large-media sync are solved
  problems in Syncthing; #106/#107 would re-solve them, less well, and we would
  own every bug.
- **Syncthing's device-ID pinning IS the threat model we were going to build.**
  A Syncthing device ID is the SHA-256 of the device's TLS certificate; every
  connection is mutual TLS with that ID pinned, and a peer must be *explicitly
  accepted* on both ends. This is exactly the pinned-self-signed-cert model of
  SPEC-0011 — already implemented, hardened, and audited.
- **Fits the bundling precedent (ADR-0020).** Syncthing is a single Go binary,
  MPL-2.0 licensed; it bundles under `Contents/Resources` like the exporters,
  runs locally, and adds no interpreter or foreign toolchain.
- **No cloud relay by default (ADR-0010).** Syncthing can run LAN-only with
  local discovery and global relay/discovery disabled, keeping the
  single-auditable-egress posture intact.
- **Less code to own, test, and doctor-check.** The security-critical surface
  (crypto, transfer resumption, discovery) moves to an audited upstream; what
  remains is config generation, a REST client, and a folder-watch worker.

## Considered Options

### (a) Build bespoke sync per ADR-0018 / SPEC-0011

Finish #106/#107: our own manifest/diff, resumable byte-range transfer, staging
and atomic adoption, bootstrap resume, notify/poll convergence, mDNS discovery,
and cert rotation, on top of the merged pairing core and mTLS listener.
**Rejected now.** It reinvents a mature, audited P2P transfer engine; it is the
largest and most bug-prone slice of the whole epic (resumable secure transfer,
reachability across VLANs and NATs, conflict and large-media handling); and it
leaves msgbrowse permanently owning and maintaining that surface. The merged
pairing/mTLS work is real, but it is the *easy* third — finishing the hard two
thirds is the cost this option asks us to pay when a component already pays it.

### (b) Bundle Syncthing, supervise it, drive it via its REST API (CHOSEN)

The `.app` bundles the Syncthing binary under `Contents/Resources`
([ADR-0020](0020-bundled-exporters-guided-setup.md) pattern). msgbrowse
supervises the daemon as a managed child, generates its entire config (folders =
the app-owned archive roots under `<data_dir>/archives/<source>`; devices =
paired peers), drives it through its loopback REST API (API-key authenticated),
and watches synced folders to trigger incremental re-ingest. The user never
edits Syncthing config or sees its GUI; msgbrowse owns the pairing UX (a QR of
this node's Syncthing device ID plus a folder introduction), the folder-watch →
re-ingest trigger, and doctor/status surfaced from Syncthing's REST API.
**Chosen** — see Decision Outcome.

### (c) Depend on a user-installed Syncthing

Require the user to install and run Syncthing themselves and point it at the
archive roots (ADR-0018's original "external Syncthing" option). **Rejected.**
It breaks the zero-config consumer app ([ADR-0020](0020-bundled-exporters-guided-setup.md)):
a `.app` that needs a separately installed daemon, hand-configured folders, and
a second GUI to babysit is exactly the friction the desktop product exists to
remove. Syncthing must be *bundled* like the exporters, not assumed.

### (d) Cloud / hosted E2EE relay sync service

Route archives through a third-party sync/relay service. **Rejected outright**,
as in ADR-0018: it violates local-only ([ADR-0010](0010-security-privacy-posture.md))
— the entire message history would transit third-party infrastructure to move
between two machines in the same house.

## Decision Outcome

**Chosen: option (b) — bundle and supervise Syncthing as the device-sync
transfer engine.** This ADR **supersedes [ADR-0018](0018-device-pairing-archive-sync.md)**:
it reverses ADR-0018's "build a bespoke pairing + mTLS + resumable-transfer
engine" decision and replaces that transport with a bundled, supervised
Syncthing, while carrying ADR-0018's two structural invariants forward unchanged.
msgbrowse retains ownership of everything that is genuinely its own and delegates
the transport to an audited component:

- **Bundle + supervise.** The `.app` ships the version-pinned, integrity-checked
  Syncthing binary in `Contents/Resources`, resolved from the bundle (never
  `$PATH`). msgbrowse starts and stops it as a managed child, bound to loopback
  for its REST/GUI API with a generated API key, started only when device sync
  is enabled (off by default per [ADR-0010](0010-security-privacy-posture.md)).
- **msgbrowse owns config generation.** It writes Syncthing's folders (the
  managed archive roots) and devices (paired peers) via the REST API; the user
  never edits Syncthing config or opens its GUI.
- **Pairing is a device-ID QR.** The QR/manual code carries this node's
  Syncthing device ID plus the archive folder introduction — a device ID and
  folder id, **not a secret token**. Syncthing's mutual-TLS device-ID trust
  governs the connection; a scanned ID grants nothing until the peer is
  accepted on both ends.
- **Re-ingest trigger.** msgbrowse watches for folder completion (Syncthing's
  REST/events API, or `fsnotify` on the synced folder) and runs the existing
  incremental import (`internal/onboardsvc`) so new messages appear without
  manual action.
- **Doctor/status.** msgbrowse surfaces Syncthing state (connected peers, folder
  completion, errors) from its REST API into Settings/Logs/Status and `doctor`;
  the user never sees Syncthing's own UI.

**The two ADR-0018 invariants are PRESERVED:**

1. **Archive-sync, not DB-replication.** Syncthing syncs the archive *files*;
   each node runs its own local [SPEC-0001](../openspec/specs/ingestion/spec.md)
   ingest into its own SQLite. The database is never placed in a synced folder
   and never crosses the wire.
2. **Importer/replica roles.** Only a node that can run the exporters imports
   from live sources; replicas receive archives and ingest them locally. A
   source has exactly one importer.

**Retire or repurpose the merged work.** Syncthing's device-ID + mutual-TLS
trust model *replaces* SPEC-0011's bespoke pairing tokens and self-signed cert
pinning. The `internal/devices` pairing-token/identity crypto and the
`internal/devices/listener` mTLS listener (#104/#105) are removed; the
QR/pairing UX *shape* and the `paired_devices` / `sync_state` schema tables are
repurposed to carry Syncthing device IDs and folder mappings.
[SPEC-0014](../openspec/specs/device-sync-syncthing/spec.md) records what
survives versus what is removed.

Requirements: [SPEC-0014 (Syncthing-based device sync)](../openspec/specs/device-sync-syncthing/spec.md),
which supersedes [SPEC-0011](../openspec/specs/device-sync/spec.md).

### Consequences

#### Good

- **Drops the hardest greenfield build.** The manifest/diff engine, resumable
  transfer, staging/adoption, bootstrap resume, discovery, and cert rotation
  (#106/#107) do not get built — an audited component provides them.
- **Robust, audited security.** Device-ID mutual TLS, transfer integrity, and
  discovery come from a mature project, not from code msgbrowse must harden and
  keep hardened.
- **Reachability beyond our LAN-only scope.** Syncthing handles NAT traversal
  and (optionally, owner-gated) relays — capabilities SPEC-0011 explicitly
  deferred — while defaulting to LAN + local discovery.
- **Cross-platform transfer engine.** Syncthing runs on macOS, Linux, and
  Windows, so the transport is not the thing blocking a future non-macOS
  replica; only the `.app` bundling is macOS-gated.

#### Bad

- **Another bundled binary to sign and notarize.** Syncthing joins the exporter
  toolchain under `Contents/Resources`, so it inherits ADR-0020's
  signing/notarization requirement — a Gatekeeper-relevant third-party
  executable in the signed app.
- **Another supervised daemon.** msgbrowse now owns a child process lifecycle:
  clean startup, graceful shutdown on app quit, no orphaned Syncthing process,
  and restart/backoff on crash.
- **Diagnostics must wrap a foreign API.** "The user never sees Syncthing's UI"
  means msgbrowse must translate Syncthing's REST/events model into its own
  status, logs, and `doctor` checks — a mapping layer to build and maintain.
- **~2 merged stories retired = sunk cost.** The #104 pairing/identity crypto and
  the #105 mTLS listener are removed; only the UX shape and schema tables carry
  forward.
- **Long-lived device IDs replace TTL tokens — a trust-model change.** SPEC-0011's
  single-use ≤10-minute pairing tokens become long-lived Syncthing device IDs
  that both peers must accept; reachability alone never grants sync, but the
  identity is durable rather than time-boxed, which must be documented.
- **Version-pinning + security-update cadence.** We now track Syncthing releases
  for security fixes and re-bundle + re-notarize to ship them, as we already do
  for the exporters (ADR-0020).

#### Neutral

- **Windows/Linux bundling stays owner-gated**, exactly as with the `.app`
  exporter bundle ([ADR-0020](0020-bundled-exporters-guided-setup.md)); the
  Syncthing transfer engine is cross-platform even though v1 bundles only the
  macOS `.app`.
- **The `/settings` pairing page stays loopback.** It now displays a device ID
  instead of a token payload, but it rides the same loopback web UI under
  [ADR-0010](0010-security-privacy-posture.md)'s CSP and trust model.
- **msgbrowse still adds a P2P listener** — but it is Syncthing's, LAN-scoped
  with global discovery/relay off by default, and adds no cloud egress; this
  amends [ADR-0010](0010-security-privacy-posture.md) the same way ADR-0018
  did, no more.
