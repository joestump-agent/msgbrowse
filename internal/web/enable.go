// The privileged Setup Enable flow: POST /setup/enable starts a cancellable
// background export→adopt→import job for a source, and GET /setup/status/{source}
// returns its live structured state for the aria-live progress surface. This is
// where the read-only Setup page (#132) becomes live (SPEC-0013 REQ "One-click
// enable and import per source").
//
// The web layer CANNOT import the cgo desktop module, so the actual
// orchestration lives behind the Enabler seam (mirroring SetDetector /
// SetPairingSource): the desktop shell injects an implementation backed by the
// BUNDLED exporter toolchain (embedded.bundledResolver, which makes
// internal/toolchain.ResolveExporters live at the export site), and `msgbrowse
// serve` injects a $PATH/config-backed one — or leaves Enable disabled (no
// Enabler wired) with a clear affordance when no tools resolve. The seam type is
// internal/onboard.Progress et al., a pure-Go package with no cgo, so the
// handlers here stay testable.
//
// Governing: SPEC-0013 REQ "One-click enable and import per source", REQ
// "Concurrency Safety" (one job per source; a duplicate Enable is rejected), REQ
// "Error Handling Standards" (structured progress + errors surfaced, no silent
// swallow), §Security (privileged POSTs gated by same-origin + per-session
// token, source from a fixed enum, MaxBytesReader body cap), §Accessibility
// (progress announced through an aria-live region polled by htmx).
package web

import (
	"bytes"
	"context"
	"errors"
	"html/template"
	"net/http"

	"github.com/joestump/msgbrowse/internal/onboard"
	"github.com/joestump/msgbrowse/internal/source"
)

// Enabler is the seam the desktop/serve shell implements to run the privileged
// Enable job. It is a thin projection of internal/onboard.Runner so the web
// layer depends only on the pure orchestration package, never on the cgo desktop
// module or the concrete store. The shell wires a real implementation with
// SetEnabler before serving; with no Enabler wired, Enable is disabled in the UI
// and the POST returns a clear "unavailable" state rather than a subprocess.
type Enabler interface {
	// Enable starts (or reports the already-running) export→import job for src and
	// returns its initial progress. A second Enable while one is in flight returns
	// onboard.ErrJobInProgress and starts nothing.
	Enable(src string) (onboard.Progress, error)
	// Refresh re-runs the SAME export→incremental-import pipeline on an
	// already-Enabled source, importing only the delta (SPEC-0013 REQ "Refresh").
	// It shares Enable's per-source concurrency guard: a Refresh while an Enable or
	// Refresh for src is in flight returns onboard.ErrJobInProgress and starts
	// nothing.
	Refresh(src string) (onboard.Progress, error)
	// Status returns the current job progress for src, ok=false when none has run.
	Status(src string) (onboard.Progress, bool)
	// Cancel requests cancellation of src's in-flight job; true if one was running.
	Cancel(src string) bool
}

// SetEnabler wires the privileged-Enable orchestrator into /setup/enable. Call
// it after NewServer and before serving begins — handlers read the field without
// locking, so late wiring would race (the SetDetector / SetPairingSource
// contract). Leaving it unset keeps Enable disabled: the Setup cards render a
// "desktop app required / configure tools" affordance and the POST 501s.
func (s *Server) SetEnabler(e Enabler) { s.enabler = e }

// enableAvailable reports whether an Enabler is wired, so the Setup template can
// render a live Enable button versus the disabled "unavailable" affordance.
func (s *Server) enableAvailable() bool { return s.enabler != nil }

