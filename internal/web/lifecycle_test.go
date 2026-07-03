// Lifecycle tests for the Listen/Serve split that the desktop shell depends
// on: ephemeral loopback binding, port discovery from the listener, real
// middleware traversal over the wire, and graceful shutdown on context cancel.
//
// Governing: ADR-0017 (desktop shell via Wails v2 wrapping the embedded
// server), SPEC-0010 REQs "Embedded server on a loopback ephemeral port" and
// "Graceful shutdown".
package web

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/store"
)

// testLogger returns a logger that discards output, keeping test output clean.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// startServe runs srv.Serve on ln in a goroutine and returns the error
// channel it reports on.
func startServe(ctx context.Context, srv *Server, ln net.Listener) <-chan error {
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	return done
}

// waitServe asserts Serve returns nil (graceful shutdown) within a deadline.
func waitServe(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v; want nil after graceful shutdown", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after context cancel")
	}
}

// TestServeEphemeralLoopbackLifecycle covers the embedded-server contract end
// to end: Listen("127.0.0.1:0") binds a loopback ephemeral port discoverable
// from the listener, GET / over real TCP traverses the same mux and security
// middleware browser mode uses, and cancelling the context shuts the server
// down and releases the port.
func TestServeEphemeralLoopbackLifecycle(t *testing.T) {
	srv, _, _ := newTestServer(t)

	ln, err := srv.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	addr := ln.Addr().String()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split bound addr %q: %v", addr, err)
	}
	if host != "127.0.0.1" {
		t.Errorf("bound host = %q; want 127.0.0.1", host)
	}
	if port == "0" || port == "" {
		t.Errorf("bound port = %q; want a resolved ephemeral port", port)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := startServe(ctx, srv, ln)

	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d; want 200", resp.StatusCode)
	}
	// The same middleware stack browser mode uses must be on the wire path:
	// strict CSP from securityHeaders, and the app shell in the body.
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP = %q; want the strict policy from securityHeaders", csp)
	}
	if !strings.Contains(string(body), "msgbrowse") {
		t.Error("GET / body does not render the app shell")
	}

	cancel()
	waitServe(t, done)

	// The loopback port must be released after shutdown (SPEC-0010 "Graceful
	// shutdown": no orphaned listener).
	relisten, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("port %s not released after shutdown: %v", port, err)
	}
	relisten.Close()
}

// TestListenEphemeralAvoidsCollision mirrors the SPEC-0010 scenario "No port
// collision with a running serve": with another listener already bound on
// loopback (a stand-in for `msgbrowse serve`), an ephemeral Listen picks its
// own distinct port and both stay usable.
func TestListenEphemeralAvoidsCollision(t *testing.T) {
	srv, _, _ := newTestServer(t)

	other, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen (stand-in serve): %v", err)
	}
	defer other.Close()

	ln, err := srv.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	if ln.Addr().String() == other.Addr().String() {
		t.Fatalf("ephemeral Listen reused the occupied address %s", other.Addr())
	}
}

// gateStore wraps the fixture store and blocks ListConversations until
// released, so a request to / can be held in flight across a shutdown.
type gateStore struct {
	Store
	entered chan struct{} // closed when a request reaches the store
	release chan struct{} // closed by the test to let the request finish
}

func (g *gateStore) ListConversations(ctx context.Context) ([]store.ConversationSummary, error) {
	close(g.entered)
	<-g.release
	return g.Store.ListConversations(ctx)
}

// TestServeDrainsInFlightRequests proves the graceful-shutdown contract: a
// request already being handled when the context is cancelled completes with
// a full response before Serve returns (http.Server.Shutdown drain), per
// SPEC-0010 "Graceful shutdown".
func TestServeDrainsInFlightRequests(t *testing.T) {
	st, cfg, _ := newTestStoreAndConfig(t)
	gs := &gateStore{Store: st, entered: make(chan struct{}), release: make(chan struct{})}
	srv, err := NewServer(gs, cfg, testLogger())
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	ln, err := srv.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := startServe(ctx, srv, ln)

	type result struct {
		status int
		body   string
		err    error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String() + "/")
		if err != nil {
			got <- result{err: err}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		got <- result{status: resp.StatusCode, body: string(b)}
	}()

	// Wait until the request is inside the handler, then start the shutdown
	// while it is still in flight.
	select {
	case <-gs.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("request never reached the store")
	}
	cancel()

	// The server must wait for the in-flight request: it cannot have finished
	// serving yet because the handler is still blocked.
	select {
	case err := <-done:
		t.Fatalf("Serve returned (%v) while a request was still in flight", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(gs.release)
	r := <-got
	if r.err != nil {
		t.Fatalf("in-flight request failed during shutdown: %v", r.err)
	}
	if r.status != http.StatusOK || !strings.Contains(r.body, "msgbrowse") {
		t.Fatalf("in-flight request: status %d, body rendered = %v; want a full 200", r.status, strings.Contains(r.body, "msgbrowse"))
	}
	waitServe(t, done)
}
