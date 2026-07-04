package store

import (
	"context"
	"testing"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
)

// seedTwoSources loads a signal conversation (2 messages, an attachment, a
// link, a reaction) and an imessage conversation (1 message), returning the
// signal conversation id.
func seedTwoSources(t *testing.T, st *Store) int64 {
	t.Helper()
	ctx := context.Background()
	sid, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatal(err)
	}
	m1 := msg("Harper", "2022-03-01 09:00:00", "Harper", "the lease is signed",
		[]signal.Attachment{{Kind: signal.KindFile, RelPath: "media/lease.pdf", OriginalName: "lease.pdf"}},
		[]signal.Link{{URL: "https://example.com/x"}})
	m1.Reactions = []signal.Reaction{{Emoji: "👍", Actor: "Me"}}
	if _, err := st.ReplaceConversationMessages(ctx, sid, source.Signal, []signal.Message{
		m1,
		msg("Harper", "2022-03-01 09:01:00", "Me", "great", nil, nil),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetIngestState(ctx, IngestState{
		ConversationID: sid, RelPath: "export/Harper/chat.md", ContentHash: "abc", MessageCount: 2,
	}); err != nil {
		t.Fatal(err)
	}

	iid, err := st.UpsertConversation(ctx, source.IMessage, "MJ")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceConversationMessages(ctx, iid, source.IMessage, []signal.Message{
		msg("MJ", "2022-03-02 10:00:00", "MJ", "hello from imessage", nil, nil),
	}); err != nil {
		t.Fatal(err)
	}
	return sid
}

// TestSourceCounts pins the per-source imported footprint the Providers cards
// show (issue #162): counts are per source and absent for empty sources.
func TestSourceCounts(t *testing.T) {
	st := newTestStore(t)
	seedTwoSources(t, st)

	counts, err := st.SourceCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := counts[source.Signal]; got.Conversations != 1 || got.Messages != 2 {
		t.Errorf("signal counts = %+v, want 1 conversation / 2 messages", got)
	}
	if got := counts[source.IMessage]; got.Conversations != 1 || got.Messages != 1 {
		t.Errorf("imessage counts = %+v, want 1 conversation / 1 message", got)
	}
	if _, ok := counts[source.WhatsApp]; ok {
		t.Error("whatsapp should be absent (nothing imported)")
	}
}

// TestDeleteSourceData is the Disable contract (issue #162): every imported
// row for the source goes — conversations, messages (and their FTS entries),
// attachments, links, reactions, ingest_state, contact identifiers — while the
// OTHER source's data is untouched.
func TestDeleteSourceData(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sid := seedTwoSources(t, st)

	// Sanity: the signal fixture populated the cascade targets.
	if n := scalar(t, st, `SELECT count(*) FROM attachments WHERE conversation_id = `+itoa64(sid)); n != 1 {
		t.Fatalf("precondition: attachments = %d, want 1", n)
	}
	if n := scalar(t, st, `SELECT count(*) FROM reactions WHERE conversation_id = `+itoa64(sid)); n != 1 {
		t.Fatalf("precondition: reactions = %d, want 1", n)
	}
	if n := ftsCount(t, st, "lease"); n != 1 {
		t.Fatalf("precondition: fts 'lease' = %d, want 1", n)
	}

	removed, err := st.DeleteSourceData(ctx, source.Signal)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Errorf("removed = %d conversations, want 1", removed)
	}

	for q, want := range map[string]int{
		`SELECT count(*) FROM conversations WHERE source = 'signal'`:       0,
		`SELECT count(*) FROM messages WHERE source = 'signal'`:            0,
		`SELECT count(*) FROM attachments`:                                 0,
		`SELECT count(*) FROM links`:                                       0,
		`SELECT count(*) FROM reactions`:                                   0,
		`SELECT count(*) FROM ingest_state`:                                0,
		`SELECT count(*) FROM contact_identifiers WHERE source = 'signal'`: 0,
		// The auto-created contact for the disabled source is orphan-collected;
		// the other source's contact stays.
		`SELECT count(*) FROM contacts WHERE display_name = 'Harper'`: 0,
		`SELECT count(*) FROM contacts WHERE display_name = 'MJ'`:     1,
		// The other source is untouched.
		`SELECT count(*) FROM conversations WHERE source = 'imessage'`: 1,
		`SELECT count(*) FROM messages WHERE source = 'imessage'`:      1,
	} {
		if n := scalar(t, st, q); n != want {
			t.Errorf("%s = %d, want %d", q, n, want)
		}
	}
	// The FTS triggers cleaned the deleted messages' index entries.
	if n := ftsCount(t, st, "lease"); n != 0 {
		t.Errorf("fts 'lease' after disable = %d, want 0", n)
	}
	if n := ftsCount(t, st, "imessage"); n != 1 {
		t.Errorf("fts 'imessage' after disable = %d, want 1 (other source intact)", n)
	}

	// SourcesPresent (the Enabled signal) no longer reports the source, so the
	// Providers card returns to Ready.
	srcs, err := st.SourcesPresent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range srcs {
		if s == source.Signal {
			t.Error("signal still present after DeleteSourceData")
		}
	}

	// Idempotent: disabling an already-empty source removes nothing and errors
	// nothing.
	removed, err = st.DeleteSourceData(ctx, source.Signal)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Errorf("second disable removed %d, want 0", removed)
	}
}
