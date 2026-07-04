// CLI tests for the `msgbrowse devices` namespace: pair (both roles, against
// live TLS listeners), list, unpair, status, and the serve wiring's
// default-off socket posture (SPEC-0011 "Default config exposes nothing
// new"). Run with -race per SPEC-0011 REQ "Concurrency Safety".
package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/log"
	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/devices"
	"github.com/joestump/msgbrowse/internal/devices/listener"
	"github.com/joestump/msgbrowse/internal/store"
)

// testDeviceCfg builds a device-sync-enabled config rooted in a temp dir.
func testDeviceCfg(t *testing.T, name string) *config.Config {
	t.Helper()
	return &config.Config{
		DataDir:    t.TempDir(),
		ListenAddr: "127.0.0.1:8787",
		LogLevel:   "error",
		DeviceSync: config.DeviceSyncConfig{
			Enabled:      true,
			ListenAddr:   "127.0.0.1:0",
			DeviceName:   name,
			PollInterval: 15 * time.Minute,
		},
	}
}

// seedPeer writes one paired device into cfg's store.
func seedPeer(t *testing.T, cfg *config.Config, p devices.Peer) {
	t.Helper()
	st, err := openStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.UpsertPairedDevice(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

func TestImportedSources(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Config
		want []string
	}{
		{"none", config.Config{}, nil},
		{"signal only", config.Config{ArchiveRoot: "/a"}, []string{"signal"}},
		{"all three", config.Config{ArchiveRoot: "/a", IMessageArchiveRoot: "/b", WhatsAppArchiveRoot: "/c"},
			[]string{"signal", "imessage", "whatsapp"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := importedSources(&c.cfg)
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("importedSources = %v, want %v", got, c.want)
			}
		})
	}
}

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	ip, ipnet, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	ipnet.IP = ip
	return ipnet
}

func TestLanHostFrom(t *testing.T) {
	cases := []struct {
		name  string
		addrs []string
		want  string
	}{
		{"prefers global ipv4", []string{"127.0.0.1/8", "169.254.10.10/16", "fe80::1/64", "2001:db8::5/64", "192.168.1.20/24"}, "192.168.1.20"},
		{"falls back to global ipv6", []string{"127.0.0.1/8", "fe80::1/64", "2001:db8::5/64"}, "2001:db8::5"},
		{"nothing usable", []string{"127.0.0.1/8", "fe80::1/64"}, ""},
		{"empty", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var addrs []net.Addr
			for _, s := range c.addrs {
				addrs = append(addrs, mustCIDR(t, s))
			}
			if got := lanHostFrom(addrs); got != c.want {
				t.Errorf("lanHostFrom = %q, want %q", got, c.want)
			}
		})
	}
}

func TestAdvertiseHost(t *testing.T) {
	t.Run("explicit host used as-is", func(t *testing.T) {
		got, err := advertiseHost("192.168.1.10:8788")
		if err != nil || got != "192.168.1.10" {
			t.Errorf("advertiseHost = %q, %v", got, err)
		}
	})
	t.Run("invalid addr errors", func(t *testing.T) {
		if _, err := advertiseHost("nope"); err == nil {
			t.Error("want error for unparseable addr")
		}
	})
	t.Run("wildcard host derives a LAN address", func(t *testing.T) {
		orig := interfaceAddrs
		defer func() { interfaceAddrs = orig }()
		interfaceAddrs = func() ([]net.Addr, error) {
			return []net.Addr{mustCIDR(t, "10.1.2.3/24")}, nil
		}
		got, err := advertiseHost(":8788")
		if err != nil || got != "10.1.2.3" {
			t.Errorf("advertiseHost(\":8788\") = %q, %v; want 10.1.2.3", got, err)
		}
	})
	t.Run("wildcard host with no usable interface errors", func(t *testing.T) {
		orig := interfaceAddrs
		defer func() { interfaceAddrs = orig }()
		interfaceAddrs = func() ([]net.Addr, error) { return nil, nil }
		if _, err := advertiseHost("0.0.0.0:8788"); err == nil {
			t.Error("want error when no LAN address is derivable")
		}
	})
}

func TestMatchPeerFingerprint(t *testing.T) {
	peers := []devices.Peer{
		{ID: 1, Name: "kitchen", Fingerprint: strings.Repeat("a", 60) + "1111"},
		{ID: 2, Name: "office", Fingerprint: strings.Repeat("a", 60) + "2222"},
		{ID: 3, Name: "attic", Fingerprint: strings.Repeat("b", 64)},
	}
	cases := []struct {
		name     string
		arg      string
		wantName string
		wantErr  string
	}{
		{"too short", "abc", "", "too short"},
		{"no match", strings.Repeat("c", 12), "", "no paired device"},
		{"ambiguous prefix", strings.Repeat("a", 12), "", "ambiguous"},
		{"unique prefix", strings.Repeat("b", 12), "attic", ""},
		{"full match", peers[0].Fingerprint, "kitchen", ""},
		{"colons and case tolerated", "BB:BB:BB:BB:BB:BB", "attic", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := matchPeerFingerprint(peers, c.arg)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != c.wantName {
				t.Errorf("matched %q, want %q", got.Name, c.wantName)
			}
		})
	}
}

