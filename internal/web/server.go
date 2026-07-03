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

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/imageconv"
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
}

// Server holds the dependencies shared by all handlers.
type Server struct {
	store               Store
	archiveRoot         string // signal-export archive (export/<conv>/<rel>)
	imessageArchiveRoot string // imessage-exporter archive (<root>/<rel>)
	derivedDir          string // cache of transcoded JPEGs (<data_dir>/derived)
	tmpl                *template.Template
	log                 *slog.Logger
	mux                 http.Handler
	staticTags          map[string]string // embedded-static ETags, keyed by path within static/
}

// NewServer constructs a Server, parsing templates and wiring routes.
func NewServer(st Store, cfg *config.Config, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		store:               st,
		archiveRoot:         cfg.ArchiveRoot,
		imessageArchiveRoot: cfg.IMessageArchiveRoot,
		derivedDir:          imageconv.DerivedDir(cfg.DataDir),
		log:                 log,
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
	srv := &http.Server{
		Handler:           s.mux,
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
