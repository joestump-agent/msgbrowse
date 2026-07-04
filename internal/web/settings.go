// The Connect/Settings page: MCP connection details plus the device-sync
// pairing section, served by the normal web app so browser and desktop modes
// render the identical template with identical data — nothing here is
// desktop-conditional (SPEC-0010 design.md migration step 1).
//
// The pairing section renders this node's Syncthing DEVICE ID as a QR plus a
// manual code (the #104 UX shape with the payload swapped per ADR-0021), the
// paste-a-code pair form, and the paired-device registry. The payload is a
// public introduction — device ID, folder ids, friendly name — never a
// secret: a scanned QR grants nothing until BOTH nodes have paired with each
// other's ID (SPEC-0014 §Trust Model).
//
// Governing: SPEC-0010 REQ "Connect/Settings page in the web app" (endpoint
// URL + JSON client-config block + `claude mcp add` line, with copy
// affordances), REQ "Server-rendered QR code" (PNG data: URI via a pure-Go
// library, no CSP change); ADR-0021 ("the /settings pairing page stays
// loopback … it now displays a device ID instead of a token payload");
// SPEC-0014 REQ "Pairing via Device ID and QR", §Authentication (loopback
// pairing routes), §Accessibility ("QR Code and Manual Device-ID Fallback").
package web

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/joestump/msgbrowse/internal/devices"
	"github.com/joestump/msgbrowse/internal/devsync"
	"github.com/joestump/msgbrowse/internal/mcp"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/syncthing"
	qrcode "github.com/skip2/go-qrcode"
)

// mcpEndpointPath is the path the MCP streamable-HTTP handler is mounted at
// when it rides the web listener (the desktop shell's
// cmd/msgbrowse-desktop/internal/embedded.MCPPath — kept in lockstep by
// TestSettingsMCPBlocks). SPEC-0010's bind surface allows no listener beyond
// the embedded server, so the MCP endpoint is a path on this server's own
// address, not a second port.
const mcpEndpointPath = "/mcp"

// qrSizePx is the rendered QR image edge in pixels. 220px keeps the compact
// device-ID payload comfortably scannable without dominating the page.
const qrSizePx = 220

// PairingSource is the device-sync seam behind the /settings pairing section
// (the SetDetector/SetEnabler pattern): serve and the desktop shell wire
// internal/devsync's Manager over the supervised Syncthing engine; tests
// wire fakes. With no source wired — device sync disabled, or the engine
// failed to start — the page renders its labeled absent states.
//
// This replaces the retired SPEC-0011 token-window source: the payload is
// now this node's Syncthing device ID + folder introduction (public data,
// SPEC-0014 §Trust Model), and pairing is a symmetric explicit-accept on
// each node rather than a secret exchange.
type PairingSource interface {
	// ActivePairing returns this node's pairing payload — its Syncthing
	// device ID, managed folder ids, and friendly name — or ok=false when
	// the engine has not answered yet.
	ActivePairing(ctx context.Context) (*devices.SyncPayload, bool)
	// Pair executes the explicit accept of the OTHER node's pairing code:
	// persist the peer, add its device to the daemon, share the managed
	// archive folders (SPEC-0014 "Pairing via Device ID and QR").
	Pair(ctx context.Context, code string) (devices.SyncPeer, error)
	// Unpair severs a paired peer: delete its registry row, remove its
	// device from the daemon, unshare every folder — local archives stay
	// (SPEC-0014 REQ "Unpair and Revoke"). Returns devices.ErrUnknownSyncPeer
	// (wrapped) for a device ID with no registry row.
	Unpair(ctx context.Context, deviceID string) (devices.SyncPeer, error)
	// Peers lists the explicitly-paired device registry for display.
	Peers(ctx context.Context) ([]devices.SyncPeer, error)
}

