// Tests for the Syncthing-era device-sync registry (schemaV10) and the
// SPEC-0011 → SPEC-0014 migration: the repurposed tables carry Syncthing
// device IDs and folder mappings, legacy fingerprint rows are cleared, and
// the peer registry round-trips what the pairing flow persists.
//
// Governing: ADR-0021, SPEC-0014 REQ "Migration from SPEC-0011" ("Schema
// tables carry Syncthing identifiers"; a node with SPEC-0011 rows is left
// coherent "with no dangling pinned certificates"), REQ "Pairing via Device
// ID and QR".
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/devices"
)

const (
	testDeviceA = "XW4UY46-VHRCAEN-OTRLIUX-BIIMJVP-KPVFKQW-4H5TU2H-MYSYKFX-S53S7AL"
	testDeviceB = "AL4V3SV-WOXMPPL-7OSHTP5-YBPGQTN-6CBXKHB-D5DWSIJ-563UQMW-5JXZFAO"
)

func newSyncStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "sync.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestSyncPeerRoundTrip: upsert persists device ID, name, and folder mapping;
// list and by-device-ID lookup return them; re-upsert updates name/folders
// while preserving identity and paired_at.
func TestSyncPeerRoundTrip(t *testing.T) {
	st := newSyncStore(t)
	ctx := context.Background()

	peer := devices.SyncPeer{
		DeviceID: testDeviceA,
		Name:     "kitchen-mac",
		Folders:  []string{"msgbrowse-signal", "msgbrowse-imessage"},
		PairedAt: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
	}
	id, err := st.UpsertSyncPeer(ctx, peer)
	if err != nil {
		t.Fatalf("UpsertSyncPeer: %v", err)
	}
	if id == 0 {
		t.Fatal("UpsertSyncPeer returned id 0")
	}

	got, err := st.GetSyncPeerByDeviceID(ctx, testDeviceA)
	if err != nil {
		t.Fatalf("GetSyncPeerByDeviceID: %v", err)
	}
	if got.Name != "kitchen-mac" || len(got.Folders) != 2 || got.Folders[0] != "msgbrowse-signal" {
		t.Errorf("round-trip = %+v", got)
	}
	if !got.PairedAt.Equal(peer.PairedAt) {
		t.Errorf("PairedAt = %v, want %v", got.PairedAt, peer.PairedAt)
	}

	// Re-pair with a new name/folder set: same row, refreshed fields,
	// original paired_at preserved (the trust decision's timestamp).
	peer.Name = "kitchen-mac-2"
	peer.Folders = []string{"msgbrowse-signal"}
	peer.PairedAt = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	id2, err := st.UpsertSyncPeer(ctx, peer)
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	if id2 != id {
		t.Errorf("re-upsert created a new row: %d != %d", id2, id)
	}
	got, err = st.GetSyncPeerByDeviceID(ctx, testDeviceA)
	if err != nil {
		t.Fatalf("GetSyncPeerByDeviceID after re-upsert: %v", err)
	}
	if got.Name != "kitchen-mac-2" || len(got.Folders) != 1 {
		t.Errorf("re-upsert did not refresh fields: %+v", got)
	}
	if !got.PairedAt.Equal(time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("re-upsert clobbered paired_at: %v", got.PairedAt)
	}
}

// TestSyncPeerValidation: a malformed device ID never reaches the registry
// (issue #157 Security Checklist), and unknown lookups return the typed
// sentinel.
func TestSyncPeerValidation(t *testing.T) {
	st := newSyncStore(t)
	ctx := context.Background()

	if _, err := st.UpsertSyncPeer(ctx, devices.SyncPeer{DeviceID: "not-a-device-id"}); !errors.Is(err, devices.ErrInvalidDeviceID) {
		t.Errorf("UpsertSyncPeer(bad id) = %v, want ErrInvalidDeviceID", err)
	}
	if _, err := st.GetSyncPeerByDeviceID(ctx, testDeviceB); !errors.Is(err, devices.ErrUnknownSyncPeer) {
		t.Errorf("GetSyncPeerByDeviceID(unpaired) = %v, want ErrUnknownSyncPeer", err)
	}
	// Lookup canonicalizes: a lowercase, dashless paste finds the peer.
	if _, err := st.UpsertSyncPeer(ctx, devices.SyncPeer{DeviceID: testDeviceB, Name: "b"}); err != nil {
		t.Fatalf("UpsertSyncPeer: %v", err)
	}
	lower := strings.ToLower(strings.ReplaceAll(testDeviceB, "-", ""))
	got, err := st.GetSyncPeerByDeviceID(ctx, lower)
	if err != nil {
		t.Fatalf("canonicalized lookup: %v", err)
	}
	if got.DeviceID != testDeviceB {
		t.Errorf("canonicalized lookup = %q, want %q", got.DeviceID, testDeviceB)
	}
}

