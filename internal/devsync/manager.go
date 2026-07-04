// The pairing manager: this node's device-ID pairing payload for the
// /settings QR, and the Pair action that adds an explicitly-scanned peer to
// the daemon config and shares the managed archive folders with it. It
// implements web.PairingSource, replacing the retired SPEC-0011 token-window
// source with the Syncthing device ID (issue #157).
//
// Accept flow (both ends must accept — SPEC-0014 "Pairing via Device ID and
// QR"): pairing is symmetric. Each node displays its own payload; the
// operator carries it to the OTHER node and pastes/scans it there. Pair()
// records the peer in the paired_devices registry (the explicit trust
// decision), adds it to the daemon's device list, and shares the managed
// folders. A node has "accepted" a peer exactly when that peer is in its
// registry; until BOTH nodes have run Pair with the other's payload,
// Syncthing refuses the connection or parks it as a pending request — which
// the Watcher then resolves only for registry members. Knowledge of a device
// ID alone never grants sync.
//
// Replica provisioning (SPEC-0014 REQ "Importer and Replica Roles"): a fresh
// replica has NO local managed roots, yet pairing it with an importer must
// end with the importer's archives syncing to it. So Pair treats the
// payload's folders[] as an introduction to honor, not a set to intersect
// with: every introduced folder id that maps onto the fixed source enum is
// PROVISIONED locally (<data_dir>/archives/<source> created via
// setup.ManagedRoot, archive-not-DB guard re-asserted), added to the daemon's
// live folder config, and shared with the peer. Ids outside the enum are
// logged and ignored — a peer selects from the deterministic managed layout,
// it never names a folder into existence.
//
// Governing: ADR-0021 ("pairing is a device-ID QR"), SPEC-0014 REQ "Pairing
// via Device ID and QR", REQ "Importer and Replica Roles", §Trust Model, REQ
// "Error Handling Standards".
package devsync

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/joestump/msgbrowse/internal/devices"
	"github.com/joestump/msgbrowse/internal/syncthing"
)

// Manager owns the pairing surface for one running device-sync engine.
// Construct with NewManager; all methods are safe for concurrent use.
type Manager struct {
	api     API
	st      PeerStore
	name    string
	folders *FolderSet
	log     *slog.Logger

	mu   sync.Mutex
	myID string // cached from SystemStatus after first success
}

// NewManager builds a Manager over a running daemon's REST API. name is this
// node's friendly device name; folders is the LIVE managed-folder set —
// shared with the Watcher so a folder Pair provisions is immediately watched
// for re-ingest (never nil).
func NewManager(api API, st PeerStore, name string, folders *FolderSet, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{api: api, st: st, name: name, folders: folders, log: log.With("component", "devsync")}
}

// deviceID returns this node's own Syncthing device ID, cached after the
// first successful read (a device ID is immutable for the life of the
// daemon's key material).
func (m *Manager) deviceID(ctx context.Context) (string, error) {
	m.mu.Lock()
	cached := m.myID
	m.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	status, err := m.api.SystemStatus(ctx)
	if err != nil {
		return "", fmt.Errorf("read own device id: %w", err)
	}
	id, err := devices.CanonicalDeviceID(status.MyID)
	if err != nil {
		return "", fmt.Errorf("daemon reported malformed device id: %w", err)
	}
	m.mu.Lock()
	m.myID = id
	m.mu.Unlock()
	return id, nil
}

// folderIDs returns the locally managed folder ids in registration order.
func (m *Manager) folderIDs() []string {
	return m.folders.IDs()
}

// ActivePairing implements web.PairingSource: the payload the /settings QR
// and manual code render — this node's device ID, the managed archive folder
// introductions, and its friendly name. ok=false when the engine has not
// answered yet (page renders its labeled absent state; a later refresh
// succeeds once the daemon is ready). The payload is public introduction
// data, never a secret (SPEC-0014 §Trust Model).
func (m *Manager) ActivePairing(ctx context.Context) (*devices.SyncPayload, bool) {
	id, err := m.deviceID(ctx)
	if err != nil {
		m.log.Warn("pairing payload unavailable", "error", err)
		return nil, false
	}
	p, err := devices.NewSyncPayload(id, m.folderIDs(), m.name)
	if err != nil {
		// Impossible with a canonical ID and generated folder ids; surfaced
		// rather than swallowed per SPEC-0014 REQ "Error Handling Standards".
		m.log.Error("pairing payload construction failed", "error", err)
		return nil, false
	}
	return p, true
}

