package web

import (
	"bytes"
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

// newTestStoreAndConfig ingests the committed fixture archive into a temp
// store and returns it with a matching config (shared by newTestServer and the
// counting-store variant in partial_test.go).
func newTestStoreAndConfig(t *testing.T) (*store.Store, *config.Config, string) {
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
	return st, cfg, archive
}

// newTestServer ingests the committed fixture archive into a temp store and
// returns a Server wired to it.
func newTestServer(t *testing.T) (*Server, *store.Store, string) {
	t.Helper()
	st, cfg, archive := newTestStoreAndConfig(t)
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

// post issues a bodyless form POST (used by the pin toggle) and returns the
// recorder without following the redirect.
func post(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
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

// TestHomeStatStrip checks the slate Home redesign (REQ-0006-007): the hero
// wordmark, the 3-cell stat strip with mono tabular values, and the bordered
// quick-link cards all render.
func TestHomeStatStrip(t *testing.T) {
	srv, _, _ := newTestServer(t)
	body := get(t, srv, "/").Body.String()
	for _, want := range []string{
		"home-hero-title", // hero wordmark
		"stat-strip",      // 3-cell stat strip container
		"stat-cell-value", // mono tabular stat value
		"Newest message",  // the third stat cell label
		"link-card",       // bordered quick-link card
	} {
		if !contains(body, want) {
			t.Errorf("home missing slate marker %q", want)
		}
	}
}

// TestBoostedNavigation verifies the HTMX-boosted navigation shell (issue #65):
// the scoped swap target exists, and the shell nav is boosted to swap only
// #main-content and push the URL (so the sidebar is preserved across navigation).
func TestBoostedNavigation(t *testing.T) {
	srv, _, _ := newTestServer(t)
	body := get(t, srv, "/").Body.String()
	for _, want := range []string{
		`id="main-content"`,         // the scoped swap target wraps page content
		`hx-boost="true"`,           // navigation is boosted
		`hx-select="#main-content"`, // only the main region is pulled from the response
		`hx-swap="outerHTML"`,       // the whole main element is replaced
		`hx-push-url="true"`,        // URL + history updated
	} {
		if !contains(body, want) {
			t.Errorf("boosted-nav marker missing: %q", want)
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

	// Day separator: the fixture is all one calendar day, so exactly one labeled
	// separator should appear (a regression here means the per-row day-sep bug
	// from source-formatted timestamps is back).
	if !contains(body, `class="day-sep"`) || !contains(body, "March 1, 2022") {
		t.Errorf("transcript missing the day separator label")
	}
	if n := strings.Count(body, `class="day-sep"`); n != 1 {
		t.Errorf("day separators = %d, want exactly 1 for a single-day fixture", n)
	}

	// Default order is newest-first: the fixture's latest message body renders
	// before its earliest.
	newest := strings.Index(body, "looks great")
	oldest := strings.Index(body, "morning! ready for the trip?")
	if newest < 0 || oldest < 0 || newest > oldest {
		t.Errorf("default transcript not newest-first (newest at %d, oldest at %d)", newest, oldest)
	}

	// The header carries the sort toggle: it links to the legacy ascending order
	// and is not pressed by default.
	if !contains(body, "?sort=asc") || !contains(body, "Oldest first") {
		t.Errorf("transcript missing the sort toggle")
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

	// Reactions (issue #50): Harper's first message carries "(- Me: 👍, MJ: 👍 -)".
	// It renders as a reaction badge with a count of 2 (same emoji, two reactors).
	for _, want := range []string{
		`class="msg-reactions"`, // the reactions row
		`class="reaction-badge"`,
		"👍",                     // the emoji
		`class="reaction-count`, // the repeat count appears (>1)
		`title="Me, MJ"`,        // actor tooltip
	} {
		if !contains(body, want) {
			t.Errorf("transcript missing reaction marker %q", want)
		}
	}
	// The reactions trailer must NOT appear as message body text or a standalone row.
	if contains(body, "(- Me") || contains(body, "-)") {
		t.Errorf("reactions trailer leaked into transcript body as text")
	}
}

// TestConversationSortAscRoundTrip verifies the ?sort=asc legacy view: oldest
// message first, the toggle pressed and linking back to the newest-first
// default.
func TestConversationSortAscRoundTrip(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv, err := st.GetConversation(context.Background(), "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}
	rec := get(t, srv, "/c/"+itoa(conv.ID)+"?sort=asc")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()

	oldest := strings.Index(body, "morning! ready for the trip?")
	newest := strings.Index(body, "looks great")
	if oldest < 0 || newest < 0 || oldest > newest {
		t.Errorf("?sort=asc not oldest-first (oldest at %d, newest at %d)", oldest, newest)
	}
	// The toggle reflects the engaged state and links back to the default.
	if !contains(body, `aria-pressed="true"`) {
		t.Errorf("sort toggle not marked pressed in asc view")
	}
	// Day-sep and grouping stay sane in the chronological walk too.
	if n := strings.Count(body, `class="day-sep"`); n != 1 {
		t.Errorf("asc day separators = %d, want 1", n)
	}
	if !contains(body, "msg-row-grouped") {
		t.Errorf("asc view missing consecutive-sender grouping")
	}
}

// TestMessagesPartialCarriesSortCursor verifies the infinite-scroll partial:
// a descending page continues strictly before the cursor and its load-more URL
// carries the sort plus the direction-correct (before_*) keyset params.
func TestMessagesPartialCarriesSortCursor(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()
	conv, err := st.GetConversation(ctx, "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}
	// First descending page of 2 straight from the store gives us a real cursor.
	first, err := st.GetMessages(ctx, conv.ID, 0, 0, 2, true)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if !first.HasMore {
		t.Fatalf("fixture should have more than 2 messages")
	}

	path := "/c/" + itoa(conv.ID) + "/messages?sort=desc&before_ts=" + itoa(first.NextTSUnix) + "&before_id=" + itoa(first.NextID)
	rec := get(t, srv, path)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	// The page after (09:05, 09:04) descending starts at the 09:03 message and
	// must not repeat the cursor row.
	if !contains(body, "and the agreement") {
		t.Errorf("descending continuation missing the next-older message")
	}
	if contains(body, "looks great") {
		t.Errorf("descending continuation repeated a row from before the cursor")
	}

	// The load-more sentinel carries the direction-correct cursor params. The
	// fixture fits in one page, so render the partial directly with HasMore set.
	sentinel := func(sort string) string {
		var buf bytes.Buffer
		err := srv.tmpl.ExecuteTemplate(&buf, "message_list", messageListData{
			ActiveID: conv.ID, Sort: sort, HasMore: true, NextTSUnix: 123, NextID: 45,
		})
		if err != nil {
			t.Fatalf("render message_list: %v", err)
		}
		return buf.String()
	}
	if got := sentinel(sortDesc); !contains(got, "/messages?sort=desc&before_ts=123&before_id=45") {
		t.Errorf("desc load-more sentinel lost sort/cursor params: %q", got)
	}
	if got := sentinel(sortAsc); !contains(got, "/messages?sort=asc&after_ts=123&after_id=45") {
		t.Errorf("asc load-more sentinel lost sort/cursor params: %q", got)
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
	// Slate re-skin (REQ-0006-011): slate surfaces, the freshness stat strip, the
	// ingest-run metric grid, the snapshot table, and tier pills.
	for _, want := range []string{"status-card", "stat-strip", "status-grid", "status-table", "tier-pill"} {
		if !contains(body, want) {
			t.Errorf("status page missing slate marker %q", want)
		}
	}
}

// TestStatusPageFormatsThousands is the end-to-end guard for issue #178:
// record an ingest run whose metrics cross 999 and assert the Status page
// HTML carries the comma-grouped strings — the full handler → template → num
// pipeline, covering duration, scanned/total/added, skipped lines, and errors.
func TestStatusPageFormatsThousands(t *testing.T) {
	srv, st, _ := newTestServer(t)
	if _, err := st.RecordIngestRun(context.Background(), store.IngestRun{
		Source:               "signal",
		StartedAt:            time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC),
		FinishedAt:           time.Date(2026, 6, 28, 12, 1, 23, 0, time.UTC),
		DurationMS:           83457,
		ConversationsScanned: 1200,
		ConversationsChanged: 1100,
		MessagesTotal:        1234567,
		MessagesAdded:        4321,
		SkippedLines:         1024,
		Errors:               2048,
	}); err != nil {
		t.Fatalf("record ingest run: %v", err)
	}
	rec := get(t, srv, "/status")
	if rec.Code != http.StatusOK {
		t.Fatalf("status page = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"83,457 ms",
		"1,100 changed / 1,200 scanned",
		"1,234,567 total (4,321 added)",
		"1,024",
		"2,048",
	} {
		if !contains(body, want) {
			t.Errorf("status page missing comma-formatted %q", want)
		}
	}
}

// helpers

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
