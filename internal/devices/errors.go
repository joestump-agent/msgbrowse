// Governing: ADR-0021 (Syncthing owns identity, transport, and discovery),
// SPEC-0014 REQ "Error Handling Standards" — sentinel errors for every failure
// mode callers distinguish programmatically, structured context carried
// alongside, never swallowed.
//
// The SPEC-0011 sentinel family (token expiry/replay, pairing windows, pinned
// certificate fingerprints, hash-manifest transfer) was retired with the
// bespoke transport it described (SPEC-0014 REQ "Migration from SPEC-0011",
// issue #158). What remains is the role-conflict sentinel, which SPEC-0014
// carries forward unchanged from ADR-0018's importer/replica model.
package devices

import "errors"

// ErrImporterConflict is returned when this node is asked to act as the
// importer for a source that already has a different importer across the
// paired set — e.g. Enabling a source whose archive is synced in from a peer
// (SPEC-0014 REQ "Importer and Replica Roles": "an attempt to register a
// second importer for an already-claimed source MUST fail with a clear error
// naming the existing importer"). Callers wrap it with the source and the
// incumbent importer's name.
var ErrImporterConflict = errors.New("devices: importer already registered for source")
