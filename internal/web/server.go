// Package web implements msgbrowse's server-rendered HTMX user interface.
//
// It is intentionally minimal: net/http with Go 1.22 pattern routing,
// html/template for rendering (which auto-escapes all message content), HTMX for
// partial updates, and a small amount of hand-written CSS. There is no SPA and no
// build step. The server binds to loopback by default and sets a strict
// Content-Security-Policy; message bodies are untrusted and always escaped.
package web

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/joestump/msgbrowse/internal/archivepath"
	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/imageconv"
	"github.com/joestump/msgbrowse/internal/setup"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Store is the subset of *store.Store the web layer depends on. It is an
// interface so tests can wrap the real store — e.g. to prove the HTMX
// partial-render path never runs the sidebar listing (SPEC-0008 REQ-0008-006)
// — without a second storage implementation.
type Store interface {
	ListConversations(ctx context.Context) ([]store.ConversationSummary, error)
	ConversationRefs(ctx context.Context) ([]store.ConversationRef, error)
	ArchiveStats(ctx context.Context) (store.ArchiveStats, error)
	NewestMessageTS(ctx context.Context) (string, error)
	GetConversationByID(ctx context.Context, id int64) (*store.ConversationSummary, error)
	ConversationSourceName(ctx context.Context, id int64) (source, name string, err error)
	GetMessages(ctx context.Context, convID, cursorTSUnix, cursorID int64, limit int, desc bool) (*store.Page, error)
	GetContext(ctx context.Context, messageID int64, window int) ([]store.MessageView, error)
	MessageConversationID(ctx context.Context, messageID int64) (int64, bool, error)
	TogglePinned(ctx context.Context, convID int64) (bool, error)
	SearchMessages(ctx context.Context, opts store.SearchOptions) ([]store.SearchHit, error)
	CountMedia(ctx context.Context, f store.GalleryFilter) (store.MediaCounts, error)
	ListAttachments(ctx context.Context, kind string, f store.GalleryFilter, cursorTSUnix, cursorID int64) (*store.MediaPage, error)
	ListLinks(ctx context.Context, f store.GalleryFilter, cur store.LinkCursor) (*store.LinkPage, error)
	LatestIngestRun(ctx context.Context) (*store.IngestRun, error)
	ListSnapshots(ctx context.Context) ([]store.Snapshot, error)
	SourcesPresent(ctx context.Context) ([]string, error)
	SourceCounts(ctx context.Context) (map[string]store.SourceCount, error)
	DeleteSourceData(ctx context.Context, src string) (int64, error)
}

// Server holds the dependencies shared by all handlers.
type Server struct {
	store Store
	// rootsCfg is the minimal config snapshot (archive roots + data dir) that
	// archiveRoots() resolves the per-source EFFECTIVE read-only archive roots
	// from: the configured cfg root when set, else the app-owned managed root
	// (<data_dir>/archives/<source>) when it exists on disk — issue #160 (the
	// desktop app imports into managed roots and sets no cfg root, so building
	// roots from cfg alone broke every /media resolve on desktop). Resolution is
	// per-call, not boot-time: the FIRST in-session Enable creates the managed
	// root after NewServer ran, and media must work without a relaunch.
	rootsCfg config.Config
	// cfgRoots are the EXPLICITLY configured archive roots only (no managed
	// fallback). They back sourceConfigured — the app-owned "Enabled" signal for
	// the Providers cards. The managed roots can exist as empty dirs before any
	// import (staging/adopt creates them), so an existing managed root must NOT
	// read as "Enabled"; store-presence is the desktop Enabled signal.
	cfgRoots   archivepath.Roots
	derivedDir string // cache of transcoded JPEGs (<data_dir>/derived)
	tmpl       *template.Template
	log        *slog.Logger
	mux        http.Handler
	staticTags map[string]string // embedded-static ETags, keyed by path within static/

	// deviceSyncEnabled mirrors config device_sync.enabled for the /settings
	// pairing section's absent state (SPEC-0010; payload contract SPEC-0011).
	deviceSyncEnabled bool
	// pairing is the live pairing-window source for /settings' QR section;
	// nil until the device-sync listener story wires SetPairingSource.
	pairing PairingSource
	// setupDetector overrides the /setup source detector (SPEC-0013); nil uses a
	// real HOME-rooted setup.NewDetector(). Tests inject a faked HOME; the desktop
	// layer (#134) injects the genuine macOS Keychain check.
	setupDetector *setup.Detector
	// enabler runs the privileged /setup/enable export→import job (SPEC-0013). It
	// is the seam wired by SetEnabler: the desktop shell backs it with the bundled
	// toolchain, `msgbrowse serve` with a $PATH/config resolver. nil disables
	// Enable (the Setup cards render an "unavailable" affordance and the POST
	// reports it) — the web layer never imports the cgo desktop module.
	enabler Enabler
	// setupTokens is the live per-session token set minted at /setup render and
	// verified on the privileged Setup POSTs (SPEC-0013 §Security same-origin +
	// per-session token). Always non-nil after NewServer.
	setupTokens *setupTokens
}

