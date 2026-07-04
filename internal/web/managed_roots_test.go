package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/ingest"
	"github.com/joestump/msgbrowse/internal/store"
)

// newManagedRootServer builds the DESKTOP shape of the world (issue #160): a
// data dir whose managed signal root (<data_dir>/archives/signal) holds a real
// exported archive, a store the archive was imported into, and a config with
// NO cfg archive roots at all. Returns the server, the store, and the managed
// root path.
func newManagedRootServer(t *testing.T) (*Server, *store.Store, string) {
	t.Helper()
	dataDir := t.TempDir()
	managed := filepath.Join(dataDir, "archives", "signal")
	mediaDir := filepath.Join(managed, "export", "Harper", "media")
	if err := os.MkdirAll(mediaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A tiny-but-real JPEG header so ServeContent sniffs image/jpeg.
	if err := os.WriteFile(filepath.Join(mediaDir, "cabin.jpg"),
		[]byte("\xff\xd8\xff\xe0\x00\x10JFIF fixture bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	chat := "[2022-03-01 09:00:00] Harper: here's the place ![cabin](media/cabin.jpg)\n"
	if err := os.WriteFile(filepath.Join(managed, "export", "Harper", "chat.md"), []byte(chat), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "web.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := ingest.Run(context.Background(), st, ingest.Options{
		ArchiveRoot: managed,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}); err != nil {
		t.Fatalf("ingest managed root: %v", err)
	}

	// The desktop config shape: only a data dir. No archive_root anywhere.
	cfg := &config.Config{DataDir: dataDir}
	srv, err := NewServer(st, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv, st, managed
}

// TestMediaServesFromManagedRoot is the issue-#160 acceptance: with empty cfg
// roots and the archive living in the managed root (the desktop app's world),
// /media resolves and serves the file instead of failing every request.
func TestMediaServesFromManagedRoot(t *testing.T) {
	srv, st, _ := newManagedRootServer(t)
	conv, err := st.GetConversation(context.Background(), "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}

	rec := get(t, srv, "/media/"+itoa(conv.ID)+"/media/cabin.jpg")
	if rec.Code != http.StatusOK {
		t.Fatalf("managed-root media status = %d, want 200 (body: %.120s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !contains(ct, "image/jpeg") {
		t.Errorf("Content-Type = %q, want image/jpeg", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != "inline" {
		t.Errorf("Content-Disposition = %q, want inline", cd)
	}

	// Traversal containment still holds against the managed root.
	rec = get(t, srv, "/media/"+itoa(conv.ID)+"/media/%2e%2e%2f%2e%2e%2f%2e%2e%2fetc%2fpasswd")
	if rec.Code == http.StatusOK {
		t.Fatalf("traversal against the managed root served a file")
	}
}

// TestMediaResolvesManagedRootCreatedAfterBoot: the managed root may not exist
// when the server constructs (the FIRST in-session Enable creates it), so root
// resolution must be live per request — media works without a relaunch
// (issue #160).
func TestMediaResolvesManagedRootCreatedAfterBoot(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "web.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// Boot the server BEFORE any managed root exists.
	srv, err := NewServer(st, &config.Config{DataDir: dataDir}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	// Now "Enable" signal: the export/import pipeline creates the managed root
	// and fills the store.
	managed := filepath.Join(dataDir, "archives", "signal")
	mediaDir := filepath.Join(managed, "export", "Harper", "media")
	if err := os.MkdirAll(mediaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mediaDir, "cabin.jpg"),
		[]byte("\xff\xd8\xff\xe0\x00\x10JFIF fixture bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	chat := "[2022-03-01 09:00:00] Harper: pic ![cabin](media/cabin.jpg)\n"
	if err := os.WriteFile(filepath.Join(managed, "export", "Harper", "chat.md"), []byte(chat), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.Run(context.Background(), st, ingest.Options{
		ArchiveRoot: managed,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	conv, err := st.GetConversation(context.Background(), "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}
	if rec := get(t, srv, "/media/"+itoa(conv.ID)+"/media/cabin.jpg"); rec.Code != http.StatusOK {
		t.Fatalf("post-Enable media status = %d, want 200 without a relaunch", rec.Code)
	}
}

// TestManagedRootDoesNotFakeEnabled: the managed roots are provisioned as
// EMPTY dirs on first desktop launch, so their mere existence must not flip a
// Providers card to Enabled (the Enabled signal is store-presence or an
// explicit cfg root).
func TestManagedRootDoesNotFakeEnabled(t *testing.T) {
	dataDir := t.TempDir()
	// Provision all three managed roots empty — the first-launch state.
	for _, src := range []string{"signal", "imessage", "whatsapp"} {
		if err := os.MkdirAll(filepath.Join(dataDir, "archives", src), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "web.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv, err := NewServer(st, &config.Config{DataDir: dataDir}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	body := get(t, srv, "/providers").Body.String()
	if contains(body, "setup-badge-enabled") {
		t.Error("empty provisioned managed roots must not render an Enabled card")
	}
}
