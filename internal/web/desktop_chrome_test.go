// Tests for the desktop-shell presentation seams the web layer exposes
// (SPEC-0010, issues #165/#167): the SetDesktopChrome template flag that pads
// the unified toolbar past the macOS traffic lights and loads the drag-region
// script, and the SetShellNotes provider that surfaces systray/dock startup
// diagnostics on the Logs page. All headless, CGO_ENABLED=0.
package web

import (
	"testing"
	"time"
)

// TestDesktopChromeFlagRendersBodyClassAndScript verifies the minimal
// template-flag mechanism (#165): with SetDesktopChrome(true) every full-page
// render carries the desktop-chrome <body> class and the /static/desktop.js
// include; without it (browser mode) neither appears.
func TestDesktopChromeFlagRendersBodyClassAndScript(t *testing.T) {
	srv, _, _ := newTestServer(t)

	body := get(t, srv, "/").Body.String()
	if contains(body, "desktop-chrome") {
		t.Error("browser mode must not carry the desktop-chrome body class")
	}
	if contains(body, "/static/desktop.js") {
		t.Error("browser mode must not load desktop.js")
	}

	srv.SetDesktopChrome(true)
	body = get(t, srv, "/").Body.String()
	if !contains(body, `class="bg-base-100 text-base-content desktop-chrome"`) {
		t.Error("desktop mode should add the desktop-chrome class to <body>")
	}
	if !contains(body, `<script src="/static/desktop.js" defer></script>`) {
		t.Error("desktop mode should load the drag-region script desktop.js")
	}
}

// TestDesktopChromeOnEveryFullPage confirms the flag flows through every
// full-page surface (they all share page_start), so the traffic-light inset
// never disappears when navigating without HTMX.
func TestDesktopChromeOnEveryFullPage(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetDesktopChrome(true)
	for _, path := range []string{"/", "/search", "/gallery", "/settings", "/logs", "/status", "/providers"} {
		body := get(t, srv, path).Body.String()
		if !contains(body, "desktop-chrome") {
			t.Errorf("GET %s missing the desktop-chrome body class", path)
		}
	}
}

// TestDesktopDragRegionInBuiltCSS guards the committed app.css against drift
// (the css.yml check rebuilds it, this fails earlier and locally): the built
// stylesheet must carry the header drag region, the no-drag opt-outs for its
// interactive children, and the unconditional traffic-light inset scoped to
// desktop-chrome — the header spans the full window at every width (#190), so
// it always owns the top-left corner and the old drawer-state-dependent
// sidebar rules are gone.
func TestDesktopDragRegionInBuiltCSS(t *testing.T) {
	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read embedded app.css: %v", err)
	}
	s := string(css)
	for _, want := range []string{
		"--wails-draggable:drag",
		"--wails-draggable:no-drag",
		".desktop-chrome .app-toolbar{padding-left:80px}",
	} {
		if !contains(s, want) {
			t.Errorf("built app.css missing %q — run `rm -rf .tools && make css` after editing input.css", want)
		}
	}
	// #190 retired the corner-ownership handoff: no rule may reference the
	// sidebar under desktop-chrome anymore.
	if contains(s, ".desktop-chrome .app-sidebar") {
		t.Error("built app.css still carries .desktop-chrome .app-sidebar rules — the #190 full-width header owns the traffic-light corner unconditionally")
	}
}