// handleSetupEnable is POST /setup/enable. It enforces the privileged-POST gate
// (same-origin + per-session token + body cap) FIRST — a failing request is
// rejected 403 with NO job started — then starts the background job for the
// fixed-enum source and renders the progress fragment. The source is read from a
// fixed enum (never a client path); an unknown source is a 400.
func (s *Server) handleSetupEnable(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return // 403 already written; no subprocess started
	}

	src := r.PostFormValue("source")
	if !source.IsKnown(src) {
		http.Error(w, "unknown source", http.StatusBadRequest)
		return
	}

	if s.enabler == nil {
		// No orchestrator wired (e.g. browser mode with no resolvable tools):
		// render the source's card with an "unavailable" note rather than 500ing.
		s.renderEnableUnavailable(w, r, src)
		return
	}

	prog, err := s.enabler.Enable(src)
	if err != nil && !errors.Is(err, onboard.ErrJobInProgress) {
		// A start-time error other than "already running" (e.g. runner shutting
		// down, unknown source): surface it as a failed progress fragment, not a
		// bare 500 — the UI must show the reason (SPEC-0013 error handling).
		s.renderProgressError(w, r, src, err)
		return
	}
	// ErrJobInProgress is not an error to the user — the existing job's progress
	// is returned and rendered, coalescing the duplicate click onto the live job.
	s.renderProgress(w, r, src, prog)
}

// handleSetupStatus is GET /setup/status/{source}. It returns the current job
// progress fragment for the aria-live region, polled by htmx (hx-trigger="every
// 1s") while the job is active. It is a safe GET (no mutation), so it needs no
// token — reading status leaks nothing a same-origin page cannot already see.
func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	src := r.PathValue("source")
	if !source.IsKnown(src) {
		http.NotFound(w, r)
		return
	}
	if s.enabler == nil {
		s.renderEnableUnavailable(w, r, src)
		return
	}
	prog, ok := s.enabler.Status(src)
	if !ok {
		// No job has run for this source: render an idle fragment so the poller has
		// a stable target (it will not be polling in this state, but a stray GET is
		// answered cleanly).
		s.renderProgress(w, r, src, onboard.Progress{Source: src})
		return
	}
	s.renderProgress(w, r, src, prog)
}

// handleSetupCancel is POST /setup/cancel. Same privileged gate as Enable (it
// changes job state); it requests cancellation and renders the updated progress.
func (s *Server) handleSetupCancel(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return
	}
	src := r.PostFormValue("source")
	if !source.IsKnown(src) {
		http.Error(w, "unknown source", http.StatusBadRequest)
		return
	}
	if s.enabler == nil {
		s.renderEnableUnavailable(w, r, src)
		return
	}
	s.enabler.Cancel(src)
	prog, _ := s.enabler.Status(src)
	prog.Source = src
	s.renderProgress(w, r, src, prog)
}

// progressData drives the setup_progress fragment: the live job state for one
// source, rendered into an aria-live region. It carries a fresh per-session
// token so the Cancel button inside the fragment can POST, and a Polling flag so
// the fragment self-schedules the next htmx poll only while the job is active.
type progressData struct {
	Source    string
	Label     string
	Phase     string
	Message   string
	ErrText   string
	Active    bool
	Done      bool
	Failed    bool
	Cancelled bool
	Token     string
	// Unavailable marks the "no Enabler / no tools" affordance state.
	Unavailable bool
	// SidebarOOB is the pre-rendered out-of-band sidebar-list swap appended to a
	// Done fragment so the newly-imported conversations appear immediately (#142).
	// Empty for every non-Done render.
	SidebarOOB template.HTML
}

// setupImportedTrigger is the HX-Trigger event name emitted on a successful
// Enable→import (SPEC-0013 REQ "One-click enable and import per source": "the
// source appears in the transcript sidebar"). setup.js listens for it and
// refreshes the sidebar so newly-imported conversations appear without a manual
// nav — the payoff of the whole flow (#142 fold-in). It is a plain event name
// (no request-derived data), safe in the HX-Trigger header.
const setupImportedTrigger = "msgbrowse:imported"

