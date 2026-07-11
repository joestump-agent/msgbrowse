package web

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/joestump/msgbrowse/internal/imageconv"
	"github.com/joestump/msgbrowse/internal/source"
)

// TestImgTileState covers the gallery/transcript placeholder decision per
// issue #15's hypotheses:
//
//   - hypothesis 1 (missing source file): a web-native format whose file is
//     NOT on disk classifies "missing" — previously imgRenderable returned
//     true by extension alone, the grid emitted an <img>, and /media 404'd
//     into the browser's broken-image glyph. This is the root-cause fix.
//   - hypothesis 3 (transcode gap): a convertible format (HEIC) is "img" only
//     once its derived JPEG actually exists on disk (stat, not path
//     expectation); with the original present but no derivative it is
//     "nopreview", and with neither it is "missing".
//   - unresolvable paths (source root not configured) classify "missing"
//     rather than rendering an <img> whose src would 400.
//
// The HEIC cases run against a temp WhatsApp root (flat layout, so no
// conversation-dir setup) to avoid writing into the committed fixture archive.
func TestImgTileState(t *testing.T) {
	srv, _, _ := newTestServer(t)

	// Web-native and on disk (fixture file) → img.
	if got := srv.imgTileState(source.Signal, "Harper", "media/cabin.jpg"); got != tileImg {
		t.Errorf("existing jpg state = %q, want %q", got, tileImg)
	}
	if !srv.imgRenderable(source.Signal, "Harper", "media/cabin.jpg") {
		t.Error("existing jpg should be renderable")
	}

	// Hypothesis 1: DB row exists, file absent → missing, NOT renderable.
	if got := srv.imgTileState(source.Signal, "Harper", "media/ghost.jpg"); got != tileMissing {
		t.Errorf("missing jpg state = %q, want %q", got, tileMissing)
	}
	if srv.imgRenderable(source.Signal, "Harper", "media/ghost.jpg") {
		t.Error("jpg with no file on disk must NOT be renderable (issue #15)")
	}

	// Unresolvable: WhatsApp root not configured yet → missing, even though
	// the extension alone reads web-native.
	if got := srv.imgTileState(source.WhatsApp, "Ada", "photo.jpg"); got != tileMissing {
		t.Errorf("unconfigured-root state = %q, want %q", got, tileMissing)
	}

	// HEIC cases on a temp WhatsApp root (roots resolve per call — #160).
	waRoot := t.TempDir()
	srv.rootsCfg.WhatsAppArchiveRoot = waRoot

	// Hypothesis 3: HEIC with no derivative and no original → missing.
	if got := srv.imgTileState(source.WhatsApp, "Ada", "IMG_0001.heic"); got != tileMissing {
		t.Errorf("absent heic state = %q, want %q", got, tileMissing)
	}

	// HEIC original present but not transcoded → download placeholder.
	heicAbs := filepath.Join(waRoot, "IMG_0001.heic")
	if err := os.WriteFile(heicAbs, []byte("heic-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := srv.imgTileState(source.WhatsApp, "Ada", "IMG_0001.heic"); got != tileNoPreview {
		t.Errorf("untranscoded heic state = %q, want %q", got, tileNoPreview)
	}
	if srv.imgRenderable(source.WhatsApp, "Ada", "IMG_0001.heic") {
		t.Error("heic with no derivative should NOT be renderable")
	}

	// Drop a fake derivative at the exact path the server will look for: the
	// stat-based gate flips to img.
	abs, ok := srv.mediaFilePath(source.WhatsApp, "Ada", "IMG_0001.heic")
	if !ok {
		t.Fatal("mediaFilePath failed to resolve")
	}
	d := imageconv.DerivedPath(srv.derivedDir, abs)
	if err := os.MkdirAll(filepath.Dir(d), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d, []byte("\xff\xd8\xff jpeg"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := srv.imgTileState(source.WhatsApp, "Ada", "IMG_0001.heic"); got != tileImg {
		t.Errorf("transcoded heic state = %q, want %q", got, tileImg)
	}
	if !srv.imgRenderable(source.WhatsApp, "Ada", "IMG_0001.heic") {
		t.Error("heic WITH a derivative should be renderable")
	}

	// The derivative also carries a HEIC whose ORIGINAL has since gone
	// missing: handleMedia serves the derivative, so the tile still displays.
	if err := os.Remove(heicAbs); err != nil {
		t.Fatal(err)
	}
	if got := srv.imgTileState(source.WhatsApp, "Ada", "IMG_0001.heic"); got != tileImg {
		t.Errorf("derivative-only heic state = %q, want %q", got, tileImg)
	}
}
