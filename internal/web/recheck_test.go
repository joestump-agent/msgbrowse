package web

import (
	"net/http"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/setup"
	"github.com/joestump/msgbrowse/internal/source"
)

// TestRecheckCrossOriginRejected: a cross-origin POST /setup/recheck is rejected
// 403 (SPEC-0013 §Security "same-origin protected for consistency with the other
// POSTs"). The gate is the shared checkSetupPOST, identical to /setup/enable.
func TestRecheckCrossOriginRejected(t *testing.T) {
	srv := newEmptyStoreServer(t)
	srv.SetDetector(detectorFor(signalPlusIMessageHome(t), false))

	tok := mintToken(t, srv) // a VALID token — the origin check alone must reject
	rec := enablePOST(t, srv, "/setup/recheck", "http://evil.example", tok, source.IMessage)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin recheck status = %d, want 403", rec.Code)
	}
}

// TestRecheckMissingTokenRejected: a same-origin POST with no token is 403.
func TestRecheckMissingTokenRejected(t *testing.T) {
	srv := newEmptyStoreServer(t)
	srv.SetDetector(detectorFor(signalPlusIMessageHome(t), false))

	rec := enablePOST(t, srv, "/setup/recheck", selfOrigin, "", source.IMessage)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing-token recheck status = %d, want 403", rec.Code)
	}
}

// TestRecheckInvalidTokenRejected: a well-formed but never-minted token is 403.
func TestRecheckInvalidTokenRejected(t *testing.T) {
	srv := newEmptyStoreServer(t)
	srv.SetDetector(detectorFor(signalPlusIMessageHome(t), false))

	bogus := strings.Repeat("ab", 32)
	rec := enablePOST(t, srv, "/setup/recheck", selfOrigin, bogus, source.IMessage)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("invalid-token recheck status = %d, want 403", rec.Code)
	}
}

// TestRecheckUnknownSourceRejected: a source outside the fixed enum is a 400 —
// no client string reaches a filesystem path (SPEC-0013 §Security "No arbitrary
// paths").
func TestRecheckUnknownSourceRejected(t *testing.T) {
	srv := newEmptyStoreServer(t)
	srv.SetDetector(detectorFor(signalPlusIMessageHome(t), false))

	tok := mintToken(t, srv)
	rec := enablePOST(t, srv, "/setup/recheck", selfOrigin, tok, "../../etc/passwd")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown-source recheck status = %d, want 400", rec.Code)
	}
}

