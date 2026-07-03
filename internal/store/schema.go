package store

import "context"

// schemaVersion is the current schema revision, recorded in SQLite's
// `user_version` pragma. On Open, the migrations runner brings any older
// database forward to this version. Bump it and append a migration whenever the
// schema changes.
const schemaVersion = 7

// SchemaVersion returns the schema revision this binary expects (and migrates a
// database forward to on Open). Read-only callers — notably `msgbrowse doctor` —
// compare it against a database's PRAGMA user_version to report drift.
func SchemaVersion() int { return schemaVersion }

// UserVersion returns the database's recorded schema version (PRAGMA
// user_version). After a successful Open this equals SchemaVersion(); doctor
// reads it directly so it can report the value without re-deriving it.
func (s *Store) UserVersion(ctx context.Context) (int, error) {
	return readUserVersion(ctx, s.db)
}

// migrations is the ordered list of per-version migrations applied on Open.
// Each entry's index is its version (1-based; index 0 is unused).
//
// Invariant: every migration MUST be idempotent within its version transition.
// The runner wraps each entry in a transaction and only sets `user_version`
// after the transaction commits.
//
// Design notes:
//   - v1 lays down the original Signal-only schema (conversations / messages /
//     attachments / links / snapshots / ingest_state / ingest_runs / FTS5
//     virtual table + triggers).
//   - v2 introduces the unified contacts model (`contacts` and
//     `contact_identifiers`) and adds a `source` column to conversations,
//     messages, and ingest_runs so the store can hold data from Signal AND
//     iMessage (and future sources) at once. Existing rows are stamped
//     source='signal' and each Signal conversation is bootstrapped with a
//     contact and identifier; see internal/source for the canonical names.
var migrations = []string{
	0: "", // unused; versions are 1-based
	1: schemaV1,
	2: schemaV2,
	3: schemaV3,
	4: schemaV4,
	5: schemaV5,
	6: schemaV6,
	7: schemaV7,
}

// schemaV1 is the initial Signal-only schema. It is preserved verbatim so a
// fresh database walks through the same sequence of changes a long-lived one
// did, which makes reasoning about either trivial.
const schemaV1 = `
CREATE TABLE IF NOT EXISTS conversations (
    id   INTEGER PRIMARY KEY,
    name TEXT NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS messages (
    id              INTEGER PRIMARY KEY,
    hash            TEXT    NOT NULL UNIQUE,
    conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    ts              TEXT    NOT NULL,
    ts_unix         INTEGER NOT NULL,
    sender          TEXT    NOT NULL,
    body            TEXT    NOT NULL,
    is_system       INTEGER NOT NULL DEFAULT 0,
    seq             INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_messages_conv_ts ON messages(conversation_id, ts_unix);
CREATE INDEX IF NOT EXISTS idx_messages_sender  ON messages(sender);
CREATE INDEX IF NOT EXISTS idx_messages_ts_unix ON messages(ts_unix);

CREATE TABLE IF NOT EXISTS attachments (
    id            INTEGER PRIMARY KEY,
    message_id    INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    kind          TEXT    NOT NULL,
    rel_path      TEXT    NOT NULL,
    original_name TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_attachments_message ON attachments(message_id);
CREATE INDEX IF NOT EXISTS idx_attachments_kind    ON attachments(kind);

CREATE TABLE IF NOT EXISTS links (
    id         INTEGER PRIMARY KEY,
    message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    url        TEXT    NOT NULL,
    domain     TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_links_message ON links(message_id);
CREATE INDEX IF NOT EXISTS idx_links_domain  ON links(domain);

CREATE TABLE IF NOT EXISTS snapshots (
    id          INTEGER PRIMARY KEY,
    filename    TEXT    NOT NULL UNIQUE,
    taken_at    TEXT    NOT NULL,
    taken_unix  INTEGER NOT NULL,
    size_bytes  INTEGER NOT NULL,
    tier        TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS ingest_state (
    conversation_id  INTEGER PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
    rel_path         TEXT    NOT NULL,
    mtime_unix       INTEGER NOT NULL,
    size_bytes       INTEGER NOT NULL,
    content_hash     TEXT    NOT NULL,
    message_count    INTEGER NOT NULL,
    last_ingested_at TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS ingest_runs (
    id                     INTEGER PRIMARY KEY,
    started_at             TEXT    NOT NULL,
    finished_at            TEXT    NOT NULL,
    duration_ms            INTEGER NOT NULL,
    conversations_scanned  INTEGER NOT NULL,
    conversations_changed  INTEGER NOT NULL,
    messages_total         INTEGER NOT NULL,
    messages_added         INTEGER NOT NULL,
    snapshots_seen         INTEGER NOT NULL,
    skipped_lines          INTEGER NOT NULL,
    errors                 INTEGER NOT NULL
);

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    body,
    content='messages',
    content_rowid='id',
    tokenize='unicode61 remove_diacritics 2'
);

CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, body) VALUES (new.id, new.body);
END;
CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, body) VALUES ('delete', old.id, old.body);
END;
CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, body) VALUES ('delete', old.id, old.body);
    INSERT INTO messages_fts(rowid, body) VALUES (new.id, new.body);
END;
`

