package web

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// gunzip decompresses a recorded response body.
func gunzip(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer zr.Close()
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return string(out)
}

// TestGzipRoundTripIdenticalBytes is the REQ-0008-007 contract: a gzip-accepting
// request for a page gets Content-Encoding: gzip + Vary, no stale
// Content-Length, and decodes to exactly the bytes a plain client receives.
func TestGzipRoundTripIdenticalBytes(t *testing.T) {
	srv, st, _ := newTestServer(t)
	for _, route := range pageRoutes(t, st) {
		plain := get(t, srv, route)
		zipped := getWith(t, srv, route, map[string]string{"Accept-Encoding": "gzip"})
		if zipped.Code != http.StatusOK {
			t.Fatalf("%s status = %d", route, zipped.Code)
		}
		if ce := zipped.Header().Get("Content-Encoding"); ce != "gzip" {
			t.Errorf("%s Content-Encoding = %q, want gzip", route, ce)
			continue
		}
		if !strings.Contains(zipped.Header().Get("Vary"), "Accept-Encoding") {
			t.Errorf("%s missing Vary: Accept-Encoding", route)
		}
		if cl := zipped.Header().Get("Content-Length"); cl != "" {
			t.Errorf("%s kept Content-Length %q on a compressed body", route, cl)
		}
		// Compare modulo the per-session Setup tokens /providers mints fresh on
		// every render (the two requests here are two renders by construction).
		if got := gunzip(t, zipped.Body.Bytes()); normalizeTokens(got) != normalizeTokens(plain.Body.String()) {
			t.Errorf("%s: decompressed body differs from identity body", route)
		}
		if zipped.Body.Len() >= plain.Body.Len() {
			t.Errorf("%s: compressed (%d) not smaller than plain (%d)", route, zipped.Body.Len(), plain.Body.Len())
		}
	}
}

// TestGzipCompressesStaticCSS covers the text/css allowlist entry via the
// embedded static file server (app.css is ~137KB — the biggest static win).
func TestGzipCompressesStaticCSS(t *testing.T) {
	srv, _, _ := newTestServer(t)
	plain := get(t, srv, "/static/app.css")
	zipped := getWith(t, srv, "/static/app.css", map[string]string{"Accept-Encoding": "gzip"})
	if ce := zipped.Header().Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("app.css Content-Encoding = %q, want gzip", ce)
	}
	if got := gunzip(t, zipped.Body.Bytes()); got != plain.Body.String() {
		t.Errorf("app.css decompressed bytes differ")
	}
}

// TestGzipExemptsMedia: /media/ responses are never compressed, even for a
// gzip-accepting client (REQ-0008-007 — media is already compressed and range
// semantics must survive).
func TestGzipExemptsMedia(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv, err := st.GetConversation(context.Background(), "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}
	rec := getWith(t, srv, "/media/"+itoa(conv.ID)+"/media/cabin.jpg",
		map[string]string{"Accept-Encoding": "gzip"})
	if rec.Code != http.StatusOK {
		t.Fatalf("media status = %d", rec.Code)
	}
	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("media Content-Encoding = %q, want none", ce)
	}
	if vary := rec.Header().Get("Vary"); strings.Contains(vary, "Accept-Encoding") {
		t.Errorf("media should not vary on Accept-Encoding (got %q)", vary)
	}
}

// TestGzipSkipsTinyBodies: responses under the 1KB threshold stay identity —
// the gzip framing would outweigh the savings.
func TestGzipSkipsTinyBodies(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// The empty live-search fragment is a ~70-byte HTML response.
	rec := getWith(t, srv, "/search/results", map[string]string{"Accept-Encoding": "gzip"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.Len() >= gzipMinSize {
		t.Fatalf("fixture fragment unexpectedly large (%d bytes); pick a smaller route", rec.Body.Len())
	}
	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("tiny body Content-Encoding = %q, want none", ce)
	}
	if !contains(rec.Body.String(), "Type a query") {
		t.Errorf("tiny body content mangled: %q", rec.Body.String())
	}
}

// TestGzipRespectsMissingAcceptEncoding: clients that don't advertise gzip get
// identity bytes.
func TestGzipRespectsMissingAcceptEncoding(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := get(t, srv, "/")
	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding = %q for a non-gzip client", ce)
	}
	if !contains(rec.Body.String(), "<!doctype html>") {
		t.Errorf("identity body mangled")
	}
}

// TestStaticETagRevalidation is the REQ-0008-008 contract: embedded statics
// carry a content-derived ETag, revalidate to 304 with no body, keep their
// Cache-Control, and the tag is stable across server restarts (embed bytes
// identical ⇒ tag identical).
func TestStaticETagRevalidation(t *testing.T) {
	srv, _, _ := newTestServer(t)

	first := get(t, srv, "/static/app.css")
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d", first.Code)
	}
	tag := first.Header().Get("ETag")
	if tag == "" || !strings.HasPrefix(tag, `"`) || !strings.HasSuffix(tag, `"`) {
		t.Fatalf("ETag = %q, want a quoted strong tag", tag)
	}
	if cc := first.Header().Get("Cache-Control"); !contains(cc, "max-age=3600") {
		t.Errorf("Cache-Control = %q", cc)
	}

	// Matching If-None-Match → 304 with no body, ETag + Cache-Control intact.
	rec := getWith(t, srv, "/static/app.css", map[string]string{"If-None-Match": tag})
	if rec.Code != http.StatusNotModified {
		t.Fatalf("revalidation status = %d, want 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 carried a %d-byte body", rec.Body.Len())
	}
	if got := rec.Header().Get("ETag"); got != tag {
		t.Errorf("304 ETag = %q, want %q", got, tag)
	}
	if cc := rec.Header().Get("Cache-Control"); !contains(cc, "max-age=3600") {
		t.Errorf("304 lost Cache-Control (%q)", cc)
	}

	// A weak-compare match and a wildcard also revalidate.
	if rec := getWith(t, srv, "/static/app.css", map[string]string{"If-None-Match": "W/" + tag}); rec.Code != http.StatusNotModified {
		t.Errorf("weak If-None-Match status = %d, want 304", rec.Code)
	}
	if rec := getWith(t, srv, "/static/app.css", map[string]string{"If-None-Match": "*"}); rec.Code != http.StatusNotModified {
		t.Errorf("wildcard If-None-Match status = %d, want 304", rec.Code)
	}

	// A stale tag re-downloads.
	if rec := getWith(t, srv, "/static/app.css", map[string]string{"If-None-Match": `"deadbeef"`}); rec.Code != http.StatusOK {
		t.Errorf("stale If-None-Match status = %d, want 200", rec.Code)
	}

	// Stability across restarts: a second server over the same embedded bytes
	// computes the identical tag.
	srv2, _, _ := newTestServer(t)
	if tag2 := get(t, srv2, "/static/app.css").Header().Get("ETag"); tag2 != tag {
		t.Errorf("ETag not stable across servers: %q vs %q", tag, tag2)
	}
}

// TestStaticETagWithGzip: a revalidating gzip-accepting client still gets a
// bare 304 (no Content-Encoding, no body).
func TestStaticETagWithGzip(t *testing.T) {
	srv, _, _ := newTestServer(t)
	tag := get(t, srv, "/static/app.css").Header().Get("ETag")
	rec := getWith(t, srv, "/static/app.css", map[string]string{
		"If-None-Match":   tag,
		"Accept-Encoding": "gzip",
	})
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304", rec.Code)
	}
	if ce := rec.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("304 Content-Encoding = %q, want none", ce)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("304 carried a body")
	}
}
