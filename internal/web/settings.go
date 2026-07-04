// The Connect/Settings page: MCP connection details plus the device-pairing
// QR, served by the normal web app so browser and desktop modes render the
// identical template with identical data — nothing here is desktop-conditional
// (SPEC-0010 design.md migration step 1).
//
// Governing: SPEC-0010 REQ "Connect/Settings page in the web app" (endpoint
// URL + JSON client-config block + `claude mcp add` line, with copy
// affordances), REQ "Server-rendered QR code" (PNG data: URI via a pure-Go
// library, no CSP change), SPEC-0010 §Security Requirements (`/settings` is
// GET-only, public to the loopback operator, behind the unchanged
// securityHeaders middleware) and §Accessibility Requirements. The QR payload
// itself is SPEC-0011's contract (internal/devices.PairingPayload) — this
// page only encodes the bytes it is handed.
package web

import (
	"encoding/base64"
	"fmt"
	"html/template"
	"net"
	"net/http"

	"github.com/joestump/msgbrowse/internal/devices"
	"github.com/joestump/msgbrowse/internal/mcp"
	qrcode "github.com/skip2/go-qrcode"
)

// mcpEndpointPath is the path the MCP streamable-HTTP handler is mounted at
// when it rides the web listener (the desktop shell's
// cmd/msgbrowse-desktop/internal/embedded.MCPPath — kept in lockstep by
// TestSettingsMCPBlocks). SPEC-0010's bind surface allows no listener beyond
// the embedded server, so the MCP endpoint is a path on this server's own
// address, not a second port.
const mcpEndpointPath = "/mcp"

// qrSizePx is the rendered QR image edge in pixels. 220px keeps the ~170-byte
// version-1 payload comfortably scannable (SPEC-0011 sizes the payload for
// exactly this) without dominating the page.
const qrSizePx = 220

// PairingSource yields the live pairing payload for the settings page's QR
// section. It is the seam between SPEC-0010 (which renders the payload) and
// SPEC-0011 (which owns pairing windows): the device-sync listener story
// (#105) implements it over devices.Window and wires it in with
// SetPairingSource before the server starts. With no source wired — or no
// window open — the page renders its labeled absent state instead of a QR.
type PairingSource interface {
	// ActivePairing returns the payload for the currently-open pairing window,
	// or ok=false when no window is open. The payload carries a live pairing
	// secret: it goes into the rendered page and nowhere else — never logs.
	ActivePairing() (payload *devices.PairingPayload, ok bool)
}

// SetPairingSource wires the device-sync pairing state into /settings. Call it
// after NewServer and before serving begins — handlers read the field without
// locking, so late wiring would race.
func (s *Server) SetPairingSource(ps PairingSource) { s.pairing = ps }

// settingsData drives the Connect/Settings page. The same struct renders in
// browser and desktop modes — SPEC-0010 requires identical template and data.
type settingsData struct {
	baseData
	// MCPEndpointURL is the live MCP endpoint derived from the address the
	// client actually reached us on, so it is correct for both `msgbrowse
	// serve`'s configured bind and the desktop shell's ephemeral port.
	MCPEndpointURL string
	// MCPConfigJSON is the copy-paste client-configuration block, built by
	// internal/mcp.ClientConfigJSON — the same builder the desktop menubar's
	// "Copy MCP Config" uses, so the two can never drift.
	MCPConfigJSON string
	// MCPAddCommand is the equivalent `claude mcp add` one-liner
	// (internal/mcp.ClaudeMCPAddCommand).
	MCPAddCommand string
	// DeviceSyncEnabled mirrors config device_sync.enabled: false renders the
	// enable-instructions state, true without a payload renders the
	// no-open-window state.
	DeviceSyncEnabled bool
	// Pairing is non-nil only while a pairing window is open.
	Pairing *settingsPairing
}

