// Package devices is the pairing core for msgbrowse's multi-device archive
// synchronization.
//
// Under ADR-0021 (Syncthing as the sync engine, superseding ADR-0018) the
// LIVE surface of this package is the device-ID era material: the version-2
// pairing payload (payload_sync.go — this node's Syncthing device ID +
// folder introduction as QR/manual code), Syncthing device-ID validation
// (deviceid.go), and the SyncPeer registry type persisted by
// internal/store's repurposed paired_devices table.
//
// The remainder — single-use pairing tokens with a bounded TTL, long-lived
// self-signed TLS identities with SHA-256 fingerprint pinning, the version-1
// token payload, and the transport-agnostic pairing exchange mounted by
// internal/devices/listener — is the RETIRED SPEC-0011 machinery, no longer
// reachable from any command or route, awaiting wholesale removal by the
// migration story (#158; SPEC-0014 REQ "Migration from SPEC-0011").
//
// This package deliberately contains NO network listener — that lives in
// internal/devices/listener. Every primitive here is exercised over
// in-memory transports (net.Pipe TLS conns, custom http.Transport dialers)
// so the trust machinery is proven independently of any socket. It uses only
// the standard library's crypto/tls and crypto/x509 — no new dependencies,
// CGO_ENABLED=0 preserved (ADR-0013).
//
// # Naming
//
// The `sync` verb belongs to ADR-0015's export→import pipeline
// (`msgbrowse sync`), so this feature adopts the **devices** namespace on
// every surface (resolving SPEC-0011 design.md's naming open question):
//
//   - Go package:  internal/devices (this package)
//   - Config:      the `device_sync` block (internal/config)
//   - CLI:         `msgbrowse devices pair|list|unpair|status` (later stories)
//   - Web routes:  `/settings/devices/...` (later stories)
//   - Schema:      `paired_devices` + `sync_state` tables (internal/store)
//
// Governing: ADR-0018 (multi-device via QR pairing and archive sync),
// SPEC-0011 REQ "Pairing Initiation", REQ "Pairing Acceptance and Mutual
// Certificate Pinning", REQ "Error Handling Standards".
package devices