// SetPairingSource wires the device-sync pairing manager into /settings. Call
// it after NewServer and before serving begins — handlers read the field
// without locking, so late wiring would race.
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
	// engine-not-ready state.
	DeviceSyncEnabled bool
	// Pairing is non-nil while the sync engine is running and has reported
	// its device ID.
	Pairing *settingsPairing
	// Peers is the explicitly-paired device registry (empty slice when none).
	Peers []settingsPeer
	// PairResult is the post-redirect banner state after a pair POST: one of
	// "ok", "invalid", "self", "unavailable", "error" — a fixed enum mapped
	// to text by the template, never request-derived prose.
	PairResult string
	// UnpairResult is the post-redirect banner state after an unpair POST:
	// one of "ok", "invalid", "unknown", "unavailable", "error" — the same
	// fixed-enum contract as PairResult (#158). The two-step confirm state is
	// carried per peer (settingsPeer.ConfirmUnpair), not here.
	UnpairResult string
	// EngineRunning / EngineStateKnown carry the live engine snapshot for the
	// pairing section's status line: StateKnown=false (no monitor wired, or
	// the snapshot failed) renders nothing rather than a guess.
	EngineRunning    bool
	EngineStateKnown bool
	// SetupToken is the per-session token the pair form submits through the
	// same checkSetupPOST gate the Setup POSTs use (SPEC-0013 §Security,
	// reused verbatim per issue #157). Empty when no pairing source is wired.
	SetupToken string
}

// settingsPairing is the pairing section's data while the engine is running.
type settingsPairing struct {
	// QRDataURI is the server-rendered PNG QR of the payload as a data: URI
	// (SPEC-0010 "Server-rendered QR code"); img-src 'self' data: already
	// permits it.
	QRDataURI template.URL
	// ManualCode is the copyable manual pairing code carrying the same fields
	// as the QR (SPEC-0014 §Accessibility: the manual code IS the
	// accessibility affordance — a QR scan is never the only path).
	ManualCode string
	// DeviceID is this node's Syncthing device ID as selectable text.
	DeviceID string
	// Name is this node's friendly device name from the payload.
	Name string
	// FolderLabels are the human labels of the archive folders the payload
	// introduces (e.g. "Signal", "iMessage").
	FolderLabels []string
}

// settingsPeer is one paired device row for the registry list.
type settingsPeer struct {
	Name     string
	DeviceID string
	ShortID  string
	// Folders are the human labels of the shared archive folders.
	Folders  []string
	PairedAt string
	// SyncedIn are the labels of sources this node RECEIVES from the peer
	// (the peer is their importer — SPEC-0014 REQ "Importer and Replica
	// Roles"); rendered as the roles line.
	SyncedIn []string
	// Live connection state from the engine (#158): StateKnown=false (engine
	// down or no monitor) renders "state unknown", never a fake
	// "disconnected". LastSeen is the recorded last connection ("" if never).
	StateKnown bool
	Connected  bool
	Paused     bool
	LastSeen   string
	// ConfirmUnpair renders this row's inline two-step unpair confirmation
	// (#158): the first Unpair POST redirects back with this row marked; only
	// the confirmed second POST (confirm=1) removes anything — the Disable
	// flow's contract, PRG-shaped for this plain-form page.
	ConfirmUnpair bool
}

// pairResultStates is the fixed enum of ?pair= banner states. Anything else
// in the query string renders nothing.
var pairResultStates = map[string]bool{
	"ok": true, "invalid": true, "self": true, "unavailable": true, "error": true,
}

// unpairResultStates is the fixed enum of ?unpair= banner states (#158);
// "confirm" is handled separately (it marks a peer row, not a banner).
var unpairResultStates = map[string]bool{
	"ok": true, "invalid": true, "unknown": true, "unavailable": true, "error": true,
}

