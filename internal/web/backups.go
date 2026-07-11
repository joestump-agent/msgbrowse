// The Backups tab (issue #2): the encrypted-DB-snapshots inventory graduated
// out of /status into its own Settings-shell section at /backups. It renders
// ONLY the snapshot story — total footprint, snapshot count, and the
// per-snapshot table (name, taken-at, size, retention tier) — reusing
// store.ListSnapshots and the footprint sum that used to live in handleStatus.
//
// HasSnapshotPipeline is preserved verbatim from #164: a machine with no
// snapshot pipeline (the desktop-onboarded shape — no recorded snapshots, no
// .snapshots dir in the signal archive) shows one neutral line instead of a
// "0 B across 0 snapshots" card that read like a failure.
package web

import (
	"net/http"

	"github.com/joestump/msgbrowse/internal/store"
)

// backupsData drives the Backups tab. It carries the snapshot inventory that
// used to hang off statusData; the surface has no stat strip, so it needs
// neither the global counts nor the ingest run.
type backupsData struct {
	baseData
	Snapshots         []store.Snapshot
	SnapshotFootprint int64
	// HasSnapshotPipeline gates the Encrypted-DB-snapshots card (issue #164):
	// true when snapshots are recorded or the signal archive carries a
	// .snapshots directory; false renders one neutral line instead of the card.
	HasSnapshotPipeline bool
}

// handleBackups renders the Backups tab — the snapshot inventory only. Like
// the other Settings-shell sections it is a safe GET with no privileged work.
// The boosted-partial path (REQ-0008-006) skips the sidebar listing via
// partialBase; the snapshot query is cheap enough to run on both paths.
func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var base baseData
	if isPartialRequest(r) {
		base = partialBase("Backups · msgbrowse", 0)
	} else {
		var err error
		base, err = s.baseData(ctx, "Backups · msgbrowse", 0)
		if err != nil {
			s.serverError(w, err)
			return
		}
	}
	snaps, err := s.store.ListSnapshots(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	var footprint int64
	for _, sn := range snaps {
		footprint += sn.SizeBytes
	}
	s.render(w, r, "backups", backupsData{
		baseData:            base,
		Snapshots:           snaps,
		SnapshotFootprint:   footprint,
		HasSnapshotPipeline: len(snaps) > 0 || s.signalSnapshotsDirExists(),
	})
}
