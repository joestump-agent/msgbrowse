// Supervisor lifecycle tests against a FAKE Syncthing binary: the test binary
// re-executes itself as a stub daemon (helper-process pattern) that parses the
// real serve flags, writes a pid file, serves just enough of the REST API
// (ping/status/version/patch-device), and exits on SIGTERM — so
// start/stop/no-orphan/context-cancel/restart-with-backoff are all proven on
// headless Linux with CGO_ENABLED=0. The real bundled binary is exercised only
// by the macOS CI leg's relocation probe (.github/workflows/desktop.yml).
//
// Governing: ADR-0021, SPEC-0014 REQ "Supervised Daemon Lifecycle", REQ
// "Concurrency Safety" ("Graceful shutdown terminates the child").
package syncthing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain routes the helper-process mode: when FAKE_SYNCTHING=1 the test
// binary behaves as the fake Syncthing daemon instead of running tests.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_SYNCTHING") == "1" {
		os.Exit(fakeSyncthingMain(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeSyncthingMain is the stub daemon. It speaks just enough Syncthing:
//   - `--version` prints a version line and exits 0 (FAKE_SYNCTHING_VERSION
//     overrides the version string, for the pinned-version mismatch test).
//   - `serve --home=... --gui-address=... --gui-apikey=...` writes
//     <home>/fake.pid, serves the stub REST API on the GUI address, and exits
//     0 on SIGTERM (unless FAKE_SYNCTHING_IGNORE_TERM=1, which forces the
//     supervisor's kill-after-grace path).
//   - FAKE_SYNCTHING_CRASH_ONCE=1 makes the FIRST run exit 1 shortly after
//     becoming ready (a marker file in home distinguishes runs), driving the
//     restart-with-backoff path.
func fakeSyncthingMain(args []string) int {
	if len(args) > 0 && args[0] == "--version" {
		v := os.Getenv("FAKE_SYNCTHING_VERSION")
		if v == "" {
			v = "v0.0.0-fake"
		}
		fmt.Printf("syncthing %s \"Fake Fjord\" (go/test) fake@build\n", v)
		return 0
	}
	if len(args) == 0 || args[0] != "serve" {
		fmt.Fprintln(os.Stderr, "fake syncthing: unsupported args", args)
		return 2
	}
	var home, guiAddr, apiKey string
	for _, a := range args[1:] {
		switch {
		case strings.HasPrefix(a, "--home="):
			home = strings.TrimPrefix(a, "--home=")
		case strings.HasPrefix(a, "--gui-address="):
			guiAddr = strings.TrimPrefix(a, "--gui-address=")
		case strings.HasPrefix(a, "--gui-apikey="):
			apiKey = strings.TrimPrefix(a, "--gui-apikey=")
		}
	}
	if home == "" || guiAddr == "" || apiKey == "" {
		fmt.Fprintln(os.Stderr, "fake syncthing: missing --home/--gui-address/--gui-apikey", args)
		return 2
	}
	if err := os.WriteFile(filepath.Join(home, "fake.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "fake syncthing: write pid:", err)
		return 1
	}

	mux := http.NewServeMux()
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-API-Key") != apiKey {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			next(w, r)
		}
	}
	mux.HandleFunc("/rest/system/ping", auth(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"ping": "pong"})
	}))
	mux.HandleFunc("/rest/system/status", auth(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"myID": "FAKEDEV-ICE0001", "uptime": 1})
	}))
	mux.HandleFunc("/rest/system/version", auth(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"version": "v0.0.0-fake"})
	}))
	mux.HandleFunc("/rest/config/devices/", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	ln, err := net.Listen("tcp", guiAddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake syncthing: listen:", err)
		return 1
	}
	go func() { _ = http.Serve(ln, mux) }()

	if os.Getenv("FAKE_SYNCTHING_CRASH_ONCE") == "1" {
		marker := filepath.Join(home, "crashed-once")
		if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
			_ = os.WriteFile(marker, []byte("1"), 0o600)
			time.Sleep(300 * time.Millisecond) // stay ready long enough for Start to succeed
			return 1                           // unexpected death
		}
	}

	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM)
	if os.Getenv("FAKE_SYNCTHING_IGNORE_TERM") == "1" {
		select {} // never exits on its own; the supervisor must kill it
	}
	<-term
	return 0
}

