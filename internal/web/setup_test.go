package web

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/setup"
	"github.com/joestump/msgbrowse/internal/store"
)

// newEmptyStoreServer builds a Server over a freshly-opened, never-ingested
// store — the SPEC-0013 "empty store / first run" precondition. No archive roots
// are configured, so every source reads as unconfigured (not Enabled).
func newEmptyStoreServer(t *testing.T) *Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "empty.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{DataDir: t.TempDir()}
	srv, err := NewServer(st, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv
}

// signalPlusIMessageHome lays out a temp HOME with Signal Desktop present (with a
// sealed encryptedKey so the keychain probe reports Needs-permission off macOS)
// and the Messages chat.db present, but NO WhatsApp — the SPEC-0013 scenario
// "Signal + iMessage present, no WhatsApp". It returns the HOME path.
func signalPlusIMessageHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	// Signal: application-support dir + a config.json with a sealed key.
	sigDir := filepath.Join(home, "Library", "Application Support", "Signal")
	if err := os.MkdirAll(sigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sigDir, "config.json"),
		[]byte(`{"encryptedKey":"deadbeef"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// iMessage: the chat.db file.
	imDir := filepath.Join(home, "Library", "Messages")
	if err := os.MkdirAll(imDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imDir, "chat.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return home
}

// detectorFor builds a faked Detector rooted at home. imessageReadable controls
// the Full Disk Access probe: when false, Open fails for chat.db so iMessage
// reads Needs-permission; when true it opens so iMessage reads Ready. The Signal
// keychain default (no injected check) reports Needs-permission for the sealed
// key, matching the non-macOS reality.
func detectorFor(home string, imessageReadable bool) setup.Detector {
	return setup.Detector{
		Home: home,
		Open: func(path string) (io.ReadCloser, error) {
			if strings.HasSuffix(path, "chat.db") && !imessageReadable {
				return nil, os.ErrPermission
			}
			return os.Open(path)
		},
	}
}

// TestSetupFullPageRendersCards asserts the full /setup document renders one card
// per source with the expected states for the "Signal + iMessage, no WhatsApp"
// machine: Signal Needs-permission (sealed key), iMessage Needs-permission (no
// FDA), WhatsApp Not-detected. The shell (sidebar/toolbar) is present on the full
// document.
func TestSetupFullPageRendersCards(t *testing.T) {
	srv := newEmptyStoreServer(t)
	srv.SetDetector(detectorFor(signalPlusIMessageHome(t), false))

	rec := get(t, srv, "/providers")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()

	// Full document carries the shell.
	for _, want := range []string{"<!doctype html>", "app-sidebar", "app-toolbar", `id="main-content"`} {
		if !contains(body, want) {
			t.Errorf("full /setup missing %q", want)
		}
	}
	// One card per source, all three present.
	for _, want := range []string{
		`aria-label="Signal: Needs permission"`,
		`aria-label="iMessage: Needs permission"`,
		`aria-label="WhatsApp: Not detected"`,
	} {
		if !contains(body, want) {
			t.Errorf("full /setup missing card %q", want)
		}
	}
	if n := strings.Count(body, `class="setup-card `); n != 3 {
		t.Errorf("full /setup rendered %d source cards, want 3", n)
	}
}

