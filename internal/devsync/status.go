// The status projection: Syncthing's live state — engine up?, per-peer
// connection, per-folder health/completion — read from the REST API and
// joined with msgbrowse's own registry (paired peers, roles, last-import
// bookkeeping) into one struct the Settings page, the /status card, the
// `devices status` CLI, and doctor all render. The user NEVER opens
// Syncthing's GUI; this mapping is how its state reaches msgbrowse's
// surfaces.
//
// Collection is deliberately degradation-tolerant (SPEC-0014 REQ "Error
// Handling Standards"): an unreachable engine yields Running=false with the
// registry-derived rows intact and states marked unknown — a truthful
// "engine down" render, never a 500 on an unrelated page. Per-folder REST
// failures degrade that folder to HealthUnknown with the error logged.
//
// Governing: ADR-0021, SPEC-0014 REQ "Status and Doctor Surfacing" ("connected
// peers, per-folder completion percentage, and errors ... into the
// Settings/Logs/Status views and into doctor"), REQ "Importer and Replica
// Roles" (the ReplicaSources projection that backs role enforcement).
package devsync

import (
	"context"
	"time"

	"github.com/joestump/msgbrowse/internal/devices"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/syncthing"
)

// Folder health tokens — msgbrowse's own defensive enum over Syncthing's
// open state set, so templates and doctor branch on a fixed vocabulary.
const (
	// HealthHealthy: idle at rest with no reported errors.
	HealthHealthy = "healthy"
	// HealthSyncing: actively scanning/transferring — normal, in-progress.
	HealthSyncing = "syncing"
	// HealthPaused: the folder is configured paused; no sync is attempted.
	HealthPaused = "paused"
	// HealthError: the daemon reports an error state or failed items.
	HealthError = "error"
	// HealthUnknown: the engine is down or the folder's status was
	// unreadable; nothing is asserted.
	HealthUnknown = "unknown"
)

// Status is one collected snapshot of device-sync state.
type Status struct {
	// Running reports the engine answered its REST API for this snapshot.
	Running bool
	// MyID is this node's own device ID ("" when the engine is down).
	MyID string
	// UptimeSeconds is the daemon's uptime (0 when down).
	UptimeSeconds int64
	// Peers is one row per REGISTRY peer (the explicitly-paired set), with
	// live connection state joined in when the engine is up.
	Peers []PeerStatus
	// Folders is one row per managed folder, with daemon-side health joined
	// in when the engine is up.
	Folders []FolderStatus
}

// PeerStatus is one paired peer with its live connection state.
type PeerStatus struct {
	devices.SyncPeer
	// StateKnown is false when the engine was unreachable (Connected is then
	// meaningless and surfaces render "unknown", never a fake "disconnected").
	StateKnown bool
	// Connected / Paused / Address mirror the daemon's connection entry.
	Connected bool
	Paused    bool
	Address   string
}

// FolderStatus is one managed folder's health snapshot.
type FolderStatus struct {
	// ID is the Syncthing folder id ("msgbrowse-<source>"); Source and Label
	// are its source id and human name.
	ID     string
	Source string
	Label  string
	// Health is one of the Health* tokens above; State is the daemon's raw
	// state string for diagnostics ("" when unknown).
	Health string
	State  string
	// Completion is the aggregate completion percentage (0–100; meaningful
	// only when Health != HealthUnknown).
	Completion float64
	// Errors counts the daemon's failed items (pull/permission errors).
	Errors int
	// LastImportAt is when a completed sync last triggered the incremental
	// re-ingest here (zero when never; from the sync_state bookkeeping).
	LastImportAt time.Time
}

