package signal

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name      string
		conv      string
		input     string
		want      []Message
		wantSkips int
	}{
		{
			name: "basic two messages",
			conv: "MJ",
			input: "[2021-12-30 02:58:19] MJ: hey are we still on for tomorrow?\n" +
				"[2021-12-30 02:59:01] Me: yep, 10am\n",
			want: []Message{
				{Conversation: "MJ", TimestampRaw: "2021-12-30 02:58:19", Sender: "MJ", Body: "hey are we still on for tomorrow?"},
				{Conversation: "MJ", TimestampRaw: "2021-12-30 02:59:01", Sender: "Me", Body: "yep, 10am"},
			},
		},
		{
			name:  "system event with empty body",
			conv:  "MJ",
			input: "[2021-12-18 03:14:37] No-Sender:\n",
			want: []Message{
				{Conversation: "MJ", TimestampRaw: "2021-12-18 03:14:37", Sender: "No-Sender", Body: "", IsSystem: true},
			},
		},
		{
			name: "multi-line body preserves newlines",
			conv: "Harper",
			input: "[2022-01-01 10:00:00] Harper: line one\n" +
				"line two\n" +
				"line three\n" +
				"[2022-01-01 10:05:00] Me: ok\n",
			want: []Message{
				{Conversation: "Harper", TimestampRaw: "2022-01-01 10:00:00", Sender: "Harper", Body: "line one\nline two\nline three"},
				{Conversation: "Harper", TimestampRaw: "2022-01-01 10:05:00", Sender: "Me", Body: "ok"},
			},
		},
		{
			name:  "colon in body keeps remainder",
			conv:  "MJ",
			input: "[2022-02-02 09:00:00] MJ: time is 09:00 sharp\n",
			want: []Message{
				{Conversation: "MJ", TimestampRaw: "2022-02-02 09:00:00", Sender: "MJ", Body: "time is 09:00 sharp"},
			},
		},
		{
			name:  "image attachment",
			conv:  "Harper",
			input: "[2022-03-03 12:00:00] Harper: look ![a cat](media/cat.jpg)\n",
			want: []Message{{
				Conversation: "Harper", TimestampRaw: "2022-03-03 12:00:00", Sender: "Harper",
				Body:        "look ![a cat](media/cat.jpg)",
				Attachments: []Attachment{{Kind: KindImage, OriginalName: "a cat", RelPath: "media/cat.jpg"}},
			}},
		},
		{
			name:  "file attachment",
			conv:  "Harper",
			input: "[2022-03-03 12:01:00] Me: [lease.pdf](media/lease.pdf)\n",
			want: []Message{{
				Conversation: "Harper", TimestampRaw: "2022-03-03 12:01:00", Sender: "Me",
				Body:        "[lease.pdf](media/lease.pdf)",
				Attachments: []Attachment{{Kind: KindFile, OriginalName: "lease.pdf", RelPath: "media/lease.pdf"}},
			}},
		},
		{
			name:  "bare and markdown links deduped",
			conv:  "MJ",
			input: "[2022-04-04 08:00:00] MJ: see https://example.com/x and [same](https://example.com/x).\n",
			want: []Message{{
				Conversation: "MJ", TimestampRaw: "2022-04-04 08:00:00", Sender: "MJ",
				Body:  "see https://example.com/x and [same](https://example.com/x).",
				Links: []Link{{URL: "https://example.com/x"}},
			}},
		},
		{
			name:  "trailing punctuation stripped from bare url",
			conv:  "MJ",
			input: "[2022-04-05 08:00:00] MJ: go to https://example.com/page.\n",
			want: []Message{{
				Conversation: "MJ", TimestampRaw: "2022-04-05 08:00:00", Sender: "MJ",
				Body:  "go to https://example.com/page.",
				Links: []Link{{URL: "https://example.com/page"}},
			}},
		},
		{
			name: "duplicate identical lines get distinct seq",
			conv: "MJ",
			input: "[2022-05-05 07:00:00] MJ: ping\n" +
				"[2022-05-05 07:00:00] MJ: ping\n",
			want: []Message{
				{Conversation: "MJ", TimestampRaw: "2022-05-05 07:00:00", Sender: "MJ", Body: "ping", Seq: 0},
				{Conversation: "MJ", TimestampRaw: "2022-05-05 07:00:00", Sender: "MJ", Body: "ping", Seq: 1},
			},
		},
		{
			name: "malformed leading line skipped",
			conv: "MJ",
			input: "garbage with no anchor\n" +
				"[2022-06-06 06:00:00] MJ: real\n",
			want: []Message{
				{Conversation: "MJ", TimestampRaw: "2022-06-06 06:00:00", Sender: "MJ", Body: "real"},
			},
			wantSkips: 1,
		},
		{
			name:      "invalid timestamp skipped",
			conv:      "MJ",
			input:     "[2022-13-40 99:99:99] MJ: nope\n",
			want:      nil,
			wantSkips: 1,
		},
		{
			name:  "unterminated final line still parsed",
			conv:  "MJ",
			input: "[2022-07-07 05:00:00] MJ: no trailing newline",
			want: []Message{
				{Conversation: "MJ", TimestampRaw: "2022-07-07 05:00:00", Sender: "MJ", Body: "no trailing newline"},
			},
		},
		{
			name:  "emoji and unicode preserved",
			conv:  "Harper",
			input: "[2022-08-08 04:00:00] Harper: café ☕ 🚀 naïve\n",
			want: []Message{
				{Conversation: "Harper", TimestampRaw: "2022-08-08 04:00:00", Sender: "Harper", Body: "café ☕ 🚀 naïve"},
			},
		},
		{
			// An anchor whose sender field is empty (e.g. an upstream export
			// glitch) must be skipped, not started. The next real message must
			// still be parsed normally.
			name: "empty sender is skipped",
			conv: "MJ",
			input: "[2022-01-01 10:00:00] : ignored body\n" +
				"[2022-01-01 10:05:00] MJ: real message\n",
			want: []Message{
				{Conversation: "MJ", TimestampRaw: "2022-01-01 10:05:00", Sender: "MJ", Body: "real message"},
			},
			wantSkips: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs, skips, err := ParseAll(tt.conv, strings.NewReader(tt.input))
			if err != nil {
				t.Fatalf("Parse returned error: %v", err)
			}
			if len(skips) != tt.wantSkips {
				t.Errorf("skips = %d, want %d (%v)", len(skips), tt.wantSkips, skips)
			}
			if len(msgs) != len(tt.want) {
				t.Fatalf("got %d messages, want %d: %#v", len(msgs), len(tt.want), msgs)
			}
			for i := range tt.want {
				assertMessage(t, i, msgs[i], tt.want[i])
			}
		})
	}
}

