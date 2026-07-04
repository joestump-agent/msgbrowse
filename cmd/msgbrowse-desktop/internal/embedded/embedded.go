// Package embedded wires the real internal/web server into the desktop
// shell: it resolves the msgbrowse configuration, opens the store, binds
// 127.0.0.1 on an ephemeral port, and serves the exact handler stack browser
// mode serves — zero handler divergence, discovered-port URL for the webview.
//
// It also mounts the MCP streamable-HTTP handler at MCPPath on that same
// listener, giving the desktop app a live MCP endpoint for the menubar's
// status line and Copy actions without adding a second listener — SPEC-0010's
// bind-surface requirement pins the shell to exactly one loopback socket.
//
// The package is pure Go (no Wails import, no cgo) so the embedded-server
// wiring is unit-testable on headless machines with CGO_ENABLED=0, keeping
// the cgo surface confined to the Wails entrypoint next door.
//
// Governing: ADR-0017 (desktop shell via Wails v2 wrapping the embedded
// server), SPEC-0010 REQ "Embedded server on a loopback ephemeral port",
// REQ "Menubar quick menu" (MCP status line + config copy), REQ "Graceful
// shutdown", and the SPEC-0010 security requirement "Bind surface" (loopback
// ephemeral bind and nothing else).
package embedded

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/llm"
	"github.com/joestump/msgbrowse/internal/mcp"
	"github.com/joestump/msgbrowse/internal/store"
	"github.com/joestump/msgbrowse/internal/web"
)

// listenAddr is the only address the embedded server ever binds. SPEC-0010's
// "Bind surface" security requirement pins the desktop shell to a loopback
// ephemeral port — the configured listen_addr is deliberately ignored here;
// it belongs to `msgbrowse serve`.
const listenAddr = "127.0.0.1:0"

// MCPPath is where the MCP streamable-HTTP handler is mounted on the embedded
// listener. It is a desktop-mode composition choice: `msgbrowse mcp --http`
// keeps serving MCP at the root of its own dedicated address, while the
// desktop app exposes MCP as a path on the one loopback socket the SPEC-0010
// bind surface allows. The copied endpoint dies with the ephemeral port on
// relaunch — a documented SPEC-0010 trade-off ("Ephemeral URLs"); the
// /settings Connect page (#100) remains the home of durable instructions.
const MCPPath = "/mcp"

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
	// A GUI app launched from Finder/Dock has no meaningful working directory
	// (macOS gives it "/"), so the default relative data_dir "./data" would
	// resolve to "/data" and fail to create — the classic "flashes in the Dock
	// then quits" crash. Anchor any non-absolute data dir under the platform
	// user-config location instead. An explicit absolute data_dir (config/env)
	// is honored unchanged, so pointing the app at an existing CLI data dir
	// still works.
	base, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("resolve user config dir: %w", err)
	}
	cfg.DataDir = resolveDataDir(cfg.DataDir, base)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// resolveDataDir returns an absolute data directory for the desktop app. An
// absolute path is returned unchanged; any relative path (including the
// "./data" default) collapses to <userConfigDir>/msgbrowse — a stable,
// writable location that does not depend on the process working directory.
func resolveDataDir(dir, userConfigDir string) string {
	if filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(userConfigDir, "msgbrowse")
}

// Server is a running embedded web server: the real internal/web handler
// stack plus the MCP streamable-HTTP handler on one loopback ephemeral port,
// and the store they share.
type Server struct {
	// URL is the base URL the webview should load, e.g. "http://127.0.0.1:49152".
	URL string

	// MCPURL is the MCP streamable-HTTP endpoint on the same listener, e.g.
	// "http://127.0.0.1:49152/mcp" — what the menubar status line shows and
	// its activation copies to the clipboard.
	MCPURL string

	store     *store.Store
	done      chan struct{} // closed when the serve loop has exited
	serveErr  error         // set before done is closed
	closeOnce sync.Once
	closeErr  error
}

// Start opens (creating if necessary) the store under cfg.DataDir, builds the
// web and MCP servers, binds the loopback ephemeral port, and begins serving
// both from it. The server runs until ctx is cancelled; cancellation drains
// in-flight requests via the same web.(*Server).ServeHandler shutdown path
// `msgbrowse serve` uses. Callers must cancel ctx and then call Close to
// release the store.
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

	// The MCP server shares the store and mirrors `msgbrowse mcp`'s LLM
	// wiring (internal/cli.newLLMClient): with llm unconfigured, keyword
	// tools work and semantic tools return errors — identical degradation.
	mcpSrv := mcp.NewServer(st, newLLMClient(cfg), mcp.Options{
		EmbedModel: cfg.LLM.EmbedModel,
		Logger:     log,
	})

	// One listener, two handlers: exact-path MCPPath goes to the MCP handler
	// (outside the web middleware — gzip buffering would break its SSE
	// streams, and its deadlines are cleared below for the same reason);
	// everything else flows through the untouched internal/web stack.
	root := http.NewServeMux()
	root.Handle(MCPPath, streamable(mcpSrv.HTTPHandler()))
	root.Handle("/", srv.Handler())

	ln, err := srv.Listen(listenAddr)
	if err != nil {
		_ = st.Close()
		return nil, err
	}

	base := "http://" + ln.Addr().String()
	e := &Server{
		URL:    base,
		MCPURL: base + MCPPath,
		store:  st,
		done:   make(chan struct{}),
	}
	go func() {
		e.serveErr = srv.ServeHandler(ctx, ln, root)
		close(e.done)
	}()
	return e, nil
}

// newLLMClient mirrors internal/cli's client construction so the embedded MCP
// server behaves exactly like `msgbrowse mcp` against the same config.
func newLLMClient(cfg *config.Config) llm.Client {
	return llm.New(llm.Options{
		BaseURL:    cfg.LLM.BaseURL,
		APIKey:     cfg.LLM.APIKey,
		ChatModel:  cfg.LLM.ChatModel,
		EmbedModel: cfg.LLM.EmbedModel,
		Timeout:    cfg.LLM.Timeout,
	})
}

// streamable adapts a long-lived streaming handler (MCP's streamable HTTP,
// which holds SSE responses open indefinitely) to the embedded http.Server's
// web-oriented read/write timeouts by clearing the per-connection deadlines
// for these requests only; the web routes keep their timeouts. It also adds
// nosniff, matching the hygiene the web middleware applies elsewhere.
func streamable(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		_ = rc.SetReadDeadline(time.Time{})
		_ = rc.SetWriteDeadline(time.Time{})
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// Done is closed once the serve loop has exited (after ctx cancellation or a
// serve failure). The shell watches it so an embedded server that dies cannot
// leave a live window pointing at nothing — and, inverted, so the process
// never outlives its window with the server still running headless.
func (e *Server) Done() <-chan struct{} { return e.done }

// Healthy reports whether the embedded server is up and answering: a
// lightweight in-process ping of the web /status route (as an HTMX partial,
// which skips the full sidebar listing) over the real loopback socket, so it
// exercises listener, mux, middleware, and store. The menubar's MCP status
// line polls it to decide between "running" and "degraded" (SPEC-0010
// "Status accuracy").
func (e *Server) Healthy(ctx context.Context) bool {
	select {
	case <-e.done:
		return false // serve loop already exited
	default:
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.URL+"/status", nil)
	if err != nil {
		return false
	}
	req.Header.Set("HX-Request", "true")
	resp, err := healthClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// healthClient bounds the health ping so a wedged server reads as degraded
// instead of hanging the menubar refresh loop.
var healthClient = &http.Client{Timeout: 2 * time.Second}

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
