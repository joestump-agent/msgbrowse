// The Logs viewer (issue #151): a diagnostic surface under Settings showing each
// source's most recent Enable/Refresh job — the exporter command line, its exit
// status, the captured stderr/stdout tail, and the import counts. Today an Enable
// failure surfaced only as "exit status N" with no detail; the onboard Runner now
// captures the exporter's combined output into the job state (a bounded per-source
// ring buffer — TOOL output only, never message content, never persisted to
// disk), and this page renders it so a WhatsApp exit-2 (or any exporter failure)
// is finally visible.
//
// It is a safe GET (no mutation, no subprocess, no token): it only reads the live
// per-source job snapshots the Runner already holds, so it needs no privileged
// gate. A source whose job is still running self-polls (aria-live) so the log tail
// updates in place, matching the Setup progress region's accessibility contract.
//
// Governing: SPEC-0013 REQ "Error Handling Standards" (errors surfaced as
// structured state, never swallowed — this is where the captured detail is read),
// §Accessibility (aria-live for a running job, state as text), §Security (GET is
// safe; the rendered command line + exporter output are server-owned strings,
// html/template-escaped).
package web

import (
	"net/http"

	"github.com/joestump/msgbrowse/internal/onboard"
	"github.com/joestump/msgbrowse/internal/source"
)

// logEntry is one source's most-recent job log, projected for the Logs template.
// Every field is a server-computed value (the resolved tool path, app-assembled
// argv, exporter output tail, exit status, import counts) — no request-derived
// content — so html/template escaping is the only encoding needed.
type logEntry struct {
	// Source is the fixed source id; Label its human name.
	Source string
	Label  string
	// HasJob reports whether any Enable/Refresh job has ever run for this source.
	// When false the entry renders an "no runs yet" placeholder rather than an
	// empty log.
	HasJob bool
	// Phase is the job's current lowercase phase token; Active is true while it is
	// still running (drives the aria-live self-poll).
	Phase  string
	Active bool
	// StatusText is a human one-line status (the job's Message): "Enabled Signal —
	// …", "WhatsApp export failed: exit status 2", etc.
	StatusText string
	// Failed / Done / Cancelled classify the terminal state for styling.
	Failed    bool
	Done      bool
	Cancelled bool
	// Command is the exporter command line that ran (tool + argv), "" before the
	// export step. ExitStatus is "0" on success or the exit/error string.
	Command    string
	ExitStatus string
	// Output is the captured combined stdout+stderr tail — the diagnostic detail
	// (the WhatsApp exit-2 argparse message). "" when nothing was captured.
	Output string
	// Summary is the import outcome once the job completed (for the counts line).
	Summary onboard.ImportResult
}

// logsData drives the Logs page. It embeds baseData for the shell and carries one
// entry per source in source.All order, so the page always lists all three.
type logsData struct {
	baseData
	// Entries is one log entry per supported source, in source.All order.
	Entries []logEntry
	// EnableAvailable reports whether an Enabler is wired: false renders the "logs
	// appear after an Enable/Refresh in the desktop app" affordance instead of
	// empty entries.
	EnableAvailable bool
	// AnyActive is true when at least one source's job is still running, so the
	// entries region self-polls (aria-live) to update the running log tail in place.
	AnyActive bool
}

// handleLogs renders the Logs viewer. GET-only (the route pattern enforces it);
// it reads only the live per-source job snapshots the Runner holds, never
// mutating state or spawning a subprocess. It follows the SPEC-0008 *_content
// partial pattern: a boosted navigation gets only <title> + #main-content.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	entries := make([]logEntry, 0, len(source.All))
	anyActive := false
	for _, src := range source.All {
		e := s.logEntryFor(src)
		if e.Active {
			anyActive = true
		}
		entries = append(entries, e)
	}

	// A running job's aria-live region self-polls /logs?fragment=entries and swaps
	// just the entries block, so the captured tail updates in place without a full
	// reload. This is a safe GET (no mutation), so no token is needed.
	if r.URL.Query().Get("fragment") == "entries" {
		s.renderFragment(w, "logs_entries", logsData{
			Entries:         entries,
			EnableAvailable: s.enableAvailable(),
			AnyActive:       anyActive,
		})
		return
	}

	var base baseData
	if isPartialRequest(r) {
		base = partialBase("Logs · msgbrowse", 0)
	} else {
		var err error
		base, err = s.baseData(r.Context(), "Logs · msgbrowse", 0)
		if err != nil {
			s.serverError(w, err)
			return
		}
	}
	s.render(w, r, "logs", logsData{
		baseData:        base,
		Entries:         entries,
		EnableAvailable: s.enableAvailable(),
		AnyActive:       anyActive,
	})
}

// logEntryFor projects one source's most-recent job snapshot into a logEntry. A
// source with no job (or no Enabler wired) reports HasJob=false so the template
// renders the placeholder.
func (s *Server) logEntryFor(src string) logEntry {
	entry := logEntry{Source: src, Label: source.Label(src)}
	if s.enabler == nil {
		return entry
	}
	prog, ok := s.enabler.Status(src)
	if !ok {
		return entry
	}
	entry.HasJob = true
	entry.Phase = string(prog.Phase)
	entry.Active = prog.Active()
	entry.StatusText = prog.Message
	entry.Failed = prog.Phase == onboard.PhaseFailed
	entry.Done = prog.Phase == onboard.PhaseDone
	entry.Cancelled = prog.Phase == onboard.PhaseCancelled
	entry.Command = prog.Log.ArgvLine()
	entry.ExitStatus = prog.Log.ExitStatus
	entry.Output = prog.Log.Output
	entry.Summary = prog.Log.Summary
	return entry
}
