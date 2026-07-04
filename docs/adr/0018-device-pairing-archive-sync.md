# ADR-0018: Multi-device via QR pairing and archive synchronization

- **Status:** Superseded by [ADR-0021](0021-syncthing-sync-engine.md)
- **Date:** 2026-07-03
- **Deciders:** Joe Stump
- **Related:** [ADR-0010 (security & privacy posture — **amended** by this ADR)](0010-security-privacy-posture.md), [ADR-0015 (onboarding: doctor/export/sync)](0015-onboarding-doctor-export-sync.md), [ADR-0003 (dual-source archive)](0003-dual-source-archive.md), [ADR-0013 (pure-Go SQLite)](0013-pure-go-sqlite-driver.md), [ADR-0017 (desktop shell)](0017-desktop-shell-wails.md)

## Context and Problem Statement

msgbrowse lives on one machine — the Mac, because only the Mac can run the
exporters: signal-export needs Signal Desktop's key, imessage-exporter needs
Full Disk Access to `~/Library/Messages`, and WhatsApp-Chat-Exporter needs the
phone backup (ADR-0015, ADR-0016). The owner wants the same archives browsable
on other machines on the LAN — a home server, a desktop upstairs — without a
cloud service in the loop.

Everything msgbrowse serves today binds loopback and trusts the local user
(ADR-0010). A second device means the first listener that ever accepts a
connection from beyond loopback, so the threat model is not a section of this
feature — it *is* the feature.

## Decision Drivers

- **Local-only ethos.** No cloud relay, ever; ADR-0010's single-auditable-egress
  posture must survive this feature intact.
- **Media must arrive with the messages.** A database-only copy gives text with
  broken images — the iMessage absolute-path saga (ADR-0015's copy-mode trap)
  proved exactly how that feels. Whatever syncs must carry the media files.
- **Archives are append-only and ingest is idempotent** (SPEC-0001). "Copy the
  files, re-ingest" is already the system's native update primitive; sync just
  needs to move files correctly.
- **Single-writer-per-source reality.** Only the Mac runs the exporters, so
  every source has exactly one producer and N consumers. There are no
  conflicting writes to reconcile — a topology fact worth exploiting, not
  engineering around.

## Considered Options

1. **Archive file sync + local re-ingest per node** — peers exchange the
   read-only archive trees (hash manifest, resumable file transfer, verified
   adoption); every node runs its own SPEC-0001 ingest into its own derived DB.
2. **SQLite replication** — Litestream/LiteFS stream one writer's WAL to read
   replicas: single-writer streaming is the wrong topology for peer devices,
   and the media files never travel. cr-sqlite adds CRDT merge for
   multi-writer databases: heavy machinery solving conflicts we structurally
   do not have.
3. **Cloud relay / hosted E2EE sync service** — violates local-only outright
   (ADR-0010); the entire message history would transit third-party
   infrastructure to move between two machines in the same house.
4. **Depend on Syncthing externally** — a real option, rejected as the primary
   answer: no pairing UX integration, no msgbrowse-aware verification or
   doctor coverage, and a second daemon to install and babysit. Nothing stops
   a user from pointing Syncthing at their archive roots manually today; it
   just isn't the product.

## Decision Outcome

**Option 1: synchronize the archives, not the database.** The DB stays what it
has always been — derived, disposable state (SPEC-0001); the archives stay the
source of truth and become the unit of replication.

- **Roles.** The node that runs the exporters for a source is that source's
  **importer**; every other paired node is a **replica** that receives the
  archive files and ingests them locally.
- **Pairing.** The importer displays a QR code (plus a copyable manual code)
  on the shared Connect/Settings page (ADR-0017 / SPEC-0010 render it; this
  ADR owns the payload), whose payload is `{endpoint, single-use pairing
  token with TTL, TLS cert fingerprint}`.
- **Transport.** Each node generates a long-lived self-signed certificate;
  fingerprints are pinned at pairing and every subsequent connection is mutual
  TLS with exact pinned-cert matching — real authentication, the deliberate
  exception to the app's no-auth loopback posture.
- **Discovery.** mDNS/DNS-SD on the LAN, with the QR-embedded literal endpoint
  as the path that always works (mDNS does not cross VLANs reliably).
- **Posture.** This decision **amends ADR-0010**: the sync listener is the
  first socket that accepts connections beyond loopback, and it does so with
  real authentication rather than loopback trust. It is OFF by default and
  strictly opt-in; the loopback web UI bind is unchanged.
- **Sync.** File-level with per-source hash manifests: diff, resumable fetch,
  verify, stage, atomic rename — then each node ingests locally and
  idempotently.

Requirements: [SPEC-0011 (device sync)](../openspec/specs/device-sync/spec.md).

### Consequences

- Good: media, reactions, and every future archive artifact sync for free —
  the transfer layer moves opaque verified files and never learns the schema.
- Good: no conflict resolution, no merge logic, no CRDT dependency; correctness
  reduces to "same bytes on disk" plus SPEC-0001's already-tested idempotence.
- Good: the DB never crosses the wire, so schema migrations never have to
  coordinate across devices; each node migrates its own derived state.
- Bad: full archive duplication per node — disk cost scales with media, and
  the first sync of a large archive is a long transfer (resumability is
  mandatory, not nice-to-have).
- Bad: msgbrowse now owns a crypto/identity surface — cert generation,
  pinning, revocation, and a token-gated pairing window — that must be built,
  tested, and doctor-checked to the standard the rest of ADR-0010 sets.
- Neutral: LLM-derived state (embeddings, facts, journals) is computed per
  node and may differ by local model config; derived state was never promised
  to be identical, only the archives are.
- Neutral: users who prefer Syncthing can still sync archive roots manually;
  msgbrowse-native sync adds pairing UX, verification, and doctor coverage on
  top of the same file-level idea.
