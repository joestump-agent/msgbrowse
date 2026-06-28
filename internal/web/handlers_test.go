package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/ingest"
	"github.com/joestump/msgbrowse/internal/store"
)

// newTestServer ingests the committed fixture archive into a temp store and
// returns a Server wired to it.
func newTestServer(t *testing.T) (*Server, *store.Store, string) {
	t.Helper()
	archive := filepath.Join("..", "..", "testdata", "archive")
	st, err := store.Open(filepath.Join(t.TempDir(), "web.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	_, err = ingest.Run(context.Background(), st, ingest.Options{
		ArchiveRoot: archive,
		Now:         func() time.Time { return time.Date(2026, 6, 27, 12, 0, 0, 0, time.UTC) },
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	cfg := &config.Config{ArchiveRoot: archive, DataDir: t.TempDir()}
	srv, err := NewServer(st, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv, st, archive
}

func get(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestIndexListsConversations(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := get(t, srv, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Harper", "Group Trip", "msgbrowse"} {
		if !contains(body, want) {
			t.Errorf("index missing %q", want)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := get(t, srv, "/")
	if csp := rec.Header().Get("Content-Security-Policy"); !contains(csp, "default-src 'none'") {
		t.Errorf("missing/weak CSP: %q", csp)
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" {
		t.Error("missing X-Frame-Options")
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff")
	}
}

func TestConversationTranscript(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv, err := st.GetConversation(context.Background(), "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}
	rec := get(t, srv, "/c/"+itoa(conv.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "Harper") || !contains(body, "packing now") {
		t.Errorf("transcript missing expected content")
	}

	// Dense-log structure (REQ-0006-005): the chat-bubble vocabulary is gone,
	// replaced by message rows with a mono timestamp gutter, a sender-colored
	// rail, the sender name, and the body.
	if contains(body, "chat-bubble") {
		t.Errorf("chat-bubble markup must not remain in the dense-log transcript")
	}
	for _, want := range []string{
		`class="msg-row`,    // a dense-log message row
		`class="msg-time`,   // the left timestamp gutter
		`09:00:00`,          // gutter shows HH:MM:SS, not the full timestamp
		`class="msg-rail`,   // the sender-colored rail
		`class="msg-sender`, // the sender name above the body
		`class="msg-text`,   // the message body
	} {
		if !contains(body, want) {
			t.Errorf("transcript missing dense-log marker %q", want)
		}
	}

	// "Me" rows carry the accent wash class + light-accent sender name.
	if !contains(body, "msg-row-me") {
		t.Errorf("transcript missing the \"Me\" accent-wash row class")
	}
	if !contains(body, "msg-sender-me") || !contains(body, ">Me<") {
		t.Errorf("transcript does not render the owner's name as \"Me\"")
	}

	// Day separator: the fixture is all one calendar day, so one labeled
	// separator should appear.
	if !contains(body, `class="day-sep"`) || !contains(body, "March 1, 2022") {
		t.Errorf("transcript missing the day separator label")
	}

	// System event (the No-Sender row) renders as a centered sys-event line, not
	// a normal message row.
	if !contains(body, `class="sys-event`) {
		t.Errorf("transcript missing the system-event line")
	}

	// Consecutive same-sender grouping: Harper posts two messages in a row
	// (rows 3 & 4), so the second is grouped (its sender name is suppressed).
	if !contains(body, "msg-row-grouped") {
		t.Errorf("transcript missing consecutive-sender grouping")
	}

	// The image attachment should render as a thumbnail pointing at the media route.
	if !contains(body, "/media/"+itoa(conv.ID)+"/media/cabin.jpg") {
		t.Errorf("transcript missing media thumbnail URL")
	}
	// The PDF attachment renders as a labeled attachment chip.
	if !contains(body, "attach-chip") || !contains(body, "lease.pdf") {
		t.Errorf("transcript missing the attachment chip")
	}
	// Untrusted markdown image syntax must not leak into the HTML.
	if contains(body, "![cabin]") {
		t.Errorf("raw image markdown leaked into output")
	}
}

func TestConversationNotFound(t *testing.T) {
	srv, _, _ := newTestServer(t)
	if rec := get(t, srv, "/c/9999"); rec.Code != http.StatusNotFound {
		t.Errorf("unknown conversation status = %d, want 404", rec.Code)
	}
	if rec := get(t, srv, "/c/abc"); rec.Code != http.StatusNotFound {
		t.Errorf("non-numeric id status = %d, want 404", rec.Code)
	}
}

func TestMediaServingAndTraversal(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv, _ := st.GetConversation(context.Background(), "Harper")
	id := itoa(conv.ID)

	// Valid image is served inline.
	rec := get(t, srv, "/media/"+id+"/media/cabin.jpg")
	if rec.Code != http.StatusOK {
		t.Fatalf("media status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !contains(ct, "image/jpeg") {
		t.Errorf("content-type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != "inline" {
		t.Errorf("disposition = %q, want inline", cd)
	}

	// Non-existent media -> 404.
	if rec := get(t, srv, "/media/"+id+"/media/nope.jpg"); rec.Code != http.StatusNotFound {
		t.Errorf("missing media status = %d, want 404", rec.Code)
	}

	// Traversal must not read outside the conversation directory: even if the mux
	// normalizes the path, the file is not /etc/passwd.
	rec = get(t, srv, "/media/"+id+"/media/%2e%2e%2f%2e%2e%2f%2e%2e%2fetc%2fpasswd")
	if rec.Code == http.StatusOK && contains(rec.Body.String(), "root:") {
		t.Errorf("path traversal succeeded")
	}
}

func TestStatusPage(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := get(t, srv, "/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status page = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"daily", "monthly", "yearly", "never opens or decrypts"} {
		if !contains(body, want) {
			t.Errorf("status page missing %q", want)
		}
	}
}

// helpers

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
