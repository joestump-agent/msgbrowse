// Tests for the #158 device-sync surfaces: importer/replica role enforcement
// on the Providers cards and the privileged Enable/Refresh POSTs, the
// two-step unpair flow behind the checkSetupPOST gate, and the status/logs
// rendering of a stubbed engine snapshot — healthy, paused, and errored
// folders must come out truthfully.
//
// Governing: SPEC-0014 REQ "Importer and Replica Roles" ("Single importer
// per source is enforced"), REQ "Unpair and Revoke" ("Unpair stops sync to
// that device immediately"), REQ "Status and Doctor Surfacing" ("A paused or
// errored sync shows in msgbrowse's status").
package web

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/devices"
	"github.com/joestump/msgbrowse/internal/devsync"
)

// testDiscardLogger returns a logger that swallows output.
func testDiscardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeMonitor is a canned SyncMonitor.
type fakeMonitor struct {
	status   *devsync.Status
	replicas map[string]devsync.ReplicaOf
	err      error
}

func (f *fakeMonitor) Status(context.Context) (*devsync.Status, error) {
	return f.status, f.err
}

func (f *fakeMonitor) ReplicaSources(context.Context) (map[string]devsync.ReplicaOf, error) {
	return f.replicas, f.err
}

// replicaMonitor marks signal as synced in from "kitchen-mac".
func replicaMonitor() *fakeMonitor {
	return &fakeMonitor{replicas: map[string]devsync.ReplicaOf{
		"signal": {PeerName: "kitchen-mac", PeerShortID: "XW4UY46"},
	}}
}

// TestProvidersSyncedReplicaCard: a synced-in source renders the synced card
// state — the "Synced" badge, the importer named, and NO Enable/Refresh/
// Disable controls — while the other sources' cards are untouched (the
// importer-unaffected half of the contract).
func TestProvidersSyncedReplicaCard(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetSyncMonitor(replicaMonitor())

	body := get(t, srv, "/providers").Body.String()

	// The signal card is the synced state, naming the importer.
	if !contains(body, `id="setup-card-signal" class="setup-card setup-card-synced"`) {
		t.Error("signal card is not in the synced state")
	}
	if !contains(body, "Synced from kitchen-mac (XW4UY46)") {
		t.Error("synced card does not name the importer peer")
	}
	// The synced card carries no action that would run an exporter or delete
	// data: no Enable/Refresh/Disable POST targets within the signal card.
	signalCard := between(t, body, `id="setup-card-signal"`, `</li>`)
	for _, action := range []string{"/setup/enable", "/setup/refresh", "/setup/disable"} {
		if contains(signalCard, action) {
			t.Errorf("synced card offers %s", action)
		}
	}
	// The fixture store has signal data imported, which would otherwise read
	// Enabled — synced must win so Refresh (exporter-running) is not offered.
	if contains(signalCard, "setup-badge-enabled") {
		t.Error("synced card fell through to the Enabled state")
	}
	// Other sources are unaffected (imessage renders one of the normal states).
	if !contains(body, `id="setup-card-imessage"`) || contains(body, "setup-card-imessage\" class=\"setup-card setup-card-synced") {
		t.Error("imessage card affected by signal's replica role")
	}
}

// TestEnableConflictOnSyncedSource is the SPEC-0014 "Single importer per
// source is enforced" scenario at the POST: Enabling a synced-in source is
// refused with a message naming the existing importer, and no job starts.
// Refresh carries the same guard. A non-synced source (the importer side)
// proceeds to the enabler untouched.
func TestEnableConflictOnSyncedSource(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetSyncMonitor(replicaMonitor())
	enabler := &fakeEnabler{}
	srv.SetEnabler(enabler)
	tok := mintToken(t, srv)

	for _, route := range []string{"/setup/enable", "/setup/refresh"} {
		rec := enablePOST(t, srv, route, selfOrigin, tok, "signal")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s conflict = %d, want 200 fragment", route, rec.Code)
		}
		body := rec.Body.String()
		if !contains(body, "already has an importer") || !contains(body, "kitchen-mac") {
			t.Errorf("%s conflict message does not name the importer: %s", route, body)
		}
		if !contains(body, "setup-progress-failed") {
			t.Errorf("%s conflict not rendered as a failed state", route)
		}
	}
	if enabler.enableCount()+enabler.refreshCount() != 0 {
		t.Errorf("enabler consulted despite the role conflict (enables=%d refreshes=%d)",
			enabler.enableCount(), enabler.refreshCount())
	}

	// The importer side is unaffected: a non-synced source reaches the enabler.
	tok = mintToken(t, srv)
	rec := enablePOST(t, srv, "/setup/enable", selfOrigin, tok, "imessage")
	if rec.Code != http.StatusOK || enabler.enableCount() != 1 {
		t.Errorf("non-synced Enable = %d, enabler calls = %d; want it to proceed", rec.Code, enabler.enableCount())
	}
}