// TestSetupCardStatesReadyEnabledNotDetected drives the remaining two states:
// iMessage Ready (FDA granted → readable chat.db), Signal Enabled (a configured
// archive root short-circuits detection), WhatsApp Not-detected. Together with
// the Needs-permission assertions above, this covers all four card states.
func TestSetupCardStatesReadyEnabledNotDetected(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "empty.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// A configured Signal archive root makes Signal read as Enabled regardless of
	// filesystem detection.
	cfg := &config.Config{DataDir: t.TempDir(), ArchiveRoot: t.TempDir()}
	srv, err := NewServer(st, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	srv.SetDetector(detectorFor(signalPlusIMessageHome(t), true))

	body := get(t, srv, "/providers").Body.String()
	for _, want := range []string{
		`aria-label="Signal: Enabled"`,
		`aria-label="iMessage: Ready"`,
		`aria-label="WhatsApp: Not detected"`,
		"setup-badge-enabled",
		"setup-badge-ready",
		"setup-badge-not-detected",
	} {
		if !contains(body, want) {
			t.Errorf("/setup missing %q", want)
		}
	}
	// The Ready card offers an (Enable) affordance; this story keeps it disabled.
	if !contains(body, `class="setup-btn" disabled`) {
		t.Error("/setup Ready card missing the read-only disabled Enable button")
	}
}

// TestSetupNeedsPermissionBadgeRenders confirms the Needs-permission state
// renders its badge class and text guidance.
func TestSetupNeedsPermissionBadgeRenders(t *testing.T) {
	srv := newEmptyStoreServer(t)
	srv.SetDetector(detectorFor(signalPlusIMessageHome(t), false))
	body := get(t, srv, "/providers").Body.String()
	for _, want := range []string{"setup-badge-needs-permission", "System Settings"} {
		if !contains(body, want) {
			t.Errorf("/setup Needs-permission card missing %q", want)
		}
	}
}

// TestSetupPartialHasNoShell is the SPEC-0008 *_content contract for /setup: a
// boosted (HX-Request) response is <title> + #main-content only — the cards but
// no sidebar/toolbar/document shell.
func TestSetupPartialHasNoShell(t *testing.T) {
	srv := newEmptyStoreServer(t)
	srv.SetDetector(detectorFor(signalPlusIMessageHome(t), false))

	rec := getPartial(t, srv, "/providers")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "<title>") || !contains(body, `id="main-content"`) {
		t.Error("/setup partial missing <title> or #main-content")
	}
	// The cards still render in the partial.
	if !contains(body, "setup-card") {
		t.Error("/setup partial missing source cards")
	}
	for _, forbidden := range []string{"<!doctype", "<html", "app-sidebar", "app-toolbar", "drawer-side"} {
		if contains(strings.ToLower(body), strings.ToLower(forbidden)) {
			t.Errorf("/setup partial leaked shell marker %q", forbidden)
		}
	}
}

// TestSetupA11yLandmarksAndHeading asserts the SPEC-0013 §Accessibility contract:
// a single <h1>, the ARIA landmarks (<main>, <nav>) from the shell, aria-labels
// on the source cards, and the labelled cards list.
func TestSetupA11yLandmarksAndHeading(t *testing.T) {
	srv := newEmptyStoreServer(t)
	srv.SetDetector(detectorFor(signalPlusIMessageHome(t), false))

	body := get(t, srv, "/providers").Body.String()
	if n := strings.Count(body, "<h1"); n != 1 {
		t.Errorf("/setup has %d <h1> elements, want exactly 1", n)
	}
	// Landmarks: the shell's <main id="main-content"> and the header's
	// primary-tab <nav> (#190: the sidebar carries no nav anymore).
	if !contains(body, `<main id="main-content"`) {
		t.Error("/setup missing <main> landmark")
	}
	if !contains(body, `<nav class="header-tabs"`) {
		t.Error("/setup missing the header tab <nav> landmark")
	}
	// The cards list is labelled and each card names its source + state.
	if !contains(body, `aria-label="Message sources"`) {
		t.Error("/setup cards list missing aria-label")
	}
	if !contains(body, `aria-label="Signal:`) {
		t.Error("/setup Signal card missing aria-label")
	}
	// Card icons are decorative (aria-hidden), since the card carries the name.
	if !contains(body, `class="setup-card-icon src-signal" aria-hidden="true"`) {
		t.Error("/setup source icon not marked decorative (aria-hidden)")
	}
}

