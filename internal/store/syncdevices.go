// The Syncthing-era device-sync registry: paired peers keyed by Syncthing
// device ID and the per-folder re-ingest bookkeeping, over the schemaV10
// tables. This replaces the SPEC-0011 fingerprint/transfer-cursor API —
// Syncthing owns identity, transport, and resumption; the store records only
// what msgbrowse itself decides (who is paired, which folders they share,
// when a completed sync last triggered an import). Both tables are node-local
// and never synchronized.
//
// Governing: ADR-0021, SPEC-0014 REQ "Migration from SPEC-0011" ("Schema
// tables carry Syncthing identifiers"), REQ "Pairing via Device ID and QR"
// (persisting the explicitly-accepted peer set that gates auto-acceptance),
// REQ "Error Handling Standards" (wrapped errors, parameterized queries).
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/joestump/msgbrowse/internal/devices"
)

// UpsertSyncPeer inserts or updates a paired peer keyed by its Syncthing
// device ID and returns the registry ID. Name, folder set, and per-source
// roles follow the latest pairing; paired_at is preserved on re-pair (the
// trust decision's original timestamp).
func (s *Store) UpsertSyncPeer(ctx context.Context, p devices.SyncPeer) (int64, error) {
	id, err := devices.CanonicalDeviceID(p.DeviceID)
	if err != nil {
		return 0, fmt.Errorf("upsert sync peer: %w", err)
	}
	folders := p.Folders
	if folders == nil {
		folders = []string{}
	}
	foldersJSON, err := json.Marshal(folders)
	if err != nil {
		return 0, fmt.Errorf("upsert sync peer %s: encode folders: %w", devices.ShortDeviceID(id), err)
	}
	roles := p.Roles
	if roles == nil {
		roles = map[string]string{}
	}
	rolesJSON, err := json.Marshal(roles)
	if err != nil {
		return 0, fmt.Errorf("upsert sync peer %s: encode roles: %w", devices.ShortDeviceID(id), err)
	}
	pairedAt := p.PairedAt
	if pairedAt.IsZero() {
		pairedAt = time.Now()
	}
	var rowID int64
	err = s.db.QueryRowContext(ctx, `
INSERT INTO paired_devices (device_id, name, folders, roles, paired_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(device_id) DO UPDATE SET
    name    = excluded.name,
    folders = excluded.folders,
    roles   = excluded.roles
RETURNING id`, id, p.Name, string(foldersJSON), string(rolesJSON), pairedAt.UTC().Format(time.RFC3339)).Scan(&rowID)
	if err != nil {
		return 0, fmt.Errorf("upsert sync peer %s: %w", devices.ShortDeviceID(id), err)
	}
	return rowID, nil
}

// DeleteSyncPeer removes a paired peer's registry row — the durable half of
// unpairing (SPEC-0014 REQ "Unpair and Revoke"): the next daemon start
// regenerates config without the peer, the watcher's scoped auto-accept
// stops honoring it, and its recorded role claims are released (so a source
// it was importer for becomes locally Enable-able again). Local archives and
// the database are NEVER touched here. Returns devices.ErrUnknownSyncPeer
// (wrapped) when no row matches.
func (s *Store) DeleteSyncPeer(ctx context.Context, deviceID string) error {
	id, err := devices.CanonicalDeviceID(deviceID)
	if err != nil {
		return fmt.Errorf("delete sync peer: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM paired_devices WHERE device_id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete sync peer %s: %w", devices.ShortDeviceID(id), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete sync peer %s: %w", devices.ShortDeviceID(id), err)
	}
	if n == 0 {
		return fmt.Errorf("device %s: %w", devices.ShortDeviceID(id), devices.ErrUnknownSyncPeer)
	}
	return nil
}

// TouchSyncPeerSeen records the last time a paired peer was observed
// connected (the watcher's DeviceConnected handler, #158). Best-effort by
// contract — a missing row (racing an unpair) is not an error.
func (s *Store) TouchSyncPeerSeen(ctx context.Context, deviceID string, at time.Time) error {
	id, err := devices.CanonicalDeviceID(deviceID)
	if err != nil {
		return fmt.Errorf("touch sync peer: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `UPDATE paired_devices SET last_seen_at = ? WHERE device_id = ?`,
		at.UTC().Format(time.RFC3339), id)
	if err != nil {
		return fmt.Errorf("touch sync peer %s: %w", devices.ShortDeviceID(id), err)
	}
	return nil
}

// ListSyncPeers returns every paired peer, ordered by pairing time. The
// supervisor's config generation and the /settings device list read this.
func (s *Store) ListSyncPeers(ctx context.Context) ([]devices.SyncPeer, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, device_id, name, folders, roles, paired_at, last_seen_at
  FROM paired_devices ORDER BY paired_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list sync peers: %w", err)
	}
	defer rows.Close()
	var peers []devices.SyncPeer
	for rows.Next() {
		p, err := scanSyncPeer(rows)
		if err != nil {
			return nil, fmt.Errorf("list sync peers: %w", err)
		}
		peers = append(peers, *p)
	}
	return peers, rows.Err()
}

// GetSyncPeerByDeviceID looks a peer up by its Syncthing device ID — the
// auto-accept watcher's question ("did the operator explicitly pair this
// device?"). Returns devices.ErrUnknownSyncPeer (wrapped, with the short ID)
// when no peer matches; an unknown device is NEVER accepted (SPEC-0014 "A
// device ID alone does not grant sync").
func (s *Store) GetSyncPeerByDeviceID(ctx context.Context, deviceID string) (*devices.SyncPeer, error) {
	id, err := devices.CanonicalDeviceID(deviceID)
	if err != nil {
		return nil, fmt.Errorf("get sync peer: %w", err)
	}
	row := s.db.QueryRowContext(ctx, `
SELECT id, device_id, name, folders, roles, paired_at, last_seen_at
  FROM paired_devices WHERE device_id = ?`, id)
	p, err := scanSyncPeer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("device %s: %w", devices.ShortDeviceID(id), devices.ErrUnknownSyncPeer)
	}
	return p, err
}

