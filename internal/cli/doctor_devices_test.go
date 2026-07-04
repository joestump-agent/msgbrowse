// doctor device-sync rows (#158; SPEC-0014 REQ "Status and Doctor
// Surfacing"): each health state — disabled, engine down, healthy, syncing,
// paused, errored, peers connected/disconnected — must come out as its own
// truthful row with a remediation hint where the spec demands one. The
// engine is an httptest stub reached exactly the production way: the
// supervisor-persisted REST address + API key under <data_dir>/syncthing/.
package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/devices"
	"github.com/joestump/msgbrowse/internal/store"
	"github.com/joestump/msgbrowse/internal/syncthing"
)

// doctorEngine is a scriptable REST stub for the doctor checks.
type doctorEngine struct {
	folderState string
	folderErrs  int
	completion  float64
	needItems   int64
	paused      bool
	connected   bool
}

// start serves the stub and writes the discovery files under dataDir so
// checkDeviceSync finds it the way it finds a real supervised daemon.
func (d *doctorEngine) start(t *testing.T, dataDir string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /rest/system/ping", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ping":"pong"}`))
	})
	mux.HandleFunc("GET /rest/system/version", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"v2.0.10"}`))
	})
	mux.HandleFunc("GET /rest/system/connections", func(w http.ResponseWriter, _ *http.Request) {
		conn := "false"
		if d.connected {
			conn = "true"
		}
		_, _ = w.Write([]byte(`{"connections":{"` + testSyncDeviceID + `":{"connected":` + conn + `}}}`))
	})
	mux.HandleFunc("GET /rest/config/folders", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, []syncthing.FolderConfig{{
			ID: "msgbrowse-signal", Path: "/x/archives/signal", Type: "sendreceive", Paused: d.paused,
		}})
	})
	mux.HandleFunc("GET /rest/db/status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"state": d.folderState, "errors": d.folderErrs, "pullErrors": 0})
	})
	mux.HandleFunc("GET /rest/db/completion", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"completion": d.completion, "needItems": d.needItems})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	home := filepath.Join(dataDir, syncthing.HomeDirName)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "apikey"), []byte("test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "api-address"), []byte(strings.TrimPrefix(srv.URL, "http://")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// doctorSyncFixture returns an enabled device-sync config over a temp data
// dir plus a real store seeded with one paired peer.
func doctorSyncFixture(t *testing.T, roles map[string]string) (*config.Config, *store.Store) {
	t.Helper()
	cfg := &config.Config{
		DataDir:    t.TempDir(),
		ListenAddr: "127.0.0.1:8787",
		DeviceSync: config.DeviceSyncConfig{Enabled: true, ListenAddr: ":8788"},
	}
	st := openTestStore(t)
	seedPeer(t, st, testSyncDeviceID, "kitchen-mac", roles)
	return cfg, st
}

// runDeviceSyncChecks executes checkDeviceSync into a buffer.
func runDeviceSyncChecks(t *testing.T, cfg *config.Config, st *store.Store) (string, *report) {
	t.Helper()
	buf := &bytes.Buffer{}
	r := &report{w: buf}
	checkDeviceSync(context.Background(), r, cfg, st)
	return buf.String(), r
}

// TestDoctorDeviceSyncDisabled: the healthy default is a single pass row.
func TestDoctorDeviceSyncDisabled(t *testing.T) {
	cfg := &config.Config{DataDir: t.TempDir(), ListenAddr: "127.0.0.1:8787"}
	out, r := runDeviceSyncChecks(t, cfg, nil)
	if !strings.Contains(out, "device sync disabled") {
		t.Errorf("output = %q", out)
	}
	if r.warnings != 0 || r.fails != 0 {
		t.Errorf("disabled default produced warnings=%d fails=%d", r.warnings, r.fails)
	}
}

// TestDoctorDeviceSyncEngineDown: enabled with no daemon running warns with
// the start hint, and the peer row reports state unknown — never a fake
// connected/disconnected claim.
func TestDoctorDeviceSyncEngineDown(t *testing.T) {
	cfg, st := doctorSyncFixture(t, nil)
	out, _ := runDeviceSyncChecks(t, cfg, st)
	if !strings.Contains(out, "sync engine not running") {
		t.Errorf("missing engine-down row:\n%s", out)
	}
	if !strings.Contains(out, "start `msgbrowse serve` or the desktop app") {
		t.Errorf("engine-down row missing its remediation hint:\n%s", out)
	}
	if !strings.Contains(out, "kitchen-mac") || !strings.Contains(out, "state unknown") {
		t.Errorf("peer inventory missing/untruthful with engine down:\n%s", out)
	}
}

// TestDoctorDeviceSyncHealthy: a running engine, a connected peer, and an
// idle 100% folder are all pass rows — including the replica-role line when
// the peer is a recorded importer.
func TestDoctorDeviceSyncHealthy(t *testing.T) {
	cfg, st := doctorSyncFixture(t, map[string]string{"signal": devices.RoleImporter})
	(&doctorEngine{folderState: "idle", completion: 100, connected: true}).start(t, cfg.DataDir)

	out, r := runDeviceSyncChecks(t, cfg, st)
	for _, want := range []string{
		"sync engine running (syncthing v2.0.10",
		"kitchen-mac", "connected",
		"folder msgbrowse-signal healthy: 100% complete",
		"replica for signal — imported on kitchen-mac",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("healthy report missing %q:\n%s", want, out)
		}
	}
	if r.fails != 0 {
		t.Errorf("healthy state produced %d fails:\n%s", r.fails, out)
	}
}

