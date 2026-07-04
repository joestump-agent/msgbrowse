// Pairing-flow tests against a STUBBED Syncthing REST API (issue #157): a
// real *syncthing.Client speaks HTTP to an httptest server imitating the
// daemon's config endpoints, so the whole pair path — decode → validate →
// persist → add device → share folders — runs exactly as in production, on
// Linux, with no daemon binary.
//
// Governing: SPEC-0014 REQ "Pairing via Device ID and QR" ("msgbrowse MUST
// add the scanned peer as a Syncthing device and share the relevant folders
// with it via the REST API"), §Trust Model ("a device ID alone does not
// grant sync"), REQ "Error Handling Standards".
package devsync

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/joestump/msgbrowse/internal/devices"
	"github.com/joestump/msgbrowse/internal/syncthing"
)

// Valid Syncthing-format device IDs (Luhn check digits intact).
const (
	selfID  = "QRUVHQ4-LQMFCKZ-JPKWU3L-TJNB6NX-XZXB2AV-FLJ5RL4-DC2QFCT-EBHK5AG"
	peerAID = "XW4UY46-VHRCAEN-OTRLIUX-BIIMJVP-KPVFKQW-4H5TU2H-MYSYKFX-S53S7AL"
	peerBID = "AL4V3SV-WOXMPPL-7OSHTP5-YBPGQTN-6CBXKHB-D5DWSIJ-563UQMW-5JXZFAO"
)

const stubAPIKey = "test-api-key"

// stubDaemon is an httptest-backed imitation of the daemon's REST surface:
// system status plus the /rest/config devices/folders sections, guarded by
// the X-API-Key header exactly like the real daemon.
type stubDaemon struct {
	mu      sync.Mutex
	devices []syncthing.DeviceConfig
	folders []syncthing.FolderConfig
	srv     *httptest.Server
}

func newStubDaemon(t *testing.T, folders []syncthing.FolderConfig) *stubDaemon {
	t.Helper()
	d := &stubDaemon{folders: folders}
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-API-Key") != stubAPIKey {
				http.Error(w, "Not Authorized", http.StatusForbidden)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("GET /rest/system/status", auth(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"myID": selfID, "uptime": 1})
	}))
	mux.HandleFunc("GET /rest/config/devices", auth(func(w http.ResponseWriter, _ *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		_ = json.NewEncoder(w).Encode(d.devices)
	}))
	mux.HandleFunc("PUT /rest/config/devices", auth(func(w http.ResponseWriter, r *http.Request) {
		var devs []syncthing.DeviceConfig
		if err := json.NewDecoder(r.Body).Decode(&devs); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d.mu.Lock()
		d.devices = devs
		d.mu.Unlock()
	}))
	mux.HandleFunc("GET /rest/config/folders", auth(func(w http.ResponseWriter, _ *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		_ = json.NewEncoder(w).Encode(d.folders)
	}))
	mux.HandleFunc("PUT /rest/config/folders", auth(func(w http.ResponseWriter, r *http.Request) {
		var folders []syncthing.FolderConfig
		if err := json.NewDecoder(r.Body).Decode(&folders); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d.mu.Lock()
		d.folders = folders
		d.mu.Unlock()
	}))
	d.srv = httptest.NewServer(mux)
	t.Cleanup(d.srv.Close)
	return d
}

// client returns a real REST client pointed at the stub.
func (d *stubDaemon) client() *syncthing.Client {
	return syncthing.NewClient(strings.TrimPrefix(d.srv.URL, "http://"), stubAPIKey)
}

func (d *stubDaemon) deviceIDs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, 0, len(d.devices))
	for _, dev := range d.devices {
		out = append(out, dev.DeviceID)
	}
	return out
}

func (d *stubDaemon) folderDeviceIDs(folderID string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, f := range d.folders {
		if f.ID == folderID {
			out := make([]string, 0, len(f.Devices))
			for _, ref := range f.Devices {
				out = append(out, ref.DeviceID)
			}
			return out
		}
	}
	return nil
}

// memPeerStore is an in-memory PeerStore.
type memPeerStore struct {
	mu      sync.Mutex
	peers   map[string]devices.SyncPeer
	imports []string // "folderID/source" records
	nextID  int64
}

