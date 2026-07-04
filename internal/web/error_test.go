package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// The issue-#161 acceptance: the media handler's failure paths return a styled
// in-app error page (shell + message + link back) with the status code intact —
// never a bare text response the webview strands the user on.

// TestMediaErrorPagesAreStyled walks each media failure path and asserts the
// status code AND the styled page markers.
func TestMediaErrorPagesAreStyled(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv, err := st.GetConversation(context.Background(), "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}
	id := itoa(conv.ID)

	cases := []struct {
		name     string
		path     string
		wantCode int
	}{
		{"missing file", "/media/" + id + "/media/nope.jpg", http.StatusNotFound},
		{"unknown conversation", "/media/999999/media/cabin.jpg", http.StatusNotFound},
		{"bad id", "/media/nope/media/cabin.jpg", http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := get(t, srv, c.path)
			if rec.Code != c.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, c.wantCode)
			}
			body := rec.Body.String()
			// A real page: document shell, styled heading, and a way back.
			for _, want := range []string{"<!doctype html>", "screen-h1", "Attachment", `href="/"`, "app-toolbar"} {
				if !contains(body, want) {
					t.Errorf("error page missing %q; body starts %.120q", want, body)
				}
			}
			if ct := rec.Header().Get("Content-Type"); !contains(ct, "text/html") {
				t.Errorf("Content-Type = %q, want text/html", ct)
			}
		})
	}
}

// TestMediaUnresolvableRootStyled400: an attachment whose source has NO
// resolvable archive root (the real-Mac "invalid path" case, issue #161) gets
// the styled 400 page, not bare text. An imessage conversation is inserted
// into a server whose iMessage root is unset, so Resolve legitimately fails.
func TestMediaUnresolvableRootStyled400(t *testing.T) {
	srv, st, _ := newManagedRootServer(t) // signal-only roots
	convID, err := st.UpsertConversation(context.Background(), "imessage", "MJ")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	rec := get(t, srv, "/media/"+itoa(convID)+"/attachments/AB/CD/IMG.HEIC")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"<!doctype html>", "screen-h1", "Attachment unavailable", `href="/"`} {
		if !contains(body, want) {
			t.Errorf("styled 400 missing %q", want)
		}
	}
}

// TestAttachmentChipsCarryDownload: non-image attachment chips in the
// transcript carry the download attribute so a click saves the file instead of
// navigating the webview (issue #161).
func TestAttachmentChipsCarryDownload(t *testing.T) {
	srv, st, _ := newTestServer(t)
	conv, err := st.GetConversation(context.Background(), "Harper")
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}
	body := get(t, srv, "/c/"+itoa(conv.ID)).Body.String()
	i := strings.Index(body, "attach-chip")
	if i < 0 {
		t.Fatal("transcript missing the fixture's file attachment chip")
	}
	// The chip's anchor tag must carry download.
	tag := body[strings.LastIndex(body[:i], "<a "):]
	tag = tag[:strings.Index(tag, ">")+1]
	if !contains(tag, "download=") {
		t.Errorf("attachment chip missing the download attribute (tag: %s)", tag)
	}
}
