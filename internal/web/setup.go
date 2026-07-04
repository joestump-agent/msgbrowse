// The in-app Setup surface: one card per messaging source (Signal / iMessage /
// WhatsApp) with a computed state — Enabled / Ready / Needs-permission /
// Not-detected — read from the shared internal/setup detection + permission
// probes plus whether that source already has a configured/imported archive.
// The GET /setup render is read-only (detection cards + first-run routing); the
// per-card Enable/Recheck controls it renders drive the privileged POSTs, which
// live in enable.go (#133) and recheck.go (#134). A Needs-permission card also
// carries its permission-guidance modal (steps + the exact System Settings deep
// link) built here from setup.GuidanceFor — detect-and-guide only (ADR-0020).
//
// Governing: ADR-0020 (self-contained desktop onboarding — the in-app Setup
// surface that renders the source-detection cards; OS consent is detect-and-guide
// only), SPEC-0013 REQ "First-run wizard versus returning launch", REQ "Source
// detection", REQ "Permission detection and guidance", and the §Accessibility
// Requirements (single h1, ARIA landmarks, aria-labels on card icons, state as
// text not color alone).
package web

import (
	"html/template"
	"net/http"

	"github.com/joestump/msgbrowse/internal/setup"
	"github.com/joestump/msgbrowse/internal/source"
)

// Card states, rendered as text (never color alone — SPEC-0013 §Accessibility
// "state MUST be conveyed as text or an accessible name, not by color alone").
// The lowercase token also drives the CSS state class (.setup-card-<state>).
const (
	// setupStateEnabled: the source already has a configured/imported archive —
	// it is live in the app. Terminal, non-actionable in this read-only story.
	setupStateEnabled = "enabled"
	// setupStateReady: detected and accessible — actionable, an Enable will run.
	setupStateReady = "ready"
	// setupStateNeedsPermission: detected but an OS consent grant is missing (Full
	// Disk Access / Signal Keychain / WhatsApp container). The card renders the
	// System Settings guidance modal + a Recheck action (#134); the state is also
	// surfaced as text so a returning user knows what is blocking the source.
	setupStateNeedsPermission = "needs-permission"
	// setupStateNotDetected: the source's well-known local store is absent (always
	// the case off macOS, and for a source the user does not have installed).
	setupStateNotDetected = "not-detected"
)

// setupCard is one source's Setup card: everything the template needs to render
// the card and its action affordance. Every field is a server-computed value or
// a fixed constant — no request-derived content — so html/template escaping is
// the only encoding needed. The same struct renders both inside the full page
// and as the standalone fragment /setup/recheck swaps back (#134), so the card
// carries the per-render Enable availability + token it needs to stand alone.
type setupCard struct {
	// Source is the fixed source id (source.Signal / .IMessage / .WhatsApp).
	Source string
	// Label is the human display name ("Signal", "iMessage", "WhatsApp").
	Label string
	// State is one of the setupState* tokens above; drives both the text badge
	// and the .setup-card-<State> style class.
	State string
	// StateLabel is the human badge text for State ("Ready", "Needs permission",
	// "Not detected", "Enabled").
	StateLabel string
	// Detail is a one-line human explanation of the state (what was found, or what
	// is blocking) — the accessible, color-independent state description.
	Detail string
	// Actionable is true for Ready / Needs-permission — the states where an
	// Enable/Recheck applies. Enabled and Not-detected render no primary action.
	Actionable bool
	// Guidance is the permission-guidance content (steps + System Settings deep
	// link) shown in the guidance modal when State is needs-permission. It is the
	// zero value for every other state (the template gates on State).
	Guidance setup.Guidance
	// EnableAvailable reports whether an Enabler is wired, so the card can render a
	// live vs. disabled Enable button when it renders standalone (recheck swap).
	EnableAvailable bool
	// Token is the fresh per-session token this render minted, carried on the
	// card's Enable/Recheck POST controls (SPEC-0013 §Security).
	Token string
}

// HasSettingsLink reports whether the card's guidance carries a System Settings
// deep link, for the template to decide whether to render the deep-link control
// (html/template cannot compare a struct field to "" inline cleanly across the
// modal define). The Signal Keychain case has no pane, so this is false there.
func (c setupCard) HasSettingsLink() bool { return c.Guidance.SettingsURL != "" }