func newMemPeerStore() *memPeerStore {
	return &memPeerStore{peers: make(map[string]devices.SyncPeer)}
}

func (m *memPeerStore) UpsertSyncPeer(_ context.Context, p devices.SyncPeer) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.peers[p.DeviceID]; ok {
		p.ID = existing.ID
	} else {
		m.nextID++
		p.ID = m.nextID
	}
	m.peers[p.DeviceID] = p
	return p.ID, nil
}

func (m *memPeerStore) ListSyncPeers(context.Context) ([]devices.SyncPeer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]devices.SyncPeer, 0, len(m.peers))
	for _, p := range m.peers {
		out = append(out, p)
	}
	return out, nil
}

func (m *memPeerStore) GetSyncPeerByDeviceID(_ context.Context, deviceID string) (*devices.SyncPeer, error) {
	id, err := devices.CanonicalDeviceID(deviceID)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.peers[id]; ok {
		return &p, nil
	}
	return nil, devices.ErrUnknownSyncPeer
}

func (m *memPeerStore) RecordSyncImport(_ context.Context, folderID, source string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.imports = append(m.imports, folderID+"/"+source)
	return nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// managedFolders is the standard two-source folder fixture.
func managedFolders() []syncthing.Folder {
	return []syncthing.Folder{
		{ID: "msgbrowse-signal", Label: "Signal", Path: "/tmp/x/archives/signal"},
		{ID: "msgbrowse-imessage", Label: "iMessage", Path: "/tmp/x/archives/imessage"},
	}
}

func stubFolderConfigs() []syncthing.FolderConfig {
	return []syncthing.FolderConfig{
		{ID: "msgbrowse-signal", Path: "/tmp/x/archives/signal", Type: "sendreceive"},
		{ID: "msgbrowse-imessage", Path: "/tmp/x/archives/imessage", Type: "sendreceive"},
	}
}

// TestPairAddsDeviceAndSharesFolders is the core SPEC-0014 pairing scenario:
// pasting another node's payload persists the peer, adds its device to the
// daemon, and shares exactly the introduced managed folders with it.
func TestPairAddsDeviceAndSharesFolders(t *testing.T) {
	daemon := newStubDaemon(t, stubFolderConfigs())
	st := newMemPeerStore()
	m := NewManager(daemon.client(), st, "studio-mac", managedFolders(), testLogger())

	payload, err := devices.NewSyncPayload(peerAID, []string{"msgbrowse-signal"}, "kitchen-mac")
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	code, err := payload.EncodeManualCode()
	if err != nil {
		t.Fatalf("manual code: %v", err)
	}

	peer, err := m.Pair(context.Background(), code)
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if peer.DeviceID != peerAID || peer.Name != "kitchen-mac" {
		t.Errorf("peer = %+v", peer)
	}

	// Persisted (the explicit-trust registry the watcher consults).
	if _, err := st.GetSyncPeerByDeviceID(context.Background(), peerAID); err != nil {
		t.Errorf("peer not persisted: %v", err)
	}
	// Device added to the daemon.
	ids := daemon.deviceIDs()
	if len(ids) != 1 || ids[0] != peerAID {
		t.Errorf("daemon devices = %v, want [%s]", ids, peerAID)
	}
	// Only the INTRODUCED folder is shared; the other managed folder is not.
	if got := daemon.folderDeviceIDs("msgbrowse-signal"); len(got) != 1 || got[0] != peerAID {
		t.Errorf("signal folder devices = %v, want [%s]", got, peerAID)
	}
	if got := daemon.folderDeviceIDs("msgbrowse-imessage"); len(got) != 0 {
		t.Errorf("imessage folder unexpectedly shared: %v", got)
	}
}

// TestPairBareDeviceIDSharesAllManaged: a bare device ID (no folder
// introduction) is the manual-entry path — every locally managed folder is
// shared.
func TestPairBareDeviceIDSharesAllManaged(t *testing.T) {
	daemon := newStubDaemon(t, stubFolderConfigs())
	st := newMemPeerStore()
	m := NewManager(daemon.client(), st, "studio-mac", managedFolders(), testLogger())

	if _, err := m.Pair(context.Background(), strings.ToLower(peerBID)); err != nil {
		t.Fatalf("Pair(bare id): %v", err)
	}
	for _, folder := range []string{"msgbrowse-signal", "msgbrowse-imessage"} {
		if got := daemon.folderDeviceIDs(folder); len(got) != 1 || got[0] != peerBID {
			t.Errorf("%s devices = %v, want [%s]", folder, got, peerBID)
		}
	}
}

// TestPairIdempotent: re-pairing the same device duplicates nothing — the
// device list and folder shares stay singular.
func TestPairIdempotent(t *testing.T) {
	daemon := newStubDaemon(t, stubFolderConfigs())
	st := newMemPeerStore()
	m := NewManager(daemon.client(), st, "studio-mac", managedFolders(), testLogger())

	for i := 0; i < 2; i++ {
		if _, err := m.Pair(context.Background(), peerAID); err != nil {
			t.Fatalf("Pair #%d: %v", i+1, err)
		}
	}
	if ids := daemon.deviceIDs(); len(ids) != 1 {
		t.Errorf("daemon devices duplicated: %v", ids)
	}
	if got := daemon.folderDeviceIDs("msgbrowse-signal"); len(got) != 1 {
		t.Errorf("folder shares duplicated: %v", got)
	}
	peers, _ := st.ListSyncPeers(context.Background())
	if len(peers) != 1 {
		t.Errorf("registry duplicated: %+v", peers)
	}
}

// TestPairRejectsSelf: scanning one's own QR is the typed ErrSelfPair, and
// NOTHING is persisted or configured.
func TestPairRejectsSelf(t *testing.T) {
	daemon := newStubDaemon(t, stubFolderConfigs())
	st := newMemPeerStore()
	m := NewManager(daemon.client(), st, "studio-mac", managedFolders(), testLogger())

	_, err := m.Pair(context.Background(), selfID)
	if !errors.Is(err, devices.ErrSelfPair) {
		t.Fatalf("Pair(self) = %v, want ErrSelfPair", err)
	}
	if peers, _ := st.ListSyncPeers(context.Background()); len(peers) != 0 {
		t.Error("self-pair persisted a peer")
	}
	if ids := daemon.deviceIDs(); len(ids) != 0 {
		t.Errorf("self-pair touched the daemon config: %v", ids)
	}
}

// TestPairRejectsInvalidCode: garbage input is the typed payload rejection,
// before any daemon or registry touch.
func TestPairRejectsInvalidCode(t *testing.T) {
	daemon := newStubDaemon(t, stubFolderConfigs())
	st := newMemPeerStore()
	m := NewManager(daemon.client(), st, "studio-mac", managedFolders(), testLogger())

	for _, code := range []string{"", "garbage", "MSGB2.!!!!", `{"v":1,"endpoint":"x:1","token":"t","fp":"ff"}`} {
		if _, err := m.Pair(context.Background(), code); !errors.Is(err, devices.ErrInvalidSyncPayload) {
			t.Errorf("Pair(%q) = %v, want ErrInvalidSyncPayload", code, err)
		}
	}
	if ids := daemon.deviceIDs(); len(ids) != 0 {
		t.Errorf("invalid codes touched the daemon config: %v", ids)
	}
}

// TestActivePairingPayload: the payload carries this node's device ID (read
// once from the daemon and cached), the managed folder ids, and the friendly
// name — public introduction data only.
func TestActivePairingPayload(t *testing.T) {
	daemon := newStubDaemon(t, stubFolderConfigs())
	m := NewManager(daemon.client(), newMemPeerStore(), "studio-mac", managedFolders(), testLogger())

	p, ok := m.ActivePairing(context.Background())
	if !ok {
		t.Fatal("ActivePairing not ok against a live stub")
	}
	if p.DeviceID != selfID || p.Name != "studio-mac" {
		t.Errorf("payload = %+v", p)
	}
	if len(p.Folders) != 2 || p.Folders[0] != "msgbrowse-signal" {
		t.Errorf("payload folders = %v", p.Folders)
	}

	// Engine down: a fresh manager against a dead endpoint reports not-ok
	// rather than erroring the page.
	daemon.srv.Close()
	m2 := NewManager(daemon.client(), newMemPeerStore(), "x", nil, testLogger())
	if _, ok := m2.ActivePairing(context.Background()); ok {
		t.Error("ActivePairing ok against a dead engine")
	}
}
