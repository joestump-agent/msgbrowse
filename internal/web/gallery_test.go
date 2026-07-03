package web

import (
	"context"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

func TestGalleryImagesTab(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv, _ := st.GetConversation(context.Background(), "Harper")

	rec := get(t, srv, "/gallery")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "image-grid") || !contains(body, "lightbox") {
		t.Errorf("images tab missing grid/lightbox")
	}
	// Slate media redesign (REQ-0006-009): tabs with the accent underline and
	// square cover tiles with a filename scrim.
	for _, want := range []string{"media-tabs", "media-tab-active", "media-tile", "media-tile-name"} {
		if !contains(body, want) {
			t.Errorf("images tab missing slate marker %q", want)
		}
	}
	// The fixture has Harper/media/cabin.jpg — its media URL should appear.
	if !contains(body, "/media/"+itoa(conv.ID)+"/media/cabin.jpg") {
		t.Errorf("images tab missing fixture image URL")
	}
}

func TestGalleryFilesTab(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv, _ := st.GetConversation(context.Background(), "Harper")

	rec := get(t, srv, "/gallery?tab=files")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "lease.pdf") {
		t.Errorf("files tab missing lease.pdf")
	}
	// Size/type are computed from the on-disk file; the fixture lease.pdf exists.
	if !contains(body, "/media/"+itoa(conv.ID)+"/media/lease.pdf") {
		t.Errorf("files tab missing file URL")
	}
}

func TestGalleryLinksTab(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := get(t, srv, "/gallery?tab=links")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	// Fixture has a yelp link and a maps link; domains group them.
	if !contains(body, "link-group") {
		t.Errorf("links tab missing groups")
	}
	if !contains(body, "yelp.com") && !contains(body, "example.com") {
		t.Errorf("links tab missing expected domains: %s", body)
	}
}

