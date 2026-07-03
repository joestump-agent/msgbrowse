// Package store is msgbrowse's persistence layer: a single SQLite database
// (relational tables + an FTS5 index, with a vector index added in a later
// slice) that lives in the writable data directory, never inside the read-only
// archive.
//
// The database is opened with WAL journaling, foreign keys enforced, and a busy
// timeout so concurrent web readers coexist with the ingester's writes. All
// schema lives in schema.go and is applied idempotently on Open.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
	_ "modernc.org/sqlite" // pure-Go SQLite driver (no cgo; FTS5 built in)
)

// Store wraps the SQLite handle and exposes typed repository methods.
type Store struct {
	db *sql.DB
}

// Open opens (creating if necessary) the SQLite database at path and applies the
// schema. The caller owns Close. path is a filesystem path, not a DSN.
func Open(path string) (*Store, error) {
	// modernc.org/sqlite applies one `PRAGMA <value>` per `_pragma` query param on
	// each new connection, and honors `_txlock` for the BEGIN mode.
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "synchronous(NORMAL)")
	// Every explicit transaction in this package is a writer (UpsertConversation,
	// ReplaceConversationMessages, ReplaceSnapshots, and the migration runner).
	// Begin them in IMMEDIATE mode so they take the write lock up front. A
	// deferred transaction that SELECTs then INSERTs would hold a shared lock and
	// then try to upgrade it; SQLite returns SQLITE_BUSY for a lock UPGRADE
	// without honoring busy_timeout, so two concurrent upserts could spuriously
	// fail. IMMEDIATE makes busy_timeout apply to the initial acquisition
	// instead. Autocommit read queries (QueryContext) are unaffected.
	q.Set("_txlock", "immediate")
	dsn := "file:" + path + "?" + q.Encode()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite handles one writer at a time; let database/sql keep a small pool for
	// concurrent readers while writes serialize via the busy timeout.
	db.SetMaxOpenConns(8)

	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// OpenReadOnly opens an EXISTING database read-only and does NOT run migrations.
// It's for inspection (e.g. `msgbrowse doctor`): unlike Open it neither creates
// the file nor writes anything (mode=ro), so it can report the on-disk schema
// version and counts truthfully without masking drift by migrating. The file
// must already exist; queries that assume a newer schema are the caller's to
// guard.
func OpenReadOnly(path string) (*Store, error) {
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(2000)")
	dsn := "file:" + path + "?" + q.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite read-only: %w", err)
	}
	return &Store{db: db}, nil
}

// DB exposes the underlying handle for read queries added in later slices.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// IngestState is the per-conversation incremental bookkeeping that lets ingest
// skip unchanged chat.md files.
type IngestState struct {
	ConversationID int64
	RelPath        string
	MTimeUnix      int64
	SizeBytes      int64
	ContentHash    string
	MessageCount   int
	LastIngestedAt time.Time
}

// Snapshot describes one encrypted raw-DB backup tar under .snapshots/.
type Snapshot struct {
	Filename  string
	TakenAt   time.Time
	SizeBytes int64
	Tier      string
}

// IngestRun is a summary of a single ingest pass, surfaced on the /status page.
// Source identifies which exporter the run came from (see internal/source); the
// /status page groups recent runs by it so one source's failures don't hide
// another's successes.
type IngestRun struct {
	Source               string
	StartedAt            time.Time
	FinishedAt           time.Time
	DurationMS           int64
	ConversationsScanned int
	ConversationsChanged int
	MessagesTotal        int
	MessagesAdded        int
	SnapshotsSeen        int
	SkippedLines         int
	Errors               int
}