// NewServer constructs a Server, parsing templates and wiring routes.
func NewServer(st Store, cfg *config.Config, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		store: st,
		// The root-resolution inputs (issue #160): a value snapshot so a caller
		// mutating cfg after NewServer cannot skew later resolutions. Effective
		// roots are computed per call via archiveRoots(); explicit cfg roots are
		// kept separately for the Enabled signal.
		rootsCfg: config.Config{
			ArchiveRoot:         cfg.ArchiveRoot,
			IMessageArchiveRoot: cfg.IMessageArchiveRoot,
			WhatsAppArchiveRoot: cfg.WhatsAppArchiveRoot,
			DataDir:             cfg.DataDir,
		},
		cfgRoots: archivepath.Roots{
			Signal:   cfg.ArchiveRoot,
			IMessage: cfg.IMessageArchiveRoot,
			WhatsApp: cfg.WhatsAppArchiveRoot,
		},
		derivedDir:        imageconv.DerivedDir(cfg.DataDir),
		log:               log,
		deviceSyncEnabled: cfg.DeviceSync.Enabled,
		setupTokens:       newSetupTokens(),
	}
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"renderBody":       renderBody,
		"mediaURL":         mediaURL,
		"humanSize":        humanSize,
		"domainOf":         domainOf,
		"highlightSnippet": highlightSnippet,
		"humanName":        humanName,
		"reactionTitle":    reactionTitle,
		"initials":         initials,
		"avatarColor":      avatarColor,
		"dateKey":          dateKey,
		"clockTime":        clockTime,
		"dateLabel":        dateLabel,
		"sourceSlug":       sourceSlug,
		"humanSource":      source.Label,
		"imgRenderable":    s.imgRenderable,
		"convRowCtx":       convRowCtx,
	}).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	s.tmpl = tmpl
	tags, err := staticETags(staticFS)
	if err != nil {
		return nil, fmt.Errorf("compute static etags: %w", err)
	}
	s.staticTags = tags
	s.mux = s.routes()
	return s, nil
}

// archiveRoots resolves the effective per-source archive roots for this
// request: the configured cfg root when set, else the managed root iff it
// exists on disk (setup.EffectiveRoots — issue #160). Resolution is per-call
// rather than cached at construction because the FIRST in-session Enable
// creates the managed root AFTER NewServer ran — media must resolve without a
// relaunch. Cost: at most three os.Stat calls (zero for a source with a cfg
// root), microseconds against the SPEC-0008 millisecond budgets.
func (s *Server) archiveRoots() archivepath.Roots {
	return setup.EffectiveRoots(&s.rootsCfg)
}

