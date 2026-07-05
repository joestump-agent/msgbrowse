package web

import (
	"fmt"
	"html"
	"html/template"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// mdTarget matches a Markdown link/image target, tolerating one level of nested
// parentheses (Signal media names contain parens, e.g. "Image_from_iOS_(1).jpg" —
// issue #66). Keep in lockstep with the parser's mdTarget in internal/signal.
const mdTarget = `(?:[^()]|\([^()]*\))+`

// bodyTokenRe matches, in priority order, a Markdown image, a Markdown link, or
// a bare http(s) URL. Everything else is treated as plain text and escaped.
var bodyTokenRe = regexp.MustCompile(
	`(!\[[^\]]*\]\(` + mdTarget + `\))` + // 1: image
		`|(\[[^\]]*\]\(` + mdTarget + `\))` + // 2: markdown link
		`|(https?://[^\s<>()\[\]"'` + "`" + `]+)`, // 3: bare url
)

// mdLinkParts extracts the text and target from a Markdown link/image token.
// The token is already delimited by bodyTokenRe, so the anchored greedy `.+`
// cannot over-match, and it keeps paren-bearing targets whole.
var mdLinkParts = regexp.MustCompile(`^!?\[([^\]]*)\]\((.+)\)$`)

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

// commaInt renders an integer with comma thousands separators ("299750" →
// "299,750") for display text. Pure ASCII digit grouping — no locale tables
// (issue #178). Templates reach it via "num" below.
func commaInt(n int64) string {
	if n < 0 {
		// Negate in uint64 space so MinInt64 doesn't overflow.
		return "-" + commaUint(uint64(-(n+1))+1)
	}
	return commaUint(uint64(n))
}

// commaUint groups the decimal digits of u in threes from the right.
func commaUint(u uint64) string {
	s := strconv.FormatUint(u, 10)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + (len(s)-1)/3)
	head := len(s) % 3
	if head == 0 {
		head = 3
	}
	b.WriteString(s[:head])
	for i := head; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// num is the template entry point for count formatting: the integer kinds
// templates actually pass (count struct fields and len are int; int64/uint64
// cover wider totals). Anything else is a template authoring bug — return an
// error so the render fails loudly instead of printing an unformatted value.
// Display text only: ids, query params, and data- attributes must stay raw.
func num(v any) (string, error) {
	switch n := v.(type) {
	case int:
		return commaInt(int64(n)), nil
	case int64:
		return commaInt(n), nil
	case uint64:
		return commaUint(n), nil
	}
	return "", fmt.Errorf("num: unsupported type %T", v)
}

// domainOf is a thin wrapper so templates can group links by domain.
func domainOf(rawurl string) string { return signal.Domain(rawurl) }

// monthNames indexes 1..12 to the full English month name for day-separator
// labels in the dense-log transcript.
var monthNames = [...]string{
	"", "January", "February", "March", "April", "May", "June",
	"July", "August", "September", "October", "November", "December",
}

// legacyIMessageLayout is the source-formatted timestamp iMessage rows carried
// BEFORE ingest canonicalized them to signal.TimestampLayout. Databases that
// have not been re-ingested still hold this shape, so the render helpers below
// parse it as a fallback rather than dumping the raw string into the gutter
// (which wrapped the 76px column) and the day separator (which fired on every
// row because no two raw strings shared a "YYYY-MM-DD" prefix).
const legacyIMessageLayout = "Jan 2, 2006 3:04:05 PM"

// isCanonicalTS reports whether ts looks like the canonical stored layout
// ("YYYY-MM-DD HH:MM:SS"). A cheap shape check, not a full parse — these
// helpers run once per transcript row, so the canonical fast path must not
// allocate.
func isCanonicalTS(ts string) bool {
	return len(ts) >= 19 && ts[4] == '-' && ts[7] == '-' && ts[10] == ' '
}

// canonicalTS returns ts in the canonical "YYYY-MM-DD HH:MM:SS" layout,
// parsing the legacy iMessage layout only on a shape mismatch. Anything that
// parses as neither is returned unchanged so callers keep their existing
// whole-string fallback.
func canonicalTS(ts string) string {
	if isCanonicalTS(ts) {
		return ts
	}
	if t, err := time.Parse(legacyIMessageLayout, ts); err == nil {
		return t.Format(signal.TimestampLayout)
	}
	return ts
}

// dateKey returns the calendar-date prefix ("YYYY-MM-DD") of a stored timestamp
// ("YYYY-MM-DD HH:MM:SS"; legacy iMessage timestamps are canonicalized first).
// The transcript compares consecutive rows' dateKey to decide when to emit a day
// separator. An unrecognized timestamp returns the whole string so two equal odd
// values still group together.
func dateKey(ts string) string {
	ts = canonicalTS(ts)
	if len(ts) >= 10 && ts[4] == '-' && ts[7] == '-' {
		return ts[:10]
	}
	return ts
}

// clockTime returns the "HH:MM:SS" portion of a stored timestamp for the
// transcript's left gutter (legacy iMessage timestamps are canonicalized
// first). Falls back to the whole string if the format is unexpected, so the
// gutter is never blank.
func clockTime(ts string) string {
	ts = canonicalTS(ts)
	if len(ts) >= 19 && ts[10] == ' ' {
		return ts[11:19]
	}
	return ts
}

// dateLabel renders a stored timestamp's calendar date as a human day-separator
// label ("2022-10-22 20:17:13" → "October 22, 2022"). Falls back to the raw
// date prefix if parsing fails.
func dateLabel(ts string) string {
	key := dateKey(ts)
	if len(key) != 10 || key[4] != '-' || key[7] != '-' {
		return key
	}
	year := key[:4]
	mo, errM := strconv.Atoi(key[5:7])
	day, errD := strconv.Atoi(key[8:10])
	if errM != nil || errD != nil || mo < 1 || mo > 12 {
		return key
	}
	return fmt.Sprintf("%s %d, %s", monthNames[mo], day, year)
}

// camelBoundary matches a lowercase/digit immediately followed by an uppercase
// letter — a word boundary in a CamelCase contact name.
var camelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)