// UpsertConversation returns the id of the (source, name) conversation,
// creating it if absent. It also ensures a contact and contact_identifier exist
// for the source-side identity and that conversations.contact_id points at it.
//
// First-time identities get an auto-created contact whose display_name equals
// the conversation name. The Slice 4.5 contacts page lets the user merge those
// auto-contacts together (e.g. signal:MJ + imessage:+15551234567 → one person).
// Auto-creation is intentionally cheap and never silently merges across
// identifiers — see ADR-0003.
func (s *Store) UpsertConversation(ctx context.Context, source, name string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()

	var (
		convID    int64
		contactID sql.NullInt64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT id, contact_id FROM conversations WHERE source = ? AND name = ?`,
		source, name).Scan(&convID, &contactID)
	switch {
	case err == sql.ErrNoRows:
		res, err := tx.ExecContext(ctx,
			`INSERT INTO conversations(source, name) VALUES(?, ?)`, source, name)
		if err != nil {
			return 0, fmt.Errorf("insert conversation %s/%s: %w", source, name, err)
		}
		convID, err = res.LastInsertId()
		if err != nil {
			return 0, err
		}
	case err != nil:
		return 0, fmt.Errorf("lookup conversation %s/%s: %w", source, name, err)
	}

	if !contactID.Valid {
		// Try to find an existing contact via the (source, identifier) tuple
		// first — handles the case where a conversation was deleted and re-
		// ingested under the same identifier.
		var existingCID sql.NullInt64
		err = tx.QueryRowContext(ctx,
			`SELECT contact_id FROM contact_identifiers WHERE source = ? AND identifier = ?`,
			source, name).Scan(&existingCID)
		switch {
		case err == sql.ErrNoRows:
			res, err := tx.ExecContext(ctx,
				`INSERT INTO contacts(display_name) VALUES(?)`, name)
			if err != nil {
				return 0, fmt.Errorf("create contact: %w", err)
			}
			newCID, err := res.LastInsertId()
			if err != nil {
				return 0, err
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO contact_identifiers(contact_id, source, identifier) VALUES(?, ?, ?)`,
				newCID, source, name); err != nil {
				return 0, fmt.Errorf("create contact_identifier: %w", err)
			}
			existingCID = sql.NullInt64{Int64: newCID, Valid: true}
		case err != nil:
			return 0, fmt.Errorf("lookup contact_identifier: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE conversations SET contact_id = ? WHERE id = ?`,
			existingCID.Int64, convID); err != nil {
			return 0, fmt.Errorf("link conversation to contact: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	rollback = false
	return convID, nil
}

// GetIngestState returns the stored state for a conversation, or (nil, nil) if
// the conversation has never been ingested.
func (s *Store) GetIngestState(ctx context.Context, convID int64) (*IngestState, error) {
	var (
		st     IngestState
		lastAt string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT conversation_id, rel_path, mtime_unix, size_bytes, content_hash, message_count, last_ingested_at
		   FROM ingest_state WHERE conversation_id = ?`, convID).
		Scan(&st.ConversationID, &st.RelPath, &st.MTimeUnix, &st.SizeBytes, &st.ContentHash, &st.MessageCount, &lastAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get ingest state: %w", err)
	}
	st.LastIngestedAt, _ = time.Parse(time.RFC3339, lastAt)
	return &st, nil
}

// SetIngestState upserts the incremental bookkeeping for a conversation.
func (s *Store) SetIngestState(ctx context.Context, st IngestState) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO ingest_state
		   (conversation_id, rel_path, mtime_unix, size_bytes, content_hash, message_count, last_ingested_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(conversation_id) DO UPDATE SET
		   rel_path=excluded.rel_path,
		   mtime_unix=excluded.mtime_unix,
		   size_bytes=excluded.size_bytes,
		   content_hash=excluded.content_hash,
		   message_count=excluded.message_count,
		   last_ingested_at=excluded.last_ingested_at`,
		st.ConversationID, st.RelPath, st.MTimeUnix, st.SizeBytes, st.ContentHash,
		st.MessageCount, st.LastIngestedAt.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("set ingest state: %w", err)
	}
	return nil
}

// ReplaceConversationMessages atomically replaces all messages (and their
// attachments and links) for a conversation with the supplied set. This makes
// re-ingesting a changed source export fully idempotent and correctly reflects
// edits and deletions, while messages.hash still uniquely keys each message.
// It returns the number of messages written.
//
// source is recorded on every inserted message so cross-source filtering and
// per-source aggregates (e.g. journal mechanical layer) work without touching
// the joined conversations row.
func (s *Store) ReplaceConversationMessages(ctx context.Context, convID int64, source string, msgs []signal.Message) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// Cascade deletes remove attachments and links for these messages.
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE conversation_id = ?`, convID); err != nil {
		return 0, fmt.Errorf("clear messages: %w", err)
	}
	// Reactions are keyed by stable hash (no FK to messages, so they are NOT
	// cascade-deleted with the message rows above); clear this conversation's
	// reactions explicitly so the re-insert below is a clean idempotent replace.
	if _, err := tx.ExecContext(ctx, `DELETE FROM reactions WHERE conversation_id = ?`, convID); err != nil {
		return 0, fmt.Errorf("clear reactions: %w", err)
	}

	insMsg, err := tx.PrepareContext(ctx,
		`INSERT INTO messages(hash, conversation_id, source, ts, ts_unix, sender, body, is_system, seq)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer insMsg.Close()
	// conversation_id is denormalized onto attachments and links (schemaV7) so
	// per-conversation counts stay single-table; stamp it on every insert.
	insAtt, err := tx.PrepareContext(ctx,
		`INSERT INTO attachments(message_id, conversation_id, kind, rel_path, original_name) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer insAtt.Close()
	insLink, err := tx.PrepareContext(ctx,
		`INSERT INTO links(message_id, conversation_id, url, domain) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer insLink.Close()
	// Reactions key by the stable per-source message hash, not message_id; the
	// UNIQUE(message_hash, emoji, actor) constraint dedups repeated (emoji, actor)
	// pairs within this replace, so use INSERT OR IGNORE.
	insReaction, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO reactions(conversation_id, message_hash, source, emoji, actor)
		 VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer insReaction.Close()

	for i := range msgs {
		m := &msgs[i]
		res, err := insMsg.ExecContext(ctx,
			m.HashWithSource(source), convID, source, m.TimestampRaw, m.Timestamp.Unix(), m.Sender, m.Body, boolToInt(m.IsSystem), m.Seq)
		if err != nil {
			return 0, fmt.Errorf("insert message %s: %w", m.ID(), err)
		}
		mid, err := res.LastInsertId()
		if err != nil {
			return 0, err
		}
		for _, a := range m.Attachments {
			if _, err := insAtt.ExecContext(ctx, mid, convID, string(a.Kind), a.RelPath, a.OriginalName); err != nil {
				return 0, fmt.Errorf("insert attachment: %w", err)
			}
		}
		for _, l := range m.Links {
			if _, err := insLink.ExecContext(ctx, mid, convID, l.URL, signal.Domain(l.URL)); err != nil {
				return 0, fmt.Errorf("insert link: %w", err)
			}
		}
		for _, r := range m.Reactions {
			if _, err := insReaction.ExecContext(ctx, convID, m.HashWithSource(source), source, r.Emoji, r.Actor); err != nil {
				return 0, fmt.Errorf("insert reaction: %w", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(msgs), nil
}

// ReplaceSnapshots replaces the snapshots inventory with the supplied set. The
// inventory is small and fully derived from the filesystem, so a full replace
// keeps it trivially consistent on every ingest.
func (s *Store) ReplaceSnapshots(ctx context.Context, snaps []Snapshot) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM snapshots`); err != nil {
		return fmt.Errorf("clear snapshots: %w", err)
	}
	ins, err := tx.PrepareContext(ctx,
		`INSERT INTO snapshots(filename, taken_at, taken_unix, size_bytes, tier) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	for _, sn := range snaps {
		if _, err := ins.ExecContext(ctx,
			sn.Filename, sn.TakenAt.Format(signal.TimestampLayout), sn.TakenAt.Unix(), sn.SizeBytes, sn.Tier); err != nil {
			return fmt.Errorf("insert snapshot %q: %w", sn.Filename, err)
		}
	}
	return tx.Commit()
}

// RecordIngestRun stores a run summary and returns its id.
func (s *Store) RecordIngestRun(ctx context.Context, r IngestRun) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO ingest_runs
		   (source, started_at, finished_at, duration_ms, conversations_scanned, conversations_changed,
		    messages_total, messages_added, snapshots_seen, skipped_lines, errors)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Source, r.StartedAt.UTC().Format(time.RFC3339), r.FinishedAt.UTC().Format(time.RFC3339), r.DurationMS,
		r.ConversationsScanned, r.ConversationsChanged, r.MessagesTotal, r.MessagesAdded,
		r.SnapshotsSeen, r.SkippedLines, r.Errors)
	if err != nil {
		return 0, fmt.Errorf("record ingest run: %w", err)
	}
	return res.LastInsertId()
}

// CountMessages returns the total number of messages in the database.
func (s *Store) CountMessages(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM messages`).Scan(&n)
	return n, err
}

// CountConversationMessages returns the number of messages in one conversation.
func (s *Store) CountConversationMessages(ctx context.Context, convID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM messages WHERE conversation_id = ?`, convID).Scan(&n)
	return n, err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