// imgRenderable reports whether an image attachment will actually display in an
// <img>: either a web-native format, or a non-web format (HEIC/TIFF) that has a
// transcoded JPEG derivative on disk. Templates use it to render a placeholder
// instead of a broken image.
func (s *Server) imgRenderable(src, convName, relPath string) bool {
	if imageconv.WebRenderable(relPath) {
		return true
	}
	if !imageconv.Convertible(relPath) {
		return false
	}
	abs, ok := s.mediaFilePath(src, convName, relPath)
	if !ok {
		return false
	}
	d := imageconv.DerivedPath(s.derivedDir, abs)
	if d == "" {
		return false
	}
	_, err := os.Stat(d)
	return err == nil
}

// Handler returns the root http.Handler (security headers already applied).
func (s *Server) Handler() http.Handler { return s.mux }

// routes builds the mux and wraps it with the middleware stack: gzip outermost
// (SPEC-0008 REQ-0008-007), then the security headers, then the mux. The order
// is safe because securityHeaders only sets response headers before delegating
// — the gzip wrapper still sees every body write and the headers land on the
// same header map either way.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	staticSub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/",
		cacheStatic(etagStatic(s.staticTags, http.FileServer(http.FS(staticSub))))))

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /search", s.handleSearch)
	mux.HandleFunc("GET /search/results", s.handleSearchResults)
	mux.HandleFunc("GET /gallery", s.handleGallery)
	mux.HandleFunc("GET /gallery/items", s.handleGalleryItems)
	mux.HandleFunc("GET /c/{id}", s.handleConversation)
	mux.HandleFunc("POST /c/{id}/pin", s.handlePin)
	mux.HandleFunc("GET /c/{id}/messages", s.handleMessages)
	mux.HandleFunc("GET /c/{id}/at/{mid}", s.handleConversationAt)
	mux.HandleFunc("GET /status", s.handleStatus)
	// The Setup surface is presented to the user as "Providers" (its route is
	// /providers); /setup 301-redirects for compatibility with any existing links
	// or bookmarks. The privileged POSTs keep the /setup/* prefix — they are
	// server-internal endpoints the rendered controls target, not user-facing URLs.
	mux.HandleFunc("GET /providers", s.handleSetup)
	mux.HandleFunc("GET /setup", redirectTo("/providers"))
	// The Logs viewer (issue #151): a safe GET diagnostic surface reached from
	// Settings; no mutation, no token.
	mux.HandleFunc("GET /logs", s.handleLogs)
	// Privileged Setup POSTs (SPEC-0013 §Security): each is gated inside its
	// handler by the same-origin + per-session-token check before any work.
	mux.HandleFunc("POST /setup/enable", s.handleSetupEnable)
	mux.HandleFunc("POST /setup/refresh", s.handleSetupRefresh)
	mux.HandleFunc("POST /setup/refresh-all", s.handleSetupRefreshAll)
	mux.HandleFunc("POST /setup/cancel", s.handleSetupCancel)
	mux.HandleFunc("POST /setup/recheck", s.handleSetupRecheck)
	mux.HandleFunc("POST /setup/disable", s.handleSetupDisable)
	mux.HandleFunc("GET /setup/status/{source}", s.handleSetupStatus)
	mux.HandleFunc("GET /settings", s.handleSettings)
	mux.HandleFunc("GET /media/{id}/{path...}", s.handleMedia)

	return gzipMiddleware(securityHeaders(mux))
}

// Run starts the HTTP server on addr and blocks until ctx is cancelled, then
// shuts down gracefully. addr should normally be loopback (127.0.0.1:8787).
// It is Listen followed by Serve; callers that need the bound address before
// serving — the desktop shell binds 127.0.0.1:0 and reads the ephemeral port
// off the listener (SPEC-0010 "Embedded server on a loopback ephemeral port")
// — call the two halves directly.
func (s *Server) Run(ctx context.Context, addr string) error {
	ln, err := s.Listen(addr)
	if err != nil {
		return err
	}
	return s.Serve(ctx, ln)
}

