package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/joestump/msgbrowse/internal/onboard"
	"github.com/joestump/msgbrowse/internal/source"
)

// fakeEnabler is a test double for the onboard.Runner seam. It records every
// Enable call so the security tests can assert a rejected POST started NO job
// (the SPEC-0013 §Security "MUST NOT start any subprocess" guarantee), and lets
// a test script the progress a Status/Enable returns.
type fakeEnabler struct {
	mu         sync.Mutex
	enables    int32 // atomic: number of Enable calls
	refreshes  int32 // atomic: number of Refresh calls
	refreshSrc []string
	cancels    int32
	progress   onboard.Progress
	enableErr  error
	refreshErr error
}

func (f *fakeEnabler) Enable(src string) (onboard.Progress, error) {
	atomic.AddInt32(&f.enables, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.progress
	p.Source = src
	if p.Phase == "" {
		p.Phase = onboard.PhaseExporting
		p.Message = "Exporting…"
	}
	return p, f.enableErr
}

// Refresh records each call (and the source) so the refresh security tests can
// assert a rejected POST started NO job, and the all-sources test can assert one
// job per Enabled source. It mirrors Enable's progress shaping.
func (f *fakeEnabler) Refresh(src string) (onboard.Progress, error) {
	atomic.AddInt32(&f.refreshes, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshSrc = append(f.refreshSrc, src)
	p := f.progress
	p.Source = src
	if p.Phase == "" {
		p.Phase = onboard.PhaseExporting
		p.Message = "Refreshing…"
	}
	return p, f.refreshErr
}

func (f *fakeEnabler) Status(src string) (onboard.Progress, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.progress
	p.Source = src
	if p.Phase == "" {
		return onboard.Progress{}, false
	}
	return p, true
}

func (f *fakeEnabler) Cancel(src string) bool {
	atomic.AddInt32(&f.cancels, 1)
	return true
}

func (f *fakeEnabler) enableCount() int32  { return atomic.LoadInt32(&f.enables) }
func (f *fakeEnabler) refreshCount() int32 { return atomic.LoadInt32(&f.refreshes) }

// refreshedSources returns the sources Refresh was called for, in call order.
func (f *fakeEnabler) refreshedSources() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.refreshSrc...)
}

// mintToken renders /setup and extracts a live per-session token by minting one
// directly against the server's token set — the same set the handler mints from
// at render — so a test can submit a valid token without scraping HTML.
func mintToken(t *testing.T, srv *Server) string {
	t.Helper()
	tok, err := srv.setupTokens.mint()
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	return tok
}

// enablePOST builds a same-origin POST /setup/enable with the given token and
// source, or omits either when empty. host defaults to the httptest host so
// Origin matches; pass a different origin to simulate cross-origin.
func enablePOST(t *testing.T, srv *Server, path, origin, token, src string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	if src != "" {
		form.Set("source", src)
	}
	if token != "" {
		form.Set(setupTokenField, token)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// selfOrigin is the origin httptest.NewRequest produces (Host = example.com,
// scheme http on loopback), so a same-origin POST carries this Origin.
const selfOrigin = "http://example.com"

// TestEnableCrossOriginRejected: a cross-origin POST is rejected 403 and starts
// NO job (SPEC-0013 §Security "WHEN a cross-origin page POSTs /setup/enable THEN
// it is rejected 403 with no subprocess started").
func TestEnableCrossOriginRejected(t *testing.T) {
	srv := newEmptyStoreServer(t)
	fe := &fakeEnabler{}
	srv.SetEnabler(fe)

	tok := mintToken(t, srv) // a VALID token — the origin check alone must reject
	rec := enablePOST(t, srv, "/setup/enable", "http://evil.example", tok, source.Signal)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST status = %d, want 403", rec.Code)
	}
	if fe.enableCount() != 0 {
		t.Fatalf("cross-origin POST started %d jobs, want 0", fe.enableCount())
	}
}

// TestEnableMissingTokenRejected: a same-origin POST with no token is rejected
// 403 and starts no job.
func TestEnableMissingTokenRejected(t *testing.T) {
	srv := newEmptyStoreServer(t)
	fe := &fakeEnabler{}
	srv.SetEnabler(fe)

	rec := enablePOST(t, srv, "/setup/enable", selfOrigin, "", source.Signal)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing-token POST status = %d, want 403", rec.Code)
	}
	if fe.enableCount() != 0 {
		t.Fatalf("missing-token POST started %d jobs, want 0", fe.enableCount())
	}
}