// schemaV2 layers the unified-contacts model on top of v1. It is safe to run
// against any database at version 1 (the only path that can reach it): the new
// tables are CREATEd, conversations is rebuilt to swap UNIQUE(name) for
// UNIQUE(source, name), and every existing Signal conversation is mapped to a
// fresh contact and identifier so the journal / contacts page see a populated
// world from day one. See docs/adr/0003-dual-source-archive.md.
//
// The runner toggles foreign keys off around the apply (SQLite's recommended
// pattern for rebuilding a referenced table) and back on afterward.
const schemaV2 = `
CREATE TABLE IF NOT EXISTS contacts (
    id           INTEGER PRIMARY KEY,
    display_name TEXT    NOT NULL,
    notes        TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS contact_identifiers (
    id         INTEGER PRIMARY KEY,
    contact_id INTEGER NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    source     TEXT    NOT NULL,
    identifier TEXT    NOT NULL,
    UNIQUE(source, identifier)
);
CREATE INDEX IF NOT EXISTS idx_contact_identifiers_contact ON contact_identifiers(contact_id);

CREATE TABLE conversations_new (
    id         INTEGER PRIMARY KEY,
    source     TEXT    NOT NULL DEFAULT 'signal',
    name       TEXT    NOT NULL,
    contact_id INTEGER REFERENCES contacts(id) ON DELETE SET NULL,
    is_group   INTEGER NOT NULL DEFAULT 0,
    UNIQUE(source, name)
);
INSERT INTO conversations_new (id, source, name, contact_id, is_group)
    SELECT id, 'signal', name, NULL, 0 FROM conversations;
DROP TABLE conversations;
ALTER TABLE conversations_new RENAME TO conversations;

ALTER TABLE messages    ADD COLUMN source TEXT NOT NULL DEFAULT 'signal';
ALTER TABLE ingest_runs ADD COLUMN source TEXT NOT NULL DEFAULT 'signal';

-- Bootstrap one contact per existing conversation. Matching by display_name is
-- safe ONLY here: at v1 conversations.name was UNIQUE, and this migration sees
-- Signal data exclusively, so the name→contact join is unambiguous and the
-- LIMIT 1 never discards a distinct person. DO NOT copy this match-by-name
-- pattern into the iMessage importer (Slice 2.5): once two sources share a
-- display_name it would silently merge two different people. Cross-source
-- linking is a deliberate, user-confirmed action on the contacts page
-- (ADR-0003), never a name-equality heuristic.
INSERT INTO contacts (display_name)
    SELECT name FROM conversations;
UPDATE conversations
   SET contact_id = (
       SELECT c.id FROM contacts c WHERE c.display_name = conversations.name LIMIT 1
   );
INSERT INTO contact_identifiers (contact_id, source, identifier)
    SELECT contact_id, source, name FROM conversations WHERE contact_id IS NOT NULL;
`

// schemaV3 adds the vector embeddings table for semantic search.
//
// Embeddings are keyed by the STABLE message hash (messages.hash), not the
// rowid: ReplaceConversationMessages deletes and re-inserts a conversation's
// rows on every re-ingest (changing rowids but not hashes), so keying by hash
// means unchanged messages keep their embeddings and only new/changed content
// is re-embedded. For that reason there is deliberately NO foreign key to
// messages — a CASCADE would wipe embeddings on every re-ingest. Embeddings for
// truly-deleted messages are harmless orphans that `embed --prune` can reclaim.
//
// vec is a little-endian float32 blob of length dim*4. The primary key is
// (message_hash, model) so embeddings from different models COEXIST: switching
// llm.embed_model (or benchmarking two models) does not overwrite and then have
// to re-embed the whole corpus on every switch — each model's vectors persist
// and a re-run under a previously-used model is a no-op.
const schemaV3 = `
CREATE TABLE IF NOT EXISTS embeddings (
    message_hash TEXT    NOT NULL,
    model        TEXT    NOT NULL,
    dim          INTEGER NOT NULL,
    vec          BLOB    NOT NULL,
    PRIMARY KEY (message_hash, model)
);
`

