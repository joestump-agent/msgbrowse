package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/onboard"
	"github.com/joestump/msgbrowse/internal/source"
)

// The /setup/disable gate tests MIRROR /setup/enable's (enable_test.go): the
// same checkSetupPOST guard protects both, unweakened (SPEC-0013 §Security) —
// 403 on cross-origin / missing token with NOTHING deleted, 400 on an unknown
// source — plus the Disable-specific two-step confirm: the first POST mutates
// nothing.

// disablePOST posts /setup/disable with the given origin, token, source, and
// optional confirm flag.
func disablePOST(t *testing.T, srv *Server, origin, token, src string, confirm bool) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	if src != "" {
		form.Set("source", src)
	}
	if token != "" {
		form.Set(setupTokenField, token)
	}
	if confirm {
		form.Set("confirm", "1")
	}
	req := httptest.NewRequest(http.MethodPost, "/setup/disable", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// signalConvCount counts the store's signal conversations, the "was anything
// deleted" probe.
func signalConvCount(t *testing.T, srv *Server) int {
	t.Helper()
	counts, err := srv.store.SourceCounts(context.Background())
	if err != nil {
		t.Fatalf("source counts: %v", err)
	}
	return counts[source.Signal].Conversations
}

// TestDisableCrossOriginRejected: a cross-origin POST /setup/disable is 403 and
// deletes NOTHING — even with a valid token.
func TestDisableCrossOriginRejected(t *testing.T) {
	srv, _, _ := newManagedRootServer(t)
	before := signalConvCount(t, srv)
	tok := mintToken(t, srv)
	rec := disablePOST(t, srv, "http://evil.example", tok, source.Signal, true)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin disable status = %d, want 403", rec.Code)
	}
	if got := signalConvCount(t, srv); got != before {
		t.Fatalf("cross-origin disable deleted data (%d -> %d conversations)", before, got)
	}
}

// TestDisableMissingTokenRejected: same-origin with no token is 403, nothing
// deleted.
func TestDisableMissingTokenRejected(t *testing.T) {
	srv, _, _ := newManagedRootServer(t)
	before := signalConvCount(t, srv)
	rec := disablePOST(t, srv, selfOrigin, "", source.Signal, true)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing-token disable status = %d, want 403", rec.Code)
	}
	if got := signalConvCount(t, srv); got != before {
		t.Fatalf("missing-token disable deleted data (%d -> %d conversations)", before, got)
	}
}

// TestDisableUnknownSourceRejected: a valid POST naming a source outside the
// fixed enum is 400 — no client string reaches a DELETE.
func TestDisableUnknownSourceRejected(t *testing.T) {
	srv, _, _ := newManagedRootServer(t)
	tok := mintToken(t, srv)
	rec := disablePOST(t, srv, selfOrigin, tok, "../../etc/passwd", true)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown-source disable status = %d, want 400", rec.Code)
	}
}

// TestDisableTwoStepConfirm: the first POST (no confirm) mutates NOTHING and
// renders the card's inline confirm affordance carrying the confirm=1 control;
// only the confirmed POST deletes.
func TestDisableTwoStepConfirm(t *testing.T) {
	srv, _, _ := newManagedRootServer(t)
	before := signalConvCount(t, srv)
	if before == 0 {
		t.Fatal("fixture should hold signal conversations")
	}

	tok := mintToken(t, srv)
	rec := disablePOST(t, srv, selfOrigin, tok, source.Signal, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm-step status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "imported conversations") || !contains(body, `"confirm": "1"`) {
		t.Errorf("confirm step missing the inline confirm affordance; got %q", body)
	}
	if !contains(body, ">Cancel<") {
		t.Errorf("confirm step missing the Cancel control")
	}
	if got := signalConvCount(t, srv); got != before {
		t.Fatalf("unconfirmed disable deleted data (%d -> %d conversations)", before, got)
	}
}