// humanName makes a conversation/sender display name readable by inserting
// spaces at CamelCase boundaries ("JonStump" → "Jon Stump", "TheStumpLoft" →
// "The Stump Loft"). Empty and imessage-exporter's literal "None" become
// "Unknown"; an email address renders as its humanized local part ("joe.stump@…"
// → "Joe Stump" — the raw address still appears in the header id-chips). Names
// that already contain spaces (e.g. group names) are left unchanged. It is
// display-only; the stored name (used in URLs and media paths) is untouched.
// Exact display names will come from the contacts page.
func humanName(name string) string {
	if name == "" || name == "None" {
		return "Unknown"
	}
	if strings.ContainsRune(name, ' ') {
		return name
	}
	if i := strings.IndexByte(name, '@'); i > 0 {
		return humanizeLocalPart(name[:i])
	}
	return camelBoundary.ReplaceAllString(name, "$1 $2")
}

// humanizeLocalPart turns an email local part into a display name: separator
// runs (dots, underscores, hyphens, plus) become spaces and each word is
// capitalized ("joe.stump" → "Joe Stump", "jsmith" → "Jsmith").
func humanizeLocalPart(local string) string {
	words := strings.FieldsFunc(local, func(r rune) bool {
		return r == '.' || r == '_' || r == '-' || r == '+'
	})
	if len(words) == 0 {
		return local
	}
	for i, w := range words {
		r := []rune(w)
		r[0] = unicode.ToUpper(r[0])
		words[i] = string(r)
	}
	return strings.Join(words, " ")
}

// reactionTitle builds the hover tooltip for a reaction badge from its reactors.
// "Loved by MJ", "MJ, Harper", or "" when no reactor is named (the badge still
// shows the emoji and count). Names are humanized for display.
func reactionTitle(r store.ReactionView) string {
	if len(r.Actors) == 0 {
		return ""
	}
	names := make([]string, len(r.Actors))
	for i, a := range r.Actors {
		names[i] = humanName(a)
	}
	return strings.Join(names, ", ")
}