// settingsPairing is the QR section's data while a pairing window is open.
type settingsPairing struct {
	// QRDataURI is the server-rendered PNG QR as a data: URI (SPEC-0010
	// "Server-rendered QR code"); img-src 'self' data: already permits it.
	QRDataURI template.URL
	// ManualCode is the copyable manual pairing code carrying the same fields
	// as the QR, so the QR is never the only path to the information
	// (SPEC-0010 §Accessibility "QR alternative").
	ManualCode string
	// Endpoint is the sync listener host:port from the payload, shown as
	// selectable text beside the QR.
	Endpoint string
}

// handleSettings renders the Connect/Settings page. GET-only (the route
// pattern enforces it), no query parameters trusted, no request body — the
// SPEC-0010 §Security Requirements posture for this endpoint.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	var base baseData
	if isPartialRequest(r) {
		// Boosted navigations skip the sidebar listing entirely (SPEC-0008
		// REQ-0008-006); this page needs no store work at all for the swap.
		base = partialBase("Settings · msgbrowse", 0)
	} else {
		var err error
		base, err = s.baseData(r.Context(), "Settings · msgbrowse", 0)
		if err != nil {
			s.serverError(w, err)
			return
		}
	}

	endpoint := mcpEndpointURL(r)
	data := settingsData{
		baseData:          base,
		MCPEndpointURL:    endpoint,
		MCPConfigJSON:     mcp.ClientConfigJSON(endpoint),
		MCPAddCommand:     mcp.ClaudeMCPAddCommand(endpoint),
		DeviceSyncEnabled: s.deviceSyncEnabled,
	}

	if s.pairing != nil {
		if p, ok := s.pairing.ActivePairing(); ok {
			pairing, err := newSettingsPairing(p)
			if err != nil {
				s.serverError(w, err)
				return
			}
			data.Pairing = pairing
		}
	}

	s.render(w, r, "settings", data)
}

// newSettingsPairing encodes a pairing payload into its two page
// presentations: the QR PNG data URI and the manual code (identical fields,
// SPEC-0011 REQ "Pairing Initiation").
func newSettingsPairing(p *devices.PairingPayload) (*settingsPairing, error) {
	qrBytes, err := p.EncodeQR()
	if err != nil {
		return nil, fmt.Errorf("encode pairing payload: %w", err)
	}
	uri, err := qrPNGDataURI(qrBytes)
	if err != nil {
		return nil, err
	}
	manual, err := p.EncodeManualCode()
	if err != nil {
		return nil, fmt.Errorf("encode manual pairing code: %w", err)
	}
	return &settingsPairing{
		QRDataURI:  uri,
		ManualCode: manual,
		Endpoint:   p.Endpoint,
	}, nil
}

// qrPNGDataURI renders opaque payload bytes as a QR code PNG and returns it
// as a data: URI for direct embedding in an <img> — no client-side QR
// generation and no CSP change (`img-src 'self' data:` already allows it,
// ADR-0010). The value is typed template.URL because html/template's URL
// sanitizer would otherwise reject the data: scheme; that is safe here — the
// entire value is server-constructed base64 of a PNG we just encoded, with no
// request-derived content.
func qrPNGDataURI(payload []byte) (template.URL, error) {
	png, err := qrcode.Encode(string(payload), qrcode.Medium, qrSizePx)
	if err != nil {
		return "", fmt.Errorf("render pairing QR: %w", err)
	}
	return template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(png)), nil
}

// mcpEndpointURL derives the MCP endpoint from the address the client used to
// reach this server, so the page always shows the live bound address: the
// desktop shell's ephemeral port and `msgbrowse serve`'s configured bind both
// come out right with zero mode-specific plumbing. The scheme is always http —
// the web server speaks plain HTTP on loopback in every mode (ADR-0010). The
// Host header is rendered only through html/template's escaping.
func mcpEndpointURL(r *http.Request) string {
	host := r.Host
	if host == "" {
		// HTTP/1.1 requires Host, so this is a defensive fallback: the
		// connection's own local address is the bound address by definition.
		if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
			host = la.String()
		}
	}
	return "http://" + host + mcpEndpointPath
}