// TestParseReactions verifies that signal-export's "(- Name: emoji, … -)" trailer
// is diverted onto the message's Reactions (not the body, and not a standalone
// message). The targeted format is sigexport/models.py's Message.to_md:
// body + "\n(- " + ", ".join(f"{r.name}: {r.emoji}") + " -)".
func TestParseReactions(t *testing.T) {
	t.Run("single message with two reactions", func(t *testing.T) {
		in := "[2022-03-01 09:00:00] Harper: nice photo!\n(- Me: 👍, MJ: ❤️ -)\n"
		msgs, skips, err := ParseAll("MJ", strings.NewReader(in))
		if err != nil {
			t.Fatal(err)
		}
		if len(skips) != 0 {
			t.Fatalf("unexpected skips: %+v", skips)
		}
		// Exactly one message — the reactions trailer did NOT become its own row.
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1: %#v", len(msgs), msgs)
		}
		if msgs[0].Body != "nice photo!" {
			t.Errorf("body leaked reactions trailer: %q", msgs[0].Body)
		}
		want := []Reaction{{Emoji: "👍", Actor: "Me"}, {Emoji: "❤️", Actor: "MJ"}}
		if len(msgs[0].Reactions) != len(want) {
			t.Fatalf("reactions = %+v, want %+v", msgs[0].Reactions, want)
		}
		for i, w := range want {
			if msgs[0].Reactions[i] != w {
				t.Errorf("reaction[%d] = %+v, want %+v", i, msgs[0].Reactions[i], w)
			}
		}
	})

	t.Run("multi-line body keeps body, strips trailer", func(t *testing.T) {
		in := "[2022-03-01 09:00:00] MJ: line one\nline two\n(- Harper: 😂 -)\n"
		msgs, _, err := ParseAll("MJ", strings.NewReader(in))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		if msgs[0].Body != "line one\nline two" {
			t.Errorf("body = %q, want %q", msgs[0].Body, "line one\nline two")
		}
		if len(msgs[0].Reactions) != 1 || msgs[0].Reactions[0] != (Reaction{Emoji: "😂", Actor: "Harper"}) {
			t.Errorf("reactions = %+v", msgs[0].Reactions)
		}
	})

	t.Run("trailer inside a quoted reply is not a reaction", func(t *testing.T) {
		// A "> (- Joe -)" line is quoted body, not signal-export's trailer; it must
		// stay in the body and produce no reactions.
		in := "[2022-03-01 09:00:00] Me: > original\n> (- Joe -)\n\nreply\n"
		msgs, _, err := ParseAll("MJ", strings.NewReader(in))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		if len(msgs[0].Reactions) != 0 {
			t.Errorf("quoted parenthetical wrongly parsed as reactions: %+v", msgs[0].Reactions)
		}
		if !strings.Contains(msgs[0].Body, "(- Joe -)") {
			t.Errorf("quoted parenthetical stripped from body: %q", msgs[0].Body)
		}
	})

	t.Run("real trailer after an earlier in-body parenthetical", func(t *testing.T) {
		// An earlier `(- … -)` line must not shadow the real trailer at the very
		// end — only the LAST such line is the trailer.
		in := "[2022-03-01 09:00:00] MJ: (- not a reaction -)\nthe actual message\n(- Me: 👍 -)\n"
		msgs, _, err := ParseAll("MJ", strings.NewReader(in))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		if msgs[0].Body != "(- not a reaction -)\nthe actual message" {
			t.Errorf("body = %q, want the earlier parenthetical kept + trailer stripped", msgs[0].Body)
		}
		if len(msgs[0].Reactions) != 1 || msgs[0].Reactions[0] != (Reaction{Emoji: "👍", Actor: "Me"}) {
			t.Errorf("reactions = %+v, want [{👍 Me}]", msgs[0].Reactions)
		}
	})

	t.Run("reactor name containing a colon", func(t *testing.T) {
		// The emoji never contains ": ", but a display name can — split on the LAST
		// ": " so the name keeps its colon and the emoji is the tail.
		in := "[2022-03-01 09:00:00] MJ: hi\n(- Dr: Smith: 👍 -)\n"
		msgs, _, err := ParseAll("MJ", strings.NewReader(in))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 || len(msgs[0].Reactions) != 1 {
			t.Fatalf("got %d messages / reactions %+v", len(msgs), msgs[0].Reactions)
		}
		if got := msgs[0].Reactions[0]; got != (Reaction{Emoji: "👍", Actor: "Dr: Smith"}) {
			t.Errorf("reaction = %+v, want {👍 \"Dr: Smith\"}", got)
		}
	})

	t.Run("reaction on a photo (trailer before an attachment)", func(t *testing.T) {
		// signal-export orders a message {text}{reactions}{attachments}: an image
		// message with a reaction puts the attachment Markdown right after the
		// trailer on the same line. The reaction must be captured and the image
		// preserved — neither may leak as literal "(- … -)" text.
		in := "[2022-03-01 09:00:00] MJ:   \n(- Joe: ❤️ -)![photo.jpeg](./media/photo.jpeg)\n"
		msgs, _, err := ParseAll("MJ", strings.NewReader(in))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		if strings.Contains(msgs[0].Body, "(- ") {
			t.Errorf("reaction trailer leaked into body: %q", msgs[0].Body)
		}
		if len(msgs[0].Reactions) != 1 || msgs[0].Reactions[0] != (Reaction{Emoji: "❤️", Actor: "Joe"}) {
			t.Errorf("reactions = %+v, want [{❤️ Joe}]", msgs[0].Reactions)
		}
		if len(msgs[0].Attachments) != 1 || msgs[0].Attachments[0].Kind != KindImage ||
			msgs[0].Attachments[0].RelPath != "./media/photo.jpeg" {
			t.Errorf("attachment not preserved: %+v", msgs[0].Attachments)
		}
	})

	t.Run("text, then reaction, then attachment", func(t *testing.T) {
		in := "[2022-03-01 09:00:00] MJ: check this out  \n(- Harper: 😂 -)![y.png](./media/y.png)\n"
		msgs, _, err := ParseAll("MJ", strings.NewReader(in))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		if !strings.Contains(msgs[0].Body, "check this out") || strings.Contains(msgs[0].Body, "(- ") {
			t.Errorf("body = %q, want text kept and trailer stripped", msgs[0].Body)
		}
		if len(msgs[0].Reactions) != 1 || msgs[0].Reactions[0] != (Reaction{Emoji: "😂", Actor: "Harper"}) {
			t.Errorf("reactions = %+v, want [{😂 Harper}]", msgs[0].Reactions)
		}
		if len(msgs[0].Attachments) != 1 || msgs[0].Attachments[0].RelPath != "./media/y.png" {
			t.Errorf("attachment not preserved: %+v", msgs[0].Attachments)
		}
	})

	t.Run("non-reaction parenthetical before an attachment is not stripped", func(t *testing.T) {
		// "(- see attached -)" has no "Name: emoji" entry, so it parses to zero
		// reactions and must be left in the body — only the image is extracted.
		in := "[2022-03-01 09:00:00] MJ: (- see attached -)![z.png](./media/z.png)\n"
		msgs, _, err := ParseAll("MJ", strings.NewReader(in))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		if len(msgs[0].Reactions) != 0 {
			t.Errorf("non-reaction parenthetical wrongly parsed as reactions: %+v", msgs[0].Reactions)
		}
		if !strings.Contains(msgs[0].Body, "(- see attached -)") {
			t.Errorf("non-reaction parenthetical stripped from body: %q", msgs[0].Body)
		}
		if len(msgs[0].Attachments) != 1 {
			t.Errorf("attachment not extracted: %+v", msgs[0].Attachments)
		}
	})

	t.Run("reaction before an attachment whose filename has parentheses", func(t *testing.T) {
		// Signal media names routinely contain "(1)", "(2006)", etc. The trailer
		// must still strip when the attachment path has inner parens.
		in := "[2023-11-02 17:54:54] MJ: Felt cute. Might delete later.  \n" +
			"(- ArneSkaarFismen: 🔥 -)![Image_from_iOS_(1).jpg](./media/Image_from_iOS_(1).jpg)\n"
		msgs, _, err := ParseAll("MJ", strings.NewReader(in))
		if err != nil {
			t.Fatal(err)
		}
		if len(msgs) != 1 {
			t.Fatalf("got %d messages, want 1", len(msgs))
		}
		if strings.Contains(msgs[0].Body, "(- ") {
			t.Errorf("trailer leaked (paren filename): %q", msgs[0].Body)
		}
		if len(msgs[0].Reactions) != 1 || msgs[0].Reactions[0] != (Reaction{Emoji: "🔥", Actor: "ArneSkaarFismen"}) {
			t.Errorf("reactions = %+v, want [{🔥 ArneSkaarFismen}]", msgs[0].Reactions)
		}
		// The trailer strips and an image attachment survives. (extract's own
		// imageRe truncates a path with inner parens — a separate pre-existing bug
		// tracked elsewhere — so we don't assert the exact RelPath here.)
		if len(msgs[0].Attachments) != 1 || msgs[0].Attachments[0].Kind != KindImage {
			t.Errorf("attachment not preserved with parens: %+v", msgs[0].Attachments)
		}
	})
}

