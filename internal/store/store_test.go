package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func msg(conv, ts, sender, body string, atts []signal.Attachment, links []signal.Link) signal.Message {
	parsed, _ := time.Parse(signal.TimestampLayout, ts)
	return signal.Message{
		Conversation: conv, Timestamp: parsed, TimestampRaw: ts,
		Sender: sender, Body: body, Attachments: atts, Links: links,
	}
}

func TestUpsertConversationIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id1, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("UpsertConversation not idempotent: %d != %d", id1, id2)
	}
}

func TestReplaceConversationMessagesAndFTS(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatal(err)
	}

	msgs := []signal.Message{
		msg("Harper", "2022-03-01 09:00:00", "Harper", "talk about the lease today",
			[]signal.Attachment{{Kind: signal.KindFile, RelPath: "media/lease.pdf", OriginalName: "lease.pdf"}},
			[]signal.Link{{URL: "https://example.com/x"}}),
		msg("Harper", "2022-03-01 09:01:00", "Me", "sounds good", nil, nil),
	}
	added, err := st.ReplaceConversationMessages(ctx, id, source.Signal, msgs)
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 {
		t.Errorf("added = %d, want 2", added)
	}

	// FTS index is populated via triggers.
	if n := ftsCount(t, st, "lease"); n != 1 {
		t.Errorf("fts match 'lease' = %d, want 1", n)
	}

	// Attachments and links written, stamped with the denormalized
	// conversation_id (schemaV7) so per-conversation counts stay single-table.
	if n := scalar(t, st, `SELECT count(*) FROM attachments`); n != 1 {
		t.Errorf("attachments = %d, want 1", n)
	}
	if n := scalar(t, st, `SELECT count(*) FROM links`); n != 1 {
		t.Errorf("links = %d, want 1", n)
	}
	if n := scalar(t, st, `SELECT count(*) FROM attachments WHERE conversation_id = `+itoa64(id)); n != 1 {
		t.Errorf("attachments with conversation_id=%d = %d, want 1", id, n)
	}
	if n := scalar(t, st, `SELECT count(*) FROM links WHERE conversation_id = `+itoa64(id)); n != 1 {
		t.Errorf("links with conversation_id=%d = %d, want 1", id, n)
	}
	if dom := scalarStr(t, st, `SELECT domain FROM links LIMIT 1`); dom != "example.com" {
		t.Errorf("link domain = %q, want example.com", dom)
	}

	// Replacing with a smaller set cascades deletes and re-syncs FTS.
	added, err = st.ReplaceConversationMessages(ctx, id, source.Signal,
		[]signal.Message{msg("Harper", "2022-03-01 09:02:00", "Me", "new content only", nil, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 {
		t.Errorf("added = %d, want 1", added)
	}
	if n := ftsCount(t, st, "lease"); n != 0 {
		t.Errorf("after replace, fts 'lease' = %d, want 0 (stale index)", n)
	}
	if n := scalar(t, st, `SELECT count(*) FROM attachments`); n != 0 {
		t.Errorf("after replace, attachments = %d, want 0 (cascade)", n)
	}
	if n := scalar(t, st, `SELECT count(*) FROM links`); n != 0 {
		t.Errorf("after replace, links = %d, want 0 (cascade)", n)
	}
}

// TestReplaceConversationMessagesReactions verifies reactions round-trip through
// ReplaceConversationMessages: they persist keyed by the stable per-source hash,
// re-ingest replaces them (no duplicates and no orphans), and MessageView.Reactions
// is populated (aggregated per emoji with a count) by the transcript query path.
func TestReplaceConversationMessagesReactions(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatal(err)
	}

	withReactions := func(m signal.Message, rs ...signal.Reaction) signal.Message {
		m.Reactions = rs
		return m
	}
	first := withReactions(
		msg("Harper", "2022-03-01 09:00:00", "Harper", "nice photo!", nil, nil),
		signal.Reaction{Emoji: "👍", Actor: "Me"},
		signal.Reaction{Emoji: "👍", Actor: "MJ"}, // same emoji, different actor → count 2
		signal.Reaction{Emoji: "❤️", Actor: "Me"},
	)
	plain := msg("Harper", "2022-03-01 09:01:00", "Me", "thanks", nil, nil)

	if _, err := st.ReplaceConversationMessages(ctx, id, source.Signal, []signal.Message{first, plain}); err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, st, `SELECT count(*) FROM reactions`); n != 3 {
		t.Fatalf("reactions rows = %d, want 3", n)
	}

	// MessageView.Reactions is populated and aggregated by the transcript query.
	transcript, err := st.ConversationTranscript(ctx, id, 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript) != 2 {
		t.Fatalf("transcript len = %d, want 2", len(transcript))
	}
	rv := transcript[0].Reactions
	if len(rv) != 2 {
		t.Fatalf("msg[0] reaction views = %+v, want 2 (👍 x2, ❤️ x1)", rv)
	}
	if rv[0].Emoji != "👍" || rv[0].Count != 2 || len(rv[0].Actors) != 2 {
		t.Errorf("aggregated 👍 view = %+v, want emoji 👍 count 2 actors [Me MJ]", rv[0])
	}
	if rv[1].Emoji != "❤️" || rv[1].Count != 1 {
		t.Errorf("aggregated ❤️ view = %+v, want emoji ❤️ count 1", rv[1])
	}
	if len(transcript[1].Reactions) != 0 {
		t.Errorf("plain message should have no reactions: %+v", transcript[1].Reactions)
	}

	// Re-ingest the SAME set: reactions are replaced, not duplicated.
	if _, err := st.ReplaceConversationMessages(ctx, id, source.Signal, []signal.Message{first, plain}); err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, st, `SELECT count(*) FROM reactions`); n != 3 {
		t.Errorf("after re-ingest, reactions rows = %d, want 3 (no dupes)", n)
	}

	// Re-ingest with the reactions removed: the conversation's reactions are cleared.
	if _, err := st.ReplaceConversationMessages(ctx, id, source.Signal,
		[]signal.Message{msg("Harper", "2022-03-01 09:00:00", "Harper", "nice photo!", nil, nil), plain}); err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, st, `SELECT count(*) FROM reactions`); n != 0 {
		t.Errorf("after reaction-free re-ingest, reactions rows = %d, want 0 (no orphans)", n)
	}
}

func TestIngestStateRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id, _ := st.UpsertConversation(ctx, source.Signal, "Harper")

	if got, err := st.GetIngestState(ctx, id); err != nil || got != nil {
		t.Fatalf("expected no state, got %v err %v", got, err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	want := IngestState{
		ConversationID: id, RelPath: "export/Harper/chat.md",
		MTimeUnix: 123, SizeBytes: 456, ContentHash: "abc",
		MessageCount: 7, LastIngestedAt: now,
	}
	if err := st.SetIngestState(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetIngestState(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.ContentHash != "abc" || got.MessageCount != 7 || got.SizeBytes != 456 || !got.LastIngestedAt.Equal(now) {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	// Upsert path.
	want.ContentHash = "def"
	want.MessageCount = 9
	if err := st.SetIngestState(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetIngestState(ctx, id)
	if got.ContentHash != "def" || got.MessageCount != 9 {
		t.Errorf("upsert failed: %+v", got)
	}
}

func TestReplaceSnapshots(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	snaps := []Snapshot{
		{Filename: "db-20240101-090000.tar", TakenAt: time.Unix(1704099600, 0).UTC(), SizeBytes: 10, Tier: "yearly"},
		{Filename: "db-20250601-090000.tar", TakenAt: time.Unix(1748768400, 0).UTC(), SizeBytes: 20, Tier: "monthly"},
	}
	if err := st.ReplaceSnapshots(ctx, snaps); err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, st, `SELECT count(*) FROM snapshots`); n != 2 {
		t.Errorf("snapshots = %d, want 2", n)
	}
	// Replace is a full swap.
	if err := st.ReplaceSnapshots(ctx, snaps[:1]); err != nil {
		t.Fatal(err)
	}
	if n := scalar(t, st, `SELECT count(*) FROM snapshots`); n != 1 {
		t.Errorf("after replace, snapshots = %d, want 1", n)
	}
}

func TestRecordIngestRun(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now()
	id, err := st.RecordIngestRun(ctx, IngestRun{
		Source:    source.Signal,
		StartedAt: now, FinishedAt: now.Add(time.Second), DurationMS: 1000,
		ConversationsScanned: 2, MessagesTotal: 11,
	})
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Error("expected non-zero run id")
	}
}

// TestUpsertConversationBootstrapsContact confirms that the unified-contacts
// behavior of Slice 1.5 fires on every fresh conversation: a contact is
// auto-created with display_name = name, a contact_identifier records the
// (source, identifier) tuple, and conversations.contact_id is linked.
func TestUpsertConversationBootstrapsContact(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatal(err)
	}

	var (
		contactID   int64
		displayName string
		identCount  int
		convContact int64
	)
	st.DB().QueryRow(`SELECT c.id, c.display_name
	                  FROM contacts c
	                  JOIN contact_identifiers ci ON ci.contact_id = c.id
	                  WHERE ci.source = ? AND ci.identifier = ?`,
		source.Signal, "Harper").Scan(&contactID, &displayName)
	if contactID == 0 || displayName != "Harper" {
		t.Errorf("contact not bootstrapped: id=%d name=%q", contactID, displayName)
	}
	st.DB().QueryRow(`SELECT count(*) FROM contact_identifiers WHERE contact_id = ?`, contactID).Scan(&identCount)
	if identCount != 1 {
		t.Errorf("contact_identifiers for contact %d = %d, want 1", contactID, identCount)
	}
	st.DB().QueryRow(`SELECT contact_id FROM conversations WHERE id = ?`, id).Scan(&convContact)
	if convContact != contactID {
		t.Errorf("conversations.contact_id = %d, want %d", convContact, contactID)
	}

	// Re-upsert is idempotent: no new contact or identifier.
	id2, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id {
		t.Errorf("conversation id changed on re-upsert: %d → %d", id, id2)
	}
	if n := scalar(t, st, `SELECT count(*) FROM contacts`); n != 1 {
		t.Errorf("re-upsert created extra contacts: %d", n)
	}
	if n := scalar(t, st, `SELECT count(*) FROM contact_identifiers`); n != 1 {
		t.Errorf("re-upsert created extra contact_identifiers: %d", n)
	}
}

// TestUpsertConversationConcurrent is a regression test for the deferred-tx
// lock-upgrade hazard: a SELECT-then-INSERT inside a deferred transaction holds
// a shared lock and then upgrades to a write lock, and SQLite returns
// SQLITE_BUSY for an upgrade without honoring busy_timeout. The fix is
// _txlock=immediate (see Open). Many goroutines upserting the same and distinct
// (source, name) pairs must all succeed and converge on stable ids, with no
// "database is locked" error and no duplicate contacts.
func TestUpsertConversationConcurrent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	const workers = 24
	names := []string{"MJ", "Harper", "Group Trip"}

	var wg sync.WaitGroup
	errs := make(chan error, workers*len(names))
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, n := range names {
				if _, err := st.UpsertConversation(ctx, source.Signal, n); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent UpsertConversation: %v", err)
	}

	// Exactly one conversation and one contact per distinct name — no dupes from
	// racing inserts.
	if n := scalar(t, st, `SELECT count(*) FROM conversations`); n != len(names) {
		t.Errorf("conversations = %d, want %d", n, len(names))
	}
	if n := scalar(t, st, `SELECT count(*) FROM contacts`); n != len(names) {
		t.Errorf("contacts = %d, want %d", n, len(names))
	}
	if n := scalar(t, st, `SELECT count(*) FROM contact_identifiers`); n != len(names) {
		t.Errorf("contact_identifiers = %d, want %d", n, len(names))
	}
}

// TestUpsertConversationSourceScoped confirms that a conversation named "MJ"
// can exist independently under Signal and iMessage (post-Slice-2.5 the second
// source will exist; here we use the constants directly). Each gets its own
// auto-contact; identifiers are scoped by source.
func TestUpsertConversationSourceScoped(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	sigID, err := st.UpsertConversation(ctx, source.Signal, "MJ")
	if err != nil {
		t.Fatal(err)
	}
	imID, err := st.UpsertConversation(ctx, source.IMessage, "MJ")
	if err != nil {
		t.Fatal(err)
	}
	if sigID == imID {
		t.Fatalf("expected distinct conversations across sources, both got %d", sigID)
	}
	if n := scalar(t, st, `SELECT count(*) FROM contacts`); n != 2 {
		t.Errorf("contacts = %d, want 2 (one auto-contact per source-side identity)", n)
	}
	if n := scalar(t, st, `SELECT count(*) FROM contact_identifiers WHERE identifier = 'MJ'`); n != 2 {
		t.Errorf("contact_identifiers with identifier='MJ' = %d, want 2 (one per source)", n)
	}
}

// helpers

func ftsCount(t *testing.T, st *Store, term string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH ?`, term).Scan(&n); err != nil {
		t.Fatalf("fts query: %v", err)
	}
	return n
}

func scalar(t *testing.T, st *Store, q string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(q).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}

func scalarStr(t *testing.T, st *Store, q string) string {
	t.Helper()
	var s string
	if err := st.DB().QueryRow(q).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return s
}

// TestSetPinnedRoundTrip verifies SetPinned flips the flag and that
// ListConversations / GetConversationByID reflect it (REQ-0006-010).
func TestSetPinnedRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Default: unpinned in both views.
	conv, err := st.GetConversationByID(ctx, id)
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}
	if conv.Pinned {
		t.Fatal("conversation should start unpinned")
	}

	// Pin it.
	if err := st.SetPinned(ctx, id, true); err != nil {
		t.Fatalf("set pinned true: %v", err)
	}
	conv, _ = st.GetConversationByID(ctx, id)
	if !conv.Pinned {
		t.Error("GetConversationByID: expected pinned after SetPinned(true)")
	}
	convs, err := st.ListConversations(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !findPinned(convs, id) {
		t.Error("ListConversations: expected pinned=true for the conversation")
	}

	// Unpin it.
	if err := st.SetPinned(ctx, id, false); err != nil {
		t.Fatalf("set pinned false: %v", err)
	}
	conv, _ = st.GetConversationByID(ctx, id)
	if conv.Pinned {
		t.Error("GetConversationByID: expected unpinned after SetPinned(false)")
	}
}

func findPinned(convs []ConversationSummary, id int64) bool {
	for _, c := range convs {
		if c.ID == id {
			return c.Pinned
		}
	}
	return false
}

func itoa64(n int64) string {
	return strconv.FormatInt(n, 10)
}

// TestTogglePinned verifies the direct-UPDATE toggle (SPEC-0008 REQ-0008-005):
// each call flips the flag without a prior read, and a missing conversation
// reports found=false instead of an error.
func TestTogglePinned(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	found, err := st.TogglePinned(ctx, id)
	if err != nil || !found {
		t.Fatalf("TogglePinned #1 = (%v, %v), want (true, nil)", found, err)
	}
	if conv, _ := st.GetConversationByID(ctx, id); conv == nil || !conv.Pinned {
		t.Error("expected pinned after first toggle")
	}

	found, err = st.TogglePinned(ctx, id)
	if err != nil || !found {
		t.Fatalf("TogglePinned #2 = (%v, %v), want (true, nil)", found, err)
	}
	if conv, _ := st.GetConversationByID(ctx, id); conv == nil || conv.Pinned {
		t.Error("expected unpinned after second toggle")
	}

	found, err = st.TogglePinned(ctx, id+999)
	if err != nil {
		t.Fatalf("TogglePinned on missing id: %v", err)
	}
	if found {
		t.Error("TogglePinned on missing id reported found=true")
	}
}

// TestConversationSourceName verifies the minimal hot-path lookup (SPEC-0008
// REQ-0008-005): it returns exactly the conversation's source and name, and
// sql.ErrNoRows for an id that does not exist.
func TestConversationSourceName(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	id, err := st.UpsertConversation(ctx, source.IMessage, "MJ")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	src, name, err := st.ConversationSourceName(ctx, id)
	if err != nil {
		t.Fatalf("ConversationSourceName: %v", err)
	}
	if src != source.IMessage || name != "MJ" {
		t.Errorf("ConversationSourceName = (%q, %q), want (%q, %q)", src, name, source.IMessage, "MJ")
	}

	if _, _, err := st.ConversationSourceName(ctx, id+999); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("missing conversation error = %v, want sql.ErrNoRows", err)
	}
}
