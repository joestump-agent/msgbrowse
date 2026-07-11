package web

import (
	"context"
	"net/http"
	"net/http/httptest"
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

// fileAnchorRE isolates the file card's download anchor so a test can assert
// its attributes without matching the unrelated meta-row links in the card.
var fileAnchorRE = regexp.MustCompile(`<a class="media-file-name"[^>]*>`)

// TestGalleryFilesDownloadNotBoosted is the issue #4 regression: the file
// card's download anchor must carry hx-boost="false" so htmx never AJAX-swaps
// the click — the binary /media response can't be swapped into #main-content,
// which is exactly the "clicking a file does nothing" symptom. It must also
// keep its download attribute (issue #161). The check runs on BOTH the initial
// files tab AND an infinite-scroll continuation fragment, since both are the
// one gallery_files_page template define.
func TestGalleryFilesDownloadNotBoosted(t *testing.T) {
	srv, _, _ := newTestServer(t)

	for _, path := range []string{"/gallery?tab=files", "/gallery/items?tab=files"} {
		rec := get(t, srv, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		anchors := fileAnchorRE.FindAllString(rec.Body.String(), -1)
		if len(anchors) == 0 {
			t.Fatalf("GET %s rendered no file download anchors", path)
		}
		for _, a := range anchors {
			if !strings.Contains(a, `hx-boost="false"`) {
				t.Errorf("GET %s: file anchor not opted out of hx-boost: %s", path, a)
			}
			if !strings.Contains(a, "download=") {
				t.Errorf("GET %s: file anchor lost its download attribute: %s", path, a)
			}
		}
	}
}

// TestMediaServesAttachment is the server-side half of issue #4: a non-image
// attachment must come back with Content-Disposition: attachment so the native
// (un-boosted) anchor navigation downloads it rather than trying to render a
// raw binary in the page.
func TestMediaServesAttachment(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv, _ := st.GetConversation(context.Background(), "Harper")

	rec := get(t, srv, "/media/"+itoa(conv.ID)+"/media/lease.pdf")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/pdf") {
		t.Errorf("Content-Type = %q, want application/pdf", ct)
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

// seedLinkConversation writes one conversation carrying a single message with
// the given link URL and returns nothing — callers query by the URL. Shared by
// the #14 copy-control tests (transcript pill + Media→Links row).
func seedLinkConversation(t *testing.T, st *store.Store, name, url string) {
	t.Helper()
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, source.Signal, name)
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := time.Parse(signal.TimestampLayout, "2022-06-01 10:00:00")
	if _, err := st.ReplaceConversationMessages(ctx, id, source.Signal, []signal.Message{
		{Conversation: name, Timestamp: parsed, TimestampRaw: "2022-06-01 10:00:00",
			Sender: "Robin", Body: "see this", Links: []signal.Link{{URL: url}}},
	}); err != nil {
		t.Fatal(err)
	}
}

// TestGalleryLinkCopyButton asserts each Media→Links row carries an icon-only
// copy control whose copy *source* is the FULL URL (issue #14): copy.js reads
// data-copy-value, so the whole URL — not just the grouped domain — reaches the
// clipboard. The control is labeled for the keyboard, and the row's own link
// still points out so click-through-to-open is untouched.
func TestGalleryLinkCopyButton(t *testing.T) {
	srv, st, _ := newTestServer(t)
	const url = "https://docs.example.org/guide/copy-me"
	seedLinkConversation(t, st, "Linky", url)

	rec := get(t, srv, "/gallery?tab=links")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	// The copy button's source is the full URL (not the domain).
	if !contains(body, `data-copy-value="`+url+`"`) {
		t.Errorf("Media→Links row missing a copy button sourced from the full URL: %s", body)
	}
	// Keyboard-operable + labeled, and the inline icon-swap button markup.
	if !contains(body, `aria-label="Copy link"`) || !contains(body, "copy-btn-inline") {
		t.Error("link copy button missing its aria-label or inline copy-btn class")
	}
	// Click-through-to-open preserved: the URL still links out.
	if !contains(body, `class="media-link-url" href="`+url+`"`) {
		t.Error("Media→Links row dropped the click-through-to-open link")
	}
}

// TestTranscriptLinkCopyButton asserts the transcript link pill (which shows
// only the DOMAIN) gains an icon-only copy control that copies the FULL URL via
// data-copy-value, without breaking the pill's link-out (issue #14).
func TestTranscriptLinkCopyButton(t *testing.T) {
	srv, st, _ := newTestServer(t)
	const url = "https://blog.example.net/a/very/long/path?ref=chat"
	seedLinkConversation(t, st, "Linky", url)
	conv, err := st.GetConversation(context.Background(), "Linky")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}

	rec := get(t, srv, "/c/"+itoa(conv.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	// The pill still links out (click-through-to-open) ...
	if !contains(body, `class="link-pill" href="`+url+`"`) {
		t.Errorf("transcript link pill dropped its link-out: %s", body)
	}
	// ... and the sibling copy button copies the full URL, labeled and inline.
	if !contains(body, `data-copy-value="`+url+`"`) {
		t.Error("transcript link tile missing a copy button sourced from the full URL")
	}
	if !contains(body, `aria-label="Copy link"`) || !contains(body, "copy-btn-inline") {
		t.Error("transcript copy button missing its aria-label or inline copy-btn class")
	}
	// The shared aria-live announce region is present in the shell (page_end).
	if !contains(body, `id="copy-announce"`) || !contains(body, `aria-live="polite"`) {
		t.Error("transcript page missing the shared #copy-announce live region")
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

// TestGalleryMultiConversationParse: parseGalleryFilter reads EVERY repeated
// ?conversation= param into the filter set (issue #6), dropping garbage,
// non-positive ids, and duplicates; the set round-trips through filterValues
// onto tab links and load-more URLs so it survives tab switches and infinite
// scroll.
func TestGalleryMultiConversationParse(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet,
		"/gallery?tab=files&conversation=3&conversation=7&conversation=3&conversation=abc&conversation=-1&conversation=", nil)
	form, filter := parseGalleryFilter(req)
	want := []int64{3, 7}
	if len(form.ConversationIDs) != 2 || form.ConversationIDs[0] != 3 || form.ConversationIDs[1] != 7 {
		t.Fatalf("form.ConversationIDs = %v, want %v", form.ConversationIDs, want)
	}
	if len(filter.ConversationIDs) != 2 || filter.ConversationIDs[0] != 3 || filter.ConversationIDs[1] != 7 {
		t.Errorf("filter.ConversationIDs = %v, want %v", filter.ConversationIDs, want)
	}

	// Tab links keep the whole set.
	q := form.GalleryQuery("images")
	if !strings.Contains(q, "conversation=3") || !strings.Contains(q, "conversation=7") {
		t.Errorf("GalleryQuery dropped part of the set: %s", q)
	}

	// Load-more URLs keep the whole set alongside the keyset cursor.
	next := form.attachmentsNextURL(&store.MediaPage{HasMore: true, NextTSUnix: 42, NextID: 9})
	for _, want := range []string{"conversation=3", "conversation=7", "after_ts=42", "after_id=9", "tab=files"} {
		if !strings.Contains(next, want) {
			t.Errorf("attachmentsNextURL missing %q: %s", want, next)
		}
	}

	// An empty selection means all conversations: no param travels.
	empty, _ := parseGalleryFilter(httptest.NewRequest(http.MethodGet, "/gallery", nil))
	if strings.Contains(empty.GalleryQuery("images"), "conversation=") {
		t.Errorf("empty selection leaked a conversation param: %s", empty.GalleryQuery("images"))
	}

	// A crafted URL with thousands of distinct ids is clamped to the cap, so
	// the id set can never approach SQLite's bound-parameter limit.
	var sb strings.Builder
	sb.WriteString("/gallery?tab=images")
	for i := 1; i <= maxConversationFilterIDs+50; i++ {
		sb.WriteString("&conversation=")
		sb.WriteString(strconv.Itoa(i))
	}
	capped, _ := parseGalleryFilter(httptest.NewRequest(http.MethodGet, sb.String(), nil))
	if len(capped.ConversationIDs) != maxConversationFilterIDs {
		t.Errorf("cap not applied: got %d ids, want %d", len(capped.ConversationIDs), maxConversationFilterIDs)
	}
}

// TestGalleryMultiConversationUI: the Media filter bar renders the CSP-clean
// multi-select (a <details> dropdown of checkboxes, no inline JS), checks the
// selected conversations, labels the collapsed control, and filters + carries
// the set across tab links (issue #6).
func TestGalleryMultiConversationUI(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()
	harper, _ := st.GetConversation(ctx, "Harper")
	group, _ := st.GetConversation(ctx, "Group Trip")
	if harper == nil || group == nil {
		t.Fatal("fixture conversations missing")
	}

	rec := get(t, srv, "/gallery?conversation="+itoa(harper.ID)+"&conversation="+itoa(group.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()

	// The control is the checkbox dropdown, not a single <select>.
	if contains(body, `<select name="conversation"`) {
		t.Error("conversation filter still renders a single-select")
	}
	for _, want := range []string{"filter-multi", "filter-multi-panel", "filter-multi-summary"} {
		if !contains(body, want) {
			t.Errorf("multi-select missing marker %q", want)
		}
	}
	// Both selected conversations render checked; the collapsed label counts them.
	for _, id := range []int64{harper.ID, group.ID} {
		if !contains(body, `value="`+itoa(id)+`" checked`) {
			t.Errorf("conversation %d checkbox not checked", id)
		}
	}
	if !contains(body, "2 conversations") {
		t.Error("collapsed multi-select label should read \"2 conversations\"")
	}
	// Tab links carry the whole set forward.
	if !contains(body, "conversation="+itoa(harper.ID)) || !contains(body, "conversation="+itoa(group.ID)) {
		t.Error("tab links dropped part of the conversation set")
	}

	// A single selection labels the control with the conversation's name and
	// checks exactly that box.
	rec = get(t, srv, "/gallery?conversation="+itoa(harper.ID))
	body = rec.Body.String()
	if !contains(body, `<span class="filter-multi-value" id="conv-filter-value">Harper</span>`) {
		t.Error("single selection should label the control with the conversation name")
	}
	if contains(body, `value="`+itoa(group.ID)+`" checked`) {
		t.Error("unselected conversation rendered checked")
	}

	// No selection = all conversations.
	rec = get(t, srv, "/gallery")
	if !contains(rec.Body.String(), "All conversations") {
		t.Error("empty selection should label the control \"All conversations\"")
	}
}

// TestGalleryMultiConversationFiltersResults: two selected conversations widen
// the result to their union (issue #6) — badge counts equal the store's own
// multi-id counts, and both conversations' media URLs appear.
func TestGalleryMultiConversationFiltersResults(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()
	harper, _ := st.GetConversation(ctx, "Harper")
	group, _ := st.GetConversation(ctx, "Group Trip")
	if harper == nil || group == nil {
		t.Fatal("fixture conversations missing")
	}

	counts, err := st.CountMedia(ctx, store.GalleryFilter{ConversationIDs: []int64{harper.ID, group.ID}})
	if err != nil {
		t.Fatal(err)
	}
	harperOnly, err := st.CountMedia(ctx, store.GalleryFilter{ConversationIDs: []int64{harper.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if counts.Images <= harperOnly.Images {
		t.Fatalf("fixture should widen the union: both=%+v harper=%+v", counts, harperOnly)
	}

	rec := get(t, srv, "/gallery?conversation="+itoa(harper.ID)+"&conversation="+itoa(group.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "Images <span class=\"media-tab-badge\">"+strconv.Itoa(counts.Images)+"</span>") {
		t.Errorf("images badge should show the union count %d", counts.Images)
	}
	// Both conversations contribute tiles.
	if !contains(body, "/media/"+itoa(harper.ID)+"/") || !contains(body, "/media/"+itoa(group.ID)+"/") {
		t.Error("union page missing one conversation's media")
	}
}

// TestGalleryLinksSortHiddenInput is the PR #16 review nit: the Links tab
// hides the (inert) Sort select, so a previously chosen non-default sort must
// travel as a hidden input — otherwise pressing Apply from Links silently
// resets the order the user picked on Images/Files.
func TestGalleryLinksSortHiddenInput(t *testing.T) {
	srv, _, _ := newTestServer(t)

	rec := get(t, srv, "/gallery?tab=links&sort=asc")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !contains(rec.Body.String(), `<input type="hidden" name="sort" value="asc">`) {
		t.Error("links tab dropped sort=asc: no hidden input in the filter form")
	}

	// The default order stays implicit — no hidden input, no ?sort= noise.
	rec = get(t, srv, "/gallery?tab=links")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if contains(rec.Body.String(), `name="sort"`) {
		t.Error("default links tab should carry no sort control or hidden input")
	}
}

// TestGallerySort covers the Media sort control (issue #5): the filter bar
// offers Newest/Oldest, ?sort=asc round-trips through parseGalleryFilter and
// flips the rendered order, and the choice rides tab links + the load-more
// sentinel so infinite scroll keeps the order. Default URLs omit ?sort=.
func TestGallerySort(t *testing.T) {
	srv, st, _ := newTestServer(t)

	// The default page carries the Sort control but no ?sort= in its tab links.
	rec := get(t, srv, "/gallery")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, `name="sort"`) || !contains(body, "Newest first") || !contains(body, "Oldest first") {
		t.Errorf("filter bar missing the sort control")
	}
	if contains(body, "sort=asc") {
		t.Errorf("default page leaked sort=asc into a URL: %s", body)
	}

	// parseSort/filter round-trip: sort=asc selects the Oldest option and rides
	// tab links + the load-more sentinel.
	rec = get(t, srv, "/gallery?tab=images&sort=asc")
	if rec.Code != http.StatusOK {
		t.Fatalf("asc status = %d", rec.Code)
	}
	body = rec.Body.String()
	if !contains(body, `<option value="asc" selected>`) {
		t.Errorf("sort=asc did not select the Oldest option")
	}
	// Tab links preserve the sort.
	if !contains(body, "sort=asc") {
		t.Errorf("tab links dropped sort=asc")
	}

	// Store-backed order flip: seed two images and confirm the oldest-first page
	// renders them ahead of the default newest-first page. The fixture's own
	// images share a coarse ordering, so use a dedicated conversation.
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, source.Signal, "Sortie")
	if err != nil {
		t.Fatal(err)
	}
	older, _ := time.Parse(signal.TimestampLayout, "2020-01-01 09:00:00")
	newer, _ := time.Parse(signal.TimestampLayout, "2021-01-01 09:00:00")
	imgAtt := func(name string) []signal.Attachment {
		return []signal.Attachment{{Kind: signal.KindImage, RelPath: "media/" + name, OriginalName: name}}
	}
	_, err = st.ReplaceConversationMessages(ctx, id, source.Signal, []signal.Message{
		{Conversation: "Sortie", Timestamp: older, TimestampRaw: "2020-01-01 09:00:00", Sender: "A", Body: "x", Attachments: imgAtt("old.jpg")},
		{Conversation: "Sortie", Timestamp: newer, TimestampRaw: "2021-01-01 09:00:00", Sender: "A", Body: "x", Attachments: imgAtt("new.jpg")},
	})
	if err != nil {
		t.Fatal(err)
	}
	descPage, err := st.ListAttachments(ctx, "image", store.GalleryFilter{ConversationIDs: []int64{id}}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(descPage.Items) != 2 || descPage.Items[0].OriginalName != "new.jpg" {
		t.Fatalf("default order = %+v, want new.jpg first", descPage.Items)
	}
	ascPage, err := st.ListAttachments(ctx, "image", store.GalleryFilter{ConversationIDs: []int64{id}, SortAsc: true}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ascPage.Items) != 2 || ascPage.Items[0].OriginalName != "old.jpg" {
		t.Errorf("SortAsc order = %+v, want old.jpg first", ascPage.Items)
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
		{"/gallery?conversation=" + itoa(conv.ID), store.GalleryFilter{ConversationIDs: []int64{conv.ID}}},
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