// TestListSyncPeersOrdered: peers list in pairing order.
func TestListSyncPeersOrdered(t *testing.T) {
	st := newSyncStore(t)
	ctx := context.Background()
	for i, d := range []string{testDeviceA, testDeviceB} {
		if _, err := st.UpsertSyncPeer(ctx, devices.SyncPeer{
			DeviceID: d,
			Name:     fmt.Sprintf("peer-%d", i),
			PairedAt: time.Date(2026, 7, 1+i, 0, 0, 0, 0, time.UTC),
		}); err != nil {
			t.Fatalf("UpsertSyncPeer %d: %v", i, err)
		}
	}
	peers, err := st.ListSyncPeers(ctx)
	if err != nil {
		t.Fatalf("ListSyncPeers: %v", err)
	}
	if len(peers) != 2 || peers[0].Name != "peer-0" || peers[1].Name != "peer-1" {
		t.Errorf("ListSyncPeers = %+v", peers)
	}
}

// TestRecordSyncImport: the folder↔source re-ingest bookkeeping upserts and
// lists (SPEC-0014 REQ "Re-ingest Trigger" bookkeeping for #158's status).
func TestRecordSyncImport(t *testing.T) {
	st := newSyncStore(t)
	ctx := context.Background()

	if err := st.RecordSyncImport(ctx, "msgbrowse-signal", "signal"); err != nil {
		t.Fatalf("RecordSyncImport: %v", err)
	}
	if err := st.RecordSyncImport(ctx, "msgbrowse-signal", "signal"); err != nil {
		t.Fatalf("RecordSyncImport upsert: %v", err)
	}
	states, err := st.SyncImportStates(ctx)
	if err != nil {
		t.Fatalf("SyncImportStates: %v", err)
	}
	if len(states) != 1 || states[0].FolderID != "msgbrowse-signal" || states[0].Source != "signal" {
		t.Errorf("SyncImportStates = %+v", states)
	}
	if states[0].LastImportAt.IsZero() {
		t.Error("LastImportAt not recorded")
	}
	if err := st.RecordSyncImport(ctx, "", "signal"); err == nil {
		t.Error("RecordSyncImport accepted an empty folder id")
	}
}