// TestEnableInvalidTokenRejected: a same-origin POST with a well-formed but
// never-minted token is rejected 403 and starts no job.
func TestEnableInvalidTokenRejected(t *testing.T) {
	srv := newEmptyStoreServer(t)
	fe := &fakeEnabler{}
	srv.SetEnabler(fe)

	bogus := strings.Repeat("ab", 32) // 64 hex chars, correct shape, never minted
	rec := enablePOST(t, srv, "/setup/enable", selfOrigin, bogus, source.Signal)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("invalid-token POST status = %d, want 403", rec.Code)
	}
	if fe.enableCount() != 0 {
		t.Fatalf("invalid-token POST started %d jobs, want 0", fe.enableCount())
	}
}

// TestEnableUnknownSourceRejected: a valid same-origin+token POST with a source
// outside the fixed enum is a 400 and starts no job — no client string can ever
// reach a filesystem path (SPEC-0013 §Security "No arbitrary paths").
func TestEnableUnknownSourceRejected(t *testing.T) {
	srv := newEmptyStoreServer(t)
	fe := &fakeEnabler{}
	srv.SetEnabler(fe)

	tok := mintToken(t, srv)
	rec := enablePOST(t, srv, "/setup/enable", selfOrigin, tok, "../../etc/passwd")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown-source POST status = %d, want 400", rec.Code)
	}
	if fe.enableCount() != 0 {
		t.Fatalf("unknown-source POST started %d jobs, want 0", fe.enableCount())
	}
}

// TestEnableHappyPath: a same-origin POST with a valid token and a known source
// starts the job and renders the progress fragment with the aria-live message.
func TestEnableHappyPath(t *testing.T) {
	srv := newEmptyStoreServer(t)
	fe := &fakeEnabler{progress: onboard.Progress{Phase: onboard.PhaseExporting, Message: "Exporting Signal…"}}
	srv.SetEnabler(fe)

	tok := mintToken(t, srv)
	rec := enablePOST(t, srv, "/setup/enable", selfOrigin, tok, source.Signal)
	if rec.Code != http.StatusOK {
		t.Fatalf("happy-path POST status = %d, want 200", rec.Code)
	}
	if fe.enableCount() != 1 {
		t.Fatalf("happy-path started %d jobs, want 1", fe.enableCount())
	}
	body := rec.Body.String()
	if !contains(body, "Exporting Signal") {
		t.Errorf("progress fragment missing the phase message; got %q", body)
	}
	// The active fragment self-polls /setup/status and offers Cancel.
	if !contains(body, `hx-get="/setup/status/signal"`) {
		t.Errorf("active progress fragment missing the status poller")
	}
	if !contains(body, "Cancel") {
		t.Errorf("active progress fragment missing the Cancel control")
	}
}

// TestEnableSecFetchSiteSameOrigin: a POST with no Origin but
// Sec-Fetch-Site: same-origin and a valid token is accepted (modern browsers
// send Sec-Fetch-Site, and older/omitted-Origin same-origin POSTs must still
// work).
func TestEnableSecFetchSiteSameOrigin(t *testing.T) {
	srv := newEmptyStoreServer(t)
	fe := &fakeEnabler{}
	srv.SetEnabler(fe)

	tok := mintToken(t, srv)
	form := url.Values{}
	form.Set("source", source.Signal)
	req := httptest.NewRequest(http.MethodPost, "/setup/enable", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set(setupTokenHeader, tok) // token via the htmx header path
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-origin Sec-Fetch-Site POST status = %d, want 200", rec.Code)
	}
	if fe.enableCount() != 1 {
		t.Fatalf("same-origin Sec-Fetch-Site started %d jobs, want 1", fe.enableCount())
	}
}

