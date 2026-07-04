// The version-2 pairing payload: the #104 QR/manual-code UX shape repurposed
// for Syncthing device-ID pairing. Where version 1 carried a live secret
// (single-use token + pinned certificate fingerprint) for the retired bespoke
// mTLS transport, version 2 carries only PUBLIC introduction data: this
// node's Syncthing device ID, the archive folder id(s) it offers, and a
// friendly device name. Possessing the payload grants nothing — Syncthing's
// mutual-TLS device-ID trust requires both peers to have accepted each
// other's device before any folder syncs (SPEC-0014 "A device ID alone does
// not grant sync").
//
// Two presentations, identical fields, mirroring the v1 shape: the QR bytes
// are the compact JSON itself, and the manual code is "MSGB2." +
// base64url(JSON) — copy/paste-safe, self-identifying, and the accessibility
// path when a camera is unavailable.
//
// Governing: ADR-0021 ("pairing is a device-ID QR … a device ID and folder
// id, not a secret token"), SPEC-0014 REQ "Pairing via Device ID and QR",
// REQ "Migration from SPEC-0011" (the QR/manual UX shape is retained with
// the payload changed from a token to a device ID).
package devices

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SyncPayloadVersion is the device-ID pairing payload protocol version.
// Version 1 was the SPEC-0011 token payload (retired by ADR-0021); a decoder
// here rejects anything but 2.
const SyncPayloadVersion = 2

// SyncManualCodePrefix prefixes the copyable manual pairing code so a pasted
// string is self-identifying ("MSGB2." + base64url payload JSON, no padding).
const SyncManualCodePrefix = "MSGB2."

// ErrInvalidSyncPayload reports pairing input that is not a valid v2 payload:
// not decodable, wrong version, malformed device ID, or out-of-range fields.
var ErrInvalidSyncPayload = errors.New("devices: invalid device-pairing payload")

// ErrSelfPair reports an attempt to pair a node with its own device ID —
// scanning one's own QR rather than the other device's.
var ErrSelfPair = errors.New("devices: cannot pair a device with itself")

// maxSyncFolders bounds the folder introductions one payload may carry; the
// managed folder set is one per source, so a handful is generous headroom.
const maxSyncFolders = 16

// maxSyncFolderIDLen bounds one folder id ("msgbrowse-<source>" today).
const maxSyncFolderIDLen = 64

// maxSyncNameLen bounds the friendly device name, long enough for any real
// hostname while keeping the payload QR-sized and the UI unstuffable.
const maxSyncNameLen = 128

// SyncPayload is the exact wire schema of the version-2 pairing payload —
// the single source of truth for the /settings QR (encoding side) and the
// pair form / CLI paste (decoding side). Compact JSON with these fields and
// no others:
//
//	{
//	  "v":        2,                       // SyncPayloadVersion (required)
//	  "deviceID": "P56IOI7-MZJNU2Y-…",     // this node's Syncthing device ID,
//	                                       // canonical 8×7 dashed form (required)
//	  "folders":  ["msgbrowse-signal"],    // archive folder ids introduced (optional)
//	  "name":     "studio-mac"             // friendly device name (optional)
//	}
//
// Every field is public: the payload is an introduction, never a credential.
type SyncPayload struct {
	// Version is the payload protocol version; always SyncPayloadVersion.
	Version int `json:"v"`
	// DeviceID is the presenting node's Syncthing device ID in canonical
	// form. The scanning node adds it as a peer and shares folders with it.
	DeviceID string `json:"deviceID"`
	// Folders are the deterministic managed archive folder ids
	// (syncthing.FolderIDPrefix + source) the presenting node offers.
	Folders []string `json:"folders,omitempty"`
	// Name is the presenting node's friendly device name, shown in the peer
	// registry and Syncthing config.
	Name string `json:"name,omitempty"`
}

// NewSyncPayload assembles and validates a v2 payload, canonicalizing the
// device ID.
func NewSyncPayload(deviceID string, folders []string, name string) (*SyncPayload, error) {
	p := &SyncPayload{
		Version:  SyncPayloadVersion,
		DeviceID: deviceID,
		Folders:  folders,
		Name:     name,
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// validate checks the invariants every encode and decode path enforces, and
// canonicalizes the device ID in place.
func (p *SyncPayload) validate() error {
	if p.Version != SyncPayloadVersion {
		return fmt.Errorf("%w: unsupported version %d (want %d)", ErrInvalidSyncPayload, p.Version, SyncPayloadVersion)
	}
	id, err := CanonicalDeviceID(p.DeviceID)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSyncPayload, err)
	}
	p.DeviceID = id
	if len(p.Folders) > maxSyncFolders {
		return fmt.Errorf("%w: %d folder ids (max %d)", ErrInvalidSyncPayload, len(p.Folders), maxSyncFolders)
	}
	for _, f := range p.Folders {
		if err := validateSyncFolderID(f); err != nil {
			return err
		}
	}
	if len(p.Name) > maxSyncNameLen {
		return fmt.Errorf("%w: device name longer than %d bytes", ErrInvalidSyncPayload, maxSyncNameLen)
	}
	return nil
}

