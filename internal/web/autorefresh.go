// The background auto-refresh scheduler: msgbrowse re-runs each Enabled source's
// export→incremental-import pipeline on a fixed interval so the archive stays
// current without a manual click (replacing the retired "Refresh all sources"
// button). It reuses refreshEnabledSources (refresh.go), so it inherits the same
// Enabled-source selection, replica skip, and per-source concurrency guard — an
// auto-refresh can never collide with a manual Refresh.
//
// Governing: SPEC-0013 REQ "Refresh" (delta-only, incremental), REQ
// "Concurrency Safety" (one job per source).
package web

import (
	"context"
	"time"
)

// StartAutoRefresh runs the periodic refresh loop until ctx is cancelled. It is
// a no-op (returns immediately) when no Enabler is wired — browser mode with no
// resolvable exporters has nothing to refresh — or when interval <= 0, the
// explicit "auto-refresh off" setting. The shell starts it in its own goroutine
// after SetEnabler; it does NOT fire an immediate refresh on start (boot
// ingest-on-start already covers launch freshness), only on each tick.
func (s *Server) StartAutoRefresh(ctx context.Context, interval time.Duration) {
	if s.enabler == nil || interval <= 0 {
		return
	}
	s.log.Info("provider auto-refresh enabled", "interval", interval.String())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n := s.refreshEnabledSources(ctx); n > 0 {
				s.log.Info("provider auto-refresh started jobs", "sources", n)
			}
		}
	}
}
