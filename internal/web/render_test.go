package web

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderBody(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		contains []string
		excludes []string
	}{
		{
			name:     "escapes html",
			in:       "hello <script>alert(1)</script>",
			contains: []string{"&lt;script&gt;"},
			excludes: []string{"<script>"},
		},
		{
			name:     "drops image markdown",
			in:       "look ![a cat](media/cat.jpg)",
			contains: []string{"look "},
			excludes: []string{"media/cat.jpg", "![", "<img"},
		},
		{
			name:     "linkifies bare url and re-escapes trailing punctuation",
			in:       "see https://example.com/x.",
			contains: []string{`href="https://example.com/x"`, ">https://example.com/x<", "/a>."},
		},
		{
			name:     "markdown link to url becomes anchor with text",
			in:       "[menu](https://example.com/menu)",
			contains: []string{`href="https://example.com/menu"`, ">menu<"},
		},
		{
			name:     "markdown link to media is dropped",
			in:       "[lease.pdf](media/lease.pdf)",
			excludes: []string{"media/lease.pdf", "<a"},
		},
		{
			name:     "newlines become br",
			in:       "line1\nline2",
			contains: []string{"line1<br>line2"},
		},
		{
			name:     "anchors carry noopener noreferrer nofollow",
			in:       "https://example.com",
			contains: []string{`rel="noopener noreferrer nofollow"`, `target="_blank"`},
		},
		{
			name: "quoted reply renders as blockquote not raw markers",
			in:   "> Apparently these are real?\n>\n> (- Joe -)\n\nlol",
			contains: []string{
				`<blockquote class="msg-quote">`,
				"Apparently these are real?",
				"(- Joe -)",
				"</blockquote>",
				"lol",
			},
			excludes: []string{"&gt; Apparently", "> Apparently"},
		},
		{
			name: "links inside a quote are still linkified and escaped",
			in:   "> see https://example.com/x and <b>",
			contains: []string{
				`<blockquote class="msg-quote">`,
				`href="https://example.com/x"`,
				"&lt;b&gt;",
			},
			excludes: []string{"<b> "},
		},
		{
			name:     "leading > quotes only the quoted run, not following text",
			in:       "> quoted\nreply text",
			contains: []string{`<blockquote class="msg-quote">quoted</blockquote>`, "reply text"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(renderBody(tt.in))
			for _, c := range tt.contains {
				if !strings.Contains(got, c) {
					t.Errorf("renderBody(%q) = %q, want contains %q", tt.in, got, c)
				}
			}
			for _, x := range tt.excludes {
				if strings.Contains(got, x) {
					t.Errorf("renderBody(%q) = %q, should not contain %q", tt.in, got, x)
				}
			}
		})
	}
}

func TestMediaURLEscaping(t *testing.T) {
	got := mediaURL(3, "media/holiday photo.jpg")
	if got != "/media/3/media/holiday%20photo.jpg" {
		t.Errorf("mediaURL = %q", got)
	}
}

func TestHumanSize(t *testing.T) {
	tests := map[int64]string{
		0:          "0 B",
		512:        "512 B",
		1024:       "1.0 KB",
		1536:       "1.5 KB",
		1048576:    "1.0 MB",
		1073741824: "1.0 GB",
	}
	for n, want := range tests {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestContainWithin(t *testing.T) {
	base := "/archive/export/Harper"

	t.Run("normal path", func(t *testing.T) {
		got, ok := containWithin(base, "media/cat.jpg")
		if !ok {
			t.Fatal("expected ok")
		}
		if got != filepath.Join(base, "media", "cat.jpg") {
			t.Errorf("path = %q", got)
		}
	})

	t.Run("traversal is contained within base", func(t *testing.T) {
		// Leading-slash anchoring neutralizes ".." so the result can never escape
		// the base directory.
		got, ok := containWithin(base, "../../../etc/passwd")
		if ok && !strings.HasPrefix(got, base) {
			t.Errorf("traversal escaped base: %q", got)
		}
	})

	t.Run("empty inputs rejected", func(t *testing.T) {
		if _, ok := containWithin("", "media/x"); ok {
			t.Error("empty base should be rejected")
		}
		if _, ok := containWithin(base, ""); ok {
			t.Error("empty rel path should be rejected")
		}
	})
}

func TestHighlightSnippet(t *testing.T) {
	start := storeSnippetStart()
	end := storeSnippetEnd()

	t.Run("wraps sentinels in mark and escapes body", func(t *testing.T) {
		in := "see " + start + "lease" + end + " <b>terms</b>"
		got := string(highlightSnippet(in))
		if !strings.Contains(got, "<mark>lease</mark>") {
			t.Errorf("missing highlight: %q", got)
		}
		if strings.Contains(got, "<b>") || !strings.Contains(got, "&lt;b&gt;") {
			t.Errorf("body HTML not escaped: %q", got)
		}
	})

	t.Run("strips stray control chars to keep marks balanced", func(t *testing.T) {
		// A crafted body byte equal to the end sentinel, with no matching start,
		// must not leak an unbalanced </mark>. The strip removes it (it is not
		// part of an FTS-inserted pair in this synthetic input only if it is a
		// lone control char — here we use a different control byte to simulate
		// arbitrary body control chars).
		in := "harmless\x01text \x07more"
		got := string(highlightSnippet(in))
		if strings.ContainsAny(got, "\x01\x07") {
			t.Errorf("control chars not stripped: %q", got)
		}
		if strings.Contains(got, "<mark>") || strings.Contains(got, "</mark>") {
			t.Errorf("no sentinels present, but marks appeared: %q", got)
		}
	})
}

// storeSnippetStart/End expose the store sentinels to tests without importing
// the constant inline at every call site.
func storeSnippetStart() string { return "\x02" }
func storeSnippetEnd() string   { return "\x03" }

func TestHumanName(t *testing.T) {
	cases := map[string]string{
		"JonStump":        "Jon Stump",
		"ChelseaStump":    "Chelsea Stump",
		"ArneSkaarFismen": "Arne Skaar Fismen",
		"TheStumpLoft":    "The Stump Loft",
		"Harper":          "Harper",     // single word, no boundary
		"Group Trip":      "Group Trip", // already spaced — unchanged
		"":                "",
	}
	for in, want := range cases {
		if got := humanName(in); got != want {
			t.Errorf("humanName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInitials(t *testing.T) {
	cases := map[string]string{
		"JonStump":   "JS",
		"Harper":     "HA",
		"Group Trip": "GT",
		"X":          "X",
		"":           "?",
	}
	for in, want := range cases {
		if got := initials(in); got != want {
			t.Errorf("initials(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAvatarColorStableAndInPalette(t *testing.T) {
	a := avatarColor("JonStump")
	if a != avatarColor("JonStump") {
		t.Error("avatarColor not deterministic")
	}
	inPalette := false
	for _, c := range avatarPalette {
		if c == a {
			inPalette = true
		}
	}
	if !inPalette {
		t.Errorf("avatarColor returned %q, not in palette", a)
	}
}