// scanner abstracts *sql.Row / *sql.Rows for scanSyncPeer.
type scanner interface{ Scan(dest ...any) error }

func scanSyncPeer(sc scanner) (*devices.SyncPeer, error) {
	var p devices.SyncPeer
	var foldersJSON, rolesJSON, pairedAt, lastSeen string
	if err := sc.Scan(&p.ID, &p.DeviceID, &p.Name, &foldersJSON, &rolesJSON, &pairedAt, &lastSeen); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(foldersJSON), &p.Folders); err != nil {
		return nil, fmt.Errorf("decode folders for peer %s: %w", p.ShortID(), err)
	}
	if err := json.Unmarshal([]byte(rolesJSON), &p.Roles); err != nil {
		return nil, fmt.Errorf("decode roles for peer %s: %w", p.ShortID(), err)
	}
	t, err := time.Parse(time.RFC3339, pairedAt)
	if err != nil {
		return nil, fmt.Errorf("parse paired_at for peer %s: %w", p.ShortID(), err)
	}
	p.PairedAt = t
	if lastSeen != "" {
		if t, err := time.Parse(time.RFC3339, lastSeen); err == nil {
			p.LastSeenAt = t
		}
	}
	return &p, nil
}

// SyncImportState is one managed folder's re-ingest bookkeeping: the
// folder↔source mapping and the last time a folder-completion event
// triggered the incremental import.
type SyncImportState struct {
	FolderID     string
	Source       string
	LastImportAt time.Time
}

// RecordSyncImport upserts a folder's sync_state row when a completed sync
// triggers the incremental re-ingest (SPEC-0014 REQ "Re-ingest Trigger").
// The status/doctor story (#158) reads it for staleness reporting.
func (s *Store) RecordSyncImport(ctx context.Context, folderID, source string) error {
	if folderID == "" || source == "" {
		return fmt.Errorf("record sync import: empty folder id or source")
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO sync_state (folder_id, source, last_import_at, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(folder_id) DO UPDATE SET
    source         = excluded.source,
    last_import_at = excluded.last_import_at,
    updated_at     = excluded.updated_at`, folderID, source, now, now)
	if err != nil {
		return fmt.Errorf("record sync import %s (%s): %w", folderID, source, err)
	}
	return nil
}

// SyncImportStates returns every managed folder's re-ingest bookkeeping,
// ordered by folder id.
func (s *Store) SyncImportStates(ctx context.Context) ([]SyncImportState, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT folder_id, source, last_import_at FROM sync_state ORDER BY folder_id`)
	if err != nil {
		return nil, fmt.Errorf("sync import states: %w", err)
	}
	defer rows.Close()
	var out []SyncImportState
	for rows.Next() {
		var st SyncImportState
		var last string
		if err := rows.Scan(&st.FolderID, &st.Source, &last); err != nil {
			return nil, fmt.Errorf("sync import states scan: %w", err)
		}
		if last != "" {
			if t, err := time.Parse(time.RFC3339, last); err == nil {
				st.LastImportAt = t
			}
		}
		out = append(out, st)
	}
	return out, rows.Err()
}
