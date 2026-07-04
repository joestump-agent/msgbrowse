// The privileged Setup Disable flow (issue #162): POST /setup/disable removes
// one source's imported conversations from the store while KEEPING its managed
// archive on disk, so the card returns to Ready and a later Enable is a fast
// local re-import instead of a fresh export.
//
// Disable is a destructive store mutation, so it carries the IDENTICAL
// privileged-POST gate as /setup/enable — same-origin + per-session token +
// MaxBytesReader body cap via checkSetupPOST, unweakened (SPEC-0013 §Security
// "Same-origin protection for privileged POSTs") — AND an inline two-step
// confirmation: the first POST only re-renders the source's card in a confirm
// state (no mutation); only the second POST, carrying confirm=1, deletes. The
// whole affordance is server-rendered htmx (CSP-safe, no JS dialogs). The
// source is read from the fixed enum, never a client path; an unknown source
// is a 400. The managed archive root is NEVER touched here — Disable is a
// store-only operation.
//
// Governing: ADR-0020, SPEC-0013 §Security "Same-origin protection for
// privileged POSTs" + "No arbitrary paths — managed roots only", issue #162.
package web

import (
	"bytes"
	"net/http"

	"github.com/joestump/msgbrowse/internal/source"
)

// handleSetupDisable is POST /setup/disable. It enforces the privileged-POST
// gate FIRST — a failing request is rejected 403 with NOTHING deleted — then
// runs the two-step confirm: without confirm=1 it renders the card's inline
// confirmation and mutates nothing; with it, it deletes the source's imported
// data (the store cascade removes messages, attachments, links, and reactions
// with their conversations) and re-renders the card, which reads Ready again
// from live detection. The response piggybacks the same out-of-band sidebar
// refresh Enable's Done fragment uses, so the removed conversations vanish
// from the sidebar immediately.
func (s *Server) handleSetupDisable(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return // 403 already written; nothing deleted
	}

	src := r.PostFormValue("source")
	if !source.IsKnown(src) {
		http.Error(w, "unknown source", http.StatusBadRequest)
		return
	}

	// Synced-in (replica) sources cannot be disabled here: the guard that
	// protects Enable/Refresh applies equally, or a stale page could delete a
	// replica's imported rows with no UI path to force re-import until the
	// next sync completion (adversarial-review fix on #172). Unpair the
	// importing device instead — the conflict fragment names it.
	if s.renderImporterConflict(w, r, src) {
		return
	}

	ctx := r.Context()
	token, err := s.setupTokens.mint()
	if err != nil {
		s.serverError(w, err)
		return
	}

	// Refuse to pull rows out from under a running Enable/Refresh for the same
	// source: the card re-renders with an explanation instead of deleting.
	if s.enabler != nil {
		if prog, ok := s.enabler.Status(src); ok && prog.Active() {
			card := s.setupCardFor(s.detector(), src, token, s.sourcesPresent(ctx), s.sourceCounts(ctx), s.replicaSources(ctx))
			card.Detail = "A job is running for " + source.Label(src) + " — wait for it to finish before disabling."
			s.renderFragment(w, "setup_card", card)
			return
		}
	}

	if r.PostFormValue("confirm") != "1" {
		// Step 1: no mutation. Render the card in its inline confirm state.
		card := s.setupCardFor(s.detector(), src, token, s.sourcesPresent(ctx), s.sourceCounts(ctx), s.replicaSources(ctx))
		card.ConfirmDisable = true
		s.renderFragment(w, "setup_card", card)
		return
	}

	// Step 2: confirmed. Delete the source's imported data (store only — the
	// managed archive on disk is untouched, so re-enable re-imports locally).
	removed, err := s.store.DeleteSourceData(ctx, src)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.log.Info("setup: source disabled", "source", src, "conversations_removed", removed)

	// Re-render the card from live detection — store-presence is gone, so it
	// reads Ready (or Needs-permission/Not-detected, whatever detection says) —
	// and piggyback the out-of-band sidebar-list refresh + HX-Trigger exactly
	// like Enable's Done fragment (#142), so the removed conversations vanish
	// from the sidebar without a manual nav.
	card := s.setupCardFor(s.detector(), src, token, s.sourcesPresent(ctx), s.sourceCounts(ctx), s.replicaSources(ctx))
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, "setup_card", card); err != nil {
		s.serverError(w, err)
		return
	}
	if base, berr := s.baseData(ctx, "", 0); berr != nil {
		s.log.Warn("setup: could not load base data for post-disable sidebar refresh", "error", berr)
	} else if oob, oerr := s.renderOOB("sidebar_lists_oob", base); oerr != nil {
		s.log.Warn("setup: could not render sidebar refresh after disable", "error", oerr)
	} else {
		buf.WriteString(string(oob))
	}
	w.Header().Set("HX-Trigger", setupImportedTrigger)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}
