package signal

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// anchorRe matches the start-of-message line: a bracketed timestamp, the sender
// (non-greedy up to the first colon), and the (possibly empty) inline body.
var anchorRe = regexp.MustCompile(`^\[(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})\]\s+(.*?):\s?(.*)$`)

var (
	// imageRe matches Markdown image syntax: ![alt](target).
	imageRe = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)
	// linkRe matches Markdown link syntax: [text](target). It is applied after
	// images are removed so an image is never also counted as a link.
	linkRe = regexp.MustCompile(`\[([^\]]*)\]\(([^)]+)\)`)
	// urlRe matches a bare http(s) URL up to the first whitespace or delimiter.
	urlRe = regexp.MustCompile(`https?://[^\s<>()\[\]"'` + "`" + `]+`)
	// reactionLineRe matches signal-export's reactions trailer at the end of a
	// message, e.g. "(- Alice: 👍, Bob: ❤️ -)". signal-export orders a message as
	// {text}{reactions}{attachments} (sigexport/models.py, Message.to_md / from_md:
	// https://github.com/carderne/signal-export/blob/main/sigexport/models.py): for
	// a text-only message the trailer is the final line, but when the message has
	// an attachment its Markdown follows the trailer on the SAME line, e.g.
	// "(- Joe: ❤️ -)![img](./media/x.jpeg)". The trailer therefore anchors to the
	// start of a line (after optional whitespace) and may be followed by one or
	// more attachment Markdown tokens before end-of-line. Group 1 captures the
	// inner "Name: emoji, …" list (non-greedy, so it stops at the first " -)");
	// group 2 captures any trailing attachment Markdown so it can be kept in the
	// cleaned body.
	reactionLineRe = regexp.MustCompile(`(?m)^[ \t]*\(- (.*?) -\)((?:[ \t]*!?\[[^\]]*\]\([^)]*\))*)[ \t]*$`)
)

// trailingURLPunct is stripped from the end of bare URLs (sentence punctuation
// that commonly abuts a link but is not part of it).
const trailingURLPunct = ".,;:!?)]}>\"'"

// ParseError describes a malformed line that the parser skipped. Ingestion logs
// these and continues; the parser never panics on bad input.
type ParseError struct {
	Line int
	Text string
	Err  error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("line %d: %v: %q", e.Line, e.Err, e.Text)
}