// Listen opens the TCP listener for addr and logs where the UI is reachable.
// Passing a ":0" port yields an ephemeral port; the caller discovers it from
// the returned listener's Addr.
func (s *Server) Listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	bound := ln.Addr().String()
	if !isLoopback(bound) {
		s.log.Warn("listening on a non-loopback address; the UI has no authentication", "addr", bound)
	}
	s.log.Info("web UI listening", "addr", "http://"+bound)
	return ln, nil
}

// Serve serves HTTP on ln and blocks until ctx is cancelled, then shuts down
// gracefully, draining in-flight requests. This is the single shutdown code
// path shared by `msgbrowse serve` (whose context is cancelled by
// SIGINT/SIGTERM) and the desktop shell (whose context is cancelled when the
// window closes) — SPEC-0010 "Graceful shutdown". Serve closes ln on return.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	return s.ServeHandler(ctx, ln, s.mux)
}

// ServeHandler is Serve with root handler h in place of the server's own mux.
// The desktop shell uses it to mount the MCP streamable-HTTP handler beside
// the web app on the one embedded loopback listener — SPEC-0010's bind
// surface allows no listener beyond the embedded server — while every web
// route still flows through s.Handler() unchanged. Timeouts and the graceful
// shutdown path are identical to Serve's.
func (s *Server) ServeHandler(ctx context.Context, ln net.Listener, h http.Handler) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// securityHeaders applies a strict CSP and related hardening to every response.
// The CSP allows only same-origin scripts/styles/images (plus data: images for
// inline placeholders) and forbids framing — message content cannot load or run
// external resources.
func securityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'none'; " +
		"script-src 'self'; " +
		"style-src 'self'; " +
		"img-src 'self' data:; " +
		"connect-src 'self'; " +
		"font-src 'self'; " +
		"base-uri 'none'; " +
		"form-action 'self'; " +
		"frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// cacheStatic adds a modest cache lifetime to embedded static assets.
func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

// staticETags computes a strong, content-derived ETag for every embedded
// static asset at startup (SPEC-0008 REQ-0008-008). Embedded files have zero
// modtimes, so http.FileServer can never revalidate by time — a sha256 prefix
// of the bytes gives clients a stable validator instead. Keys are the paths as
// requested under /static/ (e.g. "app.css").
func staticETags(fsys fs.FS) (map[string]string, error) {
	tags := make(map[string]string)
	err := fs.WalkDir(fsys, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, rerr := fs.ReadFile(fsys, p)
		if rerr != nil {
			return rerr
		}
		sum := sha256.Sum256(b)
		tags[strings.TrimPrefix(p, "static/")] = `"` + hex.EncodeToString(sum[:16]) + `"`
		return nil
	})
	if err != nil {
		return nil, err
	}
	return tags, nil
}

// etagStatic serves 304 Not Modified for embedded statics the client already
// holds (If-None-Match against the startup-computed ETag) and stamps the ETag
// on full responses so future revisits can revalidate (REQ-0008-008). Unknown
// paths fall through untouched — the file server 404s as before.
func etagStatic(tags map[string]string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// StripPrefix left a path like "app.css"; normalize defensively.
		p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if tag, ok := tags[p]; ok {
			w.Header().Set("ETag", tag)
			if etagMatch(r.Header.Get("If-None-Match"), tag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// etagMatch reports whether an If-None-Match header value matches the given
// entity tag. GET revalidation uses weak comparison, so a W/ prefix on the
// client's tag is ignored; "*" matches anything.
func etagMatch(header, tag string) bool {
	if header == "" {
		return false
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		if candidate == "*" || candidate == tag {
			return true
		}
	}
	return false
}

// redirectTo returns a handler that permanently redirects to target. It backs the
// /setup → /providers compatibility redirect (the page was renamed "Providers"):
// 301 so browsers and htmx boosted navigations both follow it transparently and
// caches learn the canonical URL. target is a fixed app-owned path, never
// request-derived.
func redirectTo(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	}
}

// isLoopback reports whether addr's host is a loopback address.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	if host == "localhost" || host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
