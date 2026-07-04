// The privileged Setup Refresh flow: POST /setup/refresh re-runs export +
// incremental import for one already-Enabled source, and POST /setup/refresh-all
// fans that out across every Enabled source. Refresh is the SPEC-0013 REQ
// "Refresh": it re-runs the SAME export→adopt→import pipeline as Enable so only
// the delta lands (the importer is incremental + idempotent), reusing the same
// background-job, progress, cancellation, and concurrency machinery — nothing
// here reimplements the pipeline.
//
// Both routes carry the IDENTICAL privileged-POST gate as /setup/enable —
// same-origin + per-session token + MaxBytesReader body cap via checkSetupPOST,
// unchanged (SPEC-0013 §Security endpoint table: "/setup/refresh … Same as
// /setup/enable … Same-origin required"). A failing gate is rejected 403 with NO
// job started; an unknown source is a 400. The source is read from the fixed
// enum, never a client path.
//
// Governing: ADR-0020, SPEC-0013 REQ "Refresh" (per-source + all-sources manual
// refresh, delta-only), REQ "Concurrency Safety" (one job per source; the
// Runner's guard rejects a duplicate same-source job — the all-sources fan-out
// starts one job per DISTINCT source so it can never corrupt via concurrent
// same-source jobs), §Security "Same-origin protection for privileged POSTs".
package web

import (
	"errors"
	"net/http"

	"github.com/joestump/msgbrowse/internal/onboard"
	"github.com/joestump/msgbrowse/internal/source"
)

// handleSetupRefresh is POST /setup/refresh. It enforces the privileged-POST gate
// FIRST — a failing request is rejected 403 with NO job started — then starts the
// background refresh job for the fixed-enum source and renders the same progress
// fragment Enable uses, so an Enabled card's Refresh shares the aria-live surface
// and the Done sidebar-refresh with Enable. The source is read from a fixed enum
// (never a client path); an unknown source is a 400.
func (s *Server) handleSetupRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return // 403 already written; no job started
	}

	src := r.PostFormValue("source")
	if !source.IsKnown(src) {
		http.Error(w, "unknown source", http.StatusBadRequest)
		return
	}

	// Refresh runs the exporters too, so the importer/replica guard applies
	// exactly as on Enable (#158; SPEC-0014 REQ "Importer and Replica Roles").
	if s.renderImporterConflict(w, r, src) {
		return
	}

	if s.enabler == nil {
		// No orchestrator wired (browser mode with no resolvable tools): render the
		// "unavailable" affordance rather than 500ing, exactly as Enable does.
		s.renderEnableUnavailable(w, r, src)
		return
	}

	prog, err := s.enabler.Refresh(src)
	if err != nil && !errors.Is(err, onboard.ErrJobInProgress) {
		// A start-time error other than "already running" (runner shutting down,
		// unknown source): surface it as a failed progress fragment, not a bare 500.
		s.renderProgressError(w, r, src, err)
		return
	}
	// ErrJobInProgress coalesces the duplicate click onto the live job — the
	// existing job's progress is returned and rendered.
	s.renderProgress(w, r, src, prog)
}

// handleSetupRefreshAll is POST /setup/refresh-all. It refreshes EVERY Enabled
// source: the single all-sources control (SPEC-0013 REQ "Refresh": "an
// all-sources Refresh"). It starts one background job per DISTINCT Enabled source
// via the Runner's per-source job model — so it never spawns two concurrent jobs
// for the same source (the Runner also guards this, per REQ "Concurrency
// Safety"). Each source's own aria-live progress region (which polls
// /setup/status/<source>) then shows that source's live state; this handler
// renders a small summary fragment naming how many sources it kicked off.
//
// Same privileged gate as the per-source route (it starts export jobs). A failing
// gate is 403 with nothing started.
func (s *Server) handleSetupRefreshAll(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return // 403 already written; no job started
	}

	if s.enabler == nil {
		s.renderRefreshAll(w, refreshAllData{Unavailable: true})
		return
	}

	// Kick off one refresh per Enabled source. Enabled matches the Providers
	// cards' signal (setupCardFor): imported conversations in the store
	// (store-presence — the desktop signal, where no cfg root is ever set,
	// issue #160) OR an explicitly configured archive root. Both are app-owned
	// values, never request-derived. A source whose job is already running
	// returns ErrJobInProgress, which is not an error here: it is simply already
	// refreshing, and its own progress region reflects that. Any OTHER
	// start-time error is counted as a failure to start.
	present := s.sourcesPresent(r.Context())
	// Synced-in sources are skipped outright: their importer is a paired peer,
	// and refreshing here would run the exporters against a replica (#158;
	// SPEC-0014 REQ "Importer and Replica Roles").
	replicas := s.replicaSources(r.Context())
	var started, alreadyRunning, failed int
	for _, src := range source.All {
		if _, synced := replicas[src]; synced {
			continue
		}
		if !present[src] && !s.sourceConfigured(src) {
			continue
		}
		_, err := s.enabler.Refresh(src)
		switch {
		case err == nil:
			started++
		case errors.Is(err, onboard.ErrJobInProgress):
			alreadyRunning++
		default:
			failed++
			s.log.Warn("setup: refresh-all could not start a source", "source", src, "error", err)
		}
	}

	s.renderRefreshAll(w, refreshAllData{
		Started:        started,
		AlreadyRunning: alreadyRunning,
		Failed:         failed,
		None:           started == 0 && alreadyRunning == 0 && failed == 0,
	})
}

// refreshAllData drives the setup_refresh_all_result fragment: the summary of an
// all-sources Refresh kick-off. It is a count-only summary (no request-derived
// text), so html/template escaping is the only encoding needed. The live
// per-source state is announced by each card's own aria-live region, not here.
type refreshAllData struct {
	// Started is the number of sources whose refresh job this request started.
	Started int
	// AlreadyRunning is the number of Enabled sources already refreshing (a job was
	// in flight) — coalesced, not an error.
	AlreadyRunning int
	// Failed is the number of sources whose refresh could not be started.
	Failed int
	// None is true when no source was Enabled, so nothing was refreshed.
	None bool
	// Unavailable marks the "no Enabler / no tools" affordance state.
	Unavailable bool
}

// renderRefreshAll renders the all-sources Refresh summary fragment into its
// aria-live region.
func (s *Server) renderRefreshAll(w http.ResponseWriter, data refreshAllData) {
	s.renderFragment(w, "setup_refresh_all_result", data)
}
