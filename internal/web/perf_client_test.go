package web

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestBuiltCSSContainment guards SPEC-0008 REQ-0008-011 against CSS drift: the
// committed, go:embed-served app.css must carry the render-containment rules
// for sidebar conversation rows and transcript rows, plus the escape hatch that
// forces jump-to-context target rows visible so #m{id} anchors land and the
// .target flash renders immediately. Exact minified strings are intentional —
// the toolchain is pinned and CI rebuilds byte-exact (ADR-0012 drift guard).
func TestBuiltCSSContainment(t *testing.T) {
	css, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	out := string(css)
	for _, want := range []string{
		// Sidebar rows: skip style/layout/paint off-screen, reserve ~52px.
		".conv-item{content-visibility:auto;contain-intrinsic-size:auto 52px}",
		// Transcript rows + system events: skip off-screen, reserve ~2.2rem.
		".msg-row,.sys-event{content-visibility:auto;contain-intrinsic-size:auto 2.2rem}",
		// Jump-to-context targets render immediately (server-set .target and
		// URL-fragment :target) so anchors + the flash keep working.
		".msg-row.target,.sys-event.target,.msg-row:target,.sys-event:target{content-visibility:visible}",
		// Theme-switch guard: theme.js flips data-theme under this class.
		".theme-switching *{transition:none!important}",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("built app.css missing %q (rebuild: rm -rf .tools && make css)", want)
		}
	}
}

// TestBuiltCSSReducedMotionGate verifies the 13 hand-written transitions are
// gated behind @media (prefers-reduced-motion: no-preference) in the built CSS
// (SPEC-0008 REQ-0008-011). Each selector's transition must appear inside a
// gated block, and no hand-written rule may declare an ungated transition.
func TestBuiltCSSReducedMotionGate(t *testing.T) {
	css, err := os.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	out := string(css)

	const gate = "@media (prefers-reduced-motion:no-preference)"
	for _, sel := range []string{
		".navbar-toggle",
		".sidebar-link",
		".conv-row",
		".pin-btn",
		".attach-chip",
		".link-pill",
		".link-card",
		".toggle-chip",
		".btn-search",
		".result-card",
		".media-tab",
		".media-tile",
		".media-list-card",
	} {
		gated := gate + "{" + sel + "{transition:"
		if !strings.Contains(out, gated) {
			t.Errorf("built app.css missing reduced-motion gate for %s (want %q; rebuild: rm -rf .tools && make css)", sel, gated)
		}
		// No ungated rule for this selector may declare a transition: walk every
		// `SEL{...}` block and require any transition inside it to be the one
		// wrapped by the media gate above.
		for idx := 0; ; {
			j := strings.Index(out[idx:], sel+"{")
			if j < 0 {
				break
			}
			pos := idx + j
			idx = pos + len(sel) + 1
			end := strings.Index(out[pos:], "}")
			if end < 0 {
				break
			}
			body := out[pos : pos+end]
			if strings.Contains(body, "transition:") && !strings.HasSuffix(out[:pos], gate+"{") {
				t.Errorf("built app.css has an ungated transition on %s: %q", sel, body)
			}
		}
	}
}

// TestTranscriptAnchorsSurviveContainment covers the regression called out in
// SPEC-0008 REQ-0008-011 / issue #78: with content-visibility containment on
// .msg-row/.sys-event, the transcript must still render every message with its
// id="m{ID}" anchor, and the jump-to-context view must still mark the target
// row (the CSS above forces it visible so the browser scrolls to it and plays
// the flash).
func TestTranscriptAnchorsSurviveContainment(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()

	conv, err := st.GetConversation(ctx, "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}

	page, err := st.GetMessages(ctx, conv.ID, 0, 0, 50, true)
	if err != nil || len(page.Messages) == 0 {
		t.Fatalf("get messages: %v (%d rows)", err, len(page.Messages))
	}

	rec := get(t, srv, "/c/"+itoa(conv.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()

	// Every rendered message row keeps its m{ID} anchor.
	for _, m := range page.Messages {
		if !contains(body, `id="m`+itoa(m.ID)+`"`) {
			t.Errorf("transcript missing anchor id=\"m%d\"", m.ID)
		}
	}
	// The contained row classes are what the anchors hang off.
	if !contains(body, `class="msg-row`) {
		t.Error("transcript missing .msg-row rows")
	}

	// Jump-to-context still marks the target row on its anchor element.
	mid := page.Messages[0].ID
	jump := get(t, srv, "/c/"+itoa(conv.ID)+"/at/"+itoa(mid))
	if jump.Code != http.StatusOK {
		t.Fatalf("jump status = %d", jump.Code)
	}
	jbody := jump.Body.String()
	if !contains(jbody, ` target" id="m`+itoa(mid)+`"`) {
		t.Errorf("jump view does not mark message %d with the .target flash class on its anchor row", mid)
	}
}

// TestThemeAndSidebarScriptsCarryPerfHooks pins the JS half of REQ-0008-011/012
// with the same string-assertion style as the CSS drift guard: theme.js must
// wrap the data-theme flip in the .theme-switching guard, and sidebar.js must
// coalesce filter input through requestAnimationFrame.
func TestThemeAndSidebarScriptsCarryPerfHooks(t *testing.T) {
	theme, err := os.ReadFile("static/theme.js")
	if err != nil {
		t.Fatalf("read theme.js: %v", err)
	}
	if !strings.Contains(string(theme), `classList.add("theme-switching")`) ||
		!strings.Contains(string(theme), `classList.remove("theme-switching")`) {
		t.Error("theme.js missing the theme-switching transition guard (REQ-0008-011)")
	}

	sidebar, err := os.ReadFile("static/sidebar.js")
	if err != nil {
		t.Fatalf("read sidebar.js: %v", err)
	}
	if !strings.Contains(string(sidebar), "requestAnimationFrame") {
		t.Error("sidebar.js missing rAF input coalescing (REQ-0008-012)")
	}
}