// TestEnableSecFetchSiteCrossSiteRejected: Sec-Fetch-Site: cross-site is rejected
// even with a valid token and no Origin header.
func TestEnableSecFetchSiteCrossSiteRejected(t *testing.T) {
	srv := newEmptyStoreServer(t)
	fe := &fakeEnabler{}
	srv.SetEnabler(fe)

	tok := mintToken(t, srv)
	form := url.Values{}
	form.Set("source", source.Signal)
	form.Set(setupTokenField, tok)
	req := httptest.NewRequest(http.MethodPost, "/setup/enable", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST status = %d, want 403", rec.Code)
	}
	if fe.enableCount() != 0 {
		t.Fatalf("cross-site POST started %d jobs, want 0", fe.enableCount())
	}
}

// TestEnableNoEnablerUnavailable: with no Enabler wired, a valid POST renders the
// "unavailable" affordance rather than 500ing or starting a job.
func TestEnableNoEnablerUnavailable(t *testing.T) {
	srv := newEmptyStoreServer(t) // no SetEnabler
	tok := mintToken(t, srv)
	rec := enablePOST(t, srv, "/setup/enable", selfOrigin, tok, source.Signal)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-enabler POST status = %d, want 200", rec.Code)
	}
	if !contains(rec.Body.String(), "desktop app") {
		t.Errorf("no-enabler fragment missing the unavailable affordance; got %q", rec.Body.String())
	}
}

// TestEnableOversizedBodyRejected: a body over the KB cap is rejected before
// processing (SPEC-0013 §Security "Request body size limits").
func TestEnableOversizedBodyRejected(t *testing.T) {
	srv := newEmptyStoreServer(t)
	fe := &fakeEnabler{}
	srv.SetEnabler(fe)

	tok := mintToken(t, srv)
	form := url.Values{}
	form.Set("source", source.Signal)
	form.Set(setupTokenField, tok)
	form.Set("padding", strings.Repeat("x", setupBodyLimit+1)) // exceed the cap
	req := httptest.NewRequest(http.MethodPost, "/setup/enable", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", selfOrigin)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("oversized-body POST status = %d, want 403", rec.Code)
	}
	if fe.enableCount() != 0 {
		t.Fatalf("oversized-body POST started %d jobs, want 0", fe.enableCount())
	}
}

// TestSetupStatusUnknownSource404: a status GET for a non-enum source 404s
// (never a filesystem path).
func TestSetupStatusUnknownSource404(t *testing.T) {
	srv := newEmptyStoreServer(t)
	fe := &fakeEnabler{}
	srv.SetEnabler(fe)
	rec := get(t, srv, "/setup/status/bogus")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown-source status GET = %d, want 404", rec.Code)
	}
}

// TestSetupPageMintsToken: the rendered /setup page embeds a per-session token in
// the live Enable button so the follow-on POST can carry it.
func TestSetupPageMintsToken(t *testing.T) {
	srv := newEmptyStoreServer(t)
	fe := &fakeEnabler{}
	srv.SetEnabler(fe)
	srv.SetDetector(detectorFor(signalPlusIMessageHome(t), true)) // iMessage Ready → live button
	body := get(t, srv, "/setup").Body.String()
	if !contains(body, `hx-post="/setup/enable"`) {
		t.Error("/setup with an Enabler wired should render a live Enable button")
	}
	if !contains(body, "X-Setup-Token") {
		t.Error("/setup live Enable button should carry the per-session token header")
	}
}
