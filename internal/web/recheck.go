// The privileged Setup Recheck flow: POST /setup/recheck re-runs the OS-consent
// permission probe for one source and re-renders its card, so a user who has
// just granted Full Disk Access (or answered the Signal Keychain "Always Allow"
// prompt) sees the card flip out of Needs-permission without reloading the page
// (SPEC-0013 REQ "Permission detection and guidance": "It MUST provide a Recheck
// action that re-runs the permission probe on the user's return and updates the
// card state").
//
// Recheck is read-only in effect (it only re-probes the filesystem/keychain) but
// state-changing in the UI, so it carries the SAME same-origin + per-session
// token gate as /setup/enable — reusing checkSetupPOST unchanged (SPEC-0013
// §Security endpoint table: "/setup/recheck … same-origin protected for
// consistency with the other POSTs"). It spawns no subprocess; a failing gate is
// rejected 403 before any probe runs.
//
// Governing: ADR-0020 (OS consent gates are detect-and-guide only — Recheck
// re-detects, never bypasses), SPEC-0013 REQ "Permission detection and
// guidance", §Security "Same-origin protection for privileged POSTs".
package web

import (
	"net/http"

	"github.com/joestump/msgbrowse/internal/source"
)

// handleSetupRecheck is POST /setup/recheck. It enforces the privileged-POST gate
// (same-origin + per-session token + body cap) FIRST — a failing request is
// rejected 403 with NO probe run — then re-computes the card for the fixed-enum
// source and swaps it in. The source is read from a fixed enum (never a client
// path); an unknown source is a 400.
func (s *Server) handleSetupRecheck(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return // 403 already written; no probe run
	}

	src := r.PostFormValue("source")
	if !source.IsKnown(src) {
		http.Error(w, "unknown source", http.StatusBadRequest)
		return
	}

	// Mint a fresh token for the re-rendered card's own Enable/Recheck controls,
	// so the swapped-in card can drive the next privileged POST (the page token
	// this request carried is still valid, but each rendered control gets a live
	// token per the mint-at-render contract).
	token, err := s.setupTokens.mint()
	if err != nil {
		s.serverError(w, err)
		return
	}

	// Re-run detection + the OS-consent probe for just this source and render the
	// resulting card fragment. The card is an <li> that hx-swap="outerHTML"
	// replaces the existing card, so a grant that is now present flips the state
	// (Needs-permission → Ready) and drops the guidance affordance. Store-presence
	// still wins (issue #149): a source that already imported reads Enabled here
	// too.
	card := s.setupCardFor(s.detector(), src, token, s.sourcesPresent(r.Context()), s.sourceCounts(r.Context()))
	s.renderFragment(w, "setup_card", card)
}
