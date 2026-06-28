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
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
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

// Server holds the dependencies shared by all handlers.
type Server struct {
	store               *store.Store
	archiveRoot         string // signal-export archive (export/<conv>/<rel>)
	imessageArchiveRoot string // imessage-exporter archive (<root>/<rel>)
	derivedDir          string // cache of transcoded JPEGs (<data_dir>/derived)
	tmpl                *template.Template
	log                 *slog.Logger
	mux                 http.Handler
}

// NewServer constructs a Server, parsing templates and wiring routes.
func NewServer(st *store.Store, cfg *config.Config, log *slog.Logger) (*Server, error) {
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
		"initials":         initials,
		"avatarColor":      avatarColor,
		"dateKey":          dateKey,
		"clockTime":        clockTime,
		"dateLabel":        dateLabel,
		"sourceSlug":       sourceSlug,
		"humanSource":      source.Label,
		"imgRenderable":    s.imgRenderable,
	}).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	s.tmpl = tmpl
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

// routes builds the mux and wraps it with the security-headers middleware.
func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	staticSub, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheStatic(http.FileServer(http.FS(staticSub)))))

	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("GET /search", s.handleSearch)
	mux.HandleFunc("GET /search/results", s.handleSearchResults)
	mux.HandleFunc("GET /gallery", s.handleGallery)
	mux.HandleFunc("GET /c/{id}", s.handleConversation)
	mux.HandleFunc("GET /c/{id}/messages", s.handleMessages)
	mux.HandleFunc("GET /c/{id}/at/{mid}", s.handleConversationAt)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /media/{id}/{path...}", s.handleMedia)

	return securityHeaders(mux)
}

// Run starts the HTTP server on addr and blocks until ctx is cancelled, then
// shuts down gracefully. addr should normally be loopback (127.0.0.1:8787).
func (s *Server) Run(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           s.mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	if !isLoopback(addr) {
		s.log.Warn("listening on a non-loopback address; the UI has no authentication", "addr", addr)
	}
	s.log.Info("web UI listening", "addr", "http://"+addr)

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