// Status collects the snapshot. It returns an error only for registry
// failures (the store is msgbrowse's own state and must be readable); engine
// unreachability is a VALUE (Running=false), not an error.
func (m *Manager) Status(ctx context.Context) (*Status, error) {
	peers, err := m.st.ListSyncPeers(ctx)
	if err != nil {
		return nil, err
	}
	st := &Status{}

	lastImports := make(map[string]time.Time)
	if states, err := m.st.SyncImportStates(ctx); err != nil {
		m.log.Warn("status: could not read sync import states", "error", err)
	} else {
		for _, s := range states {
			lastImports[s.FolderID] = s.LastImportAt
		}
	}

	sys, sysErr := m.api.SystemStatus(ctx)
	if sysErr == nil {
		st.Running = true
		st.MyID = sys.MyID
		st.UptimeSeconds = sys.Uptime
	} else {
		m.log.Warn("status: engine unreachable", "error", sysErr)
	}

	var conns map[string]syncthing.ConnectionInfo
	if st.Running {
		if c, err := m.api.Connections(ctx); err != nil {
			m.log.Warn("status: could not read connections", "error", err)
		} else {
			conns = c.Connections
		}
	}
	for _, p := range peers {
		ps := PeerStatus{SyncPeer: p}
		if conns != nil {
			ps.StateKnown = true
			if ci, ok := conns[p.DeviceID]; ok {
				ps.Connected = ci.Connected
				ps.Paused = ci.Paused
				ps.Address = ci.Address
			}
		}
		st.Peers = append(st.Peers, ps)
	}

	// Folder pause state lives in the daemon CONFIG, not /rest/db/status.
	paused := make(map[string]bool)
	if st.Running {
		if folders, err := m.api.GetFolders(ctx); err != nil {
			m.log.Warn("status: could not read folder config", "error", err)
		} else {
			for _, f := range folders {
				paused[f.ID] = f.Paused
			}
		}
	}
	for _, f := range m.folders.List() {
		src, _ := SourceForFolderID(f.ID)
		fs := FolderStatus{
			ID:           f.ID,
			Source:       src,
			Label:        source.Label(src),
			Health:       HealthUnknown,
			LastImportAt: lastImports[f.ID],
		}
		if st.Running {
			fs.Health, fs.State, fs.Completion, fs.Errors = m.folderHealth(ctx, f.ID, paused[f.ID])
		}
		st.Folders = append(st.Folders, fs)
	}
	return st, nil
}

// folderHealth reads one folder's daemon-side status + completion and maps
// them onto the fixed health enum. Any REST failure degrades to
// HealthUnknown with the error logged — never swallowed, never fatal to the
// snapshot (SPEC-0014 REQ "Error Handling Standards").
func (m *Manager) folderHealth(ctx context.Context, folderID string, cfgPaused bool) (health, state string, completion float64, errCount int) {
	fst, err := m.api.FolderStatus(ctx, folderID)
	if err != nil {
		m.log.Warn("status: could not read folder status", "folder", folderID, "error", err)
		return HealthUnknown, "", 0, 0
	}
	comp, err := m.api.FolderCompletion(ctx, folderID, "")
	if err != nil {
		m.log.Warn("status: could not read folder completion", "folder", folderID, "error", err)
		return HealthUnknown, fst.State, 0, fst.Errors + fst.PullErrors
	}
	errCount = fst.Errors + fst.PullErrors
	state = fst.State
	completion = comp.CompletionPct
	switch {
	case cfgPaused || state == "paused":
		health = HealthPaused
	case errCount > 0 || state == "error" || state == "outofsync":
		health = HealthError
	case state == "syncing" || state == "sync-preparing" || state == "sync-waiting" ||
		state == "scanning" || state == "scan-waiting" || state == "cleaning" ||
		comp.NeedItems > 0 || comp.NeedDeletes > 0:
		health = HealthSyncing
	case state == "idle":
		health = HealthHealthy
	default:
		// An unrecognized state token from a newer daemon: report it
		// truthfully as unknown rather than guessing healthy.
		health = HealthUnknown
	}
	return health, state, completion, errCount
}

// ReplicaOf names the peer that is a synced-in source's importer.
type ReplicaOf struct {
	// PeerName / PeerShortID identify the importer peer for display and for
	// the SPEC-0014 conflict error ("an error identifying the existing
	// importer").
	PeerName    string
	PeerShortID string
}

// ReplicaSources reports the sources THIS node is a replica for: source id →
// the paired peer recorded as its importer. It backs the Providers card's
// synced state and the Enable/Refresh conflict guard (SPEC-0014 REQ
// "Importer and Replica Roles": single importer per source across the paired
// set). A source appears here only while its importer peer is still paired —
// unpairing releases the claim.
func (m *Manager) ReplicaSources(ctx context.Context) (map[string]ReplicaOf, error) {
	peers, err := m.st.ListSyncPeers(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]ReplicaOf)
	for _, p := range peers {
		for src, role := range p.Roles {
			if role == devices.RoleImporter {
				out[src] = ReplicaOf{PeerName: p.Name, PeerShortID: p.ShortID()}
			}
		}
	}
	return out, nil
}
