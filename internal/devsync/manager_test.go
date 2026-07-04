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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/devices"
	"github.com/joestump/msgbrowse/internal/store"
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

// folderConfig returns the daemon's config entry for folderID, ok=false when
// the folder is not configured.
func (d *stubDaemon) folderConfig(folderID string) (syncthing.FolderConfig, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, f := range d.folders {
		if f.ID == folderID {
			return f, true
		}
	}
	return syncthing.FolderConfig{}, false
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

func (m *memPeerStore) DeleteSyncPeer(_ context.Context, deviceID string) error {
	id, err := devices.CanonicalDeviceID(deviceID)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.peers[id]; !ok {
		return devices.ErrUnknownSyncPeer
	}
	delete(m.peers, id)
	return nil
}

func (m *memPeerStore) TouchSyncPeerSeen(_ context.Context, deviceID string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.peers[deviceID]; ok {
		p.LastSeenAt = at
		m.peers[deviceID] = p
	}
	return nil
}

func (m *memPeerStore) RecordSyncImport(_ context.Context, folderID, source string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.imports = append(m.imports, folderID+"/"+source)
	return nil
}

func (m *memPeerStore) SyncImportStates(context.Context) ([]store.SyncImportState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.SyncImportState
	seen := make(map[string]bool)
	for _, rec := range m.imports {
		folderID, src, _ := strings.Cut(rec, "/")
		if seen[folderID] {
			continue
		}
		seen[folderID] = true
		out = append(out, store.SyncImportState{FolderID: folderID, Source: src, LastImportAt: time.Now()})
	}
	return out, nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testFolderSet is the standard two-source folder fixture: a real managed
// layout (signal + imessage roots) provisioned under a temp data dir, wrapped
// in the live FolderSet the Manager and Watcher share.
func testFolderSet(t *testing.T) *FolderSet {
	t.Helper()
	dataDir := t.TempDir()
	var folders []syncthing.Folder
	for _, src := range []string{"signal", "imessage"} {
		f, err := syncthing.ProvisionManagedFolder(dataDir, src)
		if err != nil {
			t.Fatalf("provision %s fixture root: %v", src, err)
		}
		folders = append(folders, f)
	}
	fs, err := NewFolderSet(dataDir, folders)
	if err != nil {
		t.Fatalf("NewFolderSet: %v", err)
	}
	return fs
}

// emptyFolderSet is a FRESH-REPLICA fixture: a data dir with NO managed roots
// at all (the state MAJOR review finding 1 exercises).
func emptyFolderSet(t *testing.T) *FolderSet {
	t.Helper()
	fs, err := NewFolderSet(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewFolderSet: %v", err)
	}
	return fs
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
	m := NewManager(daemon.client(), st, "studio-mac", testFolderSet(t), testLogger())

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
	m := NewManager(daemon.client(), st, "studio-mac", testFolderSet(t), testLogger())

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
	m := NewManager(daemon.client(), st, "studio-mac", testFolderSet(t), testLogger())

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
	m := NewManager(daemon.client(), st, "studio-mac", testFolderSet(t), testLogger())

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
	m := NewManager(daemon.client(), st, "studio-mac", testFolderSet(t), testLogger())

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
	m := NewManager(daemon.client(), newMemPeerStore(), "studio-mac", testFolderSet(t), testLogger())

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
	m2 := NewManager(daemon.client(), newMemPeerStore(), "x", emptyFolderSet(t), testLogger())
	if _, ok := m2.ActivePairing(context.Background()); ok {
		t.Error("ActivePairing ok against a dead engine")
	}
}

// TestPairFreshReplicaProvisionsIntroducedFolders is the MAJOR review-finding
// scenario (issue #157 adversarial review, finding 1): a FRESH REPLICA — no
// pre-existing managed roots, an empty daemon folder config — pairs with an
// importer whose payload introduces a folder. The replica must PROVISION the
// corresponding managed root (<data_dir>/archives/<source>), add the folder
// to the live daemon config, share it with the importer, and record the share
// in the registry — while an introduced id outside the fixed source enum is
// ignored and never becomes a path (SPEC-0014 REQ "Importer and Replica
// Roles", "msgbrowse-Owned Config Generation").
func TestPairFreshReplicaProvisionsIntroducedFolders(t *testing.T) {
	dataDir := t.TempDir()
	fs, err := NewFolderSet(dataDir, nil) // no local roots at all
	if err != nil {
		t.Fatalf("NewFolderSet: %v", err)
	}
	daemon := newStubDaemon(t, nil) // and none configured in the daemon
	st := newMemPeerStore()
	m := NewManager(daemon.client(), st, "replica", fs, testLogger())

	payload, err := devices.NewSyncPayload(peerAID, []string{"msgbrowse-signal", "not-a-real-folder"}, "importer")
	if err != nil {
		t.Fatalf("payload: %v", err)
	}
	code, err := payload.EncodeManualCode()
	if err != nil {
		t.Fatalf("manual code: %v", err)
	}
	peer, err := m.Pair(context.Background(), code)
	if err != nil {
		t.Fatalf("Pair on a fresh replica: %v", err)
	}

	// The managed root was provisioned — Syncthing-ready, under archives/.
	root := filepath.Join(dataDir, "archives", "signal")
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		t.Fatalf("managed root not provisioned at %s: %v", root, err)
	}
	for _, marker := range []string{".stignore", ".stfolder"} {
		if _, err := os.Stat(filepath.Join(root, marker)); err != nil {
			t.Errorf("provisioned root missing %s: %v", marker, err)
		}
	}
	// The archive-not-DB guard held: the provisioned path validates, and the
	// data_dir itself is still refused.
	if err := syncthing.ValidateManagedFolderPath(dataDir, root); err != nil {
		t.Errorf("provisioned root failed the archive-not-DB validation: %v", err)
	}
	if err := syncthing.ValidateManagedFolderPath(dataDir, dataDir); err == nil {
		t.Error("data_dir passed the archive-not-DB validation")
	}

	// The live daemon config gained the folder, shared with the importer.
	fc, ok := daemon.folderConfig("msgbrowse-signal")
	if !ok {
		t.Fatal("provisioned folder was not added to the daemon config")
	}
	if fc.Path != root || fc.Type != syncthing.FolderTypeSendReceive {
		t.Errorf("daemon folder config = %+v, want path %s type %s", fc, root, syncthing.FolderTypeSendReceive)
	}
	if got := daemon.folderDeviceIDs("msgbrowse-signal"); len(got) != 1 || got[0] != peerAID {
		t.Errorf("signal folder devices = %v, want [%s]", got, peerAID)
	}

	// The out-of-enum id was ignored: not shared, not provisioned, no path.
	if peer.Folders == nil || len(peer.Folders) != 1 || peer.Folders[0] != "msgbrowse-signal" {
		t.Errorf("peer folders = %v, want [msgbrowse-signal]", peer.Folders)
	}
	if _, ok := daemon.folderConfig("not-a-real-folder"); ok {
		t.Error("out-of-enum folder id reached the daemon config")
	}

	// The registry recorded the true share set, and the shared folder set —
	// which the Watcher reads — now manages the provisioned folder.
	stored, err := st.GetSyncPeerByDeviceID(context.Background(), peerAID)
	if err != nil {
		t.Fatalf("peer not persisted: %v", err)
	}
	if len(stored.Folders) != 1 || stored.Folders[0] != "msgbrowse-signal" {
		t.Errorf("registry folders = %v, want [msgbrowse-signal]", stored.Folders)
	}
	if !fs.Contains("msgbrowse-signal") {
		t.Error("provisioned folder not visible in the shared FolderSet")
	}
	if fs.Contains("not-a-real-folder") {
		t.Error("out-of-enum folder id entered the FolderSet")
	}
}