// Parse streams chat.md from r, emitting one [Message] per logical entry for the
// given conversation. A new message begins only on a line matching the timestamp
// anchor; every subsequent line is appended to the current body (newlines
// preserved) until the next anchor.
//
// emit is called in file order. If emit returns an error, Parse stops and
// returns it. onSkip, if non-nil, is called for each malformed line (a line
// before any anchor, or an anchor whose timestamp fails to parse); parsing
// continues regardless, so a single bad line never aborts a conversation.
//
// Parse reads incrementally and holds at most one message in memory at a time,
// so it is safe on very large conversations.
func Parse(conversation string, r io.Reader, emit func(Message) error, onSkip func(ParseError)) error {
	br := bufio.NewReader(r)
	seq := newSeqCounter()

	var (
		cur     *Message
		bodyBuf strings.Builder
		lineNo  int
	)

	flush := func() error {
		if cur == nil {
			return nil
		}
		body := normalizeBody(bodyBuf.String())
		// Divert a trailing reactions trailer onto this message so it never becomes
		// part of the body (or, worse, a standalone message).
		body, cur.Reactions = extractReactions(body)
		cur.Body = body
		cur.Attachments, cur.Links = extract(cur.Body)
		cur.Seq = seq.next(cur.Conversation, cur.TimestampRaw, cur.Sender, cur.Body)
		m := *cur
		cur = nil
		bodyBuf.Reset()
		return emit(m)
	}

	for {
		line, readErr := br.ReadString('\n')
		// Process the line content even on the final (unterminated) chunk.
		if len(line) > 0 || readErr == nil {
			lineNo++
			text := strings.TrimRight(line, "\r\n")
			if m := anchorRe.FindStringSubmatch(text); m != nil {
				if err := flush(); err != nil {
					return err
				}
				ts, perr := time.Parse(TimestampLayout, m[1])
				if perr != nil {
					// Anchor shape matched but the timestamp is invalid: skip it
					// rather than starting a corrupt message.
					if onSkip != nil {
						onSkip(ParseError{Line: lineNo, Text: text, Err: perr})
					}
					continue
				}
				sender := m[2]
				if sender == "" {
					// Anchor matched but the sender field is empty
					// (e.g. "[2022-01-01 10:00:00] : foo"). The parser contract
					// says malformed lines are skipped and logged, never started.
					if onSkip != nil {
						onSkip(ParseError{Line: lineNo, Text: text, Err: errEmptySender})
					}
					continue
				}
				cur = &Message{
					Conversation: conversation,
					Timestamp:    ts,
					TimestampRaw: m[1],
					Sender:       sender,
					IsSystem:     sender == SystemSender,
				}
				bodyBuf.WriteString(m[3])
			} else if cur != nil {
				// Continuation of the current message body.
				bodyBuf.WriteByte('\n')
				bodyBuf.WriteString(text)
			} else if strings.TrimSpace(text) != "" {
				// Non-blank content before any anchor: malformed, skip.
				if onSkip != nil {
					onSkip(ParseError{Line: lineNo, Text: text, Err: errNoAnchor})
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return readErr
		}
	}
	return flush()
}

// errNoAnchor flags content that appears before the first valid timestamp line.
var errNoAnchor = errors.New("content before first timestamp")

// errEmptySender flags an anchor whose sender field is empty.
var errEmptySender = errors.New("empty sender")

// ParseAll is a convenience wrapper that collects every message into a slice.
// Prefer [Parse] for large inputs; ParseAll is intended for tests and small
// conversations.
func ParseAll(conversation string, r io.Reader) ([]Message, []ParseError, error) {
	var msgs []Message
	var skips []ParseError
	err := Parse(conversation, r,
		func(m Message) error { msgs = append(msgs, m); return nil },
		func(e ParseError) { skips = append(skips, e) },
	)
	return msgs, skips, err
}

// normalizeBody trims trailing blank lines (an artifact of the line between a
// message and the next anchor) while preserving all internal newlines.
func normalizeBody(s string) string {
	return strings.TrimRight(s, "\n")
}

// extract pulls attachments and links out of a message body. Images become
// image attachments; Markdown links whose target is an http(s) URL become links,
// other Markdown links become file attachments; remaining bare URLs become
// links. Links are de-duplicated by URL, preserving first-seen order.
func extract(body string) ([]Attachment, []Link) {
	if body == "" {
		return nil, nil
	}
	var atts []Attachment
	var links []Link
	seen := map[string]bool{}

	addLink := func(u string) {
		u = strings.TrimRight(u, trailingURLPunct)
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		links = append(links, Link{URL: u})
	}

	// Images first.
	for _, m := range imageRe.FindAllStringSubmatch(body, -1) {
		atts = append(atts, Attachment{
			Kind:         KindImage,
			OriginalName: strings.TrimSpace(m[1]),
			RelPath:      strings.TrimSpace(m[2]),
		})
	}
	rest := imageRe.ReplaceAllString(body, " ")

	// Then non-image Markdown links.
	for _, m := range linkRe.FindAllStringSubmatch(rest, -1) {
		target := strings.TrimSpace(m[2])
		if isURL(target) {
			addLink(target)
			continue
		}
		atts = append(atts, Attachment{
			Kind:         KindFile,
			OriginalName: strings.TrimSpace(m[1]),
			RelPath:      target,
		})
	}
	rest = linkRe.ReplaceAllString(rest, " ")

	// Finally bare URLs anywhere in the remaining text.
	for _, u := range urlRe.FindAllString(rest, -1) {
		addLink(u)
	}
	return atts, links
}

// extractReactions splits a signal-export reactions trailer off the END of a
// message body and returns the cleaned body plus the parsed reactions. The
// trailer is "(- <Name>: <emoji>, … -)"; for a text-only message it is the final
// line, and for a message with an attachment the attachment Markdown follows it
// on the same line and is preserved in the cleaned body (so extract() still sees
// it). A body whose final "(- … -)" parses to no reactions — or that has no
// trailer at all — is returned unchanged with nil reactions.
func extractReactions(body string) (string, []Reaction) {
	all := reactionLineRe.FindAllStringSubmatchIndex(body, -1)
	if all == nil {
		return body, nil
	}
	// signal-export always writes the reactions trailer last, so the trailer (if
	// any) is the LAST matching line — take it, not the first. An earlier `(- … -)`
	// line is an in-body parenthetical and is left alone.
	loc := all[len(all)-1]
	// Confirm it really is the trailer: nothing but whitespace may follow the line.
	if strings.TrimSpace(body[loc[1]:]) != "" {
		return body, nil
	}
	reactions := parseReactionEntries(body[loc[2]:loc[3]])
	if len(reactions) == 0 {
		// The inner text parsed to no reactions, so this is an ordinary
		// parenthetical (e.g. "(- a note -)"), not signal-export's trailer — leave
		// the body untouched.
		return body, nil
	}
	// Strip only the "(- … -)" token (and any blank line it sat on). When the
	// message has an attachment, its Markdown trails the token on the same line
	// (group 2); re-append it so extract() still records the attachment.
	cleaned := strings.TrimRight(body[:loc[0]], "\n \t")
	if loc[4] >= 0 {
		if att := strings.TrimSpace(body[loc[4]:loc[5]]); att != "" {
			if cleaned != "" {
				cleaned += "\n"
			}
			cleaned += att
		}
	}
	return cleaned, reactions
}

// parseReactionEntries splits a signal-export reaction list ("Name: emoji,
// Name2: emoji2") into reactions. Each entry splits on its LAST ": ": the emoji
// never contains ": ", but a reactor's display name can, so the colon-free tail
// is the emoji and everything before it is the name. Entries without a ": "
// separator, or with an empty emoji, are skipped.
func parseReactionEntries(inner string) []Reaction {
	var reactions []Reaction
	for _, entry := range strings.Split(inner, ", ") {
		i := strings.LastIndex(entry, ": ")
		if i < 0 {
			continue
		}
		emoji := strings.TrimSpace(entry[i+2:])
		if emoji == "" {
			continue
		}
		reactions = append(reactions, Reaction{Emoji: emoji, Actor: strings.TrimSpace(entry[:i])})
	}
	return reactions
}

// ExtractLinks returns the deduplicated bare http(s) URLs in text (with trailing
// sentence punctuation trimmed), in first-seen order. It is the plain-text URL
// extractor shared with the iMessage parser, whose bodies carry no Markdown.
func ExtractLinks(text string) []Link {
	if text == "" {
		return nil
	}
	var links []Link
	seen := map[string]bool{}
	for _, u := range urlRe.FindAllString(text, -1) {
		u = strings.TrimRight(u, trailingURLPunct)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		links = append(links, Link{URL: u})
	}
	return links
}

// isURL reports whether target is an http(s) URL.
func isURL(target string) bool {
	return strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://")
}

// seqCounter assigns the per-conversation Seq disambiguator for byte-identical
// messages. The key intentionally excludes Seq itself.
type seqCounter struct {
	counts map[string]int
}

func newSeqCounter() *seqCounter { return &seqCounter{counts: map[string]int{}} }

func (s *seqCounter) next(conv, tsRaw, sender, body string) int {
	key := conv + "\x00" + tsRaw + "\x00" + sender + "\x00" + body
	n := s.counts[key]
	s.counts[key] = n + 1
	return n
}
