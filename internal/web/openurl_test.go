// Tests for the desktop external-link bridge (issue #179): the POST
// /desktop/open-url gate mirrors the Setup POST tests' shape (enable_test.go /
// disable_test.go) — 404 outside the desktop shell, 403 cross-origin, 400 on
// anything but a clean absolute http(s) URL, and the opener sees exactly the
// validated URL. All headless, CGO_ENABLED=0.
package web

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// openURLPost posts /desktop/open-url with the given Origin header (empty to
// omit) and url form value, mirroring disablePOST's shape.
func openURLPost(t *testing.T, srv *Server, origin, raw string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	if raw != "" {
		form.Set("url", raw)
	}
	req := httptest.NewRequest(http.MethodPost, "/desktop/open-url", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// newOpenURLServer wires a recording opener into a desktop-chrome test server
// — the exact configuration the shell produces via SetDesktopChrome +
// SetExternalOpener.
func newOpenURLServer(t *testing.T) (*Server, *[]string) {
	t.Helper()
	srv, _, _ := newTestServer(t)
	srv.SetDesktopChrome(true)
	opened := &[]string{}
	srv.SetExternalOpener(func(u string) error {
		*opened = append(*opened, u)
		return nil
	})
	return srv, opened
}

// TestOpenURLRouteAbsentOutsideDesktop: without an opener the route is never
// registered — 404 on EVERY method, including the GET that would otherwise
// 405 off the "POST /desktop/open-url" mux pattern and advertise a desktop
// surface plain `msgbrowse serve` doesn't have. And an opener alone is not
// enough; the desktop-chrome flag must be set too, matching the desktop.js
// interceptor's guard.
func TestOpenURLRouteAbsentOutsideDesktop(t *testing.T) {
	srv, _, _ := newTestServer(t) // browser mode: no opener, no desktop-chrome
	if rec := openURLPost(t, srv, selfOrigin, "https://example.org/x"); rec.Code != http.StatusNotFound {
		t.Fatalf("no-opener POST status = %d, want 404", rec.Code)
	}
	// Route absence, not method mismatch: a GET must 404 too. With the route
	// registered unconditionally this would be a 405 from the mux pattern.
	if rec := get(t, srv, "/desktop/open-url"); rec.Code != http.StatusNotFound {
		t.Fatalf("no-opener GET status = %d, want 404 (route must not exist)", rec.Code)
	}

	srv.SetExternalOpener(func(string) error { return nil }) // opener but no desktop-chrome
	if rec := openURLPost(t, srv, selfOrigin, "https://example.org/x"); rec.Code != http.StatusNotFound {
		t.Fatalf("opener-without-desktop-chrome status = %d, want 404", rec.Code)
	}
}

// TestOpenURLValidHTTPSAccepted: a same-origin POST with an absolute https URL
// reaches the opener with the URL byte-for-byte, and answers 204 (the client
// interceptor renders nothing).
func TestOpenURLValidHTTPSAccepted(t *testing.T) {
	srv, opened := newOpenURLServer(t)
	const target = "https://example.org/article?id=42"
	rec := openURLPost(t, srv, selfOrigin, target)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("valid https status = %d, want 204", rec.Code)
	}
	if len(*opened) != 1 || (*opened)[0] != target {
		t.Fatalf("opener saw %v, want exactly [%s]", *opened, target)
	}
}

// TestOpenURLWailsRejectedBytesAccepted pins the reason openExternal calls
// pkg/browser instead of wailsruntime.BrowserOpenURL (PR #184 review fix):
// ordinary message links carry bytes the Wails wrapper's
// ValidateAndSanitizeURL rejects — parentheses (Wikipedia article titles),
// ~ ! $ * and friends. Each must pass validExternalURL, survive the full
// form-decode path, and reach the opener byte-for-byte.
func TestOpenURLWailsRejectedBytesAccepted(t *testing.T) {
	cases := []struct {
		name, raw string
	}{
		{"parenthesized wikipedia article", "https://en.wikipedia.org/wiki/Go_(programming_language)"},
		{"tilde user path", "https://example.org/~joe/index.html"},
		{"bang and dollar in query", "https://example.org/search?q=$100!"},
		{"asterisk and brackets", "https://example.org/a*b/%5Bx%5D?list[]=1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !validExternalURL(tc.raw) {
				t.Fatalf("validExternalURL(%q) = false, want true", tc.raw)
			}
			srv, opened := newOpenURLServer(t)
			rec := openURLPost(t, srv, selfOrigin, tc.raw)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", rec.Code)
			}
			if len(*opened) != 1 || (*opened)[0] != tc.raw {
				t.Fatalf("opener saw %v, want exactly [%s] byte-for-byte", *opened, tc.raw)
			}
		})
	}
}

// TestOpenURLCrossOriginRejected: a cross-origin POST is 403 and the opener is
// never called — another local process or a hostile page must not be able to
// drive the user's browser.
func TestOpenURLCrossOriginRejected(t *testing.T) {
	srv, opened := newOpenURLServer(t)
	rec := openURLPost(t, srv, "http://evil.example", "https://example.org/x")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", rec.Code)
	}
	if len(*opened) != 0 {
		t.Fatalf("cross-origin POST reached the opener: %v", *opened)
	}
}