func TestGalleryTabPreservesFilter(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv, _ := st.GetConversation(context.Background(), "Harper")
	rec := get(t, srv, "/gallery?tab=images&conversation="+itoa(conv.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	// Tab links should carry the conversation filter forward.
	if !contains(body, "conversation="+itoa(conv.ID)) {
		t.Errorf("tab links dropped the conversation filter")
	}
}

// TestGalleryLinkEscaping confirms a crafted link URL is attribute-escaped in
// the rendered href/text (defense in depth — the parser excludes <>"' from
// bare URLs, but the store accepts any string).
func TestGalleryLinkEscaping(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, source.Signal, "Evil")
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := time.Parse(signal.TimestampLayout, "2022-06-01 10:00:00")
	_, err = st.ReplaceConversationMessages(ctx, id, source.Signal, []signal.Message{
		{Conversation: "Evil", Timestamp: parsed, TimestampRaw: "2022-06-01 10:00:00",
			Sender: "Mallory", Body: "x",
			Links: []signal.Link{{URL: `https://evil.test/"><script>alert(1)</script>`}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := get(t, srv, "/gallery?tab=links")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if contains(body, "<script>alert(1)</script>") {
		t.Errorf("crafted link URL leaked unescaped (XSS): %s", body)
	}
}

func TestGalleryBadTabDefaultsToImages(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := get(t, srv, "/gallery?tab=bogus")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !contains(rec.Body.String(), "image-grid") {
		t.Errorf("bad tab should fall back to images")
	}
}

// TestGalleryImagesLazy: every <img> on the images tab — grid thumbnails AND
// the hidden :target lightbox originals — must carry loading="lazy", so
// off-screen lightboxes stop downloading originals with the page (SPEC-0008
// REQ-0008-010).
func TestGalleryImagesLazy(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := get(t, srv, "/gallery")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "lightbox") {
		t.Fatalf("images tab missing lightbox markup")
	}
	for rest, n := body, 0; ; n++ {
		i := strings.Index(rest, "<img")
		if i < 0 {
			if n == 0 {
				t.Fatal("images tab rendered no <img> tags")
			}
			break
		}
		rest = rest[i:]
		end := strings.IndexByte(rest, '>')
		if end < 0 {
			t.Fatal("unterminated <img tag")
		}
		if tag := rest[:end]; !contains(tag, `loading="lazy"`) {
			t.Errorf("img tag missing loading=\"lazy\": %s", tag)
		}
		rest = rest[end:]
	}
}

// seedManyLinks writes n messages each carrying one distinct URL on the same
// domain, so the deduplicated links listing holds n rows.
func seedManyLinks(t *testing.T, st *store.Store, n int) {
	t.Helper()
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, source.Signal, "Linky")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2023, 5, 1, 8, 0, 0, 0, time.UTC)
	msgs := make([]signal.Message, 0, n)
	for i := 0; i < n; i++ {
		ts := base.Add(time.Duration(i) * time.Minute)
		msgs = append(msgs, signal.Message{
			Conversation: "Linky", Timestamp: ts, TimestampRaw: ts.Format(signal.TimestampLayout),
			Sender: "Me", Body: "l",
			Links: []signal.Link{{URL: "https://linkfarm.example.com/p" + strconv.Itoa(i)}},
		})
	}
	if _, err := st.ReplaceConversationMessages(ctx, id, source.Signal, msgs); err != nil {
		t.Fatal(err)
	}
}

// TestGalleryLinksPagination seeds more distinct URLs than one page holds and
// walks the links tab exactly as htmx would: the first page must stay bounded
// and end in an hx-trigger="revealed" load-more sentinel whose URL yields the
// remainder with no duplicates (SPEC-0008 REQ-0008-009; issue #77 bounds).
func TestGalleryLinksPagination(t *testing.T) {
	srv, st, _ := newTestServer(t)
	seedManyLinks(t, st, 230) // fixture adds a couple more distinct URLs

	rec := get(t, srv, "/gallery?tab=links")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	first := strings.Count(body, `class="media-link-url"`)
	if first > 200 {
		t.Errorf("first page rendered %d links, want ≤ 200", first)
	}
	if !contains(body, `hx-trigger="revealed"`) || !contains(body, "/gallery/items?") {
		t.Fatalf("first page missing the load-more sentinel")
	}
	next := extractHxGet(t, body)
	if !contains(next, "tab=links") || !contains(next, "after_url=") {
		t.Errorf("sentinel URL missing cursor params: %s", next)
	}

	rec2 := get(t, srv, next)
	if rec2.Code != http.StatusOK {
		t.Fatalf("continuation status = %d", rec2.Code)
	}
	body2 := rec2.Body.String()
	// The continuation is a fragment: no shell, no <main>.
	if contains(body2, "<main") {
		t.Errorf("continuation page rendered the full shell")
	}
	total := first + strings.Count(body2, `class="media-link-url"`)
	urls := map[string]bool{}
	for _, b := range []string{body, body2} {
		for _, m := range regexp.MustCompile(`class="media-link-url" href="([^"]+)"`).FindAllStringSubmatch(b, -1) {
			if urls[m[1]] {
				t.Errorf("duplicate link across pages: %s", m[1])
			}
			urls[m[1]] = true
		}
	}
	if len(urls) != total {
		t.Errorf("distinct urls = %d, rendered = %d", len(urls), total)
	}
	if total < 230 {
		t.Errorf("both pages rendered %d links, want ≥ 230", total)
	}
}

// extractHxGet pulls the first load-more sentinel's target URL out of rendered
// HTML, undoing html/template's attribute escaping.
func extractHxGet(t *testing.T, body string) string {
	t.Helper()
	m := regexp.MustCompile(`hx-get="([^"]+)"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no hx-get sentinel found")
	}
	return strings.ReplaceAll(m[1], "&amp;", "&")
}

// TestGalleryItemsBogusCursor: numeric cursor params are parsed as integers —
// garbage reads as zero ("from the top") and must never 500 (issue #74
// security checklist: pagination cursor params parsed as integers with
// bounds).
func TestGalleryItemsBogusCursor(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, path := range []string{
		"/gallery/items?tab=links&after_count=abc&after_ts=xyz&after_domain=x&after_url=y",
		"/gallery/items?tab=images&after_ts=1e9&after_id=--",
		"/gallery/items?tab=files&after_ts=&after_id=9999999999999999999999",
	} {
		rec := get(t, srv, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
		}
	}
}

// TestGalleryCountsMatchStore: the tab badges must equal the store's own
// counts for the fixture archive, filtered and unfiltered alike (issue #77:
// counts identical to a fixture baseline).
func TestGalleryCountsMatchStore(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()
	conv, _ := st.GetConversation(ctx, "Harper")

	for _, tc := range []struct {
		path   string
		filter store.GalleryFilter
	}{
		{"/gallery", store.GalleryFilter{}},
		{"/gallery?conversation=" + itoa(conv.ID), store.GalleryFilter{ConversationID: conv.ID}},
		{"/gallery?source=signal", store.GalleryFilter{Source: source.Signal}},
	} {
		counts, err := st.CountMedia(ctx, tc.filter)
		if err != nil {
			t.Fatal(err)
		}
		rec := get(t, srv, tc.path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", tc.path, rec.Code)
		}
		body := rec.Body.String()
		for _, want := range []string{
			"Images <span class=\"media-tab-badge\">" + strconv.Itoa(counts.Images) + "</span>",
			"Files <span class=\"media-tab-badge\">" + strconv.Itoa(counts.Files) + "</span>",
			"Links <span class=\"media-tab-badge\">" + strconv.Itoa(counts.Links) + "</span>",
		} {
			if !contains(body, want) {
				t.Errorf("%s: badge %q not found", tc.path, want)
			}
		}
	}
}
