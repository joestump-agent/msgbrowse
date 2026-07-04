// Tests for the Connect/Settings page (issues #100 + #103).
//
// Coverage per SPEC-0010: template render in browser (full document) and HTMX
// partial modes with the MCP endpoint URL, JSON client-config block, and
// `claude mcp add` line present in both; the server-rendered QR as a PNG
// data: URI with the manual pairing code as its text fallback; the
// placeholder/absent states with device sync unconfigured; unchanged security
// headers (§Security Requirements); and the §Accessibility Requirements
// attribute contract (single h1, aria-labels on icon-only copy buttons, the
// aria-live region, QR alt text).
package web

import (
	"encoding/base64"
	"html"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/devices"
	"github.com/joestump/msgbrowse/internal/mcp"
)

// staticPairing is a canned PairingSource: whatever #105's live implementation
// looks like, the page contract only needs "a payload or not".
type staticPairing struct{ p *devices.PairingPayload }

func (s staticPairing) ActivePairing() (*devices.PairingPayload, bool) { return s.p, s.p != nil }

// testPayload builds a valid SPEC-0011 v1 pairing payload.
func testPayload(t *testing.T) *devices.PairingPayload {
	t.Helper()
	p, err := devices.NewPairingPayload(
		"192.168.1.10:8788",
		base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("t", 32))),
		strings.Repeat("ab", 32),
	)
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
	if !contains(body, "Device pairing") {
		t.Error("partial missing the device-pairing section")
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

// TestSettingsEnabledNoWindowState: device_sync.enabled=true with no open
// pairing window renders the no-window absent state, still QR-free.
func TestSettingsEnabledNoWindowState(t *testing.T) {
	st, cfg, _ := newTestStoreAndConfig(t)
	cfg.DeviceSync.Enabled = true
	srv, err := NewServer(st, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	body := get(t, srv, "/settings").Body.String()
	if !contains(body, "No pairing window is open.") {
		t.Error("enabled-without-window state missing its explanatory text")
	}
	if contains(body, "data:image/png") {
		t.Error("no QR may render without an open pairing window")
	}
	if contains(body, "Device sync is not enabled.") {
		t.Error("enabled mode must not render the disabled-state instructions")
	}
}

// TestSettingsQRRendersFromPairingPayload is the SPEC-0010 "Server-rendered QR
// code" scenario: with a pairing payload available, the QR appears as an <img>
// whose src is a PNG data: URI (decodable, real PNG bytes), with alt text and
// the manual pairing code as the text path to the same payload.
func TestSettingsQRRendersFromPairingPayload(t *testing.T) {
	st, cfg, _ := newTestStoreAndConfig(t)
	cfg.DeviceSync.Enabled = true
	srv, err := NewServer(st, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	payload := testPayload(t)
	srv.SetPairingSource(staticPairing{p: payload})

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

	// Alt text states the image's purpose (§Accessibility "QR alternative").
	if !contains(body, `alt="Device pairing QR code.`) {
		t.Error("QR <img> missing purpose-stating alt text")
	}
	// The QR is never the only path: the manual code (same fields) is present
	// as selectable, copyable text, plus the sync endpoint.
	manual, err := payload.EncodeManualCode()
	if err != nil {
		t.Fatalf("encode manual code: %v", err)
	}
	if !contains(body, `<code id="pairing-code">`+manual+`</code>`) {
		t.Error("settings missing the manual pairing code text fallback")
	}
	if !contains(body, payload.Endpoint) {
		t.Error("settings missing the sync endpoint as text")
	}
	// A copy affordance covers the code too.
	if !contains(body, `aria-label="Copy manual pairing code"`) {
		t.Error("manual pairing code missing its labeled copy button")
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
