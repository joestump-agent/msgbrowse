package web

import (
	"fmt"
	"html"
	"html/template"
	"net/url"
	"regexp"
	"strings"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/store"
)

// bodyTokenRe matches, in priority order, a Markdown image, a Markdown link, or
// a bare http(s) URL. Everything else is treated as plain text and escaped.
var bodyTokenRe = regexp.MustCompile(
	`(!\[[^\]]*\]\([^)]+\))` + // 1: image
		`|(\[[^\]]*\]\([^)]+\))` + // 2: markdown link
		`|(https?://[^\s<>()\[\]"'` + "`" + `]+)`, // 3: bare url
)

// mdLinkParts extracts the text and target from a Markdown link/image token.
var mdLinkParts = regexp.MustCompile(`^!?\[([^\]]*)\]\(([^)]+)\)$`)

// renderBody converts a raw message body into safe HTML for the transcript.
// Image markdown is dropped (images are rendered separately as thumbnails);
// Markdown links to URLs become anchors; Markdown links to media are dropped
// (shown as file attachments); bare URLs are linkified. All other text is
// HTML-escaped, so message content (which is untrusted) can never inject markup.
//
// Runs of Markdown blockquote lines (signal-export renders a quoted reply as
// `> …` lines) are wrapped in a styled <blockquote> instead of leaking the raw
// `>` markers into the transcript. Each quoted line still goes through the same
// escape/linkify pipeline, so quoted content is just as safe as normal text.
func renderBody(body string) template.HTML {
	if body == "" {
		return ""
	}
	lines := strings.Split(body, "\n")
	var b strings.Builder
	for i := 0; i < len(lines); {
		if isQuoteLine(lines[i]) {
			var inner []string
			for i < len(lines) && isQuoteLine(lines[i]) {
				inner = append(inner, stripQuotePrefix(lines[i]))
				i++
			}
			b.WriteString(`<blockquote class="msg-quote">`)
			b.WriteString(renderInline(strings.Join(inner, "\n")))
			b.WriteString(`</blockquote>`)
			continue
		}
		var normal []string
		for i < len(lines) && !isQuoteLine(lines[i]) {
			normal = append(normal, lines[i])
			i++
		}
		b.WriteString(renderInline(strings.Join(normal, "\n")))
	}
	return template.HTML(b.String())
}

// isQuoteLine reports whether a line is a Markdown blockquote line.
func isQuoteLine(line string) bool { return strings.HasPrefix(line, ">") }

// stripQuotePrefix removes the leading ">" and one optional following space.
func stripQuotePrefix(line string) string {
	line = line[1:] // drop '>'
	if strings.HasPrefix(line, " ") {
		line = line[1:]
	}
	return line
}

// renderInline escapes and linkifies a run of body text (no blockquote handling)
// and returns the safe HTML. Newlines become <br>. This is the inline pipeline
// shared by normal text and the inside of a quoted block.
func renderInline(text string) string {
	var b strings.Builder
	last := 0
	for _, loc := range bodyTokenRe.FindAllStringSubmatchIndex(text, -1) {
		// Escape the plain text preceding this token.
		if loc[0] > last {
			b.WriteString(escapeText(text[last:loc[0]]))
		}
		last = loc[1]
		token := text[loc[0]:loc[1]]
		switch {
		case loc[2] >= 0: // image: drop
			// no-op
		case loc[4] >= 0: // markdown link
			if parts := mdLinkParts.FindStringSubmatch(token); parts != nil {
				txt, target := parts[1], strings.TrimSpace(parts[2])
				if isURL(target) {
					b.WriteString(anchor(target, txt))
				}
				// else: media file link — drop (rendered as attachment)
			}
		case loc[6] >= 0: // bare url
			u := strings.TrimRight(token, trailingURLPunct)
			b.WriteString(anchor(u, u))
			// Re-append any stripped trailing punctuation as escaped text.
			if len(u) < len(token) {
				b.WriteString(escapeText(token[len(u):]))
			}
		}
	}
	if last < len(text) {
		b.WriteString(escapeText(text[last:]))
	}
	return b.String()
}

// trailingURLPunct mirrors the parser's bare-URL trimming.
const trailingURLPunct = ".,;:!?)]}>\"'"

// escapeText escapes plain text and turns newlines into <br>.
func escapeText(s string) string {
	return strings.ReplaceAll(html.EscapeString(s), "\n", "<br>")
}

// anchor builds a safe external link. The href is URL- and attribute-escaped and
// carries rel attributes that prevent referrer leakage and tab-nabbing.
func anchor(href, text string) string {
	safeHref := html.EscapeString(href)
	return fmt.Sprintf(`<a href="%s" target="_blank" rel="noopener noreferrer nofollow">%s</a>`,
		safeHref, html.EscapeString(text))
}