// TestDisableConfirmedDeletesStoreKeepsArchive is the issue-#162 acceptance:
// the confirmed POST removes the source's imported data from the store, the
// card leaves the Enabled state, the response piggybacks the sidebar OOB
// refresh, and the MANAGED ARCHIVE ON DISK IS UNTOUCHED.
func TestDisableConfirmedDeletesStoreKeepsArchive(t *testing.T) {
	srv, st, managed := newManagedRootServer(t)
	if signalConvCount(t, srv) == 0 {
		t.Fatal("fixture should hold signal conversations")
	}

	tok := mintToken(t, srv)
	rec := disablePOST(t, srv, selfOrigin, tok, source.Signal, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("confirmed disable status = %d, want 200", rec.Code)
	}
	if got := signalConvCount(t, srv); got != 0 {
		t.Fatalf("confirmed disable left %d signal conversations", got)
	}
	body := rec.Body.String()
	if contains(body, "setup-badge-enabled") {
		t.Error("card still reads Enabled after disable")
	}
	if !contains(body, `hx-swap-oob="true"`) || !contains(body, `id="sidebar-conversations"`) {
		t.Error("confirmed disable missing the out-of-band sidebar refresh")
	}
	if rec.Header().Get("HX-Trigger") != setupImportedTrigger {
		t.Errorf("HX-Trigger = %q, want %q", rec.Header().Get("HX-Trigger"), setupImportedTrigger)
	}

	// The managed archive is intact — Disable is store-only, so a re-enable is a
	// fast local re-import.
	if _, err := st.GetConversation(context.Background(), "Harper"); err != nil {
		t.Fatalf("store unusable after disable: %v", err)
	}
	chat := filepath.Join(managed, "export", "Harper", "chat.md")
	if _, err := os.Stat(chat); err != nil {
		t.Errorf("managed archive file %s removed by disable: %v", chat, err)
	}
}

// TestDisableBlockedWhileJobActive: an in-flight Enable/Refresh for the source
// blocks Disable — the card re-renders with an explanation and nothing is
// deleted.
func TestDisableBlockedWhileJobActive(t *testing.T) {
	srv, _, _ := newManagedRootServer(t)
	srv.SetEnabler(&fakeEnabler{progress: onboard.Progress{Phase: onboard.PhaseExporting, Message: "Exporting…"}})
	before := signalConvCount(t, srv)

	tok := mintToken(t, srv)
	rec := disablePOST(t, srv, selfOrigin, tok, source.Signal, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("active-job disable status = %d, want 200", rec.Code)
	}
	if !contains(rec.Body.String(), "wait for it to finish") {
		t.Errorf("active-job disable missing the explanation; got %q", rec.Body.String())
	}
	if got := signalConvCount(t, srv); got != before {
		t.Fatalf("active-job disable deleted data (%d -> %d conversations)", before, got)
	}
}

// TestEnabledCardShowsCountsAndControls: an Enabled card carries the imported
// footprint ("N conversations · N messages"), the compact icon Refresh with its
// aria-label, and the Disable affordance (issue #162).
func TestEnabledCardShowsCountsAndControls(t *testing.T) {
	srv, _, _ := newManagedRootServer(t)
	body := get(t, srv, "/providers").Body.String()
	if !contains(body, "1 conversation · 1 message") {
		t.Errorf("/providers Enabled card missing the imported counts")
	}
	if !contains(body, `aria-label="Refresh Signal"`) || !contains(body, "setup-iconbtn") {
		t.Errorf("/providers Enabled card missing the compact icon Refresh")
	}
	if !contains(body, `hx-post="/setup/disable"`) {
		t.Errorf("/providers Enabled card missing the Disable control")
	}
	// The stale footer bug (issue #162): with everything actionable-or-enabled,
	// the "No sources are ready to enable" empty state must not render.
	if contains(body, "No sources are ready to enable") {
		t.Errorf("/providers footer shows the empty state while a source is Enabled")
	}
	if !contains(body, "All detected sources are enabled") {
		t.Errorf("/providers footer missing the all-enabled copy")
	}
}

// TestDisableRefusedOnSyncedSource mirrors the Enable/Refresh conflict guard:
// a synced-in (replica) source must not be disable-able — a stale page could
// otherwise delete a replica's imported rows with no UI path to re-import
// until the next sync completion (adversarial-review fix on #172).
func TestDisableRefusedOnSyncedSource(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetSyncMonitor(replicaMonitor())
	before := signalConvCount(t, srv)

	tok := mintToken(t, srv)
	rec := disablePOST(t, srv, selfOrigin, tok, "signal", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable-on-replica = %d, want 200 conflict fragment", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "kitchen-mac") {
		t.Errorf("conflict fragment should name the importer; got: %.200s", body)
	}
	if got := signalConvCount(t, srv); got != before {
		t.Fatalf("replica rows deleted despite the conflict guard: %d -> %d", before, got)
	}
}
