package web

import (
	"math"
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
			// Paren-bearing targets (issue #66): the whole token must be consumed —
			// no truncated remainder like "(1).jpg)" may leak into the text.
			name:     "image markdown with parens in target fully dropped",
			in:       "pic ![img](media/Image_from_iOS_(1).jpg) sent",
			contains: []string{"pic ", " sent"},
			excludes: []string{"Image_from_iOS", "(1).jpg", "!["},
		},
		{
			name:     "markdown url link with parens in target stays whole",
			in:       "[wiki](https://example.com/Go_(game))",
			contains: []string{`href="https://example.com/Go_(game)"`, ">wiki<"},
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
		"":                "Unknown",    // empty → placeholder, not a blank row
		"None":            "Unknown",    // imessage-exporter's literal missing-name
		// Email handles display as a humanized local part (the raw address still
		// shows in the header id-chips).
		"joe.stump@example.com": "Joe Stump",
		"j_smith@example.com":   "J Smith",
		"jsmith@example.com":    "Jsmith",
		"+15551234567":          "+15551234567", // phone handles pass through
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
		"":           "UN", // empty humanizes to "Unknown"
		"None":       "UN",
		// Phone-like handles: the last two digits beat a screenful of "+1"s.
		"+15551234567":    "67",
		"+1 555 123 4589": "89",
		// Comma-joined group names render their member count.
		"MJ, Harper, Sam":                 "3",
		"+15551234567, +15559876543":      "2",
		"A, B, C, D, E":                   "5",
		"joe.stump@example.com":           "JS", // email → humanized local part
		"Weber, Anna":                     "2",  // ambiguous "Last, First" reads as a 2-group
		"+notaphone":                      "+N", // '+' but not mostly digits — not phone-like
		"+15551234567 something wordier ": "+W", // digits diluted below half — falls through
	}
	for in, want := range cases {
		if got := initials(in); got != want {
			t.Errorf("initials(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDateKey(t *testing.T) {
	cases := map[string]string{
		"2022-03-01 09:00:00": "2022-03-01",
		"2026-12-31 23:59:59": "2026-12-31",
		"garbage":             "garbage", // unrecognized format → whole string
		// Legacy iMessage rows (ingested before ts canonicalization) parse via
		// the fallback instead of degrading to the whole string — which made the
		// transcript emit a day separator on every row.
		"Nov 13, 2015 5:53:29 AM": "2015-11-13",
		"Jun 5, 2020 2:30:00 PM":  "2020-06-05",
	}
	for in, want := range cases {
		if got := dateKey(in); got != want {
			t.Errorf("dateKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClockTime(t *testing.T) {
	cases := map[string]string{
		"2022-03-01 09:00:00": "09:00:00",
		"2026-12-31 23:59:59": "23:59:59",
		"odd":                 "odd", // unrecognized → fall back to whole string
		// Legacy iMessage rows reformat to HH:MM:SS instead of wrapping the
		// 76px gutter with the full source-formatted string.
		"Nov 13, 2015 5:53:29 AM": "05:53:29",
		"Jun 5, 2020 2:30:00 PM":  "14:30:00",
	}
	for in, want := range cases {
		if got := clockTime(in); got != want {
			t.Errorf("clockTime(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDateLabel(t *testing.T) {
	cases := map[string]string{
		"2022-03-01 09:00:00": "March 1, 2022",
		"2022-10-22 20:17:13": "October 22, 2022",
		"2026-12-09 00:00:00": "December 9, 2026",
		"not-a-date":          "not-a-date", // unparseable → echoed back
		// Legacy iMessage rows label correctly via the canonicalTS fallback.
		"Nov 13, 2015 5:53:29 AM": "November 13, 2015",
	}
	for in, want := range cases {
		if got := dateLabel(in); got != want {
			t.Errorf("dateLabel(%q) = %q, want %q", in, got, want)
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

// TestCommaInt covers the grouping boundaries (issue #178): no separator below
// 1000, the 3/4-digit edge, the int64 extremes (MinInt64 negation must not
// overflow), and negatives keeping the sign outside the grouping.
func TestCommaInt(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{12345, "12,345"},
		{299750, "299,750"},
		{-1234, "-1,234"},
		{-999, "-999"},
		{math.MaxInt64, "9,223,372,036,854,775,807"},
		{math.MinInt64, "-9,223,372,036,854,775,808"},
	}
	for _, tc := range cases {
		if got := commaInt(tc.in); got != tc.want {
			t.Errorf("commaInt(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNum checks the template-facing type switch: the integer kinds templates
// pass format, uint64 survives past MaxInt64, and anything else errors so a
// mistyped pipeline fails the render instead of printing raw.
func TestNum(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{int(2378), "2,378"},
		{int64(299750), "299,750"},
		{uint64(math.MaxUint64), "18,446,744,073,709,551,615"},
	}
	for _, tc := range cases {
		got, err := num(tc.in)
		if err != nil {
			t.Errorf("num(%v): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("num(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if _, err := num("2378"); err == nil {
		t.Error("num(string) should error, got nil")
	}
	if _, err := num(3.14); err == nil {
		t.Error("num(float64) should error, got nil")
	}
}
