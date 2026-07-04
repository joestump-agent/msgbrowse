package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/joestump/msgbrowse/internal/store"
)

// setupTokenRe matches the 256-bit per-session Setup tokens minted at render
// time (setup_security.go): 64 lowercase hex chars. /providers embeds a fresh
// token in its privileged-POST controls on EVERY render by design, so the
// byte-stability and gzip-identity contracts are asserted modulo those token
// bytes.
var setupTokenRe = regexp.MustCompile(`[0-9a-f]{64}`)

// normalizeTokens replaces per-render Setup tokens with a fixed placeholder so
// two renders of the same page compare equal everywhere else.
func normalizeTokens(s string) string { return setupTokenRe.ReplaceAllString(s, "TOKEN") }

// countingStore wraps the real store and counts ListConversations calls, so
// tests can prove the HTMX partial path never runs the sidebar listing
// (SPEC-0008 REQ-0008-006 / issue #76). All other methods pass through via the
// embedded Store.
type countingStore struct {
	*store.Store
	listCalls atomic.Int32
}

func (c *countingStore) ListConversations(ctx context.Context) ([]store.ConversationSummary, error) {
	c.listCalls.Add(1)
	return c.Store.ListConversations(ctx)
}

// newCountingTestServer is newTestServer with the counting wrapper injected.
func newCountingTestServer(t *testing.T) (*Server, *countingStore, *store.Store) {
	t.Helper()
	st, cfg, _ := newTestStoreAndConfig(t)
	cs := &countingStore{Store: st}
	srv, err := NewServer(cs, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv, cs, st
}

// getWith issues a GET with extra request headers.
func getWith(t *testing.T, srv *Server, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// getPartial issues a GET as an HTMX boosted navigation.
func getPartial(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	return getWith(t, srv, path, map[string]string{"HX-Request": "true"})
}

// pageRoutes returns every full-page route (REQ-0008-006 applies to each),
// with {id} resolved against the fixture.
func pageRoutes(t *testing.T, st *store.Store) []string {
	t.Helper()
	conv, err := st.GetConversation(context.Background(), "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}
	return []string{
		"/",
		"/c/" + itoa(conv.ID),
		"/search",
		"/gallery",
		"/status",
		"/providers",
		"/logs",
		"/settings",
	}
}

// TestPartialVsFullPerRoute is the REQ-0008-006 contract for every page route:
// an HX-Request response is <title> + #main-content only (no sidebar, no
// document shell), while the plain request still renders the full document.
func TestPartialVsFullPerRoute(t *testing.T) {
	srv, st, _ := newTestServer(t)
	for _, route := range pageRoutes(t, st) {
		t.Run(route, func(t *testing.T) {
			partial := getPartial(t, srv, route)
			if partial.Code != http.StatusOK {
				t.Fatalf("partial status = %d", partial.Code)
			}
			pbody := partial.Body.String()
			// The swap unit: a <title> for history plus the #main-content target.
			if !contains(pbody, "<title>") || !contains(pbody, "</title>") {
				t.Errorf("partial missing <title>")
			}
			if !contains(pbody, `id="main-content"`) {
				t.Errorf("partial missing #main-content")
			}
			if n := strings.Count(pbody, `id="main-content"`); n != 1 {
				t.Errorf("partial has %d main-content ids, want exactly 1", n)
			}
			// No shell: no document skeleton, no toolbar, no sidebar markup.
			for _, forbidden := range []string{"<!doctype", "<html", "app-sidebar", "app-toolbar", "toolbar-title", "drawer-side", "sidebar-filter"} {
				if contains(strings.ToLower(pbody), strings.ToLower(forbidden)) {
					t.Errorf("partial leaked shell marker %q", forbidden)
				}
			}

			full := get(t, srv, route)
			if full.Code != http.StatusOK {
				t.Fatalf("full status = %d", full.Code)
			}
			fbody := full.Body.String()
			for _, want := range []string{"<!doctype html>", "app-sidebar", "app-toolbar", `id="main-content"`, "<title>"} {
				if !contains(fbody, want) {
					t.Errorf("full document missing %q", want)
				}
			}
			// The partial must be a strict subset situation: dramatically smaller
			// than the full document on every route (it drops the whole shell).
			if len(pbody) >= len(fbody) {
				t.Errorf("partial (%d bytes) not smaller than full (%d bytes)", len(pbody), len(fbody))
			}
		})
	}
}

// TestPartialTitleMatchesPage verifies the <title> that rides along with a
// boosted conversation swap carries the page-specific title, so htmx history
// entries stay correctly labeled.
func TestPartialTitleMatchesPage(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv, err := st.GetConversation(context.Background(), "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}
	body := getPartial(t, srv, "/c/"+itoa(conv.ID)).Body.String()
	if !contains(body, "<title>Harper · msgbrowse</title>") {
		t.Errorf("partial title wrong or missing; body starts %q", body[:min(120, len(body))])
	}
}

// TestHistoryRestoreGetsFullDocument: htmx history restores re-render the whole
// body, so HX-History-Restore-Request must yield the full document even though
// HX-Request is set (REQ-0008-006).
func TestHistoryRestoreGetsFullDocument(t *testing.T) {
	srv, st, _ := newTestServer(t)
	for _, route := range pageRoutes(t, st) {
		rec := getWith(t, srv, route, map[string]string{
			"HX-Request":                 "true",
			"HX-History-Restore-Request": "true",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", route, rec.Code)
		}
		body := rec.Body.String()
		if !contains(body, "<!doctype html>") || !contains(body, "app-sidebar") {
			t.Errorf("%s: history restore did not get the full document", route)
		}
	}
}

// TestPartialSkipsSidebarListing proves via the counting stub that no
// conversation-listing SQL runs on the partial path for ANY page route, while
// full renders still load the sidebar exactly once (REQ-0008-006, issue #76).
func TestPartialSkipsSidebarListing(t *testing.T) {
	srv, cs, st := newCountingTestServer(t)
	for _, route := range pageRoutes(t, st) {
		cs.listCalls.Store(0)
		if rec := getPartial(t, srv, route); rec.Code != http.StatusOK {
			t.Fatalf("%s partial status = %d", route, rec.Code)
		}
		if n := cs.listCalls.Load(); n != 0 {
			t.Errorf("%s: partial render ran ListConversations %d times, want 0", route, n)
		}

		cs.listCalls.Store(0)
		if rec := get(t, srv, route); rec.Code != http.StatusOK {
			t.Fatalf("%s full status = %d", route, rec.Code)
		}
		if n := cs.listCalls.Load(); n != 1 {
			t.Errorf("%s: full render ran ListConversations %d times, want 1", route, n)
		}
	}
}

// TestMessagesFragmentContractUnchanged: the infinite-scroll endpoint already
// returns a fragment; the HX-Request branch must leave it alone (no <title>,
// no #main-content wrapper — htmx swaps it with hx-swap="outerHTML" on the
// sentinel, not hx-select).
func TestMessagesFragmentContractUnchanged(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv, err := st.GetConversation(context.Background(), "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}
	rec := getPartial(t, srv, "/c/"+itoa(conv.ID)+"/messages")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if contains(body, "<title>") || contains(body, `id="main-content"`) {
		t.Errorf("message_list fragment gained page-partial wrapping")
	}
	if !contains(body, `class="msg-row`) {
		t.Errorf("message_list fragment missing message rows")
	}
}

// TestSearchResultsFragmentUnchangedUnderHX: same guarantee for the live-search
// fragment endpoint.
func TestSearchResultsFragmentUnchangedUnderHX(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := getPartial(t, srv, "/search/results?q=lease")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if contains(body, "<title>") || contains(body, `id="main-content"`) {
		t.Errorf("search_results fragment gained page-partial wrapping")
	}
}

// TestFullPageByteStable: identical plain requests produce identical bytes
// (the design.md verification note for full renders), modulo the per-session
// Setup tokens /providers mints fresh on every render by design.
func TestFullPageByteStable(t *testing.T) {
	srv, st, _ := newTestServer(t)
	for _, route := range pageRoutes(t, st) {
		a := normalizeTokens(get(t, srv, route).Body.String())
		b := normalizeTokens(get(t, srv, route).Body.String())
		if a != b {
			t.Errorf("%s: two identical full requests differ", route)
		}
	}
}

// TestPartialStatStripsStayLive: the home and status stat strips live inside
// #main-content, so partial renders must still show real counts (from the
// cheap ArchiveStats aggregate) — not zeros.
func TestPartialStatStripsStayLive(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()
	total, err := st.CountMessages(ctx)
	if err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if total == 0 {
		t.Fatal("fixture should have messages")
	}
	for _, route := range []string{"/", "/status"} {
		body := getPartial(t, srv, route).Body.String()
		if !contains(body, ">"+itoa(int64(total))+"<") {
			t.Errorf("%s partial missing live message count %d", route, total)
		}
	}
}

// TestPartialDropdownsStayPopulated: the search and gallery conversation
// filters live inside #main-content; partial renders must still list every
// conversation (via the lightweight refs listing).
func TestPartialDropdownsStayPopulated(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, route := range []string{"/search", "/gallery"} {
		body := getPartial(t, srv, route).Body.String()
		for _, want := range []string{"All conversations", "Harper", "Group Trip"} {
			if !contains(body, want) {
				t.Errorf("%s partial dropdown missing %q", route, want)
			}
		}
	}
}

// TestPinFormBoosted: the pin form navigates via the boosted partial path
// (SPEC-0008 REQ-0008-005) — htmx POSTs, follows the 303, and swaps
// #main-content — while remaining a plain form for no-JS degradation.
func TestPinFormBoosted(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv, err := st.GetConversation(context.Background(), "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}
	body := get(t, srv, "/c/"+itoa(conv.ID)).Body.String()
	i := strings.Index(body, `action="/c/`+itoa(conv.ID)+`/pin"`)
	if i < 0 {
		t.Fatal("pin form missing")
	}
	// The boost attributes hang off the same <form> tag.
	formTag := body[strings.LastIndex(body[:i], "<form"):]
	formTag = formTag[:strings.Index(formTag, ">")+1]
	for _, want := range []string{`hx-boost="true"`, `hx-target="#main-content"`, `hx-select="#main-content"`, `method="post"`} {
		if !contains(formTag, want) {
			t.Errorf("pin form missing %q (tag: %s)", want, formTag)
		}
	}
	// And the redirect target itself renders as a partial when htmx follows it.
	rec := getPartial(t, srv, "/c/"+itoa(conv.ID))
	if !contains(rec.Body.String(), `id="main-content"`) || contains(rec.Body.String(), "app-sidebar") {
		t.Errorf("post-pin partial render broken")
	}
}

// TestRenderVariesOnHXRequest asserts the Vary: HX-Request header on BOTH the
// full and partial variants of a page response — the body depends on that
// header, so any HTTP cache in front must key on it (the classic htmx
// cache-poisoning footgun; adversarial-review follow-up on #85).
func TestRenderVariesOnHXRequest(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"full":    get(t, srv, "/"),
		"partial": getPartial(t, srv, "/"),
	} {
		found := false
		for _, v := range rec.Result().Header.Values("Vary") {
			if strings.Contains(v, "HX-Request") {
				found = true
			}
		}
		if !found {
			t.Errorf("%s response missing Vary: HX-Request (got %q)", name, rec.Result().Header.Values("Vary"))
		}
	}
}
