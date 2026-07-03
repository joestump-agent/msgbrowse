// Package embedded wires the real internal/web server into the desktop
// shell: it resolves the msgbrowse configuration, opens the store, binds
// 127.0.0.1 on an ephemeral port, and serves the exact handler stack browser
// mode serves — zero handler divergence, discovered-port URL for the webview.
//
// The package is pure Go (no Wails import, no cgo) so the embedded-server
// wiring is unit-testable on headless machines with CGO_ENABLED=0, keeping
// the cgo surface confined to the Wails entrypoint next door.
//
// Governing: ADR-0017 (desktop shell via Wails v2 wrapping the embedded
// server), SPEC-0010 REQ "Embedded server on a loopback ephemeral port",
// REQ "Graceful shutdown", and the SPEC-0010 security requirement "Bind
// surface" (loopback ephemeral bind and nothing else).
package embedded

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/store"
	"github.com/joestump/msgbrowse/internal/web"
)

// listenAddr is the only address the embedded server ever binds. SPEC-0010's
// "Bind surface" security requirement pins the desktop shell to a loopback
// ephemeral port — the configured listen_addr is deliberately ignored here;
// it belongs to `msgbrowse serve`.
const listenAddr = "127.0.0.1:0"

// LoadConfig resolves the msgbrowse configuration the same way the CLI does:
// defaults, then the YAML config file (explicit path or the standard search
// locations), then MSGBROWSE_* environment variables.
func LoadConfig(cfgFile string) (*config.Config, error) {
	v, err := config.Load(cfgFile)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Unmarshal(v)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Server is a running embedded web server: the real internal/web handler
// stack on a loopback ephemeral port, plus the store it owns.
type Server struct {
	// URL is the base URL the webview should load, e.g. "http://127.0.0.1:49152".
	URL string

	store     *store.Store
	done      chan struct{} // closed when the serve loop has exited
	serveErr  error         // set before done is closed
	closeOnce sync.Once
	closeErr  error
}

// Start opens (creating if necessary) the store under cfg.DataDir, builds the
// web server, binds the loopback ephemeral port, and begins serving. The
// server runs until ctx is cancelled; cancellation drains in-flight requests
// via the same web.(*Server).Serve shutdown path `msgbrowse serve` uses.
// Callers must cancel ctx and then call Close to release the store.
func Start(ctx context.Context, cfg *config.Config, log *slog.Logger) (*Server, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir %q: %w", cfg.DataDir, err)
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, store.DBFileName))
	if err != nil {
		return nil, err
	}

	srv, err := web.NewServer(st, cfg, log)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	ln, err := srv.Listen(listenAddr)
	if err != nil {
		_ = st.Close()
		return nil, err
	}

	e := &Server{
		URL:   "http://" + ln.Addr().String(),
		store: st,
		done:  make(chan struct{}),
	}
	go func() {
		e.serveErr = srv.Serve(ctx, ln)
		close(e.done)
	}()
	return e, nil
}

// Done is closed once the serve loop has exited (after ctx cancellation or a
// serve failure). The shell watches it so an embedded server that dies cannot
// leave a live window pointing at nothing — and, inverted, so the process
// never outlives its window with the server still running headless.
func (e *Server) Done() <-chan struct{} { return e.done }

// Close completes the SPEC-0010 shutdown sequence: it waits for the serve
// loop to finish draining (the caller must already have cancelled the Start
// context, or Close blocks until it is) and then closes the store. Safe to
// call more than once.
func (e *Server) Close() error {
	e.closeOnce.Do(func() {
		<-e.done
		err := e.store.Close()
		if e.serveErr != nil {
			e.closeErr = e.serveErr
		} else {
			e.closeErr = err
		}
	})
	return e.closeErr
}