// TestMigrationV10RepurposesSPEC0011Tables builds a genuine schema-v9
// database with SPEC-0011 rows (a fingerprint-pinned peer and byte-range
// transfer cursors), then opens it with the current binary and asserts the
// v10 migration rebuilt both tables in the Syncthing shape with the legacy
// rows CLEARED — no dangling pinned certificates (SPEC-0014 REQ "Migration
// from SPEC-0011").
func TestMigrationV10RepurposesSPEC0011Tables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	ctx := context.Background()

	// Build the v9 world exactly as a v9 binary would have: migrations 1..9
	// in order, then the legacy rows.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw sqlite: %v", err)
	}
	for v := 1; v <= 9; v++ {
		if _, err := db.ExecContext(ctx, migrations[v]); err != nil {
			t.Fatalf("apply legacy migration v%d: %v", v, err)
		}
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO paired_devices (name, fingerprint, address, roles, paired_at)
VALUES ('old-peer', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', '10.0.0.2:8788', '{"signal":"importer"}', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert legacy peer: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO sync_state (peer_id, source, rel_path, size_bytes, sha256, fetched_bytes, verified, updated_at)
VALUES (1, 'signal', 'export/Alice/chat.md', 100, 'ff', 50, 0, '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert legacy cursor: %v", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA user_version = 9`); err != nil {
		t.Fatalf("set user_version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	// Open with the current binary: migrations run 10.
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open at current version: %v", err)
	}
	defer st.Close()

	if v, err := st.UserVersion(ctx); err != nil || v != SchemaVersion() {
		t.Fatalf("user_version = %d (%v), want %d", v, err, SchemaVersion())
	}

	// Legacy rows are cleared (no fingerprint can convert to a device ID).
	peers, err := st.ListSyncPeers(ctx)
	if err != nil {
		t.Fatalf("ListSyncPeers on migrated db: %v", err)
	}
	if len(peers) != 0 {
		t.Errorf("legacy SPEC-0011 rows survived the migration: %+v", peers)
	}
	states, err := st.SyncImportStates(ctx)
	if err != nil {
		t.Fatalf("SyncImportStates on migrated db: %v", err)
	}
	if len(states) != 0 {
		t.Errorf("legacy sync_state rows survived: %+v", states)
	}

	// The old fingerprint column is gone; the new shape accepts device IDs.
	var n int
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('paired_devices') WHERE name = 'fingerprint'`).Scan(&n); err != nil {
		t.Fatalf("inspect columns: %v", err)
	}
	if n != 0 {
		t.Error("paired_devices still carries the SPEC-0011 fingerprint column")
	}
	if err := st.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('paired_devices') WHERE name = 'device_id'`).Scan(&n); err != nil {
		t.Fatalf("inspect columns: %v", err)
	}
	if n != 1 {
		t.Error("paired_devices missing the device_id column")
	}
	if _, err := st.UpsertSyncPeer(ctx, devices.SyncPeer{DeviceID: testDeviceA, Name: "new-peer"}); err != nil {
		t.Fatalf("new-shape insert on migrated db: %v", err)
	}
}

// TestSyncPeerRolesRoundTrip (#158; SPEC-0014 REQ "Importer and Replica
// Roles"): the per-source role map persists through the repurposed roles
// column, upserts replace it, and a role-free peer scans as an empty map —
// never an error.
func TestSyncPeerRolesRoundTrip(t *testing.T) {
	st := newSyncStore(t)
	ctx := context.Background()

	if _, err := st.UpsertSyncPeer(ctx, devices.SyncPeer{
		DeviceID: testDeviceA,
		Name:     "importer-mac",
		Folders:  []string{"msgbrowse-signal"},
		Roles:    map[string]string{"signal": devices.RoleImporter, "imessage": devices.RoleReplica},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := st.GetSyncPeerByDeviceID(ctx, testDeviceA)
	if err != nil {
		t.Fatal(err)
	}
	if got.Roles["signal"] != devices.RoleImporter || got.Roles["imessage"] != devices.RoleReplica {
		t.Errorf("roles = %v", got.Roles)
	}
	if !got.ImporterFor("signal") || got.ImporterFor("imessage") {
		t.Errorf("ImporterFor projection wrong: %v", got.Roles)
	}

	// Upsert replaces the role map (the Manager merges before persisting).
	if _, err := st.UpsertSyncPeer(ctx, devices.SyncPeer{
		DeviceID: testDeviceA, Name: "importer-mac",
		Roles: map[string]string{"signal": devices.RoleImporter},
	}); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetSyncPeerByDeviceID(ctx, testDeviceA)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Roles) != 1 || got.Roles["signal"] != devices.RoleImporter {
		t.Errorf("roles after re-upsert = %v", got.Roles)
	}

	// A peer stored with nil roles reads back as an empty, usable map state.
	if _, err := st.UpsertSyncPeer(ctx, devices.SyncPeer{DeviceID: testDeviceB, Name: "plain"}); err != nil {
		t.Fatal(err)
	}
	plain, err := st.GetSyncPeerByDeviceID(ctx, testDeviceB)
	if err != nil {
		t.Fatal(err)
	}
	if plain.ImporterFor("signal") {
		t.Error("role-free peer reports an importer role")
	}
}

// TestDeleteSyncPeer (#158; SPEC-0014 REQ "Unpair and Revoke"): the row is
// removed, a second delete reports the typed unknown-peer sentinel, and the
// OTHER peer's row is untouched.
func TestDeleteSyncPeer(t *testing.T) {
	st := newSyncStore(t)
	ctx := context.Background()
	for _, p := range []devices.SyncPeer{
		{DeviceID: testDeviceA, Name: "a"},
		{DeviceID: testDeviceB, Name: "b"},
	} {
		if _, err := st.UpsertSyncPeer(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	if err := st.DeleteSyncPeer(ctx, strings.ToLower(testDeviceA)); err != nil {
		t.Fatalf("delete (transcribed id): %v", err)
	}
	if _, err := st.GetSyncPeerByDeviceID(ctx, testDeviceA); !errors.Is(err, devices.ErrUnknownSyncPeer) {
		t.Errorf("deleted peer still readable: %v", err)
	}
	if err := st.DeleteSyncPeer(ctx, testDeviceA); !errors.Is(err, devices.ErrUnknownSyncPeer) {
		t.Errorf("double delete = %v, want ErrUnknownSyncPeer", err)
	}
	if err := st.DeleteSyncPeer(ctx, "not-a-device-id"); err == nil {
		t.Error("malformed device id accepted by delete")
	}
	if _, err := st.GetSyncPeerByDeviceID(ctx, testDeviceB); err != nil {
		t.Errorf("unrelated peer damaged by delete: %v", err)
	}
}

// TestTouchSyncPeerSeen (#158): the last-seen timestamp round-trips; touching
// an unknown peer is a no-op by contract (racing an unpair must not error).
func TestTouchSyncPeerSeen(t *testing.T) {
	st := newSyncStore(t)
	ctx := context.Background()
	if _, err := st.UpsertSyncPeer(ctx, devices.SyncPeer{DeviceID: testDeviceA, Name: "a"}); err != nil {
		t.Fatal(err)
	}
	seen := time.Date(2026, 7, 4, 12, 30, 0, 0, time.UTC)
	if err := st.TouchSyncPeerSeen(ctx, testDeviceA, seen); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSyncPeerByDeviceID(ctx, testDeviceA)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LastSeenAt.Equal(seen) {
		t.Errorf("LastSeenAt = %v, want %v", got.LastSeenAt, seen)
	}
	if err := st.TouchSyncPeerSeen(ctx, testDeviceB, seen); err != nil {
		t.Errorf("touching an unknown peer errored: %v", err)
	}
}