// TestDrawerToggleNarrowOnly is the #190 burger contract, replacing the #175
// all-widths one: the header burger exists ONLY below md (the phone-width
// overlay-drawer affordance) as a keyboard-operable control, the drawer pins
// open at md+ (~1000px desktop windows must never see a burger), and the #175
// lg+ persistent-collapse feature is fully retired from the built CSS.
func TestDrawerToggleNarrowOnly(t *testing.T) {
	srv, _, _ := newTestServer(t)
	body := get(t, srv, "/").Body.String()

	// md:hidden gates the burger to narrow viewports; the drawer starts closed
	// there, so aria-expanded begins false (sidebar-toggle.js tracks it).
	if !contains(body, `<label for="nav-drawer" class="toolbar-icon-btn md:hidden" role="button" tabindex="0" aria-expanded="false" aria-label="Toggle sidebar">`) {
		t.Error("header burger should be a keyboard-operable, md:hidden drawer toggle")
	}
	// The drawer pins open at md+, not lg+ (#190 moved the breakpoint so a
	// ~1000px window keeps the sidebar with no burger). The retired lg token is
	// spelled "lg:"+"drawer-open" so Tailwind v4's content scanner (which scans
	// .go files too) never sees it here — a plain literal would resurrect the
	// utility in a rebuilt app.css and trip the CI drift guard.
	if !contains(body, `class="app-shell drawer md:drawer-open"`) {
		t.Error("shell drawer should pin the sidebar open at md+ (md:drawer-open)")
	}
	if retired := "lg:" + "drawer-open"; contains(body, retired) {
		t.Errorf("shell still uses the retired %s breakpoint", retired)
	}
	if !contains(body, `<script src="/static/sidebar-toggle.js" defer></script>`) {
		t.Error("shell missing sidebar-toggle.js (the drawer toggle's keyboard/aria wiring)")
	}

	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read embedded app.css: %v", err)
	}
	s := string(css)
	// The #175 collapse CSS (html.sidebar-collapsed …) must be gone.
	if contains(s, "sidebar-collapsed") {
		t.Error("built app.css still carries the retired #175 sidebar-collapse rules — run `rm -rf .tools && make css` after editing input.css")
	}
	// The md drawer-open variant must have been generated for the new shell.
	if !contains(s, "md\\:drawer-open") {
		t.Error("built app.css missing the md:drawer-open drawer rules — run `rm -rf .tools && make css` after editing the templates")
	}
}

// TestLogsPageRendersShellNotes verifies the #167 observability contract: the
// desktop shell's diagnostics reach the Logs page, errors carry the failed
// badge, and browser mode (no provider) renders no shell section.
func TestLogsPageRendersShellNotes(t *testing.T) {
	srv, _, _ := newTestServer(t)

	body := get(t, srv, "/logs").Body.String()
	if contains(body, "Desktop shell") {
		t.Error("browser mode (no provider) must not render the Desktop shell section")
	}

	when := time.Date(2026, 7, 4, 9, 30, 5, 0, time.UTC)
	srv.SetShellNotes(func() []ShellNote {
		return []ShellNote{
			{Time: when, Level: ShellNoteInfo, Message: "menubar: status item registered"},
			{Time: when.Add(time.Second), Level: ShellNoteError, Message: "menubar: status item did not register"},
		}
	})
	body = get(t, srv, "/logs").Body.String()
	if !contains(body, "Desktop shell") {
		t.Error("Logs page missing the Desktop shell section with a provider wired")
	}
	if !contains(body, "menubar: status item registered") {
		t.Error("Logs page missing the info note")
	}
	if !contains(body, "menubar: status item did not register") {
		t.Error("Logs page missing the error note")
	}
	if !contains(body, "09:30:05") {
		t.Error("Logs page should render note clock times")
	}
	// Errors are conveyed as a text badge (never color alone).
	if !contains(body, `log-badge log-badge-failed">error</span>`) {
		t.Error("error notes should carry the failed text badge")
	}
}

// TestLogsEntriesFragmentSkipsShellNotes pins the fragment contract: the
// self-polling entries swap replaces only the per-source job panels, so the
// shell section (rendered outside logs_entries) is never duplicated or
// clobbered by a poll.
func TestLogsEntriesFragmentSkipsShellNotes(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetShellNotes(func() []ShellNote {
		return []ShellNote{{Time: time.Now(), Level: ShellNoteInfo, Message: "menubar: status item registered"}}
	})
	frag := get(t, srv, "/logs?fragment=entries").Body.String()
	if contains(frag, "Desktop shell") {
		t.Error("the logs_entries fragment must not include the Desktop shell section")
	}
}