// TestOpenURLNoProvenanceRejected: a POST with no Origin, no Sec-Fetch-Site,
// and no Referer (a bare programmatic client) is rejected, same as the Setup
// POSTs.
func TestOpenURLNoProvenanceRejected(t *testing.T) {
	srv, opened := newOpenURLServer(t)
	rec := openURLPost(t, srv, "", "https://example.org/x")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no-provenance status = %d, want 403", rec.Code)
	}
	if len(*opened) != 0 {
		t.Fatalf("no-provenance POST reached the opener: %v", *opened)
	}
}

// TestOpenURLGETRejected: the route is POST-only; a GET is a 405, never an
// open.
func TestOpenURLGETRejected(t *testing.T) {
	srv, opened := newOpenURLServer(t)
	rec := get(t, srv, "/desktop/open-url?url=https://example.org/x")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
	if len(*opened) != 0 {
		t.Fatalf("GET reached the opener: %v", *opened)
	}
}

// TestOpenURLRejectsBadURLs: everything outside the strict allowlist —
// non-http(s) schemes, relative values, control characters, empty, oversized —
// is 400 and the opener never runs. The response body must not echo the URL.
func TestOpenURLRejectsBadURLs(t *testing.T) {
	cases := []struct {
		name, raw string
	}{
		{"file scheme", "file:///etc/passwd"},
		{"javascript scheme", "javascript:alert(1)"},
		{"data scheme", "data:text/html,<script>alert(1)</script>"},
		{"custom scheme", "msgbrowse://open"},
		{"scheme only no host", "https://"},
		{"relative path", "/gallery"},
		{"scheme-relative", "//example.org/x"},
		{"control character", "https://example.org/\x00"},
		{"newline", "https://example.org/\nSet-Cookie: x=y"},
		{"empty", ""},
		{"oversized", "https://example.org/" + strings.Repeat("a", openURLMaxLen)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, opened := newOpenURLServer(t)
			rec := openURLPost(t, srv, selfOrigin, tc.raw)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if len(*opened) != 0 {
				t.Fatalf("invalid URL reached the opener: %v", *opened)
			}
			if tc.raw != "" && strings.Contains(rec.Body.String(), tc.raw) {
				t.Error("response echoes the submitted URL")
			}
		})
	}
}

// TestOpenURLRejectionLogged: a 400 rejection leaves a debug-level trace with
// a sanitized, truncated form of the value — "clicked and nothing happened"
// must be diagnosable from the logs — while the raw value never appears in
// full.
func TestOpenURLRejectionLogged(t *testing.T) {
	st, cfg, _ := newTestStoreAndConfig(t)
	var buf strings.Builder
	srv, err := NewServer(st, cfg, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	srv.SetDesktopChrome(true)
	srv.SetExternalOpener(func(string) error { return nil })

	raw := "javascript:alert(1)//" + strings.Repeat("a", 100)
	if rec := openURLPost(t, srv, selfOrigin, raw); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	logged := buf.String()
	if !strings.Contains(logged, "external url rejected") {
		t.Errorf("rejection left no log trace; log:\n%s", logged)
	}
	if !strings.Contains(logged, "javascript:") {
		t.Errorf("log trace does not show the rejected scheme; log:\n%s", logged)
	}
	if strings.Contains(logged, raw) {
		t.Error("log carries the full raw value; want a truncated prefix")
	}
}

// TestRejectedURLForLog pins the sanitizer the rejection log relies on:
// control bytes become '?', long values truncate to a marked prefix, and
// short clean values pass through.
func TestRejectedURLForLog(t *testing.T) {
	if got := rejectedURLForLog("https://example.org/\x00\x1f\x7f"); got != "https://example.org/???" {
		t.Errorf("control bytes: got %q", got)
	}
	long := "https://example.org/" + strings.Repeat("a", 100)
	if got, want := rejectedURLForLog(long), long[:48]+"..."; got != want {
		t.Errorf("truncation: got %q, want %q", got, want)
	}
	if got := rejectedURLForLog("ftp://x"); got != "ftp://x" {
		t.Errorf("short clean value: got %q", got)
	}
}

// TestOpenURLOpenerFailure: an opener error is a 500 with a fixed message —
// the client interceptor stays silent and the URL is not reflected.
func TestOpenURLOpenerFailure(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetDesktopChrome(true)
	srv.SetExternalOpener(func(string) error { return errors.New("no runtime context") })
	const target = "https://example.org/x"
	rec := openURLPost(t, srv, selfOrigin, target)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("opener-failure status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), target) {
		t.Error("failure response echoes the submitted URL")
	}
}

// TestDesktopJSCarriesOpenURLBridge pins the served interceptor to the served
// endpoint: if either side of the contract is renamed, this fails before a
// desktop build ships links that do nothing again.
func TestDesktopJSCarriesOpenURLBridge(t *testing.T) {
	js, err := staticFS.ReadFile("static/desktop.js")
	if err != nil {
		t.Fatalf("read embedded desktop.js: %v", err)
	}
	s := string(js)
	for _, want := range []string{
		"/desktop/open-url",
		"window.location.origin", // the same-origin carve-out for media thumbs
		"e.preventDefault()",
	} {
		if !contains(s, want) {
			t.Errorf("embedded desktop.js missing %q", want)
		}
	}
}
