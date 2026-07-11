// The privileged Setup Refresh flow: POST /setup/refresh re-runs export +
// incremental import for one already-Enabled source. Refresh is the SPEC-0013
// REQ "Refresh": it re-runs the SAME export→adopt→import pipeline as Enable so
// only the delta lands (the importer is incremental + idempotent), reusing the
// same background-job, progress, cancellation, and concurrency machinery. The
// one-shot all-sources control was retired in favor of the background
// auto-refresh scheduler (autorefresh.go), which reuses refreshEnabledSources.
//
// The route carries the IDENTICAL privileged-POST gate as /setup/enable —
// same-origin + per-session token + MaxBytesReader body cap via checkSetupPOST,
// unchanged (SPEC-0013 §Security endpoint table: "/setup/refresh … Same as
// /setup/enable … Same-origin required"). A failing gate is rejected 403 with NO
// job started; an unknown source is a 400. The source is read from the fixed
// enum, never a client path.
//
// Governing: ADR-0020, SPEC-0013 REQ "Refresh" (per-source manual refresh,
// delta-only), REQ "Concurrency Safety" (one job per source; the Runner's guard
// rejects a duplicate same-source job).
package web

import (
	"context"
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

// refreshEnabledSources starts a background refresh job for every Enabled,
// non-replica source and returns how many it kicked off. It is the shared core
// behind the auto-refresh scheduler (autorefresh.go): "Enabled" matches the
// Providers cards' signal (setupCardFor) — imported conversations in the store
// (store-presence, the desktop signal where no cfg root is ever set, issue
// #160) OR an explicitly configured archive root.
//
// Synced-in sources are skipped: their importer is a paired peer, so refreshing
// here would run the exporters against a replica (#158; SPEC-0014 REQ "Importer
// and Replica Roles"). A source whose job is already running returns
// ErrJobInProgress, which is not an error here. The Runner's per-source guard
// makes this safe to call concurrently with a manual Refresh: at most one job
// per source ever runs.
func (s *Server) refreshEnabledSources(ctx context.Context) int {
	if s.enabler == nil {
		return 0
	}
	present := s.sourcesPresent(ctx)
	replicas := s.replicaSources(ctx)
	started := 0
	for _, src := range source.All {
		if _, synced := replicas[src]; synced {
			continue
		}
		if !present[src] && !s.sourceConfigured(src) {
			continue
		}
		switch _, err := s.enabler.Refresh(src); {
		case err == nil:
			started++
		case errors.Is(err, onboard.ErrJobInProgress):
			// Already refreshing; nothing to do.
		default:
			s.log.Warn("auto-refresh: could not start a source", "source", src, "error", err)
		}
	}
	return started
}