// SettingsURL returns the guidance's System Settings deep link typed as a
// template.URL so html/template does not sanitize the `x-apple.systempreferences:`
// scheme to "#ZgotmplZ". This is SAFE because the value is a fixed, app-owned
// constant (setup.FullDiskAccessDeepLink) — never request-derived — so bypassing
// the URL-scheme guard introduces no injection surface. Called only for cards
// that HasSettingsLink; empty otherwise.
func (c setupCard) SettingsURL() template.URL {
	// #nosec G203 -- app-owned constant deep link, not user input.
	return template.URL(c.Guidance.SettingsURL)
}

// setupData drives the /setup page. It embeds baseData so the shell (navbar +
// sidebar) renders in the full document, and carries the per-source cards plus a
// count of actionable cards for the intro copy.
type setupData struct {
	baseData
	// Cards is one card per supported source, in source.All order (the SPEC-0013
	// "one card per source" contract).
	Cards []setupCard
	// AnyActionable is true when at least one card is Ready or Needs-permission,
	// so the intro can nudge the user toward an action (vs. a pure returning-user
	// view where everything is Enabled/Not-detected).
	AnyActionable bool
	// AnyEnabled is true when at least one source is Enabled, so the page renders
	// the all-sources Refresh control (SPEC-0013 REQ "Refresh": "a single control
	// that refreshes every enabled source"). Hidden when nothing is Enabled.
	AnyEnabled bool
	// EnableAvailable reports whether an Enabler is wired (desktop bundle or a
	// configured $PATH resolver): true renders live Enable buttons, false renders
	// the "desktop app required / configure tools" affordance (SPEC-0013).
	EnableAvailable bool
	// Token is a fresh per-session token minted for this render, embedded in the
	// page and submitted with every privileged Setup POST (SPEC-0013 §Security).
	Token string
}

// handleSetup renders the Setup surface: the per-source detection cards. GET-only
// (the route pattern enforces it); it performs read-only filesystem detection and
// permission probes via internal/setup and never mutates state or spawns a
// subprocess (SPEC-0013 §Security "GET routes are safe … no mutation").
//
// It follows the SPEC-0008 *_content partial pattern: a boosted navigation
// (HX-Request) gets only <title> + #main-content via the setup_content define, so
// no sidebar markup or store work rides along.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	var base baseData
	if isPartialRequest(r) {
		// Boosted swap: no sidebar listing, no store work (SPEC-0008 REQ-0008-006).
		base = partialBase("Setup · msgbrowse", 0)
	} else {
		var err error
		base, err = s.baseData(r.Context(), "Setup · msgbrowse", 0)
		if err != nil {
			s.serverError(w, err)
			return
		}
	}

	// Mint a fresh per-session token for the privileged Setup POSTs this page can
	// trigger (SPEC-0013 §Security: minted at /setup render, submitted with the
	// POST). A mint failure (crypto/rand) is a real server error, not a silent
	// degrade — an Enable without a token would then always 403.
	token, err := s.setupTokens.mint()
	if err != nil {
		s.serverError(w, err)
		return
	}
	cards := s.setupCards(token)
	anyActionable := false
	anyEnabled := false
	for _, c := range cards {
		if c.Actionable {
			anyActionable = true
		}
		if c.State == setupStateEnabled {
			anyEnabled = true
		}
	}
	s.render(w, r, "setup", setupData{
		baseData:        base,
		Cards:           cards,
		AnyActionable:   anyActionable,
		AnyEnabled:      anyEnabled,
		EnableAvailable: s.enableAvailable(),
		Token:           token,
	})
}

// setupCards computes one card per supported source from the shared detection +
// permission probes and the app-owned "already configured" signal. The result is
// in source.All order so the page always renders exactly three cards in a stable
// order.
//
// State precedence (SPEC-0013 REQ "Source detection" four states):
//   - Enabled       — the source already has a configured/imported archive
//     (config archive root set); it is live, so detection is moot.
//   - Not-detected  — the well-known local store is absent.
//   - Needs-permission — detected, but the OS consent probe reports Needed.
//   - Ready         — detected and accessible.
func (s *Server) setupCards(token string) []setupCard {
	det := s.detector()
	cards := make([]setupCard, 0, len(source.All))
	for _, src := range source.All {
		cards = append(cards, s.setupCardFor(det, src, token))
	}
	return cards
}