// initials returns up to two characters for a monogram avatar. Phone-like names
// ("+15551234567") use their last two digits so a screenful of unsaved numbers
// doesn't render identical "+1" monograms; comma-joined group names ("A, B, C")
// use the member count; everything else takes the first letters of the first and
// last humanized words, or the first two letters of a single-word name.
func initials(name string) string {
	// Comma groups first: a group of phone handles ("+1…, +1…") is still a
	// group, not one phone.
	if strings.ContainsRune(name, ',') {
		return strconv.Itoa(commaGroupSize(name))
	}
	if phoneLike(name) {
		return lastTwoDigits(name)
	}
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

// phoneLike reports whether a raw name is an unsaved phone handle: a '+' prefix
// with the remainder mostly digits (separators like spaces/dashes/parens are
// tolerated).
func phoneLike(name string) bool {
	if !strings.HasPrefix(name, "+") {
		return false
	}
	rest := name[1:]
	digits := 0
	for _, r := range rest {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	return digits >= 2 && digits*2 >= len(rest)
}

// lastTwoDigits returns the final two digits of a phone-like name (or however
// many exist, guarded by phoneLike requiring at least two).
func lastTwoDigits(name string) string {
	var d [2]byte
	n := 0
	for i := len(name) - 1; i >= 0 && n < 2; i-- {
		if name[i] >= '0' && name[i] <= '9' {
			d[1-n] = name[i]
			n++
		}
	}
	return string(d[2-n:])
}

// commaGroupSize counts the members of a comma-joined group name ("MJ, Harper,
// Sam" → 3), skipping empty segments.
func commaGroupSize(name string) int {
	n := 0
	for _, part := range strings.Split(name, ",") {
		if strings.TrimSpace(part) != "" {
			n++
		}
	}
	return n
}

// avatarPalette is the set of monogram-avatar background classes. They are
// force-included in the build via `@source inline(...)` in tailwind/input.css,
// because they are selected dynamically here and so are never seen literally in
// a template for Tailwind's content scan.
//
// The order matches the SPEC-0006 / redesign-handoff avatar palette
// (hash→index): #f43f5e #0ea5e9 #f59e0b #14b8a6 #d946ef #f97316 #6366f1
// #10b981 — i.e. rose, sky, amber, teal, fuchsia, orange, indigo, emerald
// (Tailwind's default *-500 shades equal those hex values). Keep this in
// lockstep with the @source inline(...) safelist.
var avatarPalette = []string{
	"bg-rose-500", "bg-sky-500", "bg-amber-500", "bg-teal-500",
	"bg-fuchsia-500", "bg-orange-500", "bg-indigo-500", "bg-emerald-500",
}

// sourceSlug maps a stored source string to the CSS modifier class used by the
// presence dot and source pill (`src-signal` / `src-imessage` / `src-whatsapp`,
// REQ-0009-007). The slate + slate-light variants both style these via
// theme-aware CSS in input.css, so the template never needs an inline style
// (CSP-safe). Unknown sources fall back to a neutral class so nothing renders
// unstyled.
func sourceSlug(src string) string {
	switch src {
	case source.Signal:
		return "src-signal"
	case source.IMessage:
		return "src-imessage"
	case source.WhatsApp:
		return "src-whatsapp"
	default:
		return "src-unknown"
	}
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

// convRowContext pairs a conversation with the active id for the shared
// "conv_row" sidebar partial, so the PINNED and CONVERSATIONS sections render
// from one definition (REQ-0006-010).
type convRowContext struct {
	Conv     store.ConversationSummary
	ActiveID int64
}

// convRowCtx is the FuncMap adapter that builds a convRowContext inside a
// template range (html/template has no native struct literals).
func convRowCtx(c store.ConversationSummary, activeID int64) convRowContext {
	return convRowContext{Conv: c, ActiveID: activeID}
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
