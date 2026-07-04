// Tests for the Connect/Settings page (issues #100 + #103, pairing payload
// swapped to the Syncthing device ID by #157).
//
// Coverage per SPEC-0010 + SPEC-0014: template render in browser (full
// document) and HTMX partial modes with the MCP endpoint URL, JSON
// client-config block, and `claude mcp add` line present in both; the
// server-rendered device-ID QR as a PNG data: URI with the manual code and
// selectable device ID as its text fallback (SPEC-0014 §Accessibility "QR
// Code and Manual Device-ID Fallback"); the placeholder/absent states with
// device sync unconfigured; unchanged security headers (§Security
// Requirements); and the accessibility attribute contract (single h1,
// aria-labels on icon-only copy buttons, the aria-live region, QR alt text).
package web

import (
	"context"
	"encoding/base64"
	"html"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/devices"
	"github.com/joestump/msgbrowse/internal/mcp"
)

// Valid Syncthing-format device IDs (Luhn check digits intact) for fixtures.
const (
	testSelfDeviceID = "QRUVHQ4-LQMFCKZ-JPKWU3L-TJNB6NX-XZXB2AV-FLJ5RL4-DC2QFCT-EBHK5AG"
	testPeerDeviceID = "XW4UY46-VHRCAEN-OTRLIUX-BIIMJVP-KPVFKQW-4H5TU2H-MYSYKFX-S53S7AL"
)

// staticPairing is a canned PairingSource: the page contract needs "a payload
// or not", a recordable Pair, a recordable Unpair, and a peer list.
type staticPairing struct {
	p         *devices.SyncPayload
	peers     []devices.SyncPeer
	pairErr   error
	unpairErr error
	lastCode  string
	paired    int
	unpaired  []string // device IDs Unpair was called with
}

func (s *staticPairing) ActivePairing(context.Context) (*devices.SyncPayload, bool) {
	return s.p, s.p != nil
}

func (s *staticPairing) Pair(_ context.Context, code string) (devices.SyncPeer, error) {
	s.lastCode = code
	if s.pairErr != nil {
		return devices.SyncPeer{}, s.pairErr
	}
	s.paired++
	return devices.SyncPeer{DeviceID: testPeerDeviceID, Name: "other-mac"}, nil
}

func (s *staticPairing) Unpair(_ context.Context, deviceID string) (devices.SyncPeer, error) {
	s.unpaired = append(s.unpaired, deviceID)
	if s.unpairErr != nil {
		return devices.SyncPeer{}, s.unpairErr
	}
	for i, p := range s.peers {
		if p.DeviceID == deviceID {
			s.peers = append(s.peers[:i], s.peers[i+1:]...)
			return p, nil
		}
	}
	return devices.SyncPeer{}, devices.ErrUnknownSyncPeer
}

func (s *staticPairing) Peers(context.Context) ([]devices.SyncPeer, error) {
	return s.peers, nil
}

// testPayload builds a valid SPEC-0014 v2 device-ID pairing payload.
func testPayload(t *testing.T) *devices.SyncPayload {
	t.Helper()
	p, err := devices.NewSyncPayload(testSelfDeviceID, []string{"msgbrowse-signal"}, "studio-mac")
	if err != nil {
		t.Fatalf("build pairing payload: %v", err)
	}
	return p
}

// TestSettingsMCPBlocks verifies the page's reason to exist (SPEC-0010
// "Connect/Settings page in the web app"): the MCP endpoint URL derived from
// the live request host, the JSON client-configuration block, and the
// `claude mcp add` line — the latter two byte-identical (modulo HTML escaping)
// to internal/mcp's builders, the single source the desktop menubar also uses.
func TestSettingsMCPBlocks(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := get(t, srv, "/settings")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// httptest.NewRequest sets Host: example.com — the endpoint must be built
	// from the live request host, not a configured or hardcoded address.
	const endpoint = "http://example.com/mcp"
	if !contains(body, `<code id="mcp-endpoint">`+endpoint+`</code>`) {
		t.Errorf("settings missing MCP endpoint URL %q built from the request host", endpoint)
	}
	// Golden content check against the shared builders (no duplicate builder
	// may drift): the page must carry their output verbatim, HTML-escaped.
	if want := html.EscapeString(mcp.ClientConfigJSON(endpoint)); !contains(body, want) {
		t.Errorf("settings missing the mcp.ClientConfigJSON block:\n%s", want)
	}
	if want := html.EscapeString(mcp.ClaudeMCPAddCommand(endpoint)); !contains(body, want) {
		t.Errorf("settings missing the mcp.ClaudeMCPAddCommand line: %s", want)
	}
}