// Pair executes the operator's explicit accept action for the OTHER node's
// pairing code: decode/validate the payload (QR JSON, MSGB2. manual code, or
// bare device ID), provision any introduced managed folders this node lacks
// (the fresh-replica path), persist the peer in the paired_devices registry,
// add its device to the daemon config, and configure + share the folders via
// the REST API. Idempotent — re-pairing an already-paired device refreshes
// its name/folders and re-asserts the daemon config.
//
// Folder scope: the payload's folders[] is a SOFT introduction, not a hard
// cap (see acceptPendingFolders for the events-side statement of the same
// decision). Every introduced id that maps onto the fixed source enum is
// honored — provisioned locally when absent — and ids outside the enum are
// logged and ignored; a bare device ID (no introduction) shares every locally
// managed folder, the symmetric default. A peer can never name a folder into
// existence: ids select from the deterministic managed layout only (issue
// #157 Security Checklist; SPEC-0014 "msgbrowse-Owned Config Generation").
//
// The registry write happens BEFORE the REST mutations: the durable trust
// decision must not be lost to a transient daemon hiccup. If a REST call
// fails after persistence, the error is surfaced (never swallowed) and the
// next daemon start regenerates config from the registry anyway
// (ApplyPeers), converging the daemon on the recorded state. Provisioning
// happens before the registry write for the same reason in reverse: a
// recorded share must always have its root on disk, so restarts' config
// regeneration (ExistingManagedFolders) rediscovers exactly the recorded set.
func (m *Manager) Pair(ctx context.Context, code string) (devices.SyncPeer, error) {
	payload, err := devices.DecodeSyncPayload([]byte(code))
	if err != nil {
		return devices.SyncPeer{}, err
	}

	myID, err := m.deviceID(ctx)
	if err != nil {
		return devices.SyncPeer{}, fmt.Errorf("pair device %s: %w", devices.ShortDeviceID(payload.DeviceID), err)
	}
	if payload.DeviceID == myID {
		return devices.SyncPeer{}, fmt.Errorf("device %s is this node: %w", devices.ShortDeviceID(myID), devices.ErrSelfPair)
	}

	share, folders, err := m.resolveShare(payload)
	if err != nil {
		return devices.SyncPeer{}, fmt.Errorf("pair device %s: %w", devices.ShortDeviceID(payload.DeviceID), err)
	}

	peer := devices.SyncPeer{
		DeviceID: payload.DeviceID,
		Name:     payload.Name,
		Folders:  share,
		PairedAt: time.Now(),
	}
	if peer.Name == "" {
		peer.Name = devices.ShortDeviceID(peer.DeviceID)
	}
	id, err := m.st.UpsertSyncPeer(ctx, peer)
	if err != nil {
		return devices.SyncPeer{}, fmt.Errorf("pair device %s: persist peer: %w", peer.ShortID(), err)
	}
	peer.ID = id

	if err := m.ensureDevice(ctx, peer); err != nil {
		return peer, err
	}
	if err := m.ensureFolders(ctx, folders); err != nil {
		return peer, err
	}
	if err := m.ensureFolderShares(ctx, peer.DeviceID, share); err != nil {
		return peer, err
	}
	m.log.Info("device paired", "device_id", peer.DeviceID, "name", peer.Name, "folders", share)
	return peer, nil
}

// resolveShare turns a validated payload's folder introduction into the share
// set and the concrete managed folders behind it, provisioning any known-
// source folder this node lacks (the fresh-replica path). An empty
// introduction — a bare device ID — means every locally managed folder.
// Introduced ids outside the fixed source enum are logged and dropped, never
// an error: the rest of the introduction still pairs.
func (m *Manager) resolveShare(payload *devices.SyncPayload) ([]string, []syncthing.Folder, error) {
	if len(payload.Folders) == 0 {
		return m.folderIDs(), m.folders.List(), nil
	}
	var (
		share   []string
		folders []syncthing.Folder
		seen    = make(map[string]bool, len(payload.Folders))
	)
	for _, id := range payload.Folders {
		if seen[id] {
			continue
		}
		seen[id] = true
		if _, ok := SourceForFolderID(id); !ok {
			m.log.Info("ignoring introduced folder (id outside the managed source enum)",
				"folder", id, "device_id", payload.DeviceID)
			continue
		}
		f, err := m.folders.Provision(id)
		if err != nil {
			// A known-source folder that cannot be provisioned is a hard
			// pairing error (disk trouble), surfaced per SPEC-0014 REQ "Error
			// Handling Standards" — never a silent partial pair.
			return nil, nil, err
		}
		share = append(share, id)
		folders = append(folders, f)
	}
	return share, folders, nil
}