func isURL(target string) bool {
	return strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://")
}

// mediaURL builds the in-app URL that serves an attachment for a conversation.
// The conversation is keyed by id (names contain spaces and punctuation); the
// relative path is URL-path-escaped segment by segment.
func mediaURL(convID int64, relPath string) string {
	clean := strings.TrimPrefix(relPath, "./")
	parts := strings.Split(clean, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return fmt.Sprintf("/media/%d/%s", convID, strings.Join(parts, "/"))
}

// humanSize renders a byte count as a human-readable string.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// domainOf is a thin wrapper so templates can group links by domain.
func domainOf(rawurl string) string { return signal.Domain(rawurl) }

// camelBoundary matches a lowercase/digit immediately followed by an uppercase
// letter — a word boundary in a CamelCase contact name.
var camelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)

// humanName makes a conversation/sender display name readable by inserting
// spaces at CamelCase boundaries ("JonStump" → "Jon Stump", "TheStumpLoft" →
// "The Stump Loft"). Names that already contain spaces (e.g. group names) are
// left unchanged. It is display-only; the stored name (used in URLs and media
// paths) is untouched. Exact display names will come from the contacts page.
func humanName(name string) string {
	if strings.ContainsRune(name, ' ') {
		return name
	}
	return camelBoundary.ReplaceAllString(name, "$1 $2")
}

// initials returns up to two uppercase letters for a monogram avatar: the first
// letters of the first and last humanized words, or the first two letters of a
// single-word name.
func initials(name string) string {
	fields := strings.Fields(humanName(name))
	switch len(fields) {
	case 0:
		return "?"
	case 1:
		r := []rune(fields[0])
		if len(r) >= 2 {
			return strings.ToUpper(string(r[:2]))
		}
		return strings.ToUpper(string(r))
	default:
		first := []rune(fields[0])
		last := []rune(fields[len(fields)-1])
		return strings.ToUpper(string(first[0]) + string(last[0]))
	}
}

// avatarPalette is the set of monogram-avatar background classes. They are
// force-included in the build via `@source inline(...)` in tailwind/input.css,
// because they are selected dynamically here and so are never seen literally in
// a template for Tailwind's content scan.
var avatarPalette = []string{
	"bg-rose-500", "bg-orange-500", "bg-amber-500", "bg-emerald-500",
	"bg-teal-500", "bg-sky-500", "bg-indigo-500", "bg-fuchsia-500",
}

// avatarColor deterministically maps a name to a palette class (FNV-1a hash), so
// a conversation always gets the same avatar color.
func avatarColor(name string) string {
	var h uint32 = 2166136261
	for _, b := range []byte(name) {
		h ^= uint32(b)
		h *= 16777619
	}
	return avatarPalette[h%uint32(len(avatarPalette))]
}

// highlightSnippet converts an FTS5 snippet (whose matched terms are wrapped in
// store.SnippetMark{Start,End} control characters) into safe highlighted HTML.
//
// Order matters for both safety and tag balance:
//  1. Strip C0 control characters EXCEPT the two sentinels and tab/newline. A
//     crafted message body could itself contain a literal sentinel byte, which
//     would otherwise survive escaping and emit a stray, unbalanced <mark> /
//     </mark>. (Not an XSS — <mark> carries no attribute/script context — but
//     we keep the markup well-formed.)
//  2. HTML-escape, so untrusted body text can never inject markup.
//  3. Replace the escape-surviving sentinels with <mark>…</mark>.
//  4. Collapse newlines to spaces so result rows stay single-line.
func highlightSnippet(snippet string) template.HTML {
	snippet = stripControlExceptSentinels(snippet)
	escaped := html.EscapeString(snippet)
	escaped = strings.ReplaceAll(escaped, store.SnippetMarkStart, "<mark>")
	escaped = strings.ReplaceAll(escaped, store.SnippetMarkEnd, "</mark>")
	escaped = strings.ReplaceAll(escaped, "\n", " ")
	return template.HTML(escaped)
}

// stripControlExceptSentinels removes C0 control characters from s, preserving
// the snippet highlight sentinels, tab, and newline. FTS5's snippet() inserts
// the sentinels as balanced pairs, so after this the only sentinel bytes left
// are the ones it added.
func stripControlExceptSentinels(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\t', '\n':
			return r
		}
		if s := string(r); s == store.SnippetMarkStart || s == store.SnippetMarkEnd {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1 // drop
		}
		return r
	}, s)
}