// TestSettingsPartialCarriesBothBlocks: the HTMX boosted swap unit renders the
// same MCP data as the full document (#116's *_content pattern) — SPEC-0010
// demands identical data in every render mode.
func TestSettingsPartialCarriesBothBlocks(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := getPartial(t, srv, "/settings")
	if rec.Code != http.StatusOK {
		t.Fatalf("partial status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "<title>Settings · msgbrowse</title>") {
		t.Error("partial missing the page title for htmx history")
	}
	const endpoint = "http://example.com/mcp"
	for name, want := range map[string]string{
		"endpoint URL":      endpoint,
		"JSON config block": html.EscapeString(mcp.ClientConfigJSON(endpoint)),
		"claude mcp add":    html.EscapeString(mcp.ClaudeMCPAddCommand(endpoint)),
	} {
		if !contains(body, want) {
			t.Errorf("partial missing the %s", name)
		}
	}
	// The pairing section rides along in the swap too.
	if !contains(body, "Device sync") {
		t.Error("partial missing the device-sync section")
	}
}

// TestSettingsSecurityHeaders pins the SPEC-0010 §Security posture: /settings
// flows through the unchanged securityHeaders middleware — byte-identical CSP
// to every other page (img-src 'self' data: already admits the QR, no new
// carve-outs) — and the route is GET-only.
func TestSettingsSecurityHeaders(t *testing.T) {
	srv, _, _ := newTestServer(t)
	settings := get(t, srv, "/settings")
	home := get(t, srv, "/")

	csp := settings.Header().Get("Content-Security-Policy")
	if csp == "" || csp != home.Header().Get("Content-Security-Policy") {
		t.Errorf("settings CSP diverges from the rest of the app: %q", csp)
	}
	if !contains(csp, "img-src 'self' data:") {
		t.Errorf("CSP lost the data: image source the QR relies on: %q", csp)
	}
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		"X-Frame-Options":        "DENY",
	} {
		if got := settings.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	// GET-only: the mux route pattern rejects every other method.
	if rec := post(t, srv, "/settings"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /settings = %d, want 405", rec.Code)
	}
}

// TestSettingsDeviceSyncDisabledState: with device sync unconfigured (the
// default) the pairing section renders the labeled enable-instructions state —
// the page is complete with no QR and no pairing payload.
func TestSettingsDeviceSyncDisabledState(t *testing.T) {
	srv, _, _ := newTestServer(t)
	body := get(t, srv, "/settings").Body.String()
	if !contains(body, "Device sync is not enabled.") {
		t.Error("disabled state missing its explanatory text")
	}
	if !contains(body, "device_sync:") || !contains(body, "enabled: true") {
		t.Error("disabled state missing the enable-instructions config snippet")
	}
	if contains(body, "data:image/png") {
		t.Error("no QR may render while device sync is disabled")
	}
}

// TestSettingsEnabledNoEngineState: device_sync.enabled=true with no pairing
// source answering (engine not up) renders the engine-absent state, QR-free.
func TestSettingsEnabledNoEngineState(t *testing.T) {
	st, cfg, _ := newTestStoreAndConfig(t)
	cfg.DeviceSync.Enabled = true
	srv, err := NewServer(st, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	body := get(t, srv, "/settings").Body.String()
	if !contains(body, "The sync engine is not running.") {
		t.Error("enabled-without-engine state missing its explanatory text")
	}
	if contains(body, "data:image/png") {
		t.Error("no QR may render without a running sync engine")
	}
	if contains(body, "Device sync is not enabled.") {
		t.Error("enabled mode must not render the disabled-state instructions")
	}
}

// TestSettingsQRRendersDeviceID is the SPEC-0014 "Pairing via Device ID and
// QR" render scenario: with device sync enabled and the engine reporting its
// device ID, the QR appears as an <img> whose src is a PNG data: URI
// (decodable, real PNG bytes), with the SPEC-0014 alt text and BOTH text
// fallbacks — the manual code and the selectable device ID — plus the pair
// form gated by the per-session token.
func TestSettingsQRRendersDeviceID(t *testing.T) {
	st, cfg, _ := newTestStoreAndConfig(t)
	cfg.DeviceSync.Enabled = true
	srv, err := NewServer(st, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	payload := testPayload(t)
	srv.SetPairingSource(&staticPairing{p: payload})

	rec := get(t, srv, "/settings")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// The <img> src is a data: URI carrying genuine PNG bytes. html/template
	// would have rewritten an untyped data: URL to #ZgotmplZ — this also proves
	// the template.URL plumbing held. The attribute value is HTML-escaped
	// (base64's '+' renders as &#43;), so unescape before decoding.
	m := regexp.MustCompile(`<img class="qr-img" src="data:image/png;base64,([^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("settings missing the QR <img> with a PNG data: URI src")
	}
	png, err := base64.StdEncoding.DecodeString(html.UnescapeString(m[1]))
	if err != nil {
		t.Fatalf("QR data URI is not valid base64: %v", err)
	}
	if len(png) < 8 || string(png[1:4]) != "PNG" {
		t.Error("QR data URI does not decode to PNG bytes")
	}

	// Alt text per SPEC-0014 §Accessibility: identifies the pairing purpose
	// and directs to the manual code alternative.
	if !contains(body, `alt="Device pairing QR code — a text device-ID code is provided below."`) {
		t.Error("QR <img> missing the SPEC-0014 alt text")
	}
	// The QR is never the only path: the manual code (same fields) AND the
	// device ID are present as selectable, copyable text.
	manual, err := payload.EncodeManualCode()
	if err != nil {
		t.Fatalf("encode manual code: %v", err)
	}
	if !contains(body, `<code id="pairing-code">`+manual+`</code>`) {
		t.Error("settings missing the manual pairing code text fallback")
	}
	if !contains(body, `<code id="device-id">`+testSelfDeviceID+`</code>`) {
		t.Error("settings missing the selectable device ID text")
	}
	for _, want := range []string{
		`aria-label="Copy manual pairing code"`,
		`aria-label="Copy this device's ID"`,
	} {
		if !contains(body, want) {
			t.Errorf("settings missing copy affordance %s", want)
		}
	}
	// The payload is a device ID, not a secret: the page says so.
	if !contains(body, "The ID is public") {
		t.Error("settings missing the public-ID/both-ends-accept explanation")
	}
	// The pair form posts through the privileged gate: same-origin action +
	// hidden per-session token (issue #157 Security Checklist).
	if !contains(body, `action="/settings/devices/pair"`) {
		t.Error("settings missing the pair form")
	}
	if !regexp.MustCompile(`<input type="hidden" name="setup_token" value="[0-9a-f]{64}">`).MatchString(body) {
		t.Error("pair form missing the minted per-session token")
	}
}

// TestSettingsPairedDevicesList: the explicitly-paired registry renders with
// name, short + full device ID, shared folders, and pairing time — and shows
// the labeled empty state when nothing is paired yet.
func TestSettingsPairedDevicesList(t *testing.T) {
	st, cfg, _ := newTestStoreAndConfig(t)
	cfg.DeviceSync.Enabled = true
	srv, err := NewServer(st, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	src := &staticPairing{p: testPayload(t)}
	srv.SetPairingSource(src)

	body := get(t, srv, "/settings").Body.String()
	if !contains(body, "No devices paired yet.") {
		t.Error("empty registry missing its labeled state")
	}

	src.peers = []devices.SyncPeer{{
		DeviceID: testPeerDeviceID,
		Name:     "kitchen-mac",
		Folders:  []string{"msgbrowse-signal", "msgbrowse-imessage"},
		PairedAt: time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC),
	}}
	body = get(t, srv, "/settings").Body.String()
	if !contains(body, "kitchen-mac") {
		t.Error("registry missing the peer name")
	}
	if !contains(body, testPeerDeviceID) {
		t.Error("registry missing the peer's full device ID")
	}
	if !contains(body, devices.ShortDeviceID(testPeerDeviceID)) {
		t.Error("registry missing the peer's short device ID")
	}
	if !contains(body, "Signal") || !contains(body, "iMessage") {
		t.Error("registry missing the human folder labels")
	}
}

// TestSettingsAccessibilityContract asserts the SPEC-0010 §Accessibility
// attribute requirements: exactly one h1 inside the existing landmarks, an
// aria-label on every icon-only copy button naming what it copies, and the
// polite live region that announces copy confirmations.
func TestSettingsAccessibilityContract(t *testing.T) {
	srv, _, _ := newTestServer(t)
	body := get(t, srv, "/settings").Body.String()

	// Landmarks: the shell provides <main id="main-content"> and the sidebar
	// <nav>; the page holds a single h1.
	if !contains(body, `<main id="main-content"`) {
		t.Error("settings missing the main landmark")
	}
	if n := strings.Count(body, "<h1"); n != 1 {
		t.Errorf("settings has %d h1 elements, want exactly 1", n)
	}

	// Icon-only copy buttons: every data-copy-target button carries an
	// aria-label saying what it copies.
	btns := regexp.MustCompile(`<button[^>]*data-copy-target[^>]*>`).FindAllString(body, -1)
	if len(btns) != 3 {
		t.Fatalf("settings renders %d copy buttons, want 3 (endpoint, JSON, command)", len(btns))
	}
	for _, want := range []string{
		`aria-label="Copy MCP endpoint URL"`,
		`aria-label="Copy MCP client configuration JSON"`,
		`aria-label="Copy claude mcp add command"`,
	} {
		if !contains(body, want) {
			t.Errorf("settings missing copy-button %s", want)
		}
	}
	for _, b := range btns {
		if !contains(b, `aria-label="`) {
			t.Errorf("copy button lacks an aria-label: %s", b)
		}
		if !contains(b, `type="button"`) {
			t.Errorf("copy button should be type=button (keyboard-activatable, non-submitting): %s", b)
		}
	}

	// Dynamic feedback: the polite live region copy.js announces into.
	if !contains(body, `id="copy-announce"`) || !contains(body, `aria-live="polite"`) {
		t.Error("settings missing the aria-live=polite copy-confirmation region")
	}

	// The copy wiring itself is CSP-safe: external script, no inline handlers.
	if !contains(body, `src="/static/copy.js"`) {
		t.Error("settings shell missing the self-hosted copy.js")
	}
	if contains(body, "onclick=") {
		t.Error("settings must not use inline event handlers (script-src 'self')")
	}
}

// TestSettingsSidebarNavLink: the sidebar gains a boosted Settings entry like
// the other primary nav links (it lives inside the aside's hx-boost scope).
func TestSettingsSidebarNavLink(t *testing.T) {
	srv, _, _ := newTestServer(t)
	body := get(t, srv, "/").Body.String()
	if !contains(body, `href="/settings"`) {
		t.Error("sidebar missing the /settings nav link")
	}
	if !contains(body, "<span>Settings</span>") {
		t.Error("sidebar Settings link missing its label")
	}
}

// TestSettingsMCPPathMatchesDesktopMount pins mcpEndpointPath to the desktop
// shell's mount (cmd/msgbrowse-desktop/internal/embedded.MCPPath is a nested
// module this package cannot import; this guard is the lockstep contract).
func TestSettingsMCPPathMatchesDesktopMount(t *testing.T) {
	if mcpEndpointPath != "/mcp" {
		t.Errorf("mcpEndpointPath = %q; must stay in lockstep with embedded.MCPPath (/mcp)", mcpEndpointPath)
	}
}

// TestBuiltCSSCarriesSettingsComponents guards the ADR-0012 drift rule for the
// new classes: the committed, go:embed-served app.css must carry the settings
// copy-block/QR rules (rebuild: rm -rf .tools && make css).
func TestBuiltCSSCarriesSettingsComponents(t *testing.T) {
	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	out := string(css)
	for _, want := range []string{
		".copy-block",             // bordered copyable block
		".copy-btn",               // icon-only copy button
		".copy-btn:focus-visible", // visible keyboard focus (WCAG 2.1 AA)
		".copied",                 // acknowledgment state copy.js toggles
		".qr-panel",               // QR + manual-code layout
		".qr-img",                 // QR image frame
		".sr-only",                // visually-hidden live region utility
	} {
		if !strings.Contains(out, want) {
			t.Errorf("built app.css missing %q (rebuild: rm -rf .tools && make css)", want)
		}
	}
}
