// Styled in-app error pages for the media handler (issue #161). Clicking an
// attachment navigates the webview/browser to /media/..., so a failure there
// used to strand the user on a bare "invalid path" text response with no app
// chrome and no way back. These renders keep the HTTP status code intact while
// returning a real page — shell, message, and a link back — so a dead
// attachment is a dead end no longer.
//
// Every value rendered is server-composed (fixed headings/details chosen by the
// handler, never request-derived text), so html/template escaping is the only
// encoding needed. Governing: SPEC-0006 (UI regions — the shell is the same
// page_start/page_end chrome every page uses), issue #161.
package web

import (
	"bytes"
	"net/http"
)

// errorPageData drives the error page: the shell chrome (baseData) plus the
// fixed, server-chosen heading/detail and the HTTP status it accompanies.
type errorPageData struct {
	baseData
	Status  int
	Heading string
	Detail  string
}

// mediaError renders the styled error page with the given status code. The
// sidebar chrome comes from baseData when the store cooperates; a store error
// degrades to an empty shell (never a bare text response) — the status code
// and message still land either way. As a last resort (template failure) it
// falls back to http.Error so the client always gets the right status.
func (s *Server) mediaError(w http.ResponseWriter, r *http.Request, status int, heading, detail string) {
	base, err := s.baseData(r.Context(), heading+" · msgbrowse", 0)
	if err != nil {
		// Degrade to an empty shell: chrome without the sidebar listing.
		s.log.Warn("media error page: could not load sidebar", "error", err)
		base = partialBase(heading+" · msgbrowse", 0)
	}
	data := errorPageData{baseData: base, Status: status, Heading: heading, Detail: detail}
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, "error", data); err != nil {
		s.log.Error("media error page render failed", "error", err)
		http.Error(w, detail, status)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}