// handleSettings renders the Connect/Settings page. GET-only (the route
// pattern enforces it); the only query parameter consulted is the fixed-enum
// ?pair= banner state from the pair POST's redirect.
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
	if pr := r.URL.Query().Get("pair"); pairResultStates[pr] {
		data.PairResult = pr
	}
	if ur := r.URL.Query().Get("unpair"); unpairResultStates[ur] {
		data.UnpairResult = ur
	}
	// The two-step unpair confirm state (#158): ?unpair=confirm&device=<id>
	// marks exactly the matching REGISTRY peer's row. The device parameter is
	// request-derived but strictly validated — it must canonicalize as a
	// Syncthing device ID AND equal a registry row's — so it selects from the
	// paired set, never carrying free text into the render.
	var confirmDeviceID string
	if r.URL.Query().Get("unpair") == "confirm" {
		if id, err := devices.CanonicalDeviceID(r.URL.Query().Get("device")); err == nil {
			confirmDeviceID = id
		}
	}

	if s.pairing != nil {
		if p, ok := s.pairing.ActivePairing(r.Context()); ok {
			pairing, err := newSettingsPairing(p)
			if err != nil {
				s.serverError(w, err)
				return
			}
			data.Pairing = pairing
		}
		peers, err := s.pairing.Peers(r.Context())
		if err != nil {
			// The page must still render (SPEC-0014 REQ "Error Handling
			// Standards": surfaced, not fatal to an unrelated surface).
			s.log.Warn("settings: could not list paired devices", "error", err)
		}
		// Join the live engine snapshot onto the registry rows (#158;
		// SPEC-0014 REQ "Status and Doctor Surfacing"): per-peer
		// connected/disconnected without ever opening Syncthing's GUI. A nil
		// snapshot (engine down, no monitor) leaves StateKnown=false.
		peerStates := make(map[string]devsync.PeerStatus)
		if snap := s.syncStatusSnapshot(r.Context()); snap != nil {
			data.EngineStateKnown = true
			data.EngineRunning = snap.Running
			for _, ps := range snap.Peers {
				peerStates[ps.DeviceID] = ps
			}
		}
		for _, p := range peers {
			row := settingsPeer{
				Name:          p.Name,
				DeviceID:      p.DeviceID,
				ShortID:       p.ShortID(),
				Folders:       folderLabels(p.Folders),
				PairedAt:      p.PairedAt.Local().Format("2006-01-02 15:04"),
				SyncedIn:      importerLabels(p),
				ConfirmUnpair: p.DeviceID == confirmDeviceID,
			}
			if !p.LastSeenAt.IsZero() {
				row.LastSeen = p.LastSeenAt.Local().Format("2006-01-02 15:04")
			}
			if ps, ok := peerStates[p.DeviceID]; ok && ps.StateKnown {
				row.StateKnown = true
				row.Connected = ps.Connected
				row.Paused = ps.Paused
			}
			data.Peers = append(data.Peers, row)
		}
		// The pair and unpair forms post through the same same-origin +
		// per-session token gate as the Setup POSTs (issue #157/#158
		// Security Checklists).
		tok, err := s.setupTokens.mint()
		if err != nil {
			s.serverError(w, err)
			return
		}
		data.SetupToken = tok
	}

	s.render(w, r, "settings", data)
}

// importerLabels lists the human source labels the PEER is the recorded
// importer for — the sources this node receives from it (SPEC-0014 REQ
// "Importer and Replica Roles").
func importerLabels(p devices.SyncPeer) []string {
	var out []string
	for _, src := range source.All {
		if p.ImporterFor(src) {
			out = append(out, source.Label(src))
		}
	}
	return out
}

// newSettingsPairing encodes the device-ID payload into its two page
// presentations: the QR PNG data URI and the manual code (identical fields,
// SPEC-0014 REQ "Pairing via Device ID and QR").
func newSettingsPairing(p *devices.SyncPayload) (*settingsPairing, error) {
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
		QRDataURI:    uri,
		ManualCode:   manual,
		DeviceID:     p.DeviceID,
		Name:         p.Name,
		FolderLabels: folderLabels(p.Folders),
	}, nil
}

