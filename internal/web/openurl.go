// The desktop shell's external-link bridge (issue #179). The shell's webview
// registers no new-window handler — Wails v2 installs none on WKWebView — so
// a target="_blank" navigation to another origin is silently dropped, and the
// pages are served over loopback WITHOUT the Wails JS runtime (SPEC-0010
// design decision), so no runtime-side opener exists either. desktop.js
// intercepts cross-origin link clicks on desktop-chrome pages and POSTs the
// URL here; the handler validates it and hands it to the shell-wired opener,
// which launches the OS default browser.
//
// The endpoint is desktop-only and privileged in the "drive the user's
// browser to an attacker URL" sense, so it carries checkSetupPOST's rigor
// minus the render-minted token (link anchors are on every page; there is no
// single privileged render to mint from): the same layered same-origin check
// (Origin, then Sec-Fetch-Site, then Referer), the same small body cap, and a
// strict URL allowlist — absolute http/https only, no control characters,
// bounded length. The URL is never echoed back into a response; logs carry
// only a scheme://host reduction (opener failures) or a sanitized, truncated
// prefix (validation rejections, debug level).
package web

import (
	"net/http"
	"net/url"
)

// openURLMaxLen caps the submitted URL. Real message links are far shorter;
// the cap bounds parse work and keeps the (already reduced) log field small —
// the same KB-not-MB posture as setupBodyLimit.
const openURLMaxLen = 2048

// SetExternalOpener wires the desktop shell's open-in-default-browser action
// into POST /desktop/open-url (issue #179). fn receives an already-validated
// absolute http(s) URL and is called from request goroutines, so it must be
// safe for concurrent use. Call before serving (the SetDetector /
// SetDesktopChrome wiring contract) — the mux is rebuilt here, not swapped
// under live requests. With no opener wired the route is never registered:
// browser mode has no such endpoint and answers 404 on every method, rather
// than a 405 advertising a POST surface that doesn't exist there.
func (s *Server) SetExternalOpener(fn func(url string) error) {
	s.externalOpener = fn
	s.mux = s.routes()
}

// handleOpenURL is POST /desktop/open-url: validate a clicked external link
// and open it in the OS default browser via the shell-wired opener.
func (s *Server) handleOpenURL(w http.ResponseWriter, r *http.Request) {
	// The bridge exists only inside the desktop shell: an opener wired by the
	// shell AND the desktop-chrome presentation flag that also gates the
	// desktop.js interceptor. In plain `msgbrowse serve` the route is never
	// registered (SetExternalOpener rebuilds the mux), so the nil-opener arm
	// is defense-in-depth; a desktop build whose platform never set
	// desktop-chrome still lands here — the endpoint does not exist: 404,
	// not 403.
	if s.externalOpener == nil || !s.desktopChrome {
		http.NotFound(w, r)
		return
	}

	// Body cap before any parse, then the layered same-origin check shared
	// with the Setup POSTs — a cross-origin page must not be able to drive
	// the user's browser (SPEC-0013 §Security posture, applied here).
	r.Body = http.MaxBytesReader(w, r.Body, setupBodyLimit)
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	raw := r.PostFormValue("url")
	if !validExternalURL(raw) {
		// Debug-level trace so a "clicked and nothing happened" report is
		// diagnosable from the logs (run with --verbose): sanitized + truncated
		// via rejectedURLForLog, never the raw value. The response stays a
		// fixed string — the submitted URL is never reflected back.
		s.log.Debug("external url rejected", "url", rejectedURLForLog(raw), "len", len(raw))
		http.Error(w, "invalid url", http.StatusBadRequest)
		return
	}
	if err := s.externalOpener(raw); err != nil {
		s.log.Error("open external url failed", "error", err, "url", externalURLForLog(raw))
		http.Error(w, "open failed", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validExternalURL admits exactly the URLs the opener may hand to the OS: an
// absolute http or https URL with a host, no control characters, within
// openURLMaxLen. Everything else — file:, javascript:, data:, custom schemes,
// scheme-relative or path-only values — is rejected; message links are
// attacker-influenceable input and the opener launches a browser with them.
func validExternalURL(raw string) bool {
	if raw == "" || len(raw) > openURLMaxLen {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < 0x20 || raw[i] == 0x7f {
			return false
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	// url.Parse lowercases the scheme, so this also admits "HTTPS://…" —
	// browsers treat schemes case-insensitively and normalize the same way.
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return u.Host != ""
}

// externalURLForLog reduces an already-validated URL to scheme://host: enough
// to correlate an opener failure without writing full message links — which
// can carry paths, queries, and tokens — into the log.
func externalURLForLog(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable)"
	}
	return u.Scheme + "://" + u.Host
}

// rejectedURLForLog renders a value that FAILED validation safely for the
// debug log. It cannot lean on url.Parse the way externalURLForLog does —
// the value may not parse at all — so it sanitizes bytewise: control bytes
// (the very thing validExternalURL rejects) become '?', and the value is
// truncated to a short prefix, enough to show the offending scheme or shape
// without writing a full attacker-shaped string — or a full message link —
// into the log.
func rejectedURLForLog(raw string) string {
	const max = 48
	truncated := len(raw) > max
	if truncated {
		raw = raw[:max]
	}
	b := []byte(raw)
	for i, c := range b {
		if c < 0x20 || c == 0x7f {
			b[i] = '?'
		}
	}
	if truncated {
		return string(b) + "..."
	}
	return string(b)
}