// setupCardFor builds a single source's card. Enabled short-circuits detection:
// a configured archive means the source is already live in the app regardless of
// what the current machine's filesystem probes report (the archive may have been
// imported on another run/machine).
func (s *Server) setupCardFor(det setup.Detector, src, token string) setupCard {
	card := setupCard{
		Source:          src,
		Label:           source.Label(src),
		EnableAvailable: s.enableAvailable(),
		Token:           token,
	}

	if s.sourceConfigured(src) {
		card.State = setupStateEnabled
		card.StateLabel = "Enabled"
		card.Detail = "This source is enabled and its archive is imported."
		card.Actionable = false
		return card
	}

	detection := detectSource(det, src)
	if detection.State != setup.Detected {
		card.State = setupStateNotDetected
		card.StateLabel = "Not detected"
		card.Detail = "No " + source.Label(src) + " data was found on this machine."
		card.Actionable = false
		return card
	}

	switch probeSource(det, src).State {
	case setup.PermissionNeeded:
		card.State = setupStateNeedsPermission
		card.StateLabel = "Needs permission"
		card.Detail = source.Label(src) + " was found, but macOS has not granted access yet."
		card.Actionable = true
		// Attach the source-specific guidance (steps + System Settings deep link)
		// so the modal can render it. Detect-and-guide only (ADR-0020): guidance
		// never bypasses the grant, and the Recheck action re-runs the probe.
		card.Guidance = setup.GuidanceFor(src)
	default:
		// PermissionOK or PermissionNotApplicable: the source is present and
		// accessible (a source with no applicable OS gate, e.g. Signal without a
		// sealed key, reports NotApplicable and is Ready).
		card.State = setupStateReady
		card.StateLabel = "Ready"
		card.Detail = source.Label(src) + " was found and is ready to enable."
		card.Actionable = true
	}
	return card
}

// detectSource runs the source-appropriate presence detection.
func detectSource(det setup.Detector, src string) setup.Detection {
	switch src {
	case source.Signal:
		return det.DetectSignal()
	case source.IMessage:
		return det.DetectIMessage()
	case source.WhatsApp:
		return det.DetectWhatsApp()
	default:
		return setup.Detection{Source: src, State: setup.NotDetected}
	}
}

// probeSource runs the source-appropriate OS-permission probe.
func probeSource(det setup.Detector, src string) setup.PermissionProbe {
	switch src {
	case source.Signal:
		return det.ProbeSignalKeychain()
	case source.IMessage:
		return det.ProbeFullDiskAccess()
	case source.WhatsApp:
		return det.ProbeWhatsAppContainer()
	default:
		return setup.PermissionProbe{Source: src, State: setup.PermissionNotApplicable}
	}
}

// sourceConfigured reports whether a source already has a configured archive
// root — the app-owned "Enabled" signal. It reads only the server's own captured
// config values, never a request-derived path.
func (s *Server) sourceConfigured(src string) bool {
	switch src {
	case source.Signal:
		return s.roots.Signal != ""
	case source.IMessage:
		return s.roots.IMessage != ""
	case source.WhatsApp:
		return s.roots.WhatsApp != ""
	default:
		return false
	}
}

// detector returns the Server's source detector, defaulting to a real
// HOME-rooted one. Tests inject a faked Detector (a temp HOME + faked
// Stat/Open/Keychain) via SetDetector to drive each card state deterministically
// on a non-macOS CI box.
func (s *Server) detector() setup.Detector {
	if s.setupDetector != nil {
		return *s.setupDetector
	}
	return setup.NewDetector()
}

// SetDetector overrides the source detector used by /setup. Call it after
// NewServer and before serving; handlers read the field without locking, so late
// wiring would race. It exists for tests (faked HOME) and for the desktop layer
// (#134) to inject the genuine macOS Keychain check.
func (s *Server) SetDetector(d setup.Detector) { s.setupDetector = &d }
