package web

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/source"
)

// TestToolbarContextualTitle verifies the unified toolbar's contextual title
// (#152 Option A): "msgbrowse" on home, the active conversation's display name on
// a transcript page, and that the title links home. It also asserts Option A
// dropped the old global counts from the toolbar (they moved to Status under
// Settings → Diagnostics).
func TestToolbarContextualTitle(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()

	// Home: the toolbar shows the "msgbrowse" wordmark in the contextual title.
	home := get(t, srv, "/").Body.String()
	if !contains(home, "app-toolbar") {
		t.Error("home missing the unified toolbar")
	}
	if !contains(home, `class="toolbar-title" aria-label="msgbrowse home"`) {
		t.Error("home toolbar title should be the msgbrowse-home link")
	}
	if !contains(home, ">msgbrowse</a>") {
		t.Error("home toolbar title should read 'msgbrowse'")
	}
	// Option A removed the global counts from the toolbar entirely.
	if contains(home, "navbar-counts") || contains(home, " conversations · ") {
		t.Error("Option A should have removed the global counts from the toolbar")
	}

	// Transcript: the toolbar title becomes the conversation's display name.
	conv, err := st.GetConversation(ctx, "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}
	convPage := get(t, srv, "/c/"+itoa(conv.ID)).Body.String()
	if !contains(convPage, ">"+humanName(conv.Name)+"</a>") {
		t.Errorf("transcript toolbar title should read the conversation name %q", humanName(conv.Name))
	}
	// It is still the home link (clickable back to home), not a plain heading.
	if !contains(convPage, `class="toolbar-title" aria-label="msgbrowse home"`) {
		t.Error("transcript toolbar title should still link home")
	}
}

// TestToolbarSearchForm verifies the toolbar's search pill is a boosted GET form
// targeting /search with a labelled input (#152), so Enter runs a search on the
// existing /search page.
func TestToolbarSearchForm(t *testing.T) {
	srv, _, _ := newTestServer(t)
	body := get(t, srv, "/").Body.String()

	if !contains(body, `<form action="/search" method="get" role="search"`) {
		t.Error("toolbar search form should be a GET to /search with role=search")
	}
	if !contains(body, "toolbar-search") {
		t.Error("toolbar missing the search pill")
	}
	// Boosted so Enter swaps the /search page into #main-content (boosted-nav).
	if !contains(body, `class="toolbar-search hidden sm:flex"`) ||
		!contains(body, `hx-boost="true"`) {
		t.Error("toolbar search form should be boosted (hx-boost)")
	}
	// The input is named q, labelled, and carries the placeholder.
	if !contains(body, `name="q"`) ||
		!contains(body, `aria-label="Search messages"`) ||
		!contains(body, `placeholder="Search messages"`) {
		t.Error("toolbar search input should be named q, labelled, and placeheld 'Search messages'")
	}
	// Below sm the pill is hidden and the sidebar Search link is gone (#190),
	// so an icon-only boosted /search link keeps search reachable everywhere.
	if !contains(body, `<a href="/search" class="toolbar-icon-btn toolbar-search-link sm:hidden" aria-label="Search"`) {
		t.Error("header missing the below-sm icon-only /search link")
	}
}

// TestToolbarIconButtonsLabelled verifies every icon-only toolbar control carries
// an aria-label and the header stays role=banner (#152 accessibility).
func TestToolbarIconButtonsLabelled(t *testing.T) {
	srv, _, _ := newTestServer(t)
	body := get(t, srv, "/").Body.String()

	for _, want := range []string{
		`aria-label="Toggle sidebar"`, // mobile drawer toggle
		`aria-label="Toggle theme"`,   // theme toggle
		`aria-label="Settings"`,       // settings gear → /settings
	} {
		if !contains(body, want) {
			t.Errorf("toolbar icon button missing aria-label %q", want)
		}
	}
	// The theme toggle is still the existing data-theme-toggle control.
	if !contains(body, "data-theme-toggle") {
		t.Error("toolbar missing the data-theme-toggle control")
	}
	// The settings gear links to /settings.
	if !contains(body, `href="/settings"`) {
		t.Error("toolbar settings gear should link to /settings")
	}
	// The header keeps banner semantics (daisyUI dropped, but role=banner is the
	// implicit role of <header> not nested in a section/article — which holds here).
	if !contains(body, "<header ") {
		t.Error("toolbar should be a <header> (role=banner)")
	}
}

// TestSidebarPresenceDotAndSource verifies each conversation row renders a
// monogram avatar with a source-colored presence dot (REQ-0006-004), and that
// the filter input and CONVERSATIONS section are present (REQ-0006-003).
func TestSidebarPresenceDotAndSource(t *testing.T) {
	srv, _, _ := newTestServer(t)
	body := get(t, srv, "/").Body.String()

	for _, want := range []string{
		"Filter conversations", // filter input placeholder
		"sidebar-filter",       // filter input shell
		"avatar-mono",          // monogram avatar
		"presence-dot",         // presence dot element
	} {
		if !contains(body, want) {
			t.Errorf("sidebar missing %q", want)
		}
	}
	// The fixture's conversations are Signal, so the presence dot carries the
	// signal source modifier (blue, derived from --color-info).
	if !contains(body, "presence-dot src-signal") {
		t.Errorf("sidebar presence dot missing src-signal source color")
	}
}

// TestSidebarListOnly pins the #190 sidebar reduction: the <aside> carries NO
// nav links at all (the header tabs + search pill own Search/Media now) — just
// the "Filter conversations" input directly above the Pinned/Conversations
// section heads, where sidebar.js binds the input by id.
func TestSidebarListOnly(t *testing.T) {
	srv, _, _ := newTestServer(t)
	body := get(t, srv, "/").Body.String()

	asideStart := strings.Index(body, "<aside")
	asideEnd := strings.Index(body, "</aside>")
	if asideStart < 0 || asideEnd < 0 {
		t.Fatal("home missing the sidebar <aside>")
	}
	aside := body[asideStart:asideEnd]

	if strings.Contains(aside, "<nav") {
		t.Error("sidebar must carry no <nav> — its Search/Media links moved to the header (#190)")
	}
	for _, href := range []string{`href="/search"`, `href="/gallery"`, `href="/media"`} {
		if strings.Contains(aside, href) {
			t.Errorf("sidebar must not carry %s — the header owns that surface (#190)", href)
		}
	}
	filter := strings.Index(aside, `id="sidebar-filter"`)
	sections := strings.Index(aside, "sidebar-section-head")
	if filter < 0 || sections < 0 {
		t.Fatalf("sidebar missing filter/section (filter %d, sections %d)", filter, sections)
	}
	if !(filter < sections) {
		t.Errorf("sidebar order should be filter → sections (filter %d, sections %d)", filter, sections)
	}
}

// TestHeaderFullWidthShell pins the #190 shell skeleton: the header is a
// direct child of <body>, rendered BEFORE (outside) the drawer that holds the
// sidebar and content — full window width, traffic lights always over its
// left edge in the desktop shell — and the conversation header inside the
// content scroller pins at top-0 (the drifting 54px sticky offset, whose
// see-through slit was the owner's item 1, is structurally gone — the token
// is not spelled as the utility class here because Tailwind v4 scans Go
// comments too and would resurrect it into the built app.css).
func TestHeaderFullWidthShell(t *testing.T) {
	srv, st, _ := newTestServer(t)

	body := get(t, srv, "/").Body.String()
	header := strings.Index(body, "<header class=\"app-toolbar")
	drawer := strings.Index(body, `class="app-shell drawer`)
	aside := strings.Index(body, "<aside")
	if header < 0 || drawer < 0 || aside < 0 {
		t.Fatalf("home missing header/drawer/aside (header %d, drawer %d, aside %d)", header, drawer, aside)
	}
	if !(header < drawer && drawer < aside) {
		t.Errorf("the header must precede the drawer (full-width, above the sidebar): header %d, drawer %d, aside %d", header, drawer, aside)
	}

	// #197 review: the header lays out as [left cluster | tabs | right
	// cluster] IN FLOW (a 3-column grid in input.css) — the tab strip is a
	// grid column, not an absolutely-positioned overlay, so it can never
	// paint over the search pill/icon. Pin the flow order here.
	left := strings.Index(body, `<div class="toolbar-cluster toolbar-cluster-left">`)
	tabs := strings.Index(body, `<nav class="header-tabs"`)
	right := strings.Index(body, `<div class="toolbar-cluster toolbar-cluster-right">`)
	if left < 0 || tabs < 0 || right < 0 || !(left < tabs && tabs < right) {
		t.Errorf("header should flow [left cluster | tabs | right cluster] (left %d, tabs %d, right %d)", left, tabs, right)
	}

	// #main-content is the app's scroll container (#190); it must be
	// keyboard-focusable so PageDown/Space/arrows scroll without a pointer
	// (#197 review; axe scrollable-region-focusable).
	if !contains(body, `<main id="main-content" class="flex-1" tabindex="0"`) {
		t.Error(`#main-content scroller should carry tabindex="0"`)
	}

	conv, err := st.GetConversation(context.Background(), "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}
	convPage := get(t, srv, "/c/"+itoa(conv.ID)).Body.String()
	if !contains(convPage, `class="conv-header sticky top-0 `) {
		t.Error("conv-header should pin at top-0 inside the #main-content scroller")
	}
	// Spelled "top-["+"54px]" so Tailwind v4's content scanner (which scans
	// .go files too) never sees the retired arbitrary-value token here — a
	// plain literal would resurrect the utility in a rebuilt app.css and trip
	// the CI drift guard against the committed artifact.
	if retired := "top-[" + "54px]"; contains(convPage, retired) {
		t.Errorf("conv-header still carries the retired %s offset", retired)
	}
}

// TestHeaderTabs is the #190 primary-nav contract: the header centers a
// Messages ("/") / Media ("/media") tab pair, boosted like all in-app nav,
// with the server marking the active tab on full loads — Messages on home and
// every /c/* transcript, Media on the gallery surface (/media aliases
// /gallery), NEITHER on Search/Settings/… (shell.js re-syncs the same rule
// after boosted swaps, which never re-render this shell).
func TestHeaderTabs(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv, err := st.GetConversation(context.Background(), "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}

	const (
		messagesActive = `href="/" data-nav-tab="messages" class="header-tab header-tab-active" aria-current="page"`
		messagesIdle   = `href="/" data-nav-tab="messages" class="header-tab"`
		mediaActive    = `href="/media" data-nav-tab="media" class="header-tab header-tab-active" aria-current="page"`
		mediaIdle      = `href="/media" data-nav-tab="media" class="header-tab"`
	)
	cases := []struct {
		route         string
		wantMessages  string
		wantMedia     string
		activeSummary string
	}{
		{"/", messagesActive, mediaIdle, "Messages"},
		{"/c/" + itoa(conv.ID), messagesActive, mediaIdle, "Messages"},
		{"/gallery", messagesIdle, mediaActive, "Media"},
		{"/media", messagesIdle, mediaActive, "Media"},
		{"/search", messagesIdle, mediaIdle, "neither"},
		{"/settings", messagesIdle, mediaIdle, "neither"},
	}
	for _, c := range cases {
		t.Run(c.route, func(t *testing.T) {
			body := get(t, srv, c.route).Body.String()
			if !contains(body, `<nav class="header-tabs" aria-label="Primary"`) {
				t.Fatal("page missing the centered header tab nav")
			}
			if !contains(body, c.wantMessages) || !contains(body, c.wantMedia) {
				t.Errorf("%s should mark %s active (want %q and %q)", c.route, c.activeSummary, c.wantMessages, c.wantMedia)
			}
			// Both tabs are boosted via the nav's hx-boost inheritance.
			navStart := strings.Index(body, `<nav class="header-tabs"`)
			navEnd := strings.Index(body[navStart:], "</nav>")
			if navStart < 0 || navEnd < 0 {
				t.Fatal("cannot delimit the header tab nav")
			}
			if nav := body[navStart : navStart+navEnd]; !strings.Contains(nav, `hx-boost="true"`) {
				t.Error("header tabs should be boosted (hx-boost on the nav)")
			}
		})
	}
}

// TestSelectedRowAccentRail verifies the open conversation's sidebar row carries
// the selected modifier (accent left rail + #1b2330 tint, REQ-0006-003) and that
// non-open rows do not.
func TestSelectedRowAccentRail(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv, err := st.GetConversation(context.Background(), "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}

	open := get(t, srv, "/c/"+itoa(conv.ID)).Body.String()
	if !contains(open, "conv-row-selected") {
		t.Error("open conversation row missing the selected accent rail modifier")
	}
	// The selected modifier must hang off the active conversation's own row link.
	if !contains(open, `href="/c/`+itoa(conv.ID)+`" class="conv-row conv-row-selected"`) {
		t.Errorf("selected modifier not on the active conversation row")
	}

	// On a non-conversation page nothing is selected.
	home := get(t, srv, "/").Body.String()
	if contains(home, "conv-row-selected") {
		t.Error("home page should not mark any conversation row selected")
	}
}

// TestSourceSlug verifies the Go-chosen source modifier classes used by presence
// dots and source pills (REQ-0006-004).
func TestSourceSlug(t *testing.T) {
	cases := map[string]string{
		source.Signal:   "src-signal",
		source.IMessage: "src-imessage",
		source.WhatsApp: "src-whatsapp",
		"":              "src-unknown",
		"bogus":         "src-unknown",
	}
	for in, want := range cases {
		if got := sourceSlug(in); got != want {
			t.Errorf("sourceSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBuiltCSSCarriesShellComponents guards the CSS drift requirement: the
// committed, go:embed-served app.css must contain the bespoke shell + identity
// component rules and the source-derived colors, so the clean rebuild is what
// actually ships (REQ-0006-002/003/004; ADR-0012 drift guard).
func TestBuiltCSSCarriesShellComponents(t *testing.T) {
	css, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	out := string(css)
	for _, want := range []string{
		".app-toolbar",               // unified full-width header (#152 → #190)
		".toolbar-title",             // contextual title
		".toolbar-icon-btn",          // icon buttons (drawer/theme/settings)
		".toolbar-search",            // toolbar search pill
		".toolbar-cluster",           // in-flow header side clusters (#197 review)
		".toolbar-cluster-left",      // truncating (minmax(0,1fr)) title-side cluster
		".toolbar-cluster-right",     // min-content-guaranteed controls cluster
		".header-tabs",               // centered Messages/Media tab strip (#190)
		".header-tab",                // individual tab
		".header-tab-active",         // active-tab lift
		".sidebar-filter",            // filter input
		".avatar-mono",               // monogram avatar
		".presence-dot.src-signal",   // Signal presence dot
		".presence-dot.src-imessage", // iMessage presence dot
		".presence-dot.src-whatsapp", // WhatsApp presence dot (REQ-0009-007)
		".source-pill.src-signal",    // Signal source pill
		".source-pill.src-imessage",  // iMessage source pill
		".source-pill.src-whatsapp",  // WhatsApp source pill (REQ-0009-007)
		".conv-row-selected",         // selected-row modifier
		".pin-btn",                   // pin/unpin toggle (REQ-0006-010)
		".pin-btn-active",            // pinned-state fill
		"--color-info:#3b82f6",       // Signal blue token (slate)
		"--color-success:#34c759",    // iMessage green token (slate)
		"--color-whatsapp:#25d366",   // WhatsApp green token (slate)
		"--color-whatsapp:#128c7e",   // WhatsApp teal-green token (slate-light)
	} {
		if !strings.Contains(out, want) {
			t.Errorf("built app.css missing %q (rebuild: rm -rf .tools && make css)", want)
		}
	}
	// The retired 54px sticky-offset arbitrary-value utility must stay dead:
	// Tailwind v4 scans template AND Go comments, so a literal class token
	// anywhere in the tree would re-emit the rule into the built artifact
	// (#197 review, minor 4).
	if strings.Contains(out, "top:54px") {
		t.Error("built app.css resurrects the retired 54px sticky-offset utility (reword the comment token, then rm -rf .tools && make css)")
	}

	// The presence dot reads its color from the theme variables so both slate and
	// slate-light variants restyle automatically.
	if !strings.Contains(out, ".presence-dot.src-signal{background:var(--color-info)}") {
		t.Error("presence dot should derive its color from --color-info")
	}
	if !strings.Contains(out, ".presence-dot.src-whatsapp{background:var(--color-whatsapp)}") {
		t.Error("whatsapp presence dot should derive its color from --color-whatsapp")
	}
}

// TestPinnedSection covers REQ-0006-010 end to end: with nothing pinned the
// sidebar's PINNED section wrapper renders hidden (it is ALWAYS in the DOM as
// the pin toggle's OOB swap target — #176); POSTing to /c/{id}/pin
// 303-redirects back, marks the conversation pinned, and the sidebar then
// shows the section. A second POST unpins it. The pin button on the
// conversation header reflects the current state.
func TestPinnedSection(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()

	conv, err := st.GetConversation(ctx, "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}
	cid := itoa(conv.ID)

	// Nothing pinned: the wrapper is present but hidden (class, not inline
	// style — the strict CSP forbids the latter), and the header button reads
	// "Pin".
	home := get(t, srv, "/").Body.String()
	if !contains(home, `id="sidebar-pinned-section" class="hidden"`) {
		t.Error("PINNED section wrapper should render hidden when nothing is pinned")
	}
	convPage := get(t, srv, "/c/"+cid).Body.String()
	if !contains(convPage, `action="/c/`+cid+`/pin"`) {
		t.Error("conversation header missing the pin form POST")
	}
	if !contains(convPage, ">Pin<") {
		t.Error("pin button should read 'Pin' when unpinned")
	}

	// Pin via the POST route: expect a 303 redirect back to the conversation.
	rec := post(t, srv, "/c/"+cid+"/pin")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("pin POST status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/c/"+cid {
		t.Errorf("pin redirect = %q, want %q", loc, "/c/"+cid)
	}

	// Sidebar now shows the PINNED section listing the conversation.
	home = get(t, srv, "/").Body.String()
	if contains(home, `id="sidebar-pinned-section" class="hidden"`) {
		t.Error("PINNED section should be visible after pinning")
	}
	if !contains(home, ">Pinned<") || !contains(home, `id="sidebar-pinned"`) {
		t.Error("PINNED list container missing")
	}
	// The pinned row links to the conversation and uses the shared row markup.
	if !contains(home, `<ul id="sidebar-pinned"`) || !contains(home, `href="/c/`+cid+`"`) {
		t.Error("pinned conversation row missing from PINNED section")
	}

	// Header button now reads "Unpin".
	convPage = get(t, srv, "/c/"+cid).Body.String()
	if !contains(convPage, ">Unpin<") {
		t.Error("pin button should read 'Unpin' when pinned")
	}

	// Unpin: second POST flips it back; PINNED section hides again.
	if rec := post(t, srv, "/c/"+cid+"/pin"); rec.Code != http.StatusSeeOther {
		t.Fatalf("unpin POST status = %d, want 303", rec.Code)
	}
	home = get(t, srv, "/").Body.String()
	if !contains(home, `id="sidebar-pinned-section" class="hidden"`) {
		t.Error("PINNED section should hide again after unpinning")
	}
}

// TestThemeStillSelfHosted re-checks ADR-0010/REQ-0006-001: every script the
// shell loads is same-origin, so the strict CSP (script-src 'self') holds with
// the new sidebar.js.
func TestThemeStillSelfHosted(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := get(t, srv, "/")
	body := rec.Body.String()
	for _, src := range []string{"/static/theme.js", "/static/sidebar-toggle.js", "/static/sidebar.js", "/static/shell.js", "/static/htmx.min.js"} {
		if !contains(body, `src="`+src+`"`) {
			t.Errorf("page missing self-hosted script %q", src)
		}
	}
	// No off-origin script/style references slipped in. (SVG xmlns URLs are not
	// fetched, so we check src=/href= attributes specifically.)
	for _, bad := range []string{`src="http://`, `src="https://`, `href="https://cdn`} {
		if contains(body, bad) {
			t.Errorf("page references an off-origin asset (%q); CSP would block it", bad)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !contains(csp, "script-src 'self'") {
		t.Errorf("CSP no longer restricts scripts to self: %q", csp)
	}
}
