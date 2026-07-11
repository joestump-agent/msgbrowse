package web

import (
	"strings"
	"testing"
)

// The issue-#163 acceptance, extended by #175 with the Providers tab, by #191
// with the LLM tab, by #2 with the Backups tab, and by #12 with the Contacts
// tab: Settings, Providers, Logs, Status, Backups, LLM, and Contacts render as
// one shell with sub-navigation — each page carries the shared h1 + the boosted
// sub-nav with its own tab active — while the old routes stay the canonical,
// working URLs.

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
		{"/backups", `href="/backups" class="settings-tab settings-tab-active"`},
		{"/settings/llm", `href="/settings/llm" class="settings-tab settings-tab-active"`},
		{"/settings/contacts", `href="/settings/contacts" class="settings-tab settings-tab-active"`},
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
			// All seven sections stay reachable from every tab.
			for _, href := range []string{`href="/settings"`, `href="/providers"`, `href="/logs"`, `href="/status"`, `href="/backups"`, `href="/settings/llm"`, `href="/settings/contacts"`} {
				if !contains(body, href) {
					t.Errorf("sub-nav missing %s", href)
				}
			}
			// Exactly the seven tabs, providers second (#175), Backups after
			// Status (#2), LLM after Backups (#191), Contacts last (#12).
			if n := strings.Count(body, `class="settings-tab`); n != 7 {
				t.Errorf("sub-nav has %d tabs, want 7", n)
			}
			backupsAt, statusAt := strings.Index(body, `href="/backups"`), strings.Index(body, `href="/status"`)
			if backupsAt < statusAt {
				t.Error("Backups tab should follow the Status tab (#2)")
			}
			llmAt := strings.Index(body, `href="/settings/llm"`)
			if llmAt < backupsAt {
				t.Error("LLM tab should follow the Backups tab (#191)")
			}
			if contactsAt := strings.Index(body, `href="/settings/contacts"`); contactsAt < llmAt {
				t.Error("Contacts tab should be the LAST sub-nav tab (after LLM, #12)")
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
