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
// Governing: ADR-0021 ("pairing is a device-ID QR"), SPEC-0014 REQ "Pairing
// via Device ID and QR", §Trust Model, REQ "Error Handling Standards".
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
	folders []syncthing.Folder
	log     *slog.Logger

	mu   sync.Mutex
	myID string // cached from SystemStatus after first success
}

// NewManager builds a Manager over a running daemon's REST API. name is this
// node's friendly device name; folders are the locally managed archive
// folders (the shareable set).
func NewManager(api API, st PeerStore, name string, folders []syncthing.Folder, log *slog.Logger) *Manager {
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

// folderIDs returns the locally managed folder ids in declaration order.
func (m *Manager) folderIDs() []string {
	ids := make([]string, 0, len(m.folders))
	for _, f := range m.folders {
		ids = append(ids, f.ID)
	}
	return ids
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
// bare device ID), persist the peer in the paired_devices registry, add its
// device to the daemon config, and share the relevant managed folders with
// it via the REST API. Idempotent — re-pairing an already-paired device
// refreshes its name/folders and re-asserts the daemon config.
//
// The registry write happens BEFORE the REST mutations: the durable trust
// decision must not be lost to a transient daemon hiccup. If a REST call
// fails after persistence, the error is surfaced (never swallowed) and the
// next daemon start regenerates config from the registry anyway
// (ApplyPeers), converging the daemon on the recorded state.
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

	// Folders to share: the payload's introduction intersected with what this
	// node actually manages — a peer can never name a folder into existence
	// here (folder ids from the deterministic managed set only, issue #157
	// Security Checklist). A bare device ID (no introduction) shares every
	// locally managed folder, the symmetric default.
	share := intersectFolders(m.folderIDs(), payload.Folders)

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
	if err := m.ensureFolderShares(ctx, peer.DeviceID, share); err != nil {
		return peer, err
	}
	m.log.Info("device paired", "device_id", peer.DeviceID, "name", peer.Name, "folders", share)
	return peer, nil
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

// intersectFolders returns the members of local that appear in introduced,
// preserving local order. An empty introduction means "everything local".
func intersectFolders(local, introduced []string) []string {
	if len(introduced) == 0 {
		out := make([]string, len(local))
		copy(out, local)
		return out
	}
	var out []string
	for _, id := range local {
		if containsString(introduced, id) {
			out = append(out, id)
		}
	}
	return out
}
