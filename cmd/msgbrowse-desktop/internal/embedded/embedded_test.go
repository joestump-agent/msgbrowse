// Headless unit tests for the embedded-server wiring: these run with
// CGO_ENABLED=0 and no webview toolchain, which is how the desktop story is
// verified on machines that cannot open a window.
//
// Governing: ADR-0017, SPEC-0010 REQ "Embedded server on a loopback ephemeral
// port", REQ "Graceful shutdown".
package embedded

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/store"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{DataDir: t.TempDir()}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startServer starts an embedded server against a fresh data dir and
// registers cleanup that cancels it and waits for Close.
func startServer(t *testing.T, ctx context.Context, cancel context.CancelFunc) *Server {
	t.Helper()
	es, err := Start(ctx, testConfig(t), testLogger())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := es.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return es
}

// TestStartBindsLoopbackEphemeralPort verifies the SPEC-0010 bind-surface
// contract: the URL the webview is pointed at is 127.0.0.1 on a real,
// non-zero ephemeral port.
func TestStartBindsLoopbackEphemeralPort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	es := startServer(t, ctx, cancel)

	u, err := url.Parse(es.URL)
	if err != nil {
		t.Fatalf("parse URL %q: %v", es.URL, err)
	}
	if u.Scheme != "http" || u.Hostname() != "127.0.0.1" {
		t.Errorf("URL = %q; want http://127.0.0.1:<port>", es.URL)
	}
	if u.Port() == "" || u.Port() == "0" {
		t.Errorf("URL port = %q; want a resolved ephemeral port", u.Port())
	}
}

// TestServesRealAppOverLoopback proves zero handler divergence: GET / against
// the embedded server returns the same server-rendered shell, behind the same
// strict security headers, that `msgbrowse serve` produces.
func TestServesRealAppOverLoopback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	es := startServer(t, ctx, cancel)

	resp, err := http.Get(es.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d; want 200", resp.StatusCode)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP = %q; want the strict policy from internal/web", csp)
	}
	if !strings.Contains(string(body), "msgbrowse") {
		t.Error("GET / did not render the app shell")
	}
}

// TestShutdownReleasesPortAndStore drives the full SPEC-0010 "Graceful
// shutdown" sequence headlessly: cancel the context (what window close does),
// wait for the serve loop, close the store, and confirm the loopback port and
// the SQLite database are both released.
func TestShutdownReleasesPortAndStore(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cfg := testConfig(t)
	es, err := Start(ctx, cfg, testLogger())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	addr := strings.TrimPrefix(es.URL, "http://")

	cancel()
	select {
	case <-es.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("serve loop did not exit after context cancel")
	}
	if err := es.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Port released: we can bind the exact address again.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("loopback port not released after shutdown: %v", err)
	}
	ln.Close()

	// Store released: the database opens cleanly for a fresh consumer.
	st, err := store.Open(filepath.Join(cfg.DataDir, store.DBFileName))
	if err != nil {
		t.Fatalf("store not reopenable after shutdown: %v", err)
	}
	st.Close()
}

// TestEphemeralPortsDoNotCollide mirrors SPEC-0010's "No port collision with
// a running serve" scenario: with another loopback listener already bound (a
// stand-in for `msgbrowse serve` on 8787), the embedded server picks its own
// distinct port and both work.
func TestEphemeralPortsDoNotCollide(t *testing.T) {
	other, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen (stand-in serve): %v", err)
	}
	defer other.Close()

	ctx, cancel := context.WithCancel(context.Background())
	es := startServer(t, ctx, cancel)

	if strings.TrimPrefix(es.URL, "http://") == other.Addr().String() {
		t.Fatalf("embedded server reused the occupied address %s", other.Addr())
	}
	resp, err := http.Get(es.URL + "/")
	if err != nil {
		t.Fatalf("GET / with a second listener up: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d; want 200", resp.StatusCode)
	}
}