// schemaV4 adds AI-extracted contact facts and the per-conversation cursor that
// makes fact extraction incremental.
//
// contact_facts holds atomic, cited facts ABOUT a contact (e.g. "Has a dog
// named Biscuit", category "personal"), each carrying provenance: the source,
// the stable hash of the supporting message, and that message's timestamp.
// Facts are keyed to contacts(id) (not conversations) so a person whose Signal
// and iMessage threads are merged onto one contact accumulates a single,
// deduplicated fact set. fact_hash is a stable digest of the normalized fact
// text; UNIQUE(contact_id, fact_hash) makes PutFact idempotent, so re-running
// extraction (or processing two merged conversations) never duplicates a fact.
// There is deliberately NO foreign key from source_message_hash to messages:
// like embeddings, re-ingest deletes and re-inserts message rows (new rowids,
// stable hashes), so a CASCADE would wipe facts on every import. A fact whose
// supporting message later vanishes simply renders without a jump link.
//
// fact_state is the incrementality cursor: per conversation, the hash of the
// last message fed to the extractor and the chat model that produced the facts.
// The cursor is stored as a HASH (resolved back to a (ts_unix, id) keyset
// position at run time) rather than a rowid so it survives re-ingest. Recording
// the model means a model change re-scans from the start; dedup keeps that safe.
const schemaV4 = `
CREATE TABLE IF NOT EXISTS contact_facts (
    id                  INTEGER PRIMARY KEY,
    contact_id          INTEGER NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    fact                TEXT    NOT NULL,
    category            TEXT    NOT NULL,
    fact_hash           TEXT    NOT NULL,
    source              TEXT    NOT NULL,
    source_message_hash TEXT    NOT NULL,
    source_ts           TEXT    NOT NULL,
    source_ts_unix      INTEGER NOT NULL,
    model               TEXT    NOT NULL,
    created_at          TEXT    NOT NULL,
    UNIQUE(contact_id, fact_hash)
);
CREATE INDEX IF NOT EXISTS idx_contact_facts_contact ON contact_facts(contact_id);

CREATE TABLE IF NOT EXISTS fact_state (
    conversation_id   INTEGER PRIMARY KEY REFERENCES conversations(id) ON DELETE CASCADE,
    last_message_hash TEXT    NOT NULL,
    model             TEXT    NOT NULL,
    facts_added       INTEGER NOT NULL DEFAULT 0,
    updated_at        TEXT    NOT NULL
);
`

// schemaV5 adds the per-conversation `pinned` flag that drives the sidebar's
// PINNED section (SPEC-0006 REQ-0006-010). A plain additive ALTER: every
// existing conversation defaults to 0 (unpinned), so the migration is a no-op
// for already-populated databases and idempotent on re-run via the version
// guard. Ordering elsewhere is unchanged — the sidebar template, not the query,
// splits pinned from non-pinned, keeping both sections sorted by recency.
const schemaV5 = `
ALTER TABLE conversations ADD COLUMN pinned INTEGER NOT NULL DEFAULT 0;
`

// schemaV6 adds the reactions table that captures emoji reactions / iMessage
// tapbacks and renders them as badges on the target message (issue #50).
//
// Like embeddings (schemaV3) and contact_facts (schemaV4), reactions are keyed by
// the STABLE per-source message hash (messages.HashWithSource), NOT a message
// rowid: ReplaceConversationMessages deletes and re-inserts a conversation's rows
// on every re-ingest (rowids change, hashes don't), so there is deliberately NO
// foreign key to messages — a CASCADE would wipe reactions on each import. The FK
// is to conversations(id) instead, which gives the store a cheap per-conversation
// DELETE for the same idempotent full-replace pattern the messages use. source is
// stored alongside the hash so two sources that share a display name (and could
// otherwise collide on the same bare ID) stay distinct, matching how the message
// hash is namespaced. UNIQUE(message_hash, emoji, actor) dedups re-inserts within
// a single replace and makes ingestion idempotent.
const schemaV6 = `
CREATE TABLE IF NOT EXISTS reactions (
    id              INTEGER PRIMARY KEY,
    conversation_id INTEGER NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    message_hash    TEXT    NOT NULL,
    source          TEXT    NOT NULL,
    emoji           TEXT    NOT NULL,
    actor           TEXT    NOT NULL DEFAULT '',
    UNIQUE(message_hash, emoji, actor)
);
CREATE INDEX IF NOT EXISTS idx_reactions_message_hash ON reactions(message_hash);
`

// schemaV7 denormalizes conversation_id onto attachments and links so
// per-conversation media/link counts never have to walk a conversation's
// messages (SPEC-0008 REQ-0008-003: measured 44–112× faster sidebar counts).
//
// The column is a plain copy of the owning message's conversation_id, NOT a
// foreign key: the existing message_id FK already cascade-deletes these rows
// with their message, and adding an FK retroactively would force a full table
// rebuild for no extra integrity. DEFAULT 0 exists only because SQLite requires
// a default when ALTERing a NOT NULL column onto a populated table; the
// backfill UPDATE below rewrites every row from messages in the same
// transaction, and the ingest write path (ReplaceConversationMessages) stamps
// the column on every insert from then on, so 0 never survives outside a
// corrupt database. The UPDATE ... FROM join form (not a correlated subquery)
// leaves any orphaned row — impossible while the FK holds — at 0 instead of
// failing the migration with a NULL.
const schemaV7 = `
ALTER TABLE attachments ADD COLUMN conversation_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE links       ADD COLUMN conversation_id INTEGER NOT NULL DEFAULT 0;

UPDATE attachments SET conversation_id = m.conversation_id
  FROM messages m WHERE m.id = attachments.message_id;
UPDATE links SET conversation_id = m.conversation_id
  FROM messages m WHERE m.id = links.message_id;

CREATE INDEX IF NOT EXISTS idx_attachments_conv_kind ON attachments(conversation_id, kind);
CREATE INDEX IF NOT EXISTS idx_links_conv            ON links(conversation_id);
`