// TestHeaderNavOmitsSettingsSurfaces is the information-architecture contract,
// #175 as reshaped by #190: the primary nav is the header's Messages/Media tab
// pair (the sidebar carries no nav at all — see TestSidebarListOnly). Settings'
// sole entry is the header gear, and Providers / Logs / Status & backups are
// tabs in the Settings sub-nav — none of them may appear in the primary nav.
func TestHeaderNavOmitsSettingsSurfaces(t *testing.T) {
	srv, _, _ := newTestServer(t)
	body := get(t, srv, "/").Body.String()
	navStart := strings.Index(body, `<nav class="header-tabs"`)
	navEnd := strings.Index(body, "</nav>")
	if navStart < 0 || navEnd < 0 {
		t.Fatal("home missing the header tab <nav>")
	}
	nav := body[navStart:navEnd]
	for _, href := range []string{`href="/"`, `href="/media"`} {
		if !strings.Contains(nav, href) {
			t.Errorf("header nav missing %s", href)
		}
	}
	for _, href := range []string{`href="/providers"`, `href="/settings"`, `href="/logs"`, `href="/status"`, `href="/search"`} {
		if strings.Contains(nav, href) {
			t.Errorf("header nav must not carry %s — it lives under Settings or the search affordances (#175/#190)", href)
		}
	}
	// The header gear stays the one Settings entry.
	if !contains(body, `href="/settings" class="toolbar-icon-btn" aria-label="Settings"`) {
		t.Error("header missing the settings gear (the sole Settings entry)")
	}
}

// TestFirstRunRedirectsToSetup is the SPEC-0013 REQ "First-run wizard versus
// returning launch" empty-store case: GET / against a store with zero
// conversations 303-redirects to /providers (the renamed Setup wizard), in both
// plain and boosted requests.
func TestFirstRunRedirectsToSetup(t *testing.T) {
	srv := newEmptyStoreServer(t)
	srv.SetDetector(detectorFor(signalPlusIMessageHome(t), false))

	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"plain", nil},
		{"boosted", map[string]string{"HX-Request": "true"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := getWith(t, srv, "/", tc.headers)
			if rec.Code != http.StatusSeeOther {
				t.Fatalf("empty store GET / = %d, want 303", rec.Code)
			}
			if loc := rec.Header().Get("Location"); loc != "/providers" {
				t.Errorf("redirect Location = %q, want /providers", loc)
			}
		})
	}
}

// TestReturningLaunchShowsTranscript is the returning case: a configured store
// (the fixture has conversations) renders the transcript home, NOT a redirect,
// and Providers remains reachable — as a Settings sub-nav tab since #175.
func TestReturningLaunchShowsTranscript(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := get(t, srv, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("configured store GET / = %d, want 200 (no redirect)", rec.Code)
	}
	body := rec.Body.String()
	// The home/transcript UI renders (the hero).
	if !contains(body, "home-hero-title") {
		t.Error("configured store did not render the transcript home")
	}
	// Providers is reachable from the Settings shell (#175: it left the sidebar).
	if !contains(get(t, srv, "/settings").Body.String(), `href="/providers"`) {
		t.Error("Settings shell missing the Providers tab")
	}
}

// TestBuiltCSSCarriesSetupComponents guards the ADR-0012 drift rule for the new
// Setup classes: the committed, go:embed-served app.css must carry the setup card
// + badge rules (rebuild: rm -rf .tools && make css).
func TestBuiltCSSCarriesSetupComponents(t *testing.T) {
	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	out := string(css)
	for _, want := range []string{
		".setup-cards",
		".setup-card",
		".setup-card-icon",
		".setup-badge",
		".setup-badge-enabled",
		".setup-badge-ready",
		".setup-badge-needs-permission",
		".setup-badge-not-detected",
		".setup-btn:focus-visible", // visible keyboard focus (WCAG 2.1 AA)
		// Permission-guidance modal (#134): the drift guard must carry the new
		// dialog classes so a stale app.css cannot ship the modal unstyled.
		".setup-guide",
		".setup-guide-panel",
		".setup-guide-backdrop",
		".setup-guide-steps",
		".setup-guide-result",
		".setup-guide-close:focus-visible", // keyboard focus on the close control
		// All-sources Refresh control (#135): the drift guard must carry the new
		// classes so a stale app.css cannot ship the refresh control unstyled.
		".setup-refresh-all",
		".setup-refresh-all-result",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("built app.css missing %q (rebuild: rm -rf .tools && make css)", want)
		}
	}
}
