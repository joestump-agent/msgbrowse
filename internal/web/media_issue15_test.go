package web

import (
	"context"
	"html"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
)

// newMediaFixtureServerKind builds a server whose WhatsApp archive root is a
// temp dir seeded with the given files (rel → bytes), plus one conversation
// whose messages reference the union of onDisk and dbOnly rel paths as
// attachments of the given kind. dbOnly entries get DB rows but no file — the
// issue #15/#4 "missing source file" shape.
func newMediaFixtureServerKind(t *testing.T, kind signal.AttachmentKind, onDisk map[string][]byte, dbOnly []string) (*Server, int64) {
	t.Helper()
	st, cfg, _ := newTestStoreAndConfig(t)

	waRoot := t.TempDir()
	rels := make([]string, 0, len(onDisk)+len(dbOnly))
	for rel, blob := range onDisk {
		abs := filepath.Join(waRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, blob, 0o644); err != nil {
			t.Fatal(err)
		}
		rels = append(rels, rel)
	}
	rels = append(rels, dbOnly...)

	ctx := context.Background()
	convID, err := st.UpsertConversation(ctx, source.WhatsApp, "Media Fixture")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	msgs := make([]signal.Message, 0, len(rels))
	for i, rel := range rels {
		ts := base.Add(time.Duration(i) * time.Minute)
		msgs = append(msgs, signal.Message{
			Conversation: "Media Fixture", Timestamp: ts, TimestampRaw: ts.Format(signal.TimestampLayout),
			Sender: "Ada", Body: "att",
			Attachments: []signal.Attachment{
				{Kind: kind, RelPath: rel, OriginalName: filepath.Base(rel)},
			},
		})
	}
	if _, err := st.ReplaceConversationMessages(ctx, convID, source.WhatsApp, msgs); err != nil {
		t.Fatal(err)
	}

	cfg.WhatsAppArchiveRoot = waRoot
	srv, err := NewServer(st, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return srv, convID
}

// newMediaFixtureServer is newMediaFixtureServerKind for image attachments.
func newMediaFixtureServer(t *testing.T, onDisk map[string][]byte, dbOnly []string) (*Server, int64) {
	t.Helper()
	return newMediaFixtureServerKind(t, signal.KindImage, onDisk, dbOnly)
}

// imgSrcRE pulls the src attribute out of every rendered <img> tag.
var imgSrcRE = regexp.MustCompile(`<img[^>]*\bsrc="([^"]+)"`)

// mediaImgSrcs returns the /media/... srcs of all <img> tags in body,
// html/template attribute escaping undone exactly as a browser would
// (html.UnescapeString handles &amp;, &#43;, … — not just &amp;).
func mediaImgSrcs(body string) []string {
	var out []string
	for _, m := range imgSrcRE.FindAllStringSubmatch(body, -1) {
		src := html.UnescapeString(m[1])
		if strings.HasPrefix(src, "/media/") {
			out = append(out, src)
		}
	}
	return out
}

// TestGalleryMissingImagePlaceholder is the issue #15 regression at the
// gallery layer: an attachment row whose file is absent from the archive must
// render msgbrowse's own labeled placeholder — never an <img> whose src will
// 404 into the browser's broken-image glyph, and never a download link that
// would save an error page. Present images on the same page keep rendering.
// Runs against BOTH the full gallery page and the /gallery/items
// infinite-scroll fragment, since both go through gallery_images_page.
func TestGalleryMissingImagePlaceholder(t *testing.T) {
	srv, convID := newMediaFixtureServer(t,
		map[string][]byte{"Media/real.jpg": []byte("\xff\xd8\xff\xdbfake-jpeg")},
		[]string{"Media/ghost.jpg"},
	)
	id := itoa(convID)

	for _, path := range []string{
		"/gallery?tab=images&conversation=" + id,
		"/gallery/items?tab=images&conversation=" + id,
	} {
		rec := get(t, srv, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		body := rec.Body.String()

		// The present image still renders as a real tile.
		srcs := mediaImgSrcs(body)
		foundReal := false
		for _, src := range srcs {
			if strings.Contains(src, "real.jpg") {
				foundReal = true
			}
			if strings.Contains(src, "ghost.jpg") {
				t.Errorf("GET %s: missing file rendered as <img src=%q> (would 404 → broken glyph)", path, src)
			}
		}
		if !foundReal {
			t.Errorf("GET %s: present image lost its <img> tile", path)
		}

		// The absent one renders the labeled, inert placeholder.
		if !contains(body, "media-tile-missing") || !contains(body, `<span class="ph-tag">missing</span>`) {
			t.Errorf("GET %s: no labeled missing-placeholder rendered", path)
		}
		if regexp.MustCompile(`<a[^>]*ghost\.jpg`).MatchString(body) {
			t.Errorf("GET %s: missing file rendered as a link (a click would fetch a 404)", path)
		}
	}

	// The acceptance-criteria capture: what a broken tile's /media request
	// actually returns is 404 (not 500, not an empty 200).
	rec := get(t, srv, "/media/"+id+"/Media/ghost.jpg")
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing file /media status = %d, want 404", rec.Code)
	}
}

// TestTranscriptMissingImageNoBrokenImg pins the same fix at the transcript
// layer, which branches on imgRenderable: a DB-only image renders the labeled
// attachment chip, not an <img> thumbnail destined to 404.
func TestTranscriptMissingImageNoBrokenImg(t *testing.T) {
	srv, convID := newMediaFixtureServer(t,
		map[string][]byte{"Media/real.jpg": []byte("\xff\xd8\xff\xdbfake-jpeg")},
		[]string{"Media/ghost.jpg"},
	)

	rec := get(t, srv, "/c/"+itoa(convID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, src := range mediaImgSrcs(body) {
		if strings.Contains(src, "ghost.jpg") {
			t.Errorf("transcript rendered missing file as <img src=%q>", src)
		}
	}
	if !regexp.MustCompile(`<a class="attach-chip"[^>]*ghost\.jpg`).MatchString(body) {
		t.Error("transcript missing-image did not fall back to the attachment chip")
	}
	if !contains(body, "real.jpg") {
		t.Error("transcript lost the present image")
	}
}

// TestGalleryFilesMissingInert is the issue #4 web-reproducible remnant: a
// Files-tab row whose file is absent must render as an inert labeled card,
// not a download anchor — a native click on <a download href> that answers
// 404 surfaces as a silently failed download, i.e. "clicking does nothing".
// Present files keep their hx-boost="false" download anchors.
func TestGalleryFilesMissingInert(t *testing.T) {
	srv, convID := newMediaFixtureServerKind(t, signal.KindFile,
		map[string][]byte{"Docs/real.pdf": []byte("%PDF-1.4 fake")},
		[]string{"Docs/ghost.pdf"},
	)
	id := itoa(convID)

	for _, path := range []string{
		"/gallery?tab=files&conversation=" + id,
		"/gallery/items?tab=files&conversation=" + id,
	} {
		rec := get(t, srv, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		body := rec.Body.String()
		if regexp.MustCompile(`<a[^>]*ghost\.pdf`).MatchString(body) {
			t.Errorf("GET %s: missing file rendered as a download anchor", path)
		}
		if !contains(body, "media-file-missing") || !contains(body, `<span class="ph-tag">missing</span>`) {
			t.Errorf("GET %s: missing file card lost its label", path)
		}
		// The present file keeps the real, un-boosted download anchor.
		m := regexp.MustCompile(`<a class="media-file-name"[^>]*>`).FindString(body)
		if m == "" || !strings.Contains(m, `hx-boost="false"`) || !strings.Contains(m, "real.pdf") {
			t.Errorf("GET %s: present file lost its download anchor: %q", path, m)
		}
	}
}

// TestMediaURLEncodingRoundTrip settles issue #15 hypothesis 2: RelPaths with
// spaces, unicode, '#', '%', '&', '+' and subfolders must round-trip
// mediaURL → rendered <img src> → GET → mux {path...} decode → archive file.
// Every tile the gallery emits for an on-disk file must fetch 200 — a
// mediaURL/handleMedia encoding disagreement would surface here as a 404.
func TestMediaURLEncodingRoundTrip(t *testing.T) {
	jpeg := []byte("\xff\xd8\xff\xdbfake-jpeg")
	onDisk := map[string][]byte{
		"Media/pho to #1.jpg":       jpeg, // space + hash
		"Media/фото-ö.png":          jpeg, // unicode
		"Media/100%.jpg":            jpeg, // literal percent
		"Media/sub dir/a+b&c=d.jpg": jpeg, // subfolder + '+', '&', '='
		"Media/semi;colon.jpg":      jpeg, // ';' (path-segment param char)
	}
	srv, convID := newMediaFixtureServer(t, onDisk, nil)

	rec := get(t, srv, "/gallery?tab=images&conversation="+itoa(convID))
	if rec.Code != http.StatusOK {
		t.Fatalf("gallery status = %d", rec.Code)
	}
	srcs := mediaImgSrcs(rec.Body.String())
	// Grid thumbnail + lightbox original per image share one URL; dedupe.
	seen := map[string]bool{}
	for _, src := range srcs {
		seen[src] = true
	}
	if len(seen) != len(onDisk) {
		t.Fatalf("gallery rendered %d distinct /media srcs, want %d (all files exist): %v", len(seen), len(onDisk), seen)
	}
	for src := range seen {
		res := get(t, srv, src)
		if res.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 (encoding round-trip broke)", src, res.Code)
			continue
		}
		if cd := res.Header().Get("Content-Disposition"); cd != "inline" {
			t.Errorf("GET %s: disposition = %q, want inline", src, cd)
		}
	}
}