// TestRecheckStillBlockedRerendersNeedsPermission: when the grant is still
// missing, Recheck re-renders the card in Needs-permission with its guidance
// (and the exact System Settings deep link) so the user can retry.
func TestRecheckStillBlockedRerendersNeedsPermission(t *testing.T) {
	srv := newEmptyStoreServer(t)
	srv.SetDetector(detectorFor(signalPlusIMessageHome(t), false)) // iMessage NOT readable

	tok := mintToken(t, srv)
	rec := enablePOST(t, srv, "/setup/recheck", selfOrigin, tok, source.IMessage)
	if rec.Code != http.StatusOK {
		t.Fatalf("recheck status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, `id="setup-card-imessage"`) {
		t.Error("recheck did not return the iMessage card fragment")
	}
	if !contains(body, "setup-card-needs-permission") {
		t.Error("still-blocked recheck should keep the card in needs-permission")
	}
	// The guidance modal + exact deep link are present so the user can retry.
	if !contains(body, setup.FullDiskAccessDeepLink) {
		t.Errorf("recheck fragment missing the Full Disk Access deep link %q", setup.FullDiskAccessDeepLink)
	}
	if !contains(body, `hx-post="/setup/recheck"`) {
		t.Error("recheck fragment missing its own Recheck control")
	}
}

// TestRecheckFlipsCardWhenGrantAdded is the SPEC-0013 acceptance: "WHEN the user
// grants FDA and clicks Recheck THEN the card flips out of Needs-permission". The
// injected probe now returns readable, so the re-rendered card is Ready and the
// guidance/deep-link are gone.
func TestRecheckFlipsCardWhenGrantAdded(t *testing.T) {
	srv := newEmptyStoreServer(t)
	home := signalPlusIMessageHome(t)
	// First render: iMessage is NOT readable → Needs-permission.
	srv.SetDetector(detectorFor(home, false))
	body := get(t, srv, "/providers").Body.String()
	if !contains(body, `aria-label="iMessage: Needs permission"`) {
		t.Fatalf("precondition: iMessage should start Needs-permission")
	}

	// The user grants Full Disk Access; the injected probe now reads readable.
	srv.SetDetector(detectorFor(home, true))
	tok := mintToken(t, srv)
	rec := enablePOST(t, srv, "/setup/recheck", selfOrigin, tok, source.IMessage)
	if rec.Code != http.StatusOK {
		t.Fatalf("recheck status = %d, want 200", rec.Code)
	}
	frag := rec.Body.String()
	if !contains(frag, `aria-label="iMessage: Ready"`) {
		t.Errorf("recheck should flip iMessage to Ready; got %q", frag)
	}
	if contains(frag, "setup-card-needs-permission") {
		t.Error("flipped card should no longer be needs-permission")
	}
	if contains(frag, setup.FullDiskAccessDeepLink) {
		t.Error("flipped (Ready) card should not carry the FDA guidance deep link")
	}
}

// TestSetupGuidanceModalRendersForEachGrant asserts the full /setup page renders
// the guidance modal for each missing grant with its correct content: Full Disk
// Access + the exact deep link for iMessage; the Signal Keychain "Always Allow"
// guidance (with NO deep link) for Signal.
func TestSetupGuidanceModalRendersForEachGrant(t *testing.T) {
	srv := newEmptyStoreServer(t)
	srv.SetDetector(detectorFor(signalPlusIMessageHome(t), false))
	body := get(t, srv, "/providers").Body.String()

	// The dialog is a role="dialog" with aria-modal and a labelling title.
	if !contains(body, `id="setup-guide-imessage"`) || !contains(body, `role="dialog"`) {
		t.Error("/setup missing the iMessage guidance dialog")
	}
	if !contains(body, `aria-modal="true"`) {
		t.Error("guidance dialog missing aria-modal")
	}
	// iMessage: Full Disk Access with the exact System Settings deep link.
	if !contains(body, setup.FullDiskAccessDeepLink) {
		t.Errorf("/setup missing the Full Disk Access deep link %q", setup.FullDiskAccessDeepLink)
	}
	if !contains(body, "Full Disk Access") {
		t.Error("/setup missing the Full Disk Access guidance title")
	}
	// Signal: the Keychain "Always Allow" guidance, and NO deep link for Signal.
	if !contains(body, `id="setup-guide-signal"`) {
		t.Error("/setup missing the Signal guidance dialog")
	}
	if !contains(body, "Always Allow") {
		t.Error("/setup Signal guidance should mention the Always Allow prompt")
	}
	// The Recheck control is keyboard-operable (a native <button>) and gated.
	if !contains(body, `hx-post="/setup/recheck"`) {
		t.Error("/setup guidance missing the Recheck control")
	}
	if !contains(body, "X-Setup-Token") {
		t.Error("/setup Recheck control missing the per-session token header")
	}
}

// TestSetupGuidanceModalA11y asserts the modal's accessibility affordances: the
// trigger advertises aria-haspopup/aria-controls, the dialog carries a
// role="status" aria-live recheck-result region, and it is initially hidden (so
// it is out of the tab order until opened).
func TestSetupGuidanceModalA11y(t *testing.T) {
	srv := newEmptyStoreServer(t)
	srv.SetDetector(detectorFor(signalPlusIMessageHome(t), false))
	body := get(t, srv, "/providers").Body.String()

	if !contains(body, `aria-haspopup="dialog"`) {
		t.Error("guidance trigger missing aria-haspopup=dialog")
	}
	if !contains(body, `aria-controls="setup-guide-imessage"`) {
		t.Error("guidance trigger missing aria-controls to its dialog")
	}
	// The recheck result is an aria-live region.
	if !contains(body, `class="setup-guide-result" role="status" aria-live="polite"`) {
		t.Error("guidance dialog missing the aria-live recheck-result region")
	}
	// The dialog is hidden until opened (kept out of the tab order by [hidden]).
	if !contains(body, `data-setup-guide hidden`) {
		t.Error("guidance dialog should be initially hidden")
	}
	// setup.js is loaded so the focus trap / Escape / restore behavior runs.
	if !contains(body, `/static/setup.js`) {
		t.Error("Setup page missing the setup.js include for modal focus management")
	}
}