// validateSyncFolderID enforces the deterministic managed folder-id shape:
// non-empty lowercase [a-z0-9-], bounded length. The pairing step still
// intersects introduced ids with the locally managed set — this is input
// hygiene, not authorization.
func validateSyncFolderID(id string) error {
	if id == "" || len(id) > maxSyncFolderIDLen {
		return fmt.Errorf("%w: folder id %q length out of range", ErrInvalidSyncPayload, id)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return fmt.Errorf("%w: folder id %q has invalid character %q", ErrInvalidSyncPayload, id, c)
		}
	}
	return nil
}

// EncodeQR returns the bytes the QR renderer encodes: the payload's compact
// JSON. The image is generated server-side by the settings page, exactly like
// the v1 shape (no external QR service; `img-src 'self' data:`).
func (p *SyncPayload) EncodeQR() ([]byte, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(p)
}

// EncodeManualCode returns the copyable manual pairing code carrying exactly
// the same fields as the QR: SyncManualCodePrefix + base64url (no padding) of
// the compact JSON.
func (p *SyncPayload) EncodeManualCode() (string, error) {
	raw, err := p.EncodeQR()
	if err != nil {
		return "", err
	}
	return SyncManualCodePrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// DecodeSyncPayload parses pairing input in any of its three presentations:
// raw compact JSON (a decoded QR), the "MSGB2."-prefixed manual code, or a
// bare Syncthing device ID (what Syncthing's own UI shows — pasting one
// synthesizes a payload with no folder introduction, and the pairing step
// falls back to sharing every locally managed folder). Whitespace-tolerant,
// version-gated, unknown fields rejected, device ID canonicalized — callers
// downstream can trust the shape.
func DecodeSyncPayload(data []byte) (*SyncPayload, error) {
	s := strings.TrimSpace(string(data))
	if s == "" {
		return nil, fmt.Errorf("%w: empty input", ErrInvalidSyncPayload)
	}
	if strings.HasPrefix(s, SyncManualCodePrefix) {
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, SyncManualCodePrefix))
		if err != nil {
			return nil, fmt.Errorf("%w: decode manual code: %v", ErrInvalidSyncPayload, err)
		}
		s = string(raw)
	}
	if !strings.HasPrefix(s, "{") {
		// A bare device ID (no JSON framing): the accessibility/manual-entry
		// path SPEC-0014 requires ("manual device-ID code").
		id, err := CanonicalDeviceID(s)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidSyncPayload, err)
		}
		return &SyncPayload{Version: SyncPayloadVersion, DeviceID: id}, nil
	}
	var p SyncPayload
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("%w: decode payload: %v", ErrInvalidSyncPayload, err)
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// ErrUnknownSyncPeer reports a device ID with no row in the paired_devices
// registry — the peer was never explicitly paired on this node.
var ErrUnknownSyncPeer = errors.New("devices: unknown sync peer")

// Per-source roles a PEER can play, recorded in SyncPeer.Roles (SPEC-0014 REQ
// "Importer and Replica Roles", carried forward from ADR-0018). The role is
// from THIS node's perspective: RoleImporter on a peer's row means that peer
// runs the exporters for the source and this node is its replica, so a local
// Enable for that source must fail with ErrImporterConflict naming the peer.
const (
	// RoleImporter: the peer is the source's importer — it Enables/exports the
	// source from live data; this node receives the archive via sync and runs
	// only its own local ingest (the replica).
	RoleImporter = "importer"
	// RoleReplica: the peer receives this source's archive from this node —
	// this node held the managed root before the share, so it is (or may
	// become) the importer.
	RoleReplica = "replica"
)

// SyncPeer is a paired device as persisted in the repurposed paired_devices
// registry: the peer's Syncthing device ID (its pinned mutual-TLS identity),
// its friendly name, the managed archive folders shared with it, and the
// per-source role it plays. This replaces the SPEC-0011 Peer's certificate
// fingerprint and listener address — Syncthing owns transport and discovery
// (SPEC-0014 "Schema tables carry Syncthing identifiers").
type SyncPeer struct {
	// ID is the registry rowid (0 before first persistence).
	ID int64
	// DeviceID is the peer's canonical Syncthing device ID.
	DeviceID string
	// Name is the peer's friendly device name (from the pairing payload).
	Name string
	// Folders are the managed archive folder ids shared with this peer.
	Folders []string
	// Roles maps a source id ("signal") to the role the PEER plays for it
	// (RoleImporter / RoleReplica). Recorded at the moment a folder share is
	// established: a share whose managed root this node had to PROVISION marks
	// the peer RoleImporter (the archive originates there); a share of a root
	// this node already held marks the peer RoleReplica. nil/missing entries
	// mean "no recorded role" — the source is not role-constrained by this
	// peer. Deleting the peer's row (unpair) releases its role claims, so an
	// unpaired importer's sources become locally Enable-able again.
	Roles map[string]string
	// PairedAt is when the peer was first paired on this node.
	PairedAt time.Time
	// LastSeenAt is the last observed connection time, touched by the
	// folder-watch worker on DeviceConnected events (#158). Zero when the peer
	// has never connected while msgbrowse was watching.
	LastSeenAt time.Time
}

// ImporterFor reports whether the peer is the recorded importer for src.
func (p SyncPeer) ImporterFor(src string) bool { return p.Roles[src] == RoleImporter }

// ShortID returns the peer's short device-ID form for display.
func (p SyncPeer) ShortID() string { return ShortDeviceID(p.DeviceID) }