// fakeOptions builds Options that run the fake daemon: BinPath is this test
// binary, routed into fakeSyncthingMain via the FAKE_SYNCTHING env seam.
func fakeOptions(t *testing.T, dataDir string, extraEnv ...string) Options {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return Options{
		BinPath:      exe,
		DataDir:      dataDir,
		ListenAddr:   "127.0.0.1:0",
		DeviceName:   "test-node",
		Grace:        2 * time.Second,
		ReadyTimeout: 10 * time.Second,
		extraEnv:     append([]string{"FAKE_SYNCTHING=1"}, extraEnv...),
	}
}

// makeManagedRoot creates <dataDir>/archives/<src> like setup.Provision does.
func makeManagedRoot(t *testing.T, dataDir, src string) string {
	t.Helper()
	root := filepath.Join(dataDir, "archives", src)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir managed root: %v", err)
	}
	return root
}

// readFakePID reads the pid the fake daemon recorded in its home dir.
func readFakePID(t *testing.T, dataDir string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dataDir, HomeDirName, "fake.pid"))
	if err != nil {
		t.Fatalf("read fake pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("parse fake pid: %v", err)
	}
	return pid
}

// processGone reports whether pid no longer exists (signal 0 probing).
func processGone(pid int) bool {
	err := syscall.Kill(pid, 0)
	return errors.Is(err, syscall.ESRCH)
}

// waitProcessGone polls until the pid is gone or the timeout expires.
func waitProcessGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if processGone(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d still exists after %s — orphaned syncthing child", pid, timeout)
}

// TestSupervisorStartStop is the core lifecycle: Start launches the child and
// confirms readiness over the REST API, msgbrowse's generated config and the
// folder preparation land on disk, and context cancellation cleanly stops the
// child with Wait returning nil and no orphan process left.
func TestSupervisorStartStop(t *testing.T) {
	dataDir := t.TempDir()
	root := makeManagedRoot(t, dataDir, "signal")
	folders, err := ExistingManagedFolders(dataDir)
	if err != nil {
		t.Fatalf("ExistingManagedFolders: %v", err)
	}
	if len(folders) != 1 || folders[0].Path != root {
		t.Fatalf("folders = %+v, want exactly the signal root %s", folders, root)
	}

	opts := fakeOptions(t, dataDir)
	opts.Folders = folders
	sup, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The daemon is live and authenticated: the ping must succeed.
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer pingCancel()
	if err := sup.Client().Ping(pingCtx); err != nil {
		t.Fatalf("Ping after Start: %v", err)
	}

	// msgbrowse-owned artifacts exist: config.xml, the persisted API key,
	// the folder marker, and the ignore file.
	home := filepath.Join(dataDir, HomeDirName)
	for _, p := range []string{
		filepath.Join(home, "config.xml"),
		filepath.Join(home, apiKeyFile),
		filepath.Join(root, ".stfolder"),
		filepath.Join(root, ".stignore"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected artifact missing: %v", err)
		}
	}
	// The generated config carries the API key and the loopback bind.
	cfg, err := os.ReadFile(filepath.Join(home, "config.xml"))
	if err != nil {
		t.Fatalf("read config.xml: %v", err)
	}
	if !strings.Contains(string(cfg), sup.APIKey()) {
		t.Error("config.xml does not carry the generated API key")
	}
	if !strings.Contains(string(cfg), sup.APIAddr()) {
		t.Error("config.xml does not carry the loopback GUI address")
	}

	pid := readFakePID(t, dataDir)

	// Context cancellation is the one shutdown path: clean stop, nil from
	// Wait, and the child process fully gone (no orphan).
	cancel()
	if err := sup.Wait(); err != nil {
		t.Fatalf("Wait after cancel: %v", err)
	}
	waitProcessGone(t, pid, 3*time.Second)
}

// TestSupervisorKillsUnresponsiveChild proves the SIGTERM→grace→kill ladder:
// a daemon that ignores SIGTERM is killed after the grace period, Wait still
// returns nil, and no orphan survives (SPEC-0014 "App quit stops the daemon").
func TestSupervisorKillsUnresponsiveChild(t *testing.T) {
	dataDir := t.TempDir()
	opts := fakeOptions(t, dataDir, "FAKE_SYNCTHING_IGNORE_TERM=1")
	opts.Grace = 300 * time.Millisecond
	sup, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := readFakePID(t, dataDir)

	start := time.Now()
	cancel()
	if err := sup.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed < opts.Grace {
		t.Errorf("Wait returned after %s, before the %s grace period — kill ladder skipped", elapsed, opts.Grace)
	}
	waitProcessGone(t, pid, 3*time.Second)
}