// renderProgress renders the setup_progress fragment for a job snapshot. On a
// terminal Done phase it also (a) emits the HX-Trigger so the client refreshes
// the sidebar, and (b) piggybacks an out-of-band swap of the sidebar
// conversation list in the same response, so the newly-imported conversations
// appear immediately even before/without the trigger-driven refetch (#142).
func (s *Server) renderProgress(w http.ResponseWriter, r *http.Request, src string, prog onboard.Progress) {
	tok, err := s.setupTokens.mint()
	if err != nil {
		s.serverError(w, err)
		return
	}
	data := progressData{
		Source:    src,
		Label:     source.Label(src),
		Phase:     string(prog.Phase),
		Message:   prog.Message,
		ErrText:   prog.ErrText(),
		Active:    prog.Active(),
		Done:      prog.Phase == onboard.PhaseDone,
		Failed:    prog.Phase == onboard.PhaseFailed,
		Cancelled: prog.Phase == onboard.PhaseCancelled,
		Token:     tok,
	}
	if data.Message == "" && !data.Active {
		data.Message = "Ready to enable."
	}

	if data.Done {
		// Payoff moment (#142): the import just landed, so tell the client to
		// refresh the sidebar (HX-Trigger) AND swap the fresh conversation list in
		// out-of-band, so the new conversations show up without a manual nav. A
		// listing failure is not fatal to reporting Done — fall back to the trigger
		// alone rather than turning a successful import into an error.
		w.Header().Set("HX-Trigger", setupImportedTrigger)
		if oob, err := s.sidebarOOB(r.Context()); err == nil {
			data.SidebarOOB = oob
		} else {
			s.log.Warn("setup: could not render sidebar refresh after import", "error", err)
		}
	}
	s.renderFragment(w, "setup_progress", data)
}

// sidebarOOB renders the current conversation list as the out-of-band sidebar
// swap appended to the Done fragment (#142). It returns the pre-rendered,
// already-escaped HTML for the #sidebar-conversations and #sidebar-pinned lists
// carrying hx-swap-oob so htmx replaces the live sidebar in place. It runs the
// same ListConversations the full-page shell uses, so the swapped rows are
// pixel-identical to a fresh load.
func (s *Server) sidebarOOB(ctx context.Context) (template.HTML, error) {
	base, err := s.baseData(ctx, "", 0)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, "sidebar_lists_oob", base); err != nil {
		return "", err
	}
	// The template output is server-composed from the trusted conv_row partial
	// (message bodies are never in the sidebar), so it is safe to mark as HTML for
	// interpolation into the fragment.
	return template.HTML(buf.String()), nil //nolint:gosec // server-rendered sidebar markup, no user HTML
}

// renderProgressError renders a failed progress fragment for a start-time error
// (before any job state exists), so the UI shows the reason.
func (s *Server) renderProgressError(w http.ResponseWriter, r *http.Request, src string, err error) {
	tok, terr := s.setupTokens.mint()
	if terr != nil {
		s.serverError(w, terr)
		return
	}
	s.renderFragment(w, "setup_progress", progressData{
		Source:  src,
		Label:   source.Label(src),
		Phase:   string(onboard.PhaseFailed),
		Message: "Could not start: " + err.Error(),
		ErrText: err.Error(),
		Failed:  true,
		Token:   tok,
	})
}

// renderEnableUnavailable renders the "Enable unavailable" fragment (no Enabler
// wired / no resolvable tools) so browser-mode Setup explains why Enable is not
// live instead of erroring.
func (s *Server) renderEnableUnavailable(w http.ResponseWriter, r *http.Request, src string) {
	s.renderFragment(w, "setup_progress", progressData{
		Source:      src,
		Label:       source.Label(src),
		Unavailable: true,
		Message:     "Enabling requires the desktop app with its bundled exporters, or a configured exporter path.",
	})
}

// renderFragment executes a standalone fragment template (no *_content /
// full-page wrapping) through the same buffered, escaped render path the main
// render uses, so a template error never produces a half-written response. It is
// used for the htmx-swapped progress fragments, which are never full pages.
func (s *Server) renderFragment(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}
