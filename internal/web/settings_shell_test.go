package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The issue-#163 acceptance, extended by #175 with the Providers tab and by
// #191 with the LLM tab: Settings, Providers, Logs, Status & backups, and LLM
// render as one shell with sub-navigation — each page carries the shared h1 +
// the boosted sub-nav with its own tab active — while the old routes stay the
// canonical, working URLs.

func TestSettingsShellSubNav(t *testing.T) {
	srv, _, _ := newTestServer(t)
	cases := []struct {
		route     string
		activeTab string
	}{
		{"/settings", `href="/settings" class="settings-tab settings-tab-active"`},
		{"/providers", `href="/providers" class="settings-tab settings-tab-active"`},
		{"/logs", `href="/logs" class="settings-tab settings-tab-active"`},
		{"/status", `href="/status" class="settings-tab settings-tab-active"`},
		{"/settings/llm", `href="/settings/llm" class="settings-tab settings-tab-active"`},
	}
	for _, c := range cases {
		t.Run(c.route, func(t *testing.T) {
			rec := get(t, srv, c.route)
			if rec.Code != 200 {
				t.Fatalf("status = %d", rec.Code)
			}
			body := rec.Body.String()
			if !contains(body, "settings-subnav") {
				t.Fatal("page missing the settings sub-nav")
			}
			if !contains(body, c.activeTab) {
				t.Errorf("page missing its active tab marker %q", c.activeTab)
			}
			if !contains(body, `aria-current="page"`) {
				t.Errorf("active tab missing aria-current")
			}
			// The shared shell h1.
			if !contains(body, `<h1 class="screen-h1">Settings</h1>`) {
				t.Errorf("page missing the shared Settings shell h1")
			}
			// All five sections stay reachable from every tab.
			for _, href := range []string{`href="/settings"`, `href="/providers"`, `href="/logs"`, `href="/status"`, `href="/settings/llm"`} {
				if !contains(body, href) {
					t.Errorf("sub-nav missing %s", href)
				}
			}
			// Exactly the five tabs, providers second (#175), LLM last (#191).
			if n := strings.Count(body, `class="settings-tab`); n != 5 {
				t.Errorf("sub-nav has %d tabs, want 5", n)
			}
			if llmAt, statusAt := strings.Index(body, `href="/settings/llm"`), strings.Index(body, `href="/status"`); llmAt < statusAt {
				t.Error("LLM tab should be the LAST sub-nav tab (after Status & backups)")
			}
			// Exactly one h1 per page (accessibility: single h1).
			if n := strings.Count(body, "<h1"); n != 1 {
				t.Errorf("page has %d h1 elements, want 1", n)
			}
		})
	}
}

// TestBuiltCSSCarriesSettingsShell guards the ADR-0012 drift rule for the new
// sub-nav + Providers polish classes: the committed app.css must carry them
// (rebuild: rm -rf .tools && make css).
func TestBuiltCSSCarriesSettingsShell(t *testing.T) {
	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	out := string(css)
	for _, want := range []string{
		".settings-subnav",
		".settings-tab",
		".settings-tab-active",
		".setup-iconbtn",
		".setup-btn-danger",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("built app.css missing %q (rebuild: rm -rf .tools && make css)", want)
		}
	}
}

// TestStatusSnapshotsConditional is the issue-#164 acceptance: with no
// snapshots recorded and no .snapshots dir in the signal archive (the
// desktop-onboarded shape — newManagedRootServer's temp managed root), the
// Encrypted-DB-snapshots card is replaced by one neutral line; with a
// .snapshots dir present, the card renders.
func TestStatusSnapshotsConditional(t *testing.T) {
	srv, _, managed := newManagedRootServer(t)

	body := get(t, srv, "/status").Body.String()
	if contains(body, "Encrypted DB snapshots") {
		t.Error("/status rendered the snapshots card with no snapshot pipeline")
	}
	if !contains(body, "No snapshot pipeline on this machine.") {
		t.Error("/status missing the neutral no-pipeline line")
	}

	// Grow a .snapshots dir in the (temp) archive: the pipeline exists, so the
	// card renders even before any snapshot rows are ingested.
	if err := os.MkdirAll(filepath.Join(managed, ".snapshots"), 0o755); err != nil {
		t.Fatal(err)
	}
	body = get(t, srv, "/status").Body.String()
	if !contains(body, "Encrypted DB snapshots") {
		t.Error("/status hid the snapshots card despite a .snapshots dir in the archive")
	}
	if contains(body, "No snapshot pipeline on this machine.") {
		t.Error("/status kept the no-pipeline line beside the snapshots card")
	}
}
