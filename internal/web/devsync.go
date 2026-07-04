// The device-sync observability seams (#158): the SyncMonitor behind the
// Providers cards' importer/replica role enforcement and the Settings/Status
// sync-state rendering, plus the sync event feed the Logs page shows beside
// the shell notes. Both follow the SetDetector/SetEnabler/SetPairingSource
// pattern — serve and the desktop shell wire internal/devsync's Manager and
// Notes over the supervised engine; browser mode with sync disabled wires
// nothing and every surface renders its labeled absent state.
//
// Governing: ADR-0021 ("doctor/status" is msgbrowse-owned), SPEC-0014 REQ
// "Status and Doctor Surfacing" (Syncthing state into Settings/Logs/Status;
// the user never opens Syncthing's GUI), REQ "Importer and Replica Roles"
// (the ReplicaSources projection backing single-importer enforcement).
package web

import (
	"context"

	"github.com/joestump/msgbrowse/internal/devsync"
)

// SyncMonitor is the device-sync state source the web layer reads:
// internal/devsync's *Manager implements it; tests substitute fakes.
type SyncMonitor interface {
	// Status collects the live snapshot: engine running?, per-peer
	// connection state, per-folder health/completion. Engine
	// unreachability is a value (Running=false), never an error.
	Status(ctx context.Context) (*devsync.Status, error)
	// ReplicaSources reports the sources THIS node is a replica for
	// (source id → the paired peer recorded as its importer) — the input to
	// the Providers synced-card state and the Enable/Refresh conflict guard.
	ReplicaSources(ctx context.Context) (map[string]devsync.ReplicaOf, error)
}

// SetSyncMonitor wires the device-sync state source. Call after NewServer and
// before serving begins — handlers read the field without locking, so late
// wiring would race (the SetPairingSource contract).
func (s *Server) SetSyncMonitor(m SyncMonitor) { s.syncMonitor = m }

// SetSyncNotes wires the device-sync event feed for the Logs page (#158).
// fn is called per /logs render and must be safe for concurrent use; the
// returned notes are server-owned strings, rendered through html/template
// like everything else. Call before serving.
func (s *Server) SetSyncNotes(fn func() []devsync.Note) { s.syncNotes = fn }

// replicaSources reads the synced-in source map, nil-safe for browser mode
// (no monitor wired) and degradation-tolerant: a read failure is logged and
// yields nil — the Providers page then renders from detection alone, which
// at worst shows Enable on a replica for one render; the POST-side guard in
// handleSetupEnable re-checks before any subprocess starts.
func (s *Server) replicaSources(ctx context.Context) map[string]devsync.ReplicaOf {
	if s.syncMonitor == nil {
		return nil
	}
	reps, err := s.syncMonitor.ReplicaSources(ctx)
	if err != nil {
		s.log.Warn("setup: could not read device-sync roles", "error", err)
		return nil
	}
	return reps
}

// syncStatusSnapshot reads the live device-sync snapshot, nil when no monitor
// is wired or the registry read failed (logged, page renders without the
// card's live section — SPEC-0014 REQ "Error Handling Standards": surfaced,
// never fatal to an unrelated page).
func (s *Server) syncStatusSnapshot(ctx context.Context) *devsync.Status {
	if s.syncMonitor == nil {
		return nil
	}
	st, err := s.syncMonitor.Status(ctx)
	if err != nil {
		s.log.Warn("could not collect device-sync status", "error", err)
		return nil
	}
	return st
}

// syncNoteSnapshot reads the device-sync event feed, or nothing when no
// provider is wired (sync disabled / browser mode).
func (s *Server) syncNoteSnapshot() []devsync.Note {
	if s.syncNotes == nil {
		return nil
	}
	return s.syncNotes()
}