// folderLabels maps managed folder ids ("msgbrowse-signal") to their human
// source labels ("Signal"); unknown ids pass through unchanged so nothing
// renders empty.
func folderLabels(folderIDs []string) []string {
	out := make([]string, 0, len(folderIDs))
	for _, id := range folderIDs {
		out = append(out, source.Label(strings.TrimPrefix(id, syncthing.FolderIDPrefix)))
	}
	return out
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

// handleDevicePair is POST /settings/devices/pair — the privileged action
// that accepts another node's pairing code. It enforces the SAME gate as the
// privileged Setup POSTs (checkSetupPOST: same-origin + per-session token +
// MaxBytesReader body cap, reused verbatim per issue #157 Security
// Checklist) BEFORE any work, then follows POST-redirect-GET back to
// /settings with a fixed-enum ?pair= banner state — no request-derived text
// ever enters the redirect target.
//
// Governing: SPEC-0014 §Authentication ("POST /settings/devices/pair …
// loopback"), SPEC-0013 §Security "Same-origin protection for privileged
// POSTs" (the reused gate), SPEC-0014 REQ "Error Handling Standards".
func (s *Server) handleDevicePair(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return // 403 already written; nothing was mutated
	}
	if s.pairing == nil {
		s.redirectPairResult(w, r, "unavailable")
		return
	}
	code := strings.TrimSpace(r.PostFormValue("code"))
	if code == "" {
		s.redirectPairResult(w, r, "invalid")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	peer, err := s.pairing.Pair(ctx, code)
	switch {
	case err == nil:
		s.log.Info("device paired from settings", "device_id", peer.DeviceID, "name", peer.Name)
		s.redirectPairResult(w, r, "ok")
	case errors.Is(err, devices.ErrSelfPair):
		s.redirectPairResult(w, r, "self")
	case errors.Is(err, devices.ErrInvalidSyncPayload):
		s.redirectPairResult(w, r, "invalid")
	default:
		s.log.Error("device pairing failed", "error", err)
		s.redirectPairResult(w, r, "error")
	}
}

// redirectPairResult finishes the pair POST with a 303 See Other back to the
// settings page carrying the fixed-enum banner state (PRG: a refresh never
// replays the POST).
func (s *Server) redirectPairResult(w http.ResponseWriter, r *http.Request, state string) {
	http.Redirect(w, r, "/settings?pair="+state, http.StatusSeeOther)
}

// handleDeviceUnpair is POST /settings/devices/unpair — the privileged action
// that severs a paired device (SPEC-0014 REQ "Unpair and Revoke"; issue
// #158). It enforces the SAME gate as the privileged Setup POSTs
// (checkSetupPOST: same-origin + per-session token + MaxBytesReader body cap)
// BEFORE any work, and it is TWO-STEP like the Providers Disable flow: a POST
// without confirm=1 removes nothing — it redirects back to /settings with the
// target row marked for inline confirmation — and only the confirmed second
// POST unpairs. Both legs follow POST-redirect-GET with fixed-enum ?unpair=
// states; the only request-derived value in a redirect is the device ID,
// canonicalized through devices.CanonicalDeviceID (strict format validation)
// before it is echoed.
//
// Unpairing removes the peer's registry row, its daemon device entry, and
// every folder share — sync to it stops immediately — while local archives
// and the database stay intact (destructive only to future sync, never to
// data, per the SPEC-0014 Authentication table).
func (s *Server) handleDeviceUnpair(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return // 403 already written; nothing was mutated
	}
	if s.pairing == nil {
		s.redirectUnpairResult(w, r, "unavailable")
		return
	}
	deviceID, err := devices.CanonicalDeviceID(r.PostFormValue("device_id"))
	if err != nil {
		s.redirectUnpairResult(w, r, "invalid")
		return
	}

	if r.PostFormValue("confirm") != "1" {
		// Step 1: no mutation. Redirect back with the peer's row marked for
		// the inline confirmation (rendered only if the ID matches a registry
		// row). The echoed ID is canonical — validated format, no free text.
		http.Redirect(w, r, "/settings?unpair=confirm&device="+url.QueryEscape(deviceID), http.StatusSeeOther)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	peer, err := s.pairing.Unpair(ctx, deviceID)
	switch {
	case err == nil:
		s.log.Info("device unpaired from settings", "device_id", peer.DeviceID, "name", peer.Name)
		s.redirectUnpairResult(w, r, "ok")
	case errors.Is(err, devices.ErrUnknownSyncPeer):
		s.redirectUnpairResult(w, r, "unknown")
	default:
		s.log.Error("device unpair failed", "error", err)
		s.redirectUnpairResult(w, r, "error")
	}
}

// redirectUnpairResult finishes the unpair POST with a 303 back to the
// settings page carrying the fixed-enum banner state (PRG: a refresh never
// replays the POST).
func (s *Server) redirectUnpairResult(w http.ResponseWriter, r *http.Request, state string) {
	http.Redirect(w, r, "/settings?unpair="+state, http.StatusSeeOther)
}
