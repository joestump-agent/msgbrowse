//go:build desktop

package main

import (
	"fmt"
	"net/http"
)

// bootstrapHandler serves the one-shot trampoline page the webview loads from
// the Wails asset scheme before anything else. Its only job is to point the
// window at the embedded server's loopback URL: every real request flows over
// loopback HTTP to internal/web, never through the Wails asset handler
// (SPEC-0010 design decision "Loopback HTTP on an ephemeral port, not the
// Wails asset handler"). Navigation happens via <meta http-equiv="refresh">
// — the page carries no scripts and no styles, and its CSP forbids both, so
// the strict-CSP posture (ADR-0010) holds on this page too.
//
// url is the embedded server's base URL derived from the bound listener
// (http://127.0.0.1:<port>), never user input.
func bootstrapHandler(url string) http.Handler {
	page := fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta http-equiv="refresh" content="0;url=%[1]s">
<title>msgbrowse</title>
</head>
<body>
<p><a href="%[1]s">Open msgbrowse</a></p>
</body>
</html>
`, url)
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Content-Security-Policy", "default-src 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		_, _ = w.Write([]byte(page))
	})
}
