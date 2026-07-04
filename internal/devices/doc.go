// Package devices holds the pairing-payload and identity-validation
// primitives for msgbrowse's multi-device archive synchronization under
// ADR-0021 (Syncthing as the sync engine, superseding ADR-0018).
//
// Syncthing owns identity (a device ID is the SHA-256 of a device's TLS
// certificate), transport (mutual TLS, device-ID pinned), and discovery; this
// package owns only the msgbrowse-side vocabulary layered on top:
//
//   - the version-2 pairing payload (payload_sync.go) — this node's Syncthing
//     device ID plus a managed-folder introduction, rendered as a QR and a
//     copyable manual code; PUBLIC data, never a secret;
//   - Syncthing device-ID validation and canonicalization (deviceid.go) —
//     input hygiene for every externally supplied identifier;
//   - the SyncPeer registry type persisted by internal/store's repurposed
//     paired_devices table, including the per-source importer/replica role a
//     peer plays (SPEC-0014 REQ "Importer and Replica Roles");
//   - the ErrImporterConflict sentinel (errors.go).
//
// The SPEC-0011 machinery this package used to carry — single-use pairing
// tokens with a bounded TTL, msgbrowse-issued self-signed TLS identities with
// fingerprint pinning, the version-1 token payload, and the mTLS LAN listener
// in internal/devices/listener — was REMOVED by the migration story (#158;
// SPEC-0014 REQ "Migration from SPEC-0011"): no msgbrowse-issued certificate
// is generated or pinned for device sync, and no pairing token exists to
// leak or replay.
//
// Everything here is pure Go over the standard library — no crypto beyond
// the Luhn check in device-ID validation, no network, CGO_ENABLED=0
// preserved (ADR-0013).
//
// # Naming
//
// The `sync` verb belongs to ADR-0015's export→import pipeline
// (`msgbrowse sync`), so device sync uses the **devices** namespace on every
// surface:
//
//   - Go packages: internal/devices (this vocabulary), internal/devsync (the
//     pairing manager + folder-watch worker), internal/syncthing (the
//     supervised engine)
//   - Config:      the `device_sync` block (internal/config)
//   - CLI:         `msgbrowse devices list|unpair|status`
//   - Web routes:  `/settings/devices/...`
//   - Schema:      `paired_devices` + `sync_state` tables (internal/store)
//
// Governing: ADR-0021, SPEC-0014 REQ "Pairing via Device ID and QR", REQ
// "Importer and Replica Roles", REQ "Migration from SPEC-0011", §Trust Model.
package devices