// Peers implements web.PairingSource's registry listing for the /settings
// device list.
func (m *Manager) Peers(ctx context.Context) ([]devices.SyncPeer, error) {
	return m.st.ListSyncPeers(ctx)
}

// ensureDevice adds peer to the daemon's device list if absent (refreshing
// the name when present), via read-modify-write on /rest/config/devices.
func (m *Manager) ensureDevice(ctx context.Context, peer devices.SyncPeer) error {
	devs, err := m.api.GetDevices(ctx)
	if err != nil {
		return fmt.Errorf("pair device %s: read daemon devices: %w", peer.ShortID(), err)
	}
	for i, d := range devs {
		if d.DeviceID == peer.DeviceID {
			if d.Name == peer.Name {
				return nil // already configured
			}
			devs[i].Name = peer.Name
			if err := m.api.PutDevices(ctx, devs); err != nil {
				return fmt.Errorf("pair device %s: update daemon device: %w", peer.ShortID(), err)
			}
			return nil
		}
	}
	devs = append(devs, syncthing.DeviceConfig{DeviceID: peer.DeviceID, Name: peer.Name})
	if err := m.api.PutDevices(ctx, devs); err != nil {
		return fmt.Errorf("pair device %s: add daemon device: %w", peer.ShortID(), err)
	}
	return nil
}

// ensureFolders adds each managed folder to the daemon's live folder config
// if absent, via read-modify-write on /rest/config/folders — how a folder
// provisioned mid-run (fresh replica pairing, or an accepted folder offer)
// becomes syncable before the next restart's config regeneration would pick
// it up from disk. Only FolderSet-vended folders reach here, so every path is
// already inside <data_dir>/archives/ (the archive-not-DB guard was asserted
// at provisioning).
func (m *Manager) ensureFolders(ctx context.Context, want []syncthing.Folder) error {
	if len(want) == 0 {
		return nil
	}
	current, err := m.api.GetFolders(ctx)
	if err != nil {
		return fmt.Errorf("configure managed folders: read daemon folders: %w", err)
	}
	have := make(map[string]bool, len(current))
	for _, f := range current {
		have[f.ID] = true
	}
	changed := false
	for _, f := range want {
		if have[f.ID] {
			continue
		}
		current = append(current, syncthing.FolderConfig{
			ID:    f.ID,
			Label: f.Label,
			Path:  f.Path,
			Type:  syncthing.FolderTypeSendReceive,
		})
		have[f.ID] = true
		changed = true
	}
	if !changed {
		return nil
	}
	if err := m.api.PutFolders(ctx, current); err != nil {
		return fmt.Errorf("configure managed folders: %w", err)
	}
	return nil
}

// ensureFolderShares shares each named managed folder with deviceID if it is
// not already shared, via read-modify-write on /rest/config/folders. Folder
// ids outside the daemon's configured set are skipped with a log line — the
// daemon's folders are the managed archive roots and nothing else (SPEC-0014
// REQ "msgbrowse-Owned Config Generation").
func (m *Manager) ensureFolderShares(ctx context.Context, deviceID string, folderIDs []string) error {
	if len(folderIDs) == 0 {
		return nil
	}
	folders, err := m.api.GetFolders(ctx)
	if err != nil {
		return fmt.Errorf("share folders with %s: read daemon folders: %w", devices.ShortDeviceID(deviceID), err)
	}
	changed := false
	for _, want := range folderIDs {
		found := false
		for i := range folders {
			if folders[i].ID != want {
				continue
			}
			found = true
			if !folderSharedWith(folders[i], deviceID) {
				folders[i].Devices = append(folders[i].Devices, syncthing.FolderDeviceRef{DeviceID: deviceID})
				changed = true
			}
		}
		if !found {
			m.log.Warn("skip sharing unknown folder (not in daemon config)", "folder", want, "device_id", deviceID)
		}
	}
	if !changed {
		return nil
	}
	if err := m.api.PutFolders(ctx, folders); err != nil {
		return fmt.Errorf("share folders %v with %s: %w", folderIDs, devices.ShortDeviceID(deviceID), err)
	}
	return nil
}

// folderSharedWith reports whether a folder config already lists deviceID.
func folderSharedWith(f syncthing.FolderConfig, deviceID string) bool {
	for _, d := range f.Devices {
		if d.DeviceID == deviceID {
			return true
		}
	}
	return false
}
