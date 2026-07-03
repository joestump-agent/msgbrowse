package store

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/joestump/msgbrowse/internal/source"
	_ "modernc.org/sqlite"
)

// TestMigrateV1ToV2BootstrapsContacts builds a database at schema version 1
// (the original Signal-only shape), runs the migrate runner, and asserts that
// every existing Signal conversation is bootstrapped with a contact and a
// contact_identifier — the core load-bearing behavior of Slice 1.5.
//
// This test deliberately reaches under Open() to construct a v1 database
// directly, because Open() always migrates to the latest version. Without that
// shortcut there is no way to exercise the v1 → v2 transition.
func TestMigrateV1ToV2BootstrapsContacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1-to-v2.sqlite")
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "synchronous(NORMAL)")
	dsn := "file:" + path + "?" + q.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// Apply v1 schema and stamp the user_version.
	if _, err := db.ExecContext(ctx, schemaV1); err != nil {
		t.Fatalf("v1 schema: %v", err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		t.Fatalf("stamp v1: %v", err)
	}
	// Seed two Signal conversations.
	if _, err := db.ExecContext(ctx, `INSERT INTO conversations(name) VALUES('Harper'), ('MJ')`); err != nil {
		t.Fatalf("seed conversations: %v", err)
	}

	// Run the migrate runner through a fresh Store wrapper on the same DB.
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrate v1→v2: %v", err)
	}

	v, err := readUserVersion(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Errorf("user_version = %d, want %d", v, schemaVersion)
	}

	// Both conversations got a contact + identifier.
	var contactCount, identCount int
	if err := db.QueryRow(`SELECT count(*) FROM contacts`).Scan(&contactCount); err != nil {
		t.Fatal(err)
	}
	if contactCount != 2 {
		t.Errorf("contacts = %d, want 2", contactCount)
	}
	if err := db.QueryRow(`SELECT count(*) FROM contact_identifiers WHERE source = 'signal'`).Scan(&identCount); err != nil {
		t.Fatal(err)
	}
	if identCount != 2 {
		t.Errorf("contact_identifiers (signal) = %d, want 2", identCount)
	}

	// Every conversation row got a contact_id link.
	var unlinked int
	if err := db.QueryRow(`SELECT count(*) FROM conversations WHERE contact_id IS NULL`).Scan(&unlinked); err != nil {
		t.Fatal(err)
	}
	if unlinked != 0 {
		t.Errorf("conversations with NULL contact_id = %d, want 0", unlinked)
	}

	// Source column was added to conversations and stamped 'signal'.
	var nonSignalConv int
	if err := db.QueryRow(`SELECT count(*) FROM conversations WHERE source != 'signal'`).Scan(&nonSignalConv); err != nil {
		t.Fatal(err)
	}
	if nonSignalConv != 0 {
		t.Errorf("conversations with non-signal source = %d, want 0", nonSignalConv)
	}
}

// TestMigrateV5AddsPinnedColumn confirms schema v5 adds conversations.pinned as
// a NOT NULL column defaulting to 0 (REQ-0006-010): a fresh database has the
// column and a newly-inserted conversation is unpinned by default.
func TestMigrateV5AddsPinnedColumn(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// Column exists with a 0 default.
	var dflt sql.NullString
	var notNull int
	found := false
	rows, err := st.DB().QueryContext(ctx, `PRAGMA table_info(conversations)`)
	if err != nil {
		t.Fatalf("table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid    int
			name   string
			ctype  string
			nn     int
			defval sql.NullString
			pk     int
		)
		if err := rows.Scan(&cid, &name, &ctype, &nn, &defval, &pk); err != nil {
			t.Fatal(err)
		}
		if name == "pinned" {
			found = true
			dflt = defval
			notNull = nn
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("conversations.pinned column missing after migrate")
	}
	if notNull != 1 {
		t.Errorf("pinned should be NOT NULL (notnull=%d)", notNull)
	}
	if !dflt.Valid || dflt.String != "0" {
		t.Errorf("pinned default = %q, want \"0\"", dflt.String)
	}

	// A brand-new conversation defaults to unpinned.
	id, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	conv, err := st.GetConversationByID(ctx, id)
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v", err)
	}
	if conv.Pinned {
		t.Error("new conversation should default to unpinned")
	}
}

