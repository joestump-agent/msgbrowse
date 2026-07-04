// CLI tests for the rebuilt `msgbrowse devices` namespace (ADR-0021): the
// read-only `devices list` over the paired_devices registry — empty state,
// seeded rows through a REAL store, and the error path. The pairing flow
// itself lives behind /settings (SPEC-0014 REQ "Pairing via Device ID and
// QR") and is covered by internal/devsync and internal/web; what the CLI owns
// is rendering the registry truthfully, including folder shares widened by
// accepted offers (issue #157 review finding 2).
package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/devices"
	"github.com/joestump/msgbrowse/internal/devsync"
	"github.com/joestump/msgbrowse/internal/store"
	"github.com/joestump/msgbrowse/internal/syncthing"
)

// A valid Syncthing-format device ID (Luhn check digits intact) — the store
// canonicalizes device IDs on write, so fixtures must be real ones.
const testSyncDeviceID = "XW4UY46-VHRCAEN-OTRLIUX-BIIMJVP-KPVFKQW-4H5TU2H-MYSYKFX-S53S7AL"

// openTestStore opens a real store in a temp dir; the CLI path under test is
// exactly what `devices list` reads after openStore.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "msgbrowse.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestDevicesListEmpty: with no paired peers the command prints the guidance
// line (pairing lives in the web UI) and no table.
func TestDevicesListEmpty(t *testing.T) {
	st := openTestStore(t)
	out := &bytes.Buffer{}
	if err := runDevicesList(context.Background(), st, out); err != nil {
		t.Fatalf("runDevicesList: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "No devices paired") {
		t.Errorf("empty-state output = %q, want the no-devices guidance", got)
	}
	if strings.Contains(out.String(), "DEVICE ID") {
		t.Error("empty state rendered the table header")
	}
}

// TestDevicesListRendersPeers: seeded peers come out as tabwriter rows —
// header plus name, the full device ID (whose prefix is the ShortID shown in
// /settings), and the comma-joined folder share set from the registry.
func TestDevicesListRendersPeers(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.UpsertSyncPeer(context.Background(), devices.SyncPeer{
		DeviceID: testSyncDeviceID,
		Name:     "kitchen-mac",
		Folders:  []string{"msgbrowse-signal", "msgbrowse-imessage"},
		PairedAt: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed peer: %v", err)
	}

	out := &bytes.Buffer{}
	if err := runDevicesList(context.Background(), st, out); err != nil {
		t.Fatalf("runDevicesList: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"NAME", "DEVICE ID", "FOLDERS", "PAIRED", // tabwriter header
		"kitchen-mac",
		testSyncDeviceID,
		devices.ShortDeviceID(testSyncDeviceID), // the ID prefix, part of the full ID
		"msgbrowse-signal,msgbrowse-imessage",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// errPeerLister scripts the registry read failing (e.g. a corrupt table).
type errPeerLister struct{ err error }

func (e errPeerLister) ListSyncPeers(context.Context) ([]devices.SyncPeer, error) {
	return nil, e.err
}

// TestDevicesListErrorPath: a registry read failure is returned to the caller
// (cobra prints it and exits non-zero), never swallowed into an empty state.
func TestDevicesListErrorPath(t *testing.T) {
	boom := errors.New("paired_devices unreadable")
	out := &bytes.Buffer{}
	err := runDevicesList(context.Background(), errPeerLister{err: boom}, out)
	if !errors.Is(err, boom) {
		t.Fatalf("runDevicesList error = %v, want %v", err, boom)
	}
	if out.Len() != 0 {
		t.Errorf("error path wrote output: %q", out.String())
	}
}

const testSyncDeviceIDB = "AL4V3SV-WOXMPPL-7OSHTP5-YBPGQTN-6CBXKHB-D5DWSIJ-563UQMW-5JXZFAO"

// stubEngine runs an httptest imitation of the daemon's REST surface for the
// unpair/status verbs, seeded with one shared folder and both fixture peers.
type stubEngine struct {
	mu      sync.Mutex
	devices []syncthing.DeviceConfig
	folders []syncthing.FolderConfig
	srv     *httptest.Server
}

func newStubEngine(t *testing.T) *stubEngine {
	t.Helper()
	e := &stubEngine{
		devices: []syncthing.DeviceConfig{{DeviceID: testSyncDeviceID, Name: "kitchen-mac"}},
		folders: []syncthing.FolderConfig{{
			ID: "msgbrowse-signal", Path: "/tmp/x/archives/signal", Type: "sendreceive",
			Devices: []syncthing.FolderDeviceRef{{DeviceID: testSyncDeviceID}},
		}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /rest/system/ping", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ping":"pong"}`))
	})
	mux.HandleFunc("GET /rest/system/status", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"myID":"` + testSyncDeviceIDB + `","uptime":7200}`))
	})
	mux.HandleFunc("GET /rest/system/connections", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"connections":{"` + testSyncDeviceID + `":{"connected":true,"address":"192.168.1.20:22000"}}}`))
	})
	mux.HandleFunc("GET /rest/db/status", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"state":"idle","errors":0,"pullErrors":0}`))
	})
	mux.HandleFunc("GET /rest/db/completion", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"completion":100}`))
	})
	mux.HandleFunc("GET /rest/config/devices", func(w http.ResponseWriter, _ *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		writeJSON(w, e.devices)
	})
	mux.HandleFunc("PUT /rest/config/devices", func(w http.ResponseWriter, r *http.Request) {
		var devs []syncthing.DeviceConfig
		if err := json.NewDecoder(r.Body).Decode(&devs); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		e.mu.Lock()
		e.devices = devs
		e.mu.Unlock()
	})
	mux.HandleFunc("GET /rest/config/folders", func(w http.ResponseWriter, _ *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		writeJSON(w, e.folders)
	})
	mux.HandleFunc("PUT /rest/config/folders", func(w http.ResponseWriter, r *http.Request) {
		var folders []syncthing.FolderConfig
		if err := json.NewDecoder(r.Body).Decode(&folders); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		e.mu.Lock()
		e.folders = folders
		e.mu.Unlock()
	})
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// dialer returns an engineDialer resolving to the stub.
func (e *stubEngine) dialer() engineDialer {
	return func(context.Context) (devsync.API, error) {
		return syncthing.NewClient(strings.TrimPrefix(e.srv.URL, "http://"), ""), nil
	}
}

// downDialer scripts "engine not running".
func downDialer(context.Context) (devsync.API, error) {
	return nil, syncthing.ErrNotRunning
}

func seedPeer(t *testing.T, st *store.Store, id, name string, roles map[string]string) {
	t.Helper()
	if _, err := st.UpsertSyncPeer(context.Background(), devices.SyncPeer{
		DeviceID: id, Name: name, Folders: []string{"msgbrowse-signal"}, Roles: roles,
		PairedAt: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed peer: %v", err)
	}
}

// TestDevicesUnpairByPrefixRemovesRowAndDaemonEntry: the short-ID prefix
// resolves the peer; the registry row is deleted; the RUNNING engine loses
// the device entry and its folder share (SPEC-0014 "Unpair stops sync to
// that device immediately").
func TestDevicesUnpairByPrefixRemovesRowAndDaemonEntry(t *testing.T) {
	st := openTestStore(t)
	seedPeer(t, st, testSyncDeviceID, "kitchen-mac", nil)
	engine := newStubEngine(t)

	out := &bytes.Buffer{}
	if err := runDevicesUnpair(context.Background(), st, engine.dialer(), "XW4UY46", out); err != nil {
		t.Fatalf("runDevicesUnpair: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Unpaired kitchen-mac") || !strings.Contains(got, "sync to it stopped") {
		t.Errorf("output = %q", got)
	}
	// Registry row gone.
	if peers, _ := st.ListSyncPeers(context.Background()); len(peers) != 0 {
		t.Errorf("registry still holds %d peers after unpair", len(peers))
	}
	// Daemon device + share gone.
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if len(engine.devices) != 0 {
		t.Errorf("daemon devices after unpair = %+v", engine.devices)
	}
	if len(engine.folders) != 1 || len(engine.folders[0].Devices) != 0 {
		t.Errorf("daemon folder shares after unpair = %+v", engine.folders)
	}
}

// TestDevicesUnpairEngineDown: with no running engine the registry removal
// still succeeds (the durable revocation) and the output says config
// regenerates on the next start.
func TestDevicesUnpairEngineDown(t *testing.T) {
	st := openTestStore(t)
	seedPeer(t, st, testSyncDeviceID, "kitchen-mac", nil)

	out := &bytes.Buffer{}
	if err := runDevicesUnpair(context.Background(), st, downDialer, testSyncDeviceID, out); err != nil {
		t.Fatalf("runDevicesUnpair: %v", err)
	}
	if !strings.Contains(out.String(), "Sync engine not running") {
		t.Errorf("output = %q", out.String())
	}
	if peers, _ := st.ListSyncPeers(context.Background()); len(peers) != 0 {
		t.Error("registry row survived engine-down unpair")
	}
}

// TestDevicesUnpairResolution: unknown ids and ambiguous prefixes are errors
// that name the problem — never a guess (unpair is destructive to sync).
func TestDevicesUnpairResolution(t *testing.T) {
	st := openTestStore(t)
	seedPeer(t, st, testSyncDeviceID, "kitchen-mac", nil)
	seedPeer(t, st, testSyncDeviceIDB, "attic-mac", nil)

	out := &bytes.Buffer{}
	if err := runDevicesUnpair(context.Background(), st, downDialer, "ZZZZZZZ", out); err == nil ||
		!errors.Is(err, devices.ErrUnknownSyncPeer) {
		t.Errorf("unknown prefix error = %v, want ErrUnknownSyncPeer", err)
	}
	if err := runDevicesUnpair(context.Background(), st, downDialer, "not a device", out); err == nil {
		t.Error("garbage argument accepted")
	}
	// Nothing was deleted by the failed resolutions.
	if peers, _ := st.ListSyncPeers(context.Background()); len(peers) != 2 {
		t.Errorf("failed resolutions mutated the registry: %d peers", len(peers))
	}

	// Ambiguity is a naming error, never a guess: resolvePeerArg is pure, so
	// assert it directly over two peers sharing a prefix.
	shared := []devices.SyncPeer{
		{DeviceID: "XW4UY46-AAAAAAA", Name: "one"},
		{DeviceID: "XW4UY46-BBBBBBB", Name: "two"},
	}
	if _, err := resolvePeerArg(shared, "XW4UY46"); err == nil ||
		!strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("ambiguous prefix error = %v, want an ambiguity error naming candidates", err)
	}
	if p, err := resolvePeerArg(shared, "XW4UY46-A"); err != nil || p.Name != "one" {
		t.Errorf("unique prefix resolution = (%v, %v), want peer one", p, err)
	}
}

// TestDevicesStatusTables: the status verb renders the engine line, the peer
// table with live connection state, and the folder table with completion —
// and the roles column in `devices list` names imported sources.
func TestDevicesStatusTables(t *testing.T) {
	st := openTestStore(t)
	seedPeer(t, st, testSyncDeviceID, "kitchen-mac", map[string]string{"signal": devices.RoleImporter})
	engine := newStubEngine(t)

	out := &bytes.Buffer{}
	if err := runDevicesStatus(context.Background(), st, engine.dialer(), out); err != nil {
		t.Fatalf("runDevicesStatus: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"Engine: running",
		"DEVICE", "STATE", // peer table header
		"kitchen-mac", "connected",
		"FOLDER", "COMPLETE", // folder table header
		"msgbrowse-signal", "idle", "100%",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("status output missing %q:\n%s", want, got)
		}
	}

	// The list verb surfaces the recorded role.
	out.Reset()
	if err := runDevicesList(context.Background(), st, out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "IMPORTS") || !strings.Contains(out.String(), "signal") {
		t.Errorf("list output missing the imports column:\n%s", out.String())
	}
}

// TestDevicesStatusEngineDown: the engine-down state renders truthfully with
// the registry rows intact and peer state unknown.
func TestDevicesStatusEngineDown(t *testing.T) {
	st := openTestStore(t)
	seedPeer(t, st, testSyncDeviceID, "kitchen-mac", nil)

	out := &bytes.Buffer{}
	if err := runDevicesStatus(context.Background(), st, downDialer, out); err != nil {
		t.Fatalf("runDevicesStatus: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Engine: not running") {
		t.Errorf("missing engine-down line:\n%s", got)
	}
	if !strings.Contains(got, "kitchen-mac") || !strings.Contains(got, "unknown") {
		t.Errorf("registry peer not rendered with unknown state:\n%s", got)
	}
}