// TestRefreshAllSkipsSyncedSources: the all-sources Refresh never fans out to
// a replica's synced-in source.
func TestRefreshAllSkipsSyncedSources(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetSyncMonitor(replicaMonitor())
	enabler := &fakeEnabler{}
	srv.SetEnabler(enabler)
	tok := mintToken(t, srv)

	rec := refreshAllPOST(t, srv, selfOrigin, tok)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh-all = %d", rec.Code)
	}
	for _, src := range enabler.refreshedSources() {
		if src == "signal" {
			t.Error("refresh-all ran the exporter pipeline for a synced-in source")
		}
	}
}

// unpairPOST posts the unpair form.
func unpairPOST(t *testing.T, srv *Server, origin, token string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	if token != "" {
		form.Set(setupTokenField, token)
	}
	req := httptest.NewRequest(http.MethodPost, "/settings/devices/unpair", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// pairedFixture returns a staticPairing holding one paired peer.
func pairedFixture(t *testing.T) *staticPairing {
	return &staticPairing{
		p: testPayload(t),
		peers: []devices.SyncPeer{{
			DeviceID: testPeerDeviceID,
			Name:     "kitchen-mac",
			Folders:  []string{"msgbrowse-signal"},
			PairedAt: time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
		}},
	}
}

// TestUnpairSecurityGate: cross-origin and tokenless unpair POSTs are 403
// BEFORE the pairing source is consulted — a hostile page cannot sever sync.
func TestUnpairSecurityGate(t *testing.T) {
	src := pairedFixture(t)
	srv := newPairServer(t, src)
	tok := mintToken(t, srv)

	rec := unpairPOST(t, srv, "http://evil.example", tok, url.Values{"device_id": {testPeerDeviceID}, "confirm": {"1"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin unpair = %d, want 403", rec.Code)
	}
	rec = unpairPOST(t, srv, selfOrigin, "", url.Values{"device_id": {testPeerDeviceID}, "confirm": {"1"}})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("tokenless unpair = %d, want 403", rec.Code)
	}
	if len(src.unpaired) != 0 {
		t.Error("pairing source consulted despite the 403s")
	}
}

// TestUnpairTwoStepConfirm: the first POST (no confirm) mutates NOTHING and
// redirects to the confirm state; the settings render then carries the
// inline confirmation for exactly that peer; only the confirm=1 POST unpairs.
func TestUnpairTwoStepConfirm(t *testing.T) {
	src := pairedFixture(t)
	srv := newPairServer(t, src)
	tok := mintToken(t, srv)

	// Step 1: no confirm → redirect to the confirm state, nothing unpaired.
	rec := unpairPOST(t, srv, selfOrigin, tok, url.Values{"device_id": {testPeerDeviceID}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("step-1 unpair = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	want := "/settings?unpair=confirm&device=" + url.QueryEscape(testPeerDeviceID)
	if loc != want {
		t.Fatalf("step-1 redirect = %q, want %q", loc, want)
	}
	if len(src.unpaired) != 0 {
		t.Fatal("step-1 unpair POST mutated state")
	}

	// The confirm render: the peer's row carries the confirmation affordance.
	body := get(t, srv, loc).Body.String()
	if !contains(body, "Unpair kitchen-mac?") {
		t.Error("confirm state missing the inline confirmation")
	}
	if !contains(body, `name="confirm" value="1"`) {
		t.Error("confirm state missing the confirm=1 form")
	}

	// A device id NOT in the registry renders no confirmation.
	other := get(t, srv, "/settings?unpair=confirm&device="+url.QueryEscape(testSelfDeviceID)).Body.String()
	if contains(other, `name="confirm" value="1"`) {
		t.Error("confirm affordance rendered for an unpaired device id")
	}

	// Step 2: confirm=1 → the source unpairs, PRG to ?unpair=ok.
	tok = mintToken(t, srv)
	rec = unpairPOST(t, srv, selfOrigin, tok, url.Values{"device_id": {testPeerDeviceID}, "confirm": {"1"}})
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings?unpair=ok" {
		t.Fatalf("confirmed unpair = %d → %q, want 303 → /settings?unpair=ok", rec.Code, rec.Header().Get("Location"))
	}
	if len(src.unpaired) != 1 || src.unpaired[0] != testPeerDeviceID {
		t.Errorf("unpair calls = %v, want exactly the confirmed device", src.unpaired)
	}
}

// TestUnpairErrorStates maps the sentinels onto fixed banner enums, and the
// banners render server-owned text only.
func TestUnpairErrorStates(t *testing.T) {
	t.Run("malformed device id", func(t *testing.T) {
		src := pairedFixture(t)
		srv := newPairServer(t, src)
		tok := mintToken(t, srv)
		rec := unpairPOST(t, srv, selfOrigin, tok, url.Values{"device_id": {"<script>"}, "confirm": {"1"}})
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings?unpair=invalid" {
			t.Fatalf("malformed-id unpair = %d → %q", rec.Code, rec.Header().Get("Location"))
		}
		if len(src.unpaired) != 0 {
			t.Error("malformed id reached the pairing source")
		}
	})
	t.Run("unknown peer", func(t *testing.T) {
		src := pairedFixture(t)
		src.unpairErr = devices.ErrUnknownSyncPeer
		srv := newPairServer(t, src)
		tok := mintToken(t, srv)
		rec := unpairPOST(t, srv, selfOrigin, tok, url.Values{"device_id": {testPeerDeviceID}, "confirm": {"1"}})
		if rec.Header().Get("Location") != "/settings?unpair=unknown" {
			t.Errorf("unknown-peer redirect = %q", rec.Header().Get("Location"))
		}
	})
	t.Run("engine error", func(t *testing.T) {
		src := pairedFixture(t)
		src.unpairErr = errors.New("daemon exploded")
		srv := newPairServer(t, src)
		tok := mintToken(t, srv)
		rec := unpairPOST(t, srv, selfOrigin, tok, url.Values{"device_id": {testPeerDeviceID}, "confirm": {"1"}})
		if rec.Header().Get("Location") != "/settings?unpair=error" {
			t.Errorf("engine-error redirect = %q", rec.Header().Get("Location"))
		}
	})
	t.Run("no pairing source", func(t *testing.T) {
		srv := newPairServer(t, nil)
		tok := mintToken(t, srv)
		rec := unpairPOST(t, srv, selfOrigin, tok, url.Values{"device_id": {testPeerDeviceID}, "confirm": {"1"}})
		if rec.Header().Get("Location") != "/settings?unpair=unavailable" {
			t.Errorf("sourceless redirect = %q", rec.Header().Get("Location"))
		}
	})
	t.Run("banners", func(t *testing.T) {
		srv := newPairServer(t, pairedFixture(t))
		cases := map[string]string{
			"ok":      "Device unpaired.",
			"invalid": "That device ID was not recognized.",
			"unknown": "That device is not paired.",
			"error":   "Unpairing did not fully complete.",
		}
		for state, wantText := range cases {
			body := get(t, srv, "/settings?unpair="+state).Body.String()
			if !contains(body, wantText) {
				t.Errorf("?unpair=%s missing banner %q", state, wantText)
			}
		}
	})
}

// TestSettingsPeerLiveState: the peer rows carry the engine-reported
// connection state, and an engine-down snapshot renders "State unknown" —
// never a fake Disconnected.
func TestSettingsPeerLiveState(t *testing.T) {
	src := pairedFixture(t)
	srv := newPairServer(t, src)

	srv.SetSyncMonitor(&fakeMonitor{status: &devsync.Status{
		Running: true,
		Peers: []devsync.PeerStatus{{
			SyncPeer:   src.peers[0],
			StateKnown: true,
			Connected:  true,
		}},
	}})
	body := get(t, srv, "/settings").Body.String()
	if !contains(body, ">Connected</span>") {
		t.Error("connected peer badge missing")
	}
	if !contains(body, "Unpair") {
		t.Error("peer row missing its Unpair control")
	}

	// Engine down: peers keep StateKnown=false → "State unknown".
	srv.SetSyncMonitor(&fakeMonitor{status: &devsync.Status{
		Running: false,
		Peers:   []devsync.PeerStatus{{SyncPeer: src.peers[0]}},
	}})
	body = get(t, srv, "/settings").Body.String()
	if !contains(body, ">State unknown</span>") {
		t.Error("engine-down peer state not rendered as unknown")
	}
	if !contains(body, ">Engine not running</span>") {
		t.Error("engine-down note missing from the pairing section")
	}
}

// TestStatusPageDeviceSyncCard: the /status Device-sync card renders the
// engine state, peers, and folder health truthfully from the snapshot —
// paused and errored folders show as such, with completion percentages.
func TestStatusPageDeviceSyncCard(t *testing.T) {
	st, cfg, _ := newTestStoreAndConfig(t)
	cfg.DeviceSync.Enabled = true
	cfg.DeviceSync.ListenAddr = "127.0.0.1:0"
	srv, err := NewServer(st, cfg, testDiscardLogger())
	if err != nil {
		t.Fatal(err)
	}
	srv.SetSyncMonitor(&fakeMonitor{status: &devsync.Status{
		Running: true,
		Peers: []devsync.PeerStatus{
			{SyncPeer: devices.SyncPeer{DeviceID: testPeerDeviceID, Name: "kitchen-mac"}, StateKnown: true, Connected: true},
		},
		Folders: []devsync.FolderStatus{
			{ID: "msgbrowse-signal", Source: "signal", Label: "Signal", Health: devsync.HealthHealthy, Completion: 100},
			{ID: "msgbrowse-imessage", Source: "imessage", Label: "iMessage", Health: devsync.HealthPaused, Completion: 40},
			{ID: "msgbrowse-whatsapp", Source: "whatsapp", Label: "WhatsApp", Health: devsync.HealthError, Completion: 80, Errors: 3},
		},
	}})

	body := get(t, srv, "/status").Body.String()
	if !contains(body, "Device sync") || !contains(body, ">Running</span>") {
		t.Error("device-sync card / engine state missing")
	}
	if !contains(body, "kitchen-mac") || !contains(body, ">Connected</span>") {
		t.Error("peer row missing from the status card")
	}
	if !contains(body, ">Healthy</span>") || !contains(body, "100%") {
		t.Error("healthy folder not rendered truthfully")
	}
	if !contains(body, ">Paused</span>") || !contains(body, "40%") {
		t.Error("paused folder not rendered truthfully")
	}
	if !contains(body, "Error (3 items)") || !contains(body, "80%") {
		t.Error("errored folder not rendered truthfully")
	}
}

// TestStatusPageDeviceSyncAbsentStates: disabled renders the neutral line;
// enabled-with-no-snapshot renders the unavailable note.
func TestStatusPageDeviceSyncAbsentStates(t *testing.T) {
	srv, _, _ := newTestServer(t) // sync disabled
	body := get(t, srv, "/status").Body.String()
	if !contains(body, "Device sync is off") {
		t.Error("disabled state missing its neutral line")
	}

	st, cfg, _ := newTestStoreAndConfig(t)
	cfg.DeviceSync.Enabled = true
	cfg.DeviceSync.ListenAddr = "127.0.0.1:0"
	srv2, err := NewServer(st, cfg, testDiscardLogger())
	if err != nil {
		t.Fatal(err)
	}
	body = get(t, srv2, "/status").Body.String()
	if !contains(body, "its state is unavailable") {
		t.Error("enabled-but-unreadable state missing")
	}
}

// TestLogsDeviceSyncFeed: the Logs page renders the sync event feed beside
// the shell notes, and error notes carry the failed badge.
func TestLogsDeviceSyncFeed(t *testing.T) {
	srv, _, _ := newTestServer(t)
	notes := devsync.NewNotes(8)
	notes.Add(devsync.NoteInfo, "Paired kitchen-mac (XW4UY46) — sharing 1 folder(s)")
	notes.Add(devsync.NoteError, "event long-poll failed")
	srv.SetSyncNotes(notes.Snapshot)

	body := get(t, srv, "/logs").Body.String()
	if !contains(body, "Device sync") || !contains(body, "Paired kitchen-mac (XW4UY46)") {
		t.Error("sync feed missing from the Logs page")
	}
	if !contains(body, "event long-poll failed") {
		t.Error("error note missing")
	}

	// No provider (sync disabled): the section is absent entirely.
	srv2, _, _ := newTestServer(t)
	body = get(t, srv2, "/logs").Body.String()
	if contains(body, `id="log-title-devsync"`) {
		t.Error("sync feed rendered with no provider wired")
	}
}

// between extracts the substring from the first occurrence of start through
// the following end marker, for scoping assertions to one card.
func between(t *testing.T, s, start, end string) string {
	t.Helper()
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatalf("marker %q not found", start)
	}
	rest := s[i:]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("end marker %q not found after %q", end, start)
	}
	return rest[:j]
}