// TestMigrateV6ToV7BackfillsConversationID builds a database at schema v6
// (attachments/links keyed only by message_id), seeds it, and runs the migrate
// runner. Schema v7 must add the denormalized conversation_id to both tables,
// backfill every existing row from messages, and create the counting indexes —
// with per-conversation counts identical before and after (SPEC-0008
// REQ-0008-003).
func TestMigrateV6ToV7BackfillsConversationID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v6-to-v7.sqlite")
	// No foreign_keys pragma here: this raw handle only lays down DDL and seed
	// rows; the migrate runner manages enforcement on its own connection.
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout%285000%29")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// Walk the real migration chain up to v6, exactly as a database that
	// predates v7 did, then stamp it.
	for v := 1; v <= 6; v++ {
		if _, err := db.ExecContext(ctx, migrations[v]); err != nil {
			t.Fatalf("apply v%d: %v", v, err)
		}
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 6"); err != nil {
		t.Fatalf("stamp v6: %v", err)
	}

	// Seed two conversations with attachments and links hanging off their
	// messages — the v6 shape has no conversation_id on either child table.
	seed := `
INSERT INTO conversations(id, source, name) VALUES (1, 'signal', 'Harper'), (2, 'imessage', 'MJ');
INSERT INTO messages(id, hash, conversation_id, source, ts, ts_unix, sender, body) VALUES
  (1, 'h1', 1, 'signal',   '2022-03-01 09:00:00',      1646125200, 'Harper', 'two photos'),
  (2, 'h2', 1, 'signal',   '2022-03-01 09:01:00',      1646125260, 'Me',     'a lease'),
  (3, 'h3', 2, 'imessage', 'Nov 13, 2015 5:53:29 AM',  1447394009, 'MJ',     'a link');
INSERT INTO attachments(message_id, kind, rel_path, original_name) VALUES
  (1, 'image', 'media/a.jpg', 'a.jpg'),
  (1, 'image', 'media/b.jpg', 'b.jpg'),
  (2, 'file',  'media/lease.pdf', 'lease.pdf');
INSERT INTO links(message_id, url, domain) VALUES
  (3, 'https://example.com/x', 'example.com'),
  (1, 'https://example.org/y', 'example.org');`
	if _, err := db.ExecContext(ctx, seed); err != nil {
		t.Fatalf("seed v6 data: %v", err)
	}

	// Per-conversation counts at v6, via the only route v6 has: the messages join.
	type counts struct{ images, files, links int }
	joinCounts := func() map[int64]counts {
		out := map[int64]counts{}
		for _, convID := range []int64{1, 2} {
			var c counts
			if err := db.QueryRowContext(ctx,
				`SELECT
				   COALESCE(SUM(CASE WHEN a.kind='image' THEN 1 ELSE 0 END), 0),
				   COALESCE(SUM(CASE WHEN a.kind='file'  THEN 1 ELSE 0 END), 0)
				 FROM attachments a JOIN messages m ON m.id = a.message_id
				 WHERE m.conversation_id = ?`, convID).Scan(&c.images, &c.files); err != nil {
				t.Fatalf("join attachment counts: %v", err)
			}
			if err := db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM links l JOIN messages m ON m.id = l.message_id
				  WHERE m.conversation_id = ?`, convID).Scan(&c.links); err != nil {
				t.Fatalf("join link counts: %v", err)
			}
			out[convID] = c
		}
		return out
	}
	want := joinCounts()
	if (want[1] != counts{images: 2, files: 1, links: 1}) || (want[2] != counts{links: 1}) {
		t.Fatalf("seed produced unexpected v6 counts: %+v", want)
	}

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrate v6→v7: %v", err)
	}
	if v, err := readUserVersion(ctx, db); err != nil || v != schemaVersion {
		t.Fatalf("user_version = %d (err %v), want %d", v, err, schemaVersion)
	}

	// Single-table counts through the backfilled column match the v6 join
	// counts exactly.
	for _, convID := range []int64{1, 2} {
		var got counts
		if err := db.QueryRowContext(ctx,
			`SELECT
			   COALESCE(SUM(kind = 'image'), 0),
			   COALESCE(SUM(kind = 'file'),  0)
			 FROM attachments WHERE conversation_id = ?`, convID).Scan(&got.images, &got.files); err != nil {
			t.Fatalf("single-table attachment counts: %v", err)
		}
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM links WHERE conversation_id = ?`, convID).Scan(&got.links); err != nil {
			t.Fatalf("single-table link counts: %v", err)
		}
		if got != want[convID] {
			t.Errorf("conversation %d counts after migrate = %+v, want %+v", convID, got, want[convID])
		}
	}

	// The backfill reached every row: nothing left at the ALTER's placeholder 0.
	var unfilled int
	if err := db.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM attachments WHERE conversation_id = 0)
		      + (SELECT COUNT(*) FROM links WHERE conversation_id = 0)`).Scan(&unfilled); err != nil {
		t.Fatal(err)
	}
	if unfilled != 0 {
		t.Errorf("%d attachment/link rows left with conversation_id = 0 after backfill", unfilled)
	}

	// The counting indexes exist.
	var idx int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index'
		  AND name IN ('idx_attachments_conv_kind', 'idx_links_conv')`).Scan(&idx); err != nil {
		t.Fatal(err)
	}
	if idx != 2 {
		t.Errorf("counting indexes present = %d, want 2", idx)
	}
}

// TestMigrateFreshDBStampsLatest ensures a brand-new database lands directly
// on the latest schema version after Open().
func TestMigrateFreshDBStampsLatest(t *testing.T) {
	st := newTestStore(t)
	v, err := readUserVersion(context.Background(), st.DB())
	if err != nil {
		t.Fatal(err)
	}
	if v != schemaVersion {
		t.Errorf("fresh DB user_version = %d, want %d", v, schemaVersion)
	}
}