func assertMessage(t *testing.T, i int, got, want Message) {
	t.Helper()
	if got.Sender != want.Sender {
		t.Errorf("msg[%d].Sender = %q, want %q", i, got.Sender, want.Sender)
	}
	if got.TimestampRaw != want.TimestampRaw {
		t.Errorf("msg[%d].TimestampRaw = %q, want %q", i, got.TimestampRaw, want.TimestampRaw)
	}
	if got.Body != want.Body {
		t.Errorf("msg[%d].Body = %q, want %q", i, got.Body, want.Body)
	}
	if got.IsSystem != want.IsSystem {
		t.Errorf("msg[%d].IsSystem = %v, want %v", i, got.IsSystem, want.IsSystem)
	}
	if got.Seq != want.Seq {
		t.Errorf("msg[%d].Seq = %d, want %d", i, got.Seq, want.Seq)
	}
	if !got.Timestamp.Equal(want.Timestamp) && !want.Timestamp.IsZero() {
		t.Errorf("msg[%d].Timestamp = %v, want %v", i, got.Timestamp, want.Timestamp)
	}
	if len(got.Attachments) != len(want.Attachments) {
		t.Fatalf("msg[%d] attachments = %#v, want %#v", i, got.Attachments, want.Attachments)
	}
	for j := range want.Attachments {
		if got.Attachments[j] != want.Attachments[j] {
			t.Errorf("msg[%d].Attachments[%d] = %#v, want %#v", i, j, got.Attachments[j], want.Attachments[j])
		}
	}
	if len(got.Links) != len(want.Links) {
		t.Fatalf("msg[%d] links = %#v, want %#v", i, got.Links, want.Links)
	}
	for j := range want.Links {
		if got.Links[j] != want.Links[j] {
			t.Errorf("msg[%d].Links[%d] = %#v, want %#v", i, j, got.Links[j], want.Links[j])
		}
	}
}

func TestMessageIDStableAndSeqDistinct(t *testing.T) {
	in := "[2022-05-05 07:00:00] MJ: ping\n[2022-05-05 07:00:00] MJ: ping\n"
	a, _, err := ParseAll("MJ", strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := ParseAll("MJ", strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	// Stable across runs.
	for i := range a {
		if a[i].ID() != b[i].ID() {
			t.Errorf("ID not stable for msg %d: %s != %s", i, a[i].ID(), b[i].ID())
		}
	}
	// Distinct between the two identical lines (seq disambiguation).
	if a[0].ID() == a[1].ID() {
		t.Errorf("identical lines produced identical IDs: %s", a[0].ID())
	}
}