// TestSupervisorRestartsAfterCrash proves restart-with-backoff: the fake
// daemon dies unexpectedly after its first readiness, and the supervisor
// respawns it (a new pid, REST answering again) while the context stays live.
func TestSupervisorRestartsAfterCrash(t *testing.T) {
	dataDir := t.TempDir()
	opts := fakeOptions(t, dataDir, "FAKE_SYNCTHING_CRASH_ONCE=1")
	sup, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sup.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	firstPID := readFakePID(t, dataDir)

	// The first child exits 1 ~300ms in; the supervisor backs off (1s) and
	// respawns. Poll for a NEW pid answering the REST ping.
	deadline := time.Now().Add(15 * time.Second)
	restarted := false
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(filepath.Join(dataDir, HomeDirName, "fake.pid"))
		if err == nil {
			if pid, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && pid != firstPID {
				pingCtx, pingCancel := context.WithTimeout(context.Background(), time.Second)
				perr := sup.Client().Ping(pingCtx)
				pingCancel()
				if perr == nil {
					restarted = true
					break
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !restarted {
		t.Fatal("supervisor did not restart the crashed daemon")
	}

	secondPID := readFakePID(t, dataDir)
	cancel()
	if err := sup.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	waitProcessGone(t, secondPID, 3*time.Second)
}

// TestSupervisorIntegrityFailures: a missing binary and a pinned-version
// mismatch are typed startup failures, and in both cases NO child process is
// ever launched (SPEC-0014 "Tampered bundled binary refuses to launch").
func TestSupervisorIntegrityFailures(t *testing.T) {
	t.Run("missing binary", func(t *testing.T) {
		dataDir := t.TempDir()
		opts := fakeOptions(t, dataDir)
		opts.BinPath = filepath.Join(dataDir, "no-such-syncthing")
		sup, err := New(opts)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		err = sup.Start(context.Background())
		if !errors.Is(err, ErrBinaryNotFound) {
			t.Fatalf("Start err = %v, want ErrBinaryNotFound", err)
		}
	})
	t.Run("pinned version mismatch", func(t *testing.T) {
		dataDir := t.TempDir()
		opts := fakeOptions(t, dataDir, "FAKE_SYNCTHING_VERSION=v0.0.1-wrong")
		opts.PinnedVersion = "v9.9.9"
		// Route the version probe through the fake env: the probe execs the
		// binary directly (not via spawn), so the runner injects the env.
		opts.verifyRunner = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return []byte("syncthing v0.0.1-wrong \"Fake\" (go/test)\n"), nil
		}
		sup, err := New(opts)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		err = sup.Start(context.Background())
		if !errors.Is(err, ErrIntegrity) {
			t.Fatalf("Start err = %v, want ErrIntegrity", err)
		}
		if _, statErr := os.Stat(filepath.Join(dataDir, HomeDirName, "fake.pid")); !errors.Is(statErr, os.ErrNotExist) {
			t.Error("a child was launched despite the failed integrity check")
		}
	})
}

// TestSupervisorRejectsUnmanagedFolder: a folder outside the managed archive
// roots aborts startup before any process exists — the config-generation
// guard that keeps the DB out of synced folders (SPEC-0014 "The database is
// never in a synced folder").
func TestSupervisorRejectsUnmanagedFolder(t *testing.T) {
	dataDir := t.TempDir()
	opts := fakeOptions(t, dataDir)
	opts.Folders = []Folder{{ID: "evil", Label: "evil", Path: dataDir}} // data_dir itself: refused
	sup, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = sup.Start(context.Background())
	if !errors.Is(err, ErrUnmanagedFolder) {
		t.Fatalf("Start err = %v, want ErrUnmanagedFolder", err)
	}
	if _, statErr := os.Stat(filepath.Join(dataDir, HomeDirName, "fake.pid")); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("a child was launched despite the unmanaged folder")
	}
}

// TestSupervisorAPIKeyStableAcrossRestarts: the generated API key persists,
// so clients built before a daemon restart stay valid.
func TestSupervisorAPIKeyStableAcrossRestarts(t *testing.T) {
	home := t.TempDir()
	k1, err := loadOrCreateAPIKey(home)
	if err != nil {
		t.Fatalf("loadOrCreateAPIKey: %v", err)
	}
	if len(k1) != 64 {
		t.Errorf("api key length = %d, want 64 hex chars", len(k1))
	}
	k2, err := loadOrCreateAPIKey(home)
	if err != nil {
		t.Fatalf("loadOrCreateAPIKey (second): %v", err)
	}
	if k1 != k2 {
		t.Error("api key changed between loads; must be stable")
	}
	fi, err := os.Stat(filepath.Join(home, apiKeyFile))
	if err != nil {
		t.Fatalf("stat api key file: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("api key file mode = %o, want 0600", perm)
	}
}
