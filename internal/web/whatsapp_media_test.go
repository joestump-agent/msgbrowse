package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
)

// waMediaRel mirrors the layout the WhatsApp parser stores: RelPaths relative
// to the whatsapp-chat-exporter output root (SPEC-0009 REQ-0009-006).
const waMediaRel = "Message/Media/15550001111@s.whatsapp.net"

// newWhatsAppTestServer extends the fixture server with a WhatsApp archive
// root: a temp exporter-output directory holding a jpeg, a webp sticker, and
// an opus voice note, plus one imported WhatsApp conversation referencing
// them root-relative.
func newWhatsAppTestServer(t *testing.T) (*Server, int64) {
	t.Helper()
	st, cfg, _ := newTestStoreAndConfig(t)

	waRoot := t.TempDir()
	mediaDir := filepath.Join(waRoot, filepath.FromSlash(waMediaRel))
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, blob := range map[string][]byte{
		"photo.jpg":    []byte("\xff\xd8\xff\xdbfake-jpeg"),
		"sticker.webp": []byte("RIFF0000WEBPfake"),
		"note.opus":    []byte("OggSfake-opus"),
	} {
		if err := os.WriteFile(filepath.Join(mediaDir, name), blob, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ctx := context.Background()
	convID, err := st.UpsertConversation(ctx, source.WhatsApp, "Ada Fixture")
	if err != nil {
		t.Fatal(err)
	}
	ts, _ := time.Parse(signal.TimestampLayout, "2026-07-01 10:00:00")
	msgs := []signal.Message{
		{
			Conversation: "Ada Fixture", Timestamp: ts, TimestampRaw: "2026-07-01 10:00:00",
			Sender: "Ada Fixture", Body: "media drop",
			Attachments: []signal.Attachment{
				{Kind: signal.KindImage, RelPath: waMediaRel + "/photo.jpg", OriginalName: "photo.jpg"},
				{Kind: signal.KindImage, RelPath: waMediaRel + "/sticker.webp", OriginalName: "sticker.webp"},
				{Kind: signal.KindFile, RelPath: waMediaRel + "/note.opus", OriginalName: "note.opus"},
			},
		},
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

// TestWhatsAppMediaServing covers REQ-0009-006's happy paths under the new
// root: web-native formats (jpeg, webp stickers) serve inline; voice notes
// (opus) come back as a download, matching the transcript's file-chip
// rendering.
func TestWhatsAppMediaServing(t *testing.T) {
	srv, convID := newWhatsAppTestServer(t)
	id := itoa(convID)

	rec := get(t, srv, "/media/"+id+"/"+waMediaRel+"/photo.jpg")
	if rec.Code != http.StatusOK {
		t.Fatalf("jpeg status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !contains(ct, "image/jpeg") {
		t.Errorf("jpeg content-type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != "inline" {
		t.Errorf("jpeg disposition = %q, want inline", cd)
	}

	// Stickers are webp — web-native, rendered as images (REQ-0009-006).
	rec = get(t, srv, "/media/"+id+"/"+waMediaRel+"/sticker.webp")
	if rec.Code != http.StatusOK {
		t.Fatalf("webp status = %d, want 200", rec.Code)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != "inline" {
		t.Errorf("webp disposition = %q, want inline", cd)
	}

	// Voice notes are opus — served as a download to match the file chip.
	rec = get(t, srv, "/media/"+id+"/"+waMediaRel+"/note.opus")
	if rec.Code != http.StatusOK {
		t.Fatalf("opus status = %d, want 200", rec.Code)
	}
	if cd := rec.Header().Get("Content-Disposition"); !contains(cd, "attachment") {
		t.Errorf("opus disposition = %q, want attachment", cd)
	}

	// Missing file under the root is a plain 404.
	if rec := get(t, srv, "/media/"+id+"/"+waMediaRel+"/nope.jpg"); rec.Code != http.StatusNotFound {
		t.Errorf("missing media status = %d, want 404", rec.Code)
	}
}

// TestWhatsAppMediaTraversal proves the whatsapp branch of archivepath.Resolve
// carries the same traversal containment as the other sources: a crafted `../`
// path never reads outside the WhatsApp root (REQ-0009-006 scenario).
func TestWhatsAppMediaTraversal(t *testing.T) {
	srv, convID := newWhatsAppTestServer(t)
	id := itoa(convID)

	// Even if the mux normalizes the encoded path, the response must never be
	// a file from outside the whatsapp root.
	rec := get(t, srv, "/media/"+id+"/%2e%2e%2f%2e%2e%2f%2e%2e%2fetc%2fpasswd")
	if rec.Code == http.StatusOK && contains(rec.Body.String(), "root:") {
		t.Errorf("path traversal escaped the whatsapp root")
	}
	rec = get(t, srv, "/media/"+id+"/Message/%2e%2e/%2e%2e/%2e%2e/%2e%2e/etc/passwd")
	if rec.Code == http.StatusOK && contains(rec.Body.String(), "root:") {
		t.Errorf("nested path traversal escaped the whatsapp root")
	}
}

// TestWhatsAppMediaUnsetRoot proves fail-closed resolution: with no
// whatsapp_archive_root configured, a WhatsApp media request is rejected as
// invalid rather than falling back to another source's archive.
func TestWhatsAppMediaUnsetRoot(t *testing.T) {
	srv, convID := newWhatsAppTestServer(t)
	// Simulate an unset root on the same store: clear the resolution inputs
	// (roots are resolved per request from rootsCfg — issue #160).
	srv.rootsCfg.WhatsAppArchiveRoot = ""
	srv.rootsCfg.DataDir = t.TempDir() // no managed root either
	rec := get(t, srv, "/media/"+itoa(convID)+"/"+waMediaRel+"/photo.jpg")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unset whatsapp root status = %d, want 400", rec.Code)
	}
}

// TestWhatsAppSourceStyling covers the REQ-0009-007 surface: the conversation
// header and sidebar row carry the src-whatsapp presence-dot modifier chosen
// by sourceSlug, with the human label as its tooltip.
func TestWhatsAppSourceStyling(t *testing.T) {
	srv, convID := newWhatsAppTestServer(t)
	body := get(t, srv, "/c/"+itoa(convID)).Body.String()
	if !contains(body, "presence-dot src-whatsapp") {
		t.Error("conversation page missing the src-whatsapp presence dot")
	}
	if !contains(body, `title="WhatsApp"`) {
		t.Error("presence dot missing the WhatsApp source tooltip")
	}
}
