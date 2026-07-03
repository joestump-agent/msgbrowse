package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
)

// legacyListConversations is the pre-SPEC-0008 ListConversations, ported
// verbatim (base GROUP BY + a 3-query fill loop per conversation) to serve as
// the golden oracle for the set-based rewrite: on data whose ts strings sort
// the same lexicographically and chronologically, the rewrite must reproduce
// its output field-for-field. It is also the baseline for the archive-scale
// benchmarks in query_bench_test.go. Do NOT "fix" or optimize this copy — its
// value is being exactly what shipped before the rewrite, including the
// MIN/MAX(ts) string aggregation that REQ-0008-002 retired.
func legacyListConversations(ctx context.Context, s *Store) ([]ConversationSummary, error) {
	const q = `
SELECT c.id, c.name, c.source, c.pinned,
       COUNT(m.id)                              AS msg_count,
       COALESCE(MIN(m.ts), '')                  AS first_ts,
       COALESCE(MAX(m.ts), '')                  AS last_ts,
       COALESCE(MAX(m.ts_unix), 0)              AS last_unix
  FROM conversations c
  LEFT JOIN messages m ON m.conversation_id = c.id
 GROUP BY c.id, c.name, c.source, c.pinned
 ORDER BY last_unix DESC, c.name ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ConversationSummary
	for rows.Next() {
		var cs ConversationSummary
		if err := rows.Scan(&cs.ID, &cs.Name, &cs.Source, &cs.Pinned, &cs.MessageCount, &cs.FirstTS, &cs.LastTS, &cs.LastTSUnix); err != nil {
			return nil, err
		}
		out = append(out, cs)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		if out[i].MessageCount == 0 {
			continue
		}
		if err := legacyFillLastMessage(ctx, s, &out[i]); err != nil {
			return nil, err
		}
		if err := legacyFillCounts(ctx, s, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func legacyFillLastMessage(ctx context.Context, s *Store, cs *ConversationSummary) error {
	var body string
	err := s.db.QueryRowContext(ctx,
		`SELECT sender, body FROM messages
		  WHERE conversation_id = ?
		  ORDER BY ts_unix DESC, id DESC LIMIT 1`, cs.ID).Scan(&cs.LastSender, &body)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	cs.LastPreview = preview(body, 80)
	return nil
}

func legacyFillCounts(ctx context.Context, s *Store, cs *ConversationSummary) error {
	err := s.db.QueryRowContext(ctx,
		`SELECT
		   COALESCE(SUM(CASE WHEN a.kind='image' THEN 1 ELSE 0 END), 0),
		   COALESCE(SUM(CASE WHEN a.kind='file'  THEN 1 ELSE 0 END), 0)
		 FROM attachments a
		 JOIN messages m ON m.id = a.message_id
		 WHERE m.conversation_id = ?`, cs.ID).Scan(&cs.ImageCount, &cs.FileCount)
	if err != nil {
		return err
	}
	return s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM links l
		   JOIN messages m ON m.id = l.message_id
		  WHERE m.conversation_id = ?`, cs.ID).Scan(&cs.LinkCount)
}

// loadFixtureArchive parses every chat.md under the committed Signal fixture
// archive into the store via the real parser + ingest write path, and adds one
// message-less conversation to cover the zero-message row shape.
func loadFixtureArchive(t *testing.T, st *Store) {
	t.Helper()
	ctx := context.Background()
	exportDir := filepath.Join("..", "..", "testdata", "archive", "export")
	entries, err := os.ReadDir(exportDir)
	if err != nil {
		t.Fatalf("read fixture archive: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		f, err := os.Open(filepath.Join(exportDir, e.Name(), "chat.md"))
		if err != nil {
			t.Fatalf("open fixture chat.md: %v", err)
		}
		msgs, perrs, err := signal.ParseAll(e.Name(), f)
		f.Close()
		if err != nil {
			t.Fatalf("parse fixture %s: %v", e.Name(), err)
		}
		if len(perrs) > 0 {
			t.Fatalf("fixture %s has parse errors: %v", e.Name(), perrs)
		}
		id, err := st.UpsertConversation(ctx, source.Signal, e.Name())
		if err != nil {
			t.Fatalf("upsert %s: %v", e.Name(), err)
		}
		if _, err := st.ReplaceConversationMessages(ctx, id, source.Signal, msgs); err != nil {
			t.Fatalf("replace messages %s: %v", e.Name(), err)
		}
	}
	if _, err := st.UpsertConversation(ctx, source.Signal, "Empty Thread"); err != nil {
		t.Fatalf("upsert empty conversation: %v", err)
	}
}

// TestListConversationsGoldenEquality pins the set-based rewrite to the legacy
// fill loop (REQ-0008-001): on the fixture archive — whose "YYYY-MM-DD ..." ts
// strings sort identically by string and by time, so the legacy output is
// correct — the single-statement listing must match the old code field-by-field,
// including ordering, previews, and every count, plus the zero-message row.
func TestListConversationsGoldenEquality(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	loadFixtureArchive(t, st)

	want, err := legacyListConversations(ctx, st)
	if err != nil {
		t.Fatalf("legacy listing: %v", err)
	}
	got, err := st.ListConversations(ctx)
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}

	if len(got) != len(want) {
		t.Fatalf("listing length = %d, legacy = %d", len(got), len(want))
	}
	if len(want) < 3 {
		t.Fatalf("fixture too small to be meaningful: %d conversations", len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("row %d differs:\n new:    %+v\n legacy: %+v", i, got[i], want[i])
		}
	}
}