// TestDoctorDeviceSyncPausedFolder: a paused folder is a warn with the
// unpause hint (the SPEC-0014 "paused or errored sync shows" scenario).
func TestDoctorDeviceSyncPausedFolder(t *testing.T) {
	cfg, st := doctorSyncFixture(t, nil)
	(&doctorEngine{folderState: "idle", completion: 73, paused: true, connected: true}).start(t, cfg.DataDir)

	out, r := runDeviceSyncChecks(t, cfg, st)
	if !strings.Contains(out, "folder msgbrowse-signal is paused (73% complete)") {
		t.Errorf("paused folder row missing:\n%s", out)
	}
	if !strings.Contains(out, "a paused folder syncs nothing") {
		t.Errorf("paused row missing its remediation hint:\n%s", out)
	}
	if r.warnings == 0 {
		t.Error("paused folder did not warn")
	}
}

// TestDoctorDeviceSyncErroredFolder: failed items are a FAIL row with the
// permissions/Logs hint, and doctor's exit contract (fails > 0) trips.
func TestDoctorDeviceSyncErroredFolder(t *testing.T) {
	cfg, st := doctorSyncFixture(t, nil)
	(&doctorEngine{folderState: "idle", folderErrs: 4, completion: 90, connected: true}).start(t, cfg.DataDir)

	out, r := runDeviceSyncChecks(t, cfg, st)
	if !strings.Contains(out, "folder msgbrowse-signal reports errors") || !strings.Contains(out, "4 failed items") {
		t.Errorf("errored folder row missing:\n%s", out)
	}
	if !strings.Contains(out, "check file permissions") {
		t.Errorf("errored row missing its remediation hint:\n%s", out)
	}
	if r.fails == 0 {
		t.Error("errored folder did not fail the report")
	}
}

// TestDoctorDeviceSyncSyncingFolder: mid-transfer is a pass (normal
// condition) that names the remaining delta.
func TestDoctorDeviceSyncSyncingFolder(t *testing.T) {
	cfg, st := doctorSyncFixture(t, nil)
	(&doctorEngine{folderState: "syncing", completion: 62, needItems: 9, connected: true}).start(t, cfg.DataDir)

	out, r := runDeviceSyncChecks(t, cfg, st)
	if !strings.Contains(out, "folder msgbrowse-signal syncing: 62% complete (9 items to go)") {
		t.Errorf("syncing folder row missing:\n%s", out)
	}
	if r.fails != 0 {
		t.Errorf("syncing state failed the report:\n%s", out)
	}
}

// TestDoctorDeviceSyncDisconnectedPeer: a running engine with a disconnected
// peer warns and names it.
func TestDoctorDeviceSyncDisconnectedPeer(t *testing.T) {
	cfg, st := doctorSyncFixture(t, nil)
	(&doctorEngine{folderState: "idle", completion: 100, connected: false}).start(t, cfg.DataDir)

	out, r := runDeviceSyncChecks(t, cfg, st)
	if !strings.Contains(out, "kitchen-mac") || !strings.Contains(out, "disconnected") {
		t.Errorf("disconnected peer not named:\n%s", out)
	}
	if !strings.Contains(out, "disconnected peers sync nothing") {
		t.Errorf("disconnected row missing its hint:\n%s", out)
	}
	if r.warnings == 0 {
		t.Error("disconnected peer did not warn")
	}
}