func TestDevicesListEmpty(t *testing.T) {
	cfg := testDeviceCfg(t, "node")
	var out bytes.Buffer
	if err := devicesList(context.Background(), &out, cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no devices paired") {
		t.Errorf("list output = %q, want 'no devices paired'", out.String())
	}
}

func TestDevicesListWithPeers(t *testing.T) {
	cfg := testDeviceCfg(t, "node")
	fp := strings.Repeat("d", 64)
	seedPeer(t, cfg, devices.Peer{
		Name:        "kitchen-server",
		Fingerprint: fp,
		Address:     "192.168.1.20:8788",
		Roles:       map[string]devices.Role{"signal": devices.RoleReplica},
		PairedAt:    time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
	})

	var out bytes.Buffer
	if err := devicesList(context.Background(), &out, cfg); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NAME", "FINGERPRINT", "kitchen-server", fp, "192.168.1.20:8788", "signal:replica"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("list output missing %q:\n%s", want, out.String())
		}
	}
}

func TestDevicesUnpair(t *testing.T) {
	cfg := testDeviceCfg(t, "node")
	fp := strings.Repeat("e", 64)
	seedPeer(t, cfg, devices.Peer{
		Name: "old-laptop", Fingerprint: fp, Address: "192.168.1.30:8788",
		Roles: map[string]devices.Role{"signal": devices.RoleReplica}, PairedAt: time.Now(),
	})

	var out bytes.Buffer
	if err := devicesUnpair(context.Background(), &out, cfg, fp[:16]); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Unpaired old-laptop") {
		t.Errorf("unpair output = %q", out.String())
	}

	// The peer is gone; a second unpair cannot find it.
	if err := devicesUnpair(context.Background(), &out, cfg, fp[:16]); err == nil {
		t.Error("second unpair should fail: peer already removed")
	}

	var list bytes.Buffer
	if err := devicesList(context.Background(), &list, cfg); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list.String(), "no devices paired") {
		t.Errorf("after unpair, list = %q", list.String())
	}
}

func TestDevicesStatus(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		cfg := testDeviceCfg(t, "node")
		cfg.DeviceSync.Enabled = false
		var out bytes.Buffer
		if err := devicesStatus(context.Background(), &out, cfg); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "device sync: disabled") {
			t.Errorf("status = %q", out.String())
		}
	})

	t.Run("enabled with identity and peer", func(t *testing.T) {
		cfg := testDeviceCfg(t, "mac-importer")
		id, _, err := devices.LoadOrCreateIdentity(devices.IdentityDir(cfg.DataDir), "mac-importer")
		if err != nil {
			t.Fatal(err)
		}
		seedPeer(t, cfg, devices.Peer{
			Name: "kitchen-server", Fingerprint: strings.Repeat("f", 64), Address: "192.168.1.20:8788",
			Roles: map[string]devices.Role{"signal": devices.RoleImporter}, PairedAt: time.Now(),
		})

		var out bytes.Buffer
		if err := devicesStatus(context.Background(), &out, cfg); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"device sync: enabled",
			"device name:   mac-importer",
			id.Fingerprint(),
			"kitchen-server",
			"signal: last adopted manifest generation 0",
		} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("status missing %q:\n%s", want, out.String())
			}
		}
	})
}

func TestDevicesPairRequiresOptIn(t *testing.T) {
	cfg := testDeviceCfg(t, "node")
	cfg.DeviceSync.Enabled = false
	if err := devicesPairImporter(context.Background(), io.Discard, cfg); err == nil ||
		!strings.Contains(err.Error(), "device sync is disabled") {
		t.Errorf("importer pair with sync disabled = %v, want opt-in error", err)
	}
	if err := devicesPairReplica(context.Background(), io.Discard, cfg, "MSGB1.x"); err == nil ||
		!strings.Contains(err.Error(), "device sync is disabled") {
		t.Errorf("replica pair with sync disabled = %v, want opt-in error", err)
	}
}