// imsg builds a message whose raw ts string uses iMessage's month-name-first
// format, where lexicographic order disagrees with chronological order.
func imsg(conv string, ts time.Time, raw, sender, body string) signal.Message {
	return signal.Message{
		Conversation: conv, Timestamp: ts, TimestampRaw: raw,
		Sender: sender, Body: body,
	}
}

// TestSummaryTimestampsChronological covers REQ-0008-002: with iMessage-format
// ts strings ("Nov 13, 2015 ..." sorts AFTER "Apr 01, 2017 ..." as a string),
// ListConversations, GetConversationByID, and NewestMessageTS must all pick
// first/last by ts_unix, where the legacy MIN/MAX(ts) aggregation provably got
// it wrong.
func TestSummaryTimestampsChronological(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	const (
		firstRaw = "Nov 13, 2015 5:53:29 AM"
		lastRaw  = "Apr 01, 2017 12:00:00 PM"
	)
	id, err := st.UpsertConversation(ctx, source.IMessage, "MJ")
	if err != nil {
		t.Fatal(err)
	}
	msgs := []signal.Message{
		imsg("MJ", time.Date(2015, 11, 13, 5, 53, 29, 0, time.UTC), firstRaw, "MJ", "hello from 2015"),
		imsg("MJ", time.Date(2017, 4, 1, 12, 0, 0, 0, time.UTC), lastRaw, "Me", "hello from 2017"),
	}
	if _, err := st.ReplaceConversationMessages(ctx, id, source.IMessage, msgs); err != nil {
		t.Fatal(err)
	}

	// The legacy string aggregation is chronologically wrong on this data —
	// that is the bug being fixed, and this pins the test to a real divergence.
	legacy, err := legacyListConversations(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if legacy[0].FirstTS != lastRaw {
		t.Fatalf("expected legacy MIN(ts) to sort alphabetically (got FirstTS=%q); fixture no longer exercises the bug", legacy[0].FirstTS)
	}

	convs, err := st.ListConversations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(convs) != 1 {
		t.Fatalf("conversations = %d, want 1", len(convs))
	}
	if convs[0].FirstTS != firstRaw || convs[0].LastTS != lastRaw {
		t.Errorf("listing first/last = %q / %q, want %q / %q",
			convs[0].FirstTS, convs[0].LastTS, firstRaw, lastRaw)
	}
	if convs[0].LastSender != "Me" || convs[0].LastPreview != "hello from 2017" {
		t.Errorf("listing last sender/preview = %q / %q, want Me / hello from 2017",
			convs[0].LastSender, convs[0].LastPreview)
	}

	conv, err := st.GetConversationByID(ctx, id)
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}
	if conv.FirstTS != firstRaw || conv.LastTS != lastRaw {
		t.Errorf("GetConversationByID first/last = %q / %q, want %q / %q",
			conv.FirstTS, conv.LastTS, firstRaw, lastRaw)
	}

	newest, err := st.NewestMessageTS(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if newest != lastRaw {
		t.Errorf("NewestMessageTS = %q, want %q (row at MAX(ts_unix), not MAX(ts))", newest, lastRaw)
	}
}

// TestNewestMessageTSEmpty keeps the documented empty-database contract: ""
// and no error.
func TestNewestMessageTSEmpty(t *testing.T) {
	st := newTestStore(t)
	newest, err := st.NewestMessageTS(context.Background())
	if err != nil {
		t.Fatalf("NewestMessageTS on empty db: %v", err)
	}
	if newest != "" {
		t.Errorf("NewestMessageTS on empty db = %q, want \"\"", newest)
	}
}
