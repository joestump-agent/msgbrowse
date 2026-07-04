// Package devsync is the msgbrowse-owned half of Syncthing device sync: the
// device-ID pairing flow (issue #157) and the folder-completion → incremental
// re-ingest worker, layered on the internal/syncthing supervisor and REST
// client from #156. Syncthing owns identity, transport, and discovery; this
// package owns what is genuinely msgbrowse's — which peers the operator
// explicitly paired, which managed archive folders they share, and when a
// completed sync should trigger the existing incremental import.
//
// It is pure Go with no cgo, importable by both `msgbrowse serve`
// (internal/cli) and the desktop embedded server
// (cmd/msgbrowse-desktop/internal/embedded), mirroring internal/onboardsvc's
// placement.
//
// Trust model (SPEC-0014 §Trust Model): a Syncthing device ID is public;
// pairing records the operator's explicit acceptance of exactly one device
// ID, and only that recorded acceptance ever drives configuration changes —
// the watcher auto-accepts pending devices/folders ONLY for device IDs in the
// paired_devices registry, never blanket. Folder scope within a paired
// device is the fixed source enum: an introduced or offered folder id that
// maps onto "msgbrowse-<source>" is honored — provisioning the managed root
// when this node lacks it (the replica role) and persisting the share to the
// registry so restarts regenerate it — while any other id is refused. See
// Manager.Pair and Watcher.acceptPendingFolders for the full statement of
// that decision.
//
// Governing: ADR-0021 ("msgbrowse owns config generation … the pairing UX …
// the folder-watch → re-ingest trigger"), SPEC-0014 REQ "Pairing via Device
// ID and QR", REQ "Re-ingest Trigger", REQ "Concurrency Safety", REQ "Error
// Handling Standards".
package devsync

import (
	"context"
	"time"

	"github.com/joestump/msgbrowse/internal/devices"
	"github.com/joestump/msgbrowse/internal/store"
	"github.com/joestump/msgbrowse/internal/syncthing"
)

// API is the slice of the Syncthing REST client this package drives.
// *syncthing.Client satisfies it; tests substitute a stub REST server or a
// scripted fake.
type API interface {
	// SystemStatus reports the daemon's status, including this node's own
	// device ID (the pairing payload's DeviceID).
	SystemStatus(ctx context.Context) (*syncthing.SystemStatus, error)
	// GetDevices / PutDevices read and replace the daemon's device list —
	// how a paired peer is added (SPEC-0014 "msgbrowse adds the scanned peer
	// as a Syncthing device") and removed again on unpair.
	GetDevices(ctx context.Context) ([]syncthing.DeviceConfig, error)
	PutDevices(ctx context.Context, devs []syncthing.DeviceConfig) error
	// GetFolders / PutFolders read and replace the folder list — how the
	// managed archive folders are shared with a paired peer, and unshared
	// from it on unpair.
	GetFolders(ctx context.Context) ([]syncthing.FolderConfig, error)
	PutFolders(ctx context.Context, folders []syncthing.FolderConfig) error
	// FolderCompletion is the authoritative completion gate for the
	// re-ingest trigger (no import against a mid-transfer folder) and the
	// per-folder percentage the status surfaces render.
	FolderCompletion(ctx context.Context, folderID, deviceID string) (*syncthing.Completion, error)
	// FolderStatus reports a folder's daemon-side state and error counters —
	// how a paused/errored folder reaches Settings/Status/doctor (SPEC-0014
	// REQ "Status and Doctor Surfacing").
	FolderStatus(ctx context.Context, folderID string) (*syncthing.FolderStatus, error)
	// Connections reports per-device connection state for the peer list.
	Connections(ctx context.Context) (*syncthing.Connections, error)
	// Events is the long-poll event feed the watcher consumes.
	Events(ctx context.Context, since int64, types []string, timeout time.Duration) ([]syncthing.Event, error)
}

// PeerStore is the slice of *store.Store this package persists through: the
// explicitly-paired peer registry (the ONLY set auto-acceptance consults) and
// the per-folder re-ingest bookkeeping.
type PeerStore interface {
	UpsertSyncPeer(ctx context.Context, p devices.SyncPeer) (int64, error)
	ListSyncPeers(ctx context.Context) ([]devices.SyncPeer, error)
	GetSyncPeerByDeviceID(ctx context.Context, deviceID string) (*devices.SyncPeer, error)
	// DeleteSyncPeer removes a peer's registry row — the durable half of
	// unpair (SPEC-0014 REQ "Unpair and Revoke").
	DeleteSyncPeer(ctx context.Context, deviceID string) error
	// TouchSyncPeerSeen records a peer's last observed connection time.
	TouchSyncPeerSeen(ctx context.Context, deviceID string, at time.Time) error
	RecordSyncImport(ctx context.Context, folderID, source string) error
	// SyncImportStates is the per-folder re-ingest bookkeeping the status
	// surfaces read for last-import staleness.
	SyncImportStates(ctx context.Context) ([]store.SyncImportState, error)
}

// ApplyPeers folds the persisted peer registry into the supervisor's config
// inputs: every paired peer becomes a Syncthing device, and each managed
// folder is shared with the peers whose recorded folder set includes it.
// The supervisor regenerates config.xml on every daemon start, so this is
// what makes pairing survive restarts. Pure and deterministic; both serve
// and the desktop shell call it before syncthing.New.
func ApplyPeers(folders []syncthing.Folder, peers []devices.SyncPeer) ([]syncthing.Folder, []syncthing.Device) {
	devs := make([]syncthing.Device, 0, len(peers))
	for _, p := range peers {
		devs = append(devs, syncthing.Device{ID: p.DeviceID, Name: p.Name})
	}
	out := make([]syncthing.Folder, len(folders))
	copy(out, folders)
	for i := range out {
		for _, p := range peers {
			if !containsString(p.Folders, out[i].ID) {
				continue
			}
			if !containsString(out[i].DeviceIDs, p.DeviceID) {
				out[i].DeviceIDs = append(out[i].DeviceIDs, p.DeviceID)
			}
		}
	}
	return out, devs
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
