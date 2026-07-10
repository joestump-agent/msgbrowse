// Tests for POST /settings/devices/pair (issue #157): the privileged pairing
// POST rides the SAME same-origin + per-session-token gate as the Setup POSTs
// (checkSetupPOST, reused verbatim — SPEC-0013 §Security "Same-origin
// protection for privileged POSTs"), and its outcomes follow POST-redirect-GET
// with a fixed-enum banner state, never request-derived text.
//
// Governing: SPEC-0014 §Authentication ("POST /settings/devices/pair …
// loopback"), REQ "Pairing via Device ID and QR", issue #157 Security
// Checklist (input validation via the payload decoder; no work before the
// 403).
package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/devices"
)

// newPairServer builds a device-sync-enabled server with the given pairing
// source wired.
func newPairServer(t *testing.T, ps PairingSource) *Server {
	t.Helper()
	st, cfg, _ := newTestStoreAndConfig(t)
	cfg.DeviceSync = config.DeviceSyncConfig{Enabled: true, ListenAddr: "127.0.0.1:0"}
	srv, err := NewServer(st, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	// The device-sync UI is gated behind the compile-time feature flag; the
	// tagged build that wires pairing also sets it, so mirror that here.
	srv.SetDeviceSyncFeature(true)
	if ps != nil {
		srv.SetPairingSource(ps)
	}
	return srv
}

// pairPOST posts the pair form with the given origin/token/code.
func pairPOST(t *testing.T, srv *Server, origin, token, code string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	if code != "" {
		form.Set("code", code)
	}
	if token != "" {
		form.Set(setupTokenField, token)
	}
	req := httptest.NewRequest(http.MethodPost, "/settings/devices/pair", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestPairCrossOriginRejected: a cross-origin POST is rejected 403 BEFORE the
// pairing source is consulted — no device is added by a hostile page driving
// the loopback UI.
func TestPairCrossOriginRejected(t *testing.T) {
	src := &staticPairing{p: testPayload(t)}
	srv := newPairServer(t, src)
	tok := mintToken(t, srv)

	rec := pairPOST(t, srv, "http://evil.example", tok, testPeerDeviceID)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin pair = %d, want 403", rec.Code)
	}
	if src.paired != 0 || src.lastCode != "" {
		t.Error("pairing source was consulted despite the 403")
	}
}

// TestPairMissingTokenRejected: same-origin but no minted token → 403, no
// pairing work.
func TestPairMissingTokenRejected(t *testing.T) {
	src := &staticPairing{p: testPayload(t)}
	srv := newPairServer(t, src)

	rec := pairPOST(t, srv, selfOrigin, "", testPeerDeviceID)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("tokenless pair = %d, want 403", rec.Code)
	}
	if src.paired != 0 {
		t.Error("pairing source was consulted despite the missing token")
	}
}

// TestPairSuccessRedirects: a gated, valid pair POST calls the source with
// the submitted code and follows PRG to the fixed ?pair=ok state.
func TestPairSuccessRedirects(t *testing.T) {
	src := &staticPairing{p: testPayload(t)}
	srv := newPairServer(t, src)
	tok := mintToken(t, srv)

	rec := pairPOST(t, srv, selfOrigin, tok, testPeerDeviceID)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("pair = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/settings?pair=ok" {
		t.Errorf("redirect = %q, want /settings?pair=ok", loc)
	}
	if src.paired != 1 || src.lastCode != testPeerDeviceID {
		t.Errorf("source called %d times with %q; want once with the code", src.paired, src.lastCode)
	}
}

// TestPairErrorStates maps the pairing sentinels onto their fixed banner
// enums: invalid payloads, self-pairing, and engine errors each redirect to
// their own state, and no free text enters the Location header.
func TestPairErrorStates(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"invalid payload", devices.ErrInvalidSyncPayload, "/settings?pair=invalid"},
		{"self pair", devices.ErrSelfPair, "/settings?pair=self"},
		{"engine error", io.ErrUnexpectedEOF, "/settings?pair=error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := &staticPairing{p: testPayload(t), pairErr: c.err}
			srv := newPairServer(t, src)
			tok := mintToken(t, srv)
			rec := pairPOST(t, srv, selfOrigin, tok, testPeerDeviceID)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("pair = %d, want 303", rec.Code)
			}
			if loc := rec.Header().Get("Location"); loc != c.want {
				t.Errorf("redirect = %q, want %q", loc, c.want)
			}
		})
	}
}

// TestPairEmptyCodeInvalid: an empty code short-circuits to ?pair=invalid
// without consulting the source.
func TestPairEmptyCodeInvalid(t *testing.T) {
	src := &staticPairing{p: testPayload(t)}
	srv := newPairServer(t, src)
	tok := mintToken(t, srv)

	rec := pairPOST(t, srv, selfOrigin, tok, "")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings?pair=invalid" {
		t.Fatalf("empty-code pair = %d → %q, want 303 → /settings?pair=invalid", rec.Code, rec.Header().Get("Location"))
	}
	if src.paired != 0 {
		t.Error("pairing source was consulted for an empty code")
	}
}

// TestPairUnavailableWithoutSource: with no pairing source wired (sync
// disabled or engine down), a gated POST redirects to ?pair=unavailable and
// mutates nothing.
func TestPairUnavailableWithoutSource(t *testing.T) {
	srv := newPairServer(t, nil)
	tok := mintToken(t, srv)

	rec := pairPOST(t, srv, selfOrigin, tok, testPeerDeviceID)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings?pair=unavailable" {
		t.Fatalf("sourceless pair = %d → %q, want 303 → /settings?pair=unavailable", rec.Code, rec.Header().Get("Location"))
	}
}

// TestPairResultBanner: the fixed-enum ?pair= states each render their
// server-owned banner text, and an out-of-enum value renders nothing.
func TestPairResultBanner(t *testing.T) {
	srv := newPairServer(t, &staticPairing{p: testPayload(t)})

	cases := map[string]string{
		"ok":      "Device paired.",
		"invalid": "That pairing code was not recognized.",
		"self":    "That is this device",
		"error":   "Pairing failed.",
	}
	for state, want := range cases {
		body := get(t, srv, "/settings?pair="+state).Body.String()
		if !contains(body, want) {
			t.Errorf("?pair=%s missing banner text %q", state, want)
		}
	}
	body := get(t, srv, "/settings?pair=<script>alert(1)</script>").Body.String()
	if contains(body, `role="status"`) {
		t.Error("out-of-enum pair state rendered a banner")
	}
}