// startImporterListener runs a live device-sync listener backed by a REAL
// store (registry included), returning its bound addr and pairing payload.
func startImporterListener(t *testing.T, window *devices.Window) (addr string, id *devices.Identity, st *store.Store) {
	t.Helper()
	cfg := testDeviceCfg(t, "mac-importer")
	cfg.ArchiveRoot = "/signal" // imports the signal source
	var err error
	st, err = openStore(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	id, _, err = devices.LoadOrCreateIdentity(devices.IdentityDir(cfg.DataDir), "mac-importer")
	if err != nil {
		t.Fatal(err)
	}
	imp := &devices.Importer{
		DeviceName: "mac-importer",
		Sources:    importedSources(cfg),
		Store:      st,
		Logger:     log.New(io.Discard),
	}
	imp.SetWindow(window)
	l := &listener.Listener{
		Identity: id,
		Importer: imp,
		Registry: storeRegistry{st},
		Addr:     "127.0.0.1:0",
		Logger:   log.New(io.Discard),
	}
	ctx, cancel := context.WithCancel(context.Background())
	ln, err := l.Listen(ctx)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- l.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("importer listener did not shut down")
		}
	})
	return ln.Addr().String(), id, st
}

// TestDevicesPairReplicaEndToEnd completes the replica-side CLI flow against
// a live importer listener: decode → fingerprint-verified TLS → token →
// mutual pin, both registries persisted (issue #105 acceptance).
func TestDevicesPairReplicaEndToEnd(t *testing.T) {
	window, err := devices.OpenWindow(0)
	if err != nil {
		t.Fatal(err)
	}
	addr, importerID, importerStore := startImporterListener(t, window)

	payload, err := devices.NewPairingPayload(addr, window.Token(), importerID.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	code, err := payload.EncodeManualCode()
	if err != nil {
		t.Fatal(err)
	}

	replicaCfg := testDeviceCfg(t, "kitchen-server")
	var out bytes.Buffer
	if err := devicesPairReplica(context.Background(), &out, replicaCfg, code); err != nil {
		t.Fatalf("devicesPairReplica: %v", err)
	}
	if !strings.Contains(out.String(), "Paired with mac-importer") {
		t.Errorf("replica output = %q", out.String())
	}
	if !strings.Contains(out.String(), "signal") {
		t.Errorf("replica output should list served sources: %q", out.String())
	}

	// Replica pinned the importer (role: importer for signal).
	rst, err := store.OpenReadOnly(dbPath(replicaCfg))
	if err != nil {
		t.Fatal(err)
	}
	defer rst.Close()
	peers, err := rst.ListPairedDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].Fingerprint != importerID.Fingerprint() ||
		peers[0].Roles["signal"] != devices.RoleImporter {
		t.Errorf("replica registry = %+v, want the importer pinned", peers)
	}

	// Importer pinned the replica symmetrically.
	ipeers, err := importerStore.ListPairedDevices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ipeers) != 1 || ipeers[0].Name != "kitchen-server" ||
		ipeers[0].Roles["signal"] != devices.RoleReplica {
		t.Errorf("importer registry = %+v, want the replica pinned", ipeers)
	}
}

// syncBuffer is a goroutine-safe io.Writer for capturing CLI output that is
// written from another goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

var manualCodeRE = regexp.MustCompile(`MSGB1\.[A-Za-z0-9_-]+`)

// TestDevicesPairImporterEndToEnd drives the importer-side CLI flow: it
// opens a window, binds the listener, prints the QR + manual code, and the
// command completes successfully once a replica pairs with the printed code.
func TestDevicesPairImporterEndToEnd(t *testing.T) {
	importerCfg := testDeviceCfg(t, "mac-importer")
	importerCfg.ArchiveRoot = "/signal"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out := &syncBuffer{}
	errCh := make(chan error, 1)
	go func() { errCh <- devicesPairImporter(ctx, out, importerCfg) }()

	// Wait for the manual code to appear on the "terminal".
	var code string
	deadline := time.Now().Add(10 * time.Second)
	for code == "" {
		if m := manualCodeRE.FindString(out.String()); m != "" {
			code = m
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("manual pairing code never printed; output:\n%s", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The printed output must include the QR block AND the manual code —
	// the manual code is the accessibility fallback (SPEC-0011).
	if !strings.Contains(out.String(), "Manual pairing code") {
		t.Errorf("output missing the manual-code section:\n%s", out.String())
	}

	replicaCfg := testDeviceCfg(t, "kitchen-server")
	if err := devicesPairReplica(ctx, io.Discard, replicaCfg, code); err != nil {
		t.Fatalf("replica pairing against printed code: %v", err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("devicesPairImporter = %v, want success after pairing", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("devicesPairImporter did not return after a successful pairing")
	}
	if !strings.Contains(out.String(), "Paired with kitchen-server") {
		t.Errorf("importer output missing pairing confirmation:\n%s", out.String())
	}
}

// NOTE: the startDeviceSync tests moved to serve_test.go when ADR-0021
// swapped serve's device-sync path from the bespoke SPEC-0011 mTLS listener
// to the supervised Syncthing engine. The supervised lifecycle itself
// (start/stop/no-orphan/context-cancel/restart) is proven against a fake
// Syncthing binary in internal/syncthing's suite.
