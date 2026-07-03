package web

import (
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/joestump/msgbrowse/internal/archivepath"
	"github.com/joestump/msgbrowse/internal/imageconv"
)

// handleMedia serves an attachment from a conversation's folder in the read-only
// archive. The conversation is keyed by id; the request path is the attachment's
// relative path (e.g. "media/cabin.jpg"). Path traversal is prevented by
// rejecting any cleaned path that escapes the conversation directory.
func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	// Media serving only needs source+name to resolve the file path; the
	// minimal lookup avoids GetConversationByID's aggregation (measured 105ms
	// per image request on the reference archive — SPEC-0008 REQ-0008-005).
	src, convName, err := s.store.ConversationSourceName(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}

	rel := r.PathValue("path")
	full, ok := s.mediaFilePath(src, convName, rel)
	if !ok {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	// Browsers can't render HEIC/HEIF/TIFF. If `msgbrowse media` transcoded a
	// JPEG derivative at import time, serve that inline so the photo displays;
	// otherwise fall through to the original (a download) — the gallery shows a
	// placeholder for these rather than a broken <img>.
	if imageconv.Convertible(full) {
		if d := imageconv.DerivedPath(s.derivedDir, full); d != "" {
			if df, derr := os.Open(d); derr == nil {
				defer df.Close()
				if di, serr := df.Stat(); serr == nil && !di.IsDir() {
					w.Header().Set("Content-Type", "image/jpeg")
					w.Header().Set("Content-Disposition", "inline")
					http.ServeContent(w, r, "image.jpg", di.ModTime(), df)
					return
				}
			}
		}
	}

	f, err := os.Open(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}

	// Inline images; force download for everything else. The CSP already blocks
	// active content, and nosniff prevents type confusion. SVG is deliberately
	// NOT inlined (it can carry script), even though isImageExt returns false
	// for it via the map — the explicit guard below makes the intent obvious to
	// a future reader who might "clean up" the map entry.
	ext := strings.ToLower(filepath.Ext(full))
	switch {
	case ext == ".svg":
		w.Header().Set("Content-Disposition", "attachment; filename=\""+filepath.Base(full)+"\"")
	case isImageExt(full):
		w.Header().Set("Content-Disposition", "inline")
	default:
		w.Header().Set("Content-Disposition", "attachment; filename=\""+filepath.Base(full)+"\"")
	}
	// http.ServeContent sets Content-Type from the extension (and sniffs if
	// unknown) and supports range requests.
	http.ServeContent(w, r, filepath.Base(full), info.ModTime(), f)
}

// mediaFilePath resolves an attachment's relative path to an absolute file path
// under the correct archive for its source, returning ok=false on traversal or
// misconfiguration:
//
//   - signal:   <archiveRoot>/export/<conversation>/<rel> (media is per-conv)
//   - imessage: <imessageArchiveRoot>/<rel> (a flat export; rel is e.g.
//     "attachments/AB/CD/IMG.HEIC" relative to the export root)
//
// Both go through containWithin, which neutralizes ".." and verifies the result
// stays inside the base directory.
func (s *Server) mediaFilePath(src, convName, rel string) (string, bool) {
	return archivepath.Resolve(src, s.archiveRoot, s.imessageArchiveRoot, convName, rel)
}

// containWithin is a thin wrapper over archivepath.Contain (the shared,
// traversal-safe containment used by both the web layer and the transcoder).
func containWithin(base, rel string) (string, bool) {
	return archivepath.Contain(base, rel)
}

// imageExts are the extensions served inline. SVG is intentionally absent: the
// caller handles it with an explicit guard so a future "let's add svg to the
// map" PR cannot accidentally re-enable inline serving of script-capable SVGs.
var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true,
	".webp": true, ".bmp": true,
}

func isImageExt(path string) bool {
	return imageExts[strings.ToLower(filepath.Ext(path))]
}
