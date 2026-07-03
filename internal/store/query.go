package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/joestump/msgbrowse/internal/signal"
)

// ConversationSummary is the sidebar/overview view of a conversation.
type ConversationSummary struct {
	ID           int64
	Name         string
	Source       string // "signal" | "imessage" — selects how media paths resolve
	Pinned       bool   // sidebar PINNED section membership (REQ-0006-010)
	MessageCount int
	FirstTS      string // "YYYY-MM-DD HH:MM:SS" of the earliest message ("" if none)
	LastTS       string // of the latest message
	LastTSUnix   int64
	LastSender   string
	LastPreview  string // truncated body of the latest message
	ImageCount   int
	FileCount    int
	LinkCount    int
	// Identifiers are the contact's cross-source handles (e.g. an iMessage
	// phone/email merged onto a Signal contact), excluding the conversation's
	// own source-side identity. Populated for the single-conversation view
	// (GetConversationByID); nil in the sidebar list.
	Identifiers []ContactIdentifier
	// Facts are the AI-extracted, cited facts about the contact. Populated for
	// the single-conversation view (GetConversationByID); nil in the sidebar
	// list. Empty until `msgbrowse facts` has run.
	Facts []ContactFact
}

// MessageView is a single message rendered for the transcript, with its
// attachments and links attached. ID is the internal rowid (cursor for
// pagination and context lookups); Hash is the stable public identifier.
type MessageView struct {
	ID          int64
	Hash        string
	Sender      string
	IsOwner     bool
	IsSystem    bool
	TS          string
	TSUnix      int64
	Body        string
	Attachments []AttachmentView
	Links       []LinkView
	Reactions   []ReactionView
}

// ReactionView is one emoji badge for a message, aggregated for display: Count is
// how many reactors applied this exact emoji and Actors lists their names (for a
// tooltip), in first-seen order. Reactions with no named actor still count.
type ReactionView struct {
	Emoji  string
	Count  int
	Actors []string
}

// AttachmentView is an attachment row for display.
type AttachmentView struct {
	Kind         string // "image" | "file"
	RelPath      string
	OriginalName string
}

// LinkView is a link row for display.
type LinkView struct {
	URL    string
	Domain string
}

// Page is a slice of messages plus the keyset cursor for the next page.
type Page struct {
	Messages   []MessageView
	NextTSUnix int64
	NextID     int64
	HasMore    bool
}

// ListConversations returns every conversation with summary stats, ordered by
// most-recent activity first. Conversations with no messages sort last.
//
// Everything is produced by ONE set-based statement (SPEC-0008 REQ-0008-001;
// the old base-query-plus-per-conversation fill loop measured 1.0–1.6s and
// 6,810 queries on the reference archive, this shape 346–388ms). First/last
// timestamps come from the rows selected by ts_unix ordering — the fm/lm rowid
// joins — never from MIN/MAX(ts) string aggregation, which sorts iMessage's
// month-name-first strings alphabetically (REQ-0008-002). The attachment/link
// counts group the denormalized conversation_id column directly (schemaV7)
// instead of joining messages. The last-message body ships as a 320-char prefix
// — plenty for the 80-rune preview — so 2,000+ full bodies never cross the
// driver. Deliberately NOT a window function: the ROW_NUMBER() variant measured
// 2.1s on the same archive (see SPEC-0008 design.md's measured rejects).
func (s *Store) ListConversations(ctx context.Context) ([]ConversationSummary, error) {
	const q = `
SELECT c.id, c.name, c.source, c.pinned,
       COALESCE(ms.msg_count, 0)             AS msg_count,
       COALESCE(fm.ts, '')                   AS first_ts,
       COALESCE(lm.ts, '')                   AS last_ts,
       COALESCE(ms.last_unix, 0)             AS last_unix,
       COALESCE(lm.sender, '')               AS last_sender,
       COALESCE(substr(lm.body, 1, 320), '') AS last_body,
       COALESCE(ac.image_count, 0)           AS image_count,
       COALESCE(ac.file_count, 0)            AS file_count,
       COALESCE(lc.link_count, 0)            AS link_count
  FROM conversations c
  LEFT JOIN (SELECT conversation_id, COUNT(*) msg_count, MAX(ts_unix) last_unix
               FROM messages GROUP BY conversation_id) ms ON ms.conversation_id = c.id
  LEFT JOIN messages fm ON fm.id = (SELECT m2.id FROM messages m2 WHERE m2.conversation_id = c.id
                                     ORDER BY m2.ts_unix ASC,  m2.id ASC  LIMIT 1)
  LEFT JOIN messages lm ON lm.id = (SELECT m2.id FROM messages m2 WHERE m2.conversation_id = c.id
                                     ORDER BY m2.ts_unix DESC, m2.id DESC LIMIT 1)
  LEFT JOIN (SELECT conversation_id,
                    SUM(kind = 'image') image_count, SUM(kind = 'file') file_count
               FROM attachments GROUP BY conversation_id) ac ON ac.conversation_id = c.id
  LEFT JOIN (SELECT conversation_id, COUNT(*) link_count
               FROM links GROUP BY conversation_id) lc ON lc.conversation_id = c.id
 ORDER BY COALESCE(ms.last_unix, 0) DESC, c.name ASC`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list conversations: %w", err)
	}
	defer rows.Close()

	var out []ConversationSummary
	for rows.Next() {
		var (
			cs   ConversationSummary
			body string
		)
		if err := rows.Scan(&cs.ID, &cs.Name, &cs.Source, &cs.Pinned, &cs.MessageCount,
			&cs.FirstTS, &cs.LastTS, &cs.LastTSUnix, &cs.LastSender, &body,
			&cs.ImageCount, &cs.FileCount, &cs.LinkCount); err != nil {
			return nil, err
		}
		cs.LastPreview = preview(body, 80)
		out = append(out, cs)
	}
	return out, rows.Err()
}

// SetPinned sets a conversation's pinned flag to an explicit state
// (REQ-0006-010). The sidebar's PINNED section lists conversations where
// pinned=1; ordering within each section stays by most-recent activity (the
// template does the split), so toggling pin only moves a row between sections,
// it does not re-sort.
func (s *Store) SetPinned(ctx context.Context, convID int64, pinned bool) error {
	v := 0
	if pinned {
		v = 1
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET pinned = ? WHERE id = ?`, v, convID); err != nil {
		return fmt.Errorf("set pinned: %w", err)
	}
	return nil
}

// TogglePinned flips a conversation's pinned flag in a single UPDATE — no
// read-modify-write, so the pin route never pays for a summary fetch first
// (SPEC-0008 REQ-0008-005). It reports whether the conversation exists, which
// is all the handler needs for its 404-vs-redirect decision.
func (s *Store) TogglePinned(ctx context.Context, convID int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET pinned = 1 - pinned WHERE id = ?`, convID)
	if err != nil {
		return false, fmt.Errorf("toggle pinned: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// fillLastMessage and fillCounts enrich a SINGLE conversation's summary
// (GetConversationByID). The sidebar listing must never call them — that was
// the 3-queries-per-conversation N+1 that SPEC-0008 REQ-0008-001 removed.
func (s *Store) fillLastMessage(ctx context.Context, cs *ConversationSummary) error {
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

func (s *Store) fillCounts(ctx context.Context, cs *ConversationSummary) error {
	// Single-table counts via the denormalized conversation_id (schemaV7) and
	// its idx_attachments_conv_kind / idx_links_conv indexes — no messages join.
	err := s.db.QueryRowContext(ctx,
		`SELECT
		   COALESCE(SUM(kind = 'image'), 0),
		   COALESCE(SUM(kind = 'file'),  0)
		 FROM attachments
		 WHERE conversation_id = ?`, cs.ID).Scan(&cs.ImageCount, &cs.FileCount)
	if err != nil {
		return err
	}
	return s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM links WHERE conversation_id = ?`, cs.ID).Scan(&cs.LinkCount)
}

// GetConversation returns a single conversation summary by name.
func (s *Store) GetConversation(ctx context.Context, name string) (*ConversationSummary, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM conversations WHERE name = ?`, name).Scan(&id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.GetConversationByID(ctx, id)
}

// GetConversationByID returns a single conversation summary by id.
//
// First/last timestamps are read from the rows selected by ts_unix ordering —
// indexed LIMIT-1 probes on idx_messages_conv_ts — not MIN/MAX(ts) string
// aggregation, which is chronologically wrong for iMessage's month-name-first
// ts strings (SPEC-0008 REQ-0008-002).
func (s *Store) GetConversationByID(ctx context.Context, id int64) (*ConversationSummary, error) {
	cs := ConversationSummary{ID: id}
	err := s.db.QueryRowContext(ctx,
		`SELECT c.name, c.source, c.pinned,
		        (SELECT COUNT(*) FROM messages m WHERE m.conversation_id = c.id),
		        COALESCE((SELECT m.ts FROM messages m WHERE m.conversation_id = c.id
		                   ORDER BY m.ts_unix ASC,  m.id ASC  LIMIT 1), ''),
		        COALESCE((SELECT m.ts FROM messages m WHERE m.conversation_id = c.id
		                   ORDER BY m.ts_unix DESC, m.id DESC LIMIT 1), ''),
		        COALESCE((SELECT m.ts_unix FROM messages m WHERE m.conversation_id = c.id
		                   ORDER BY m.ts_unix DESC, m.id DESC LIMIT 1), 0)
		   FROM conversations c
		  WHERE c.id = ?`, id).
		Scan(&cs.Name, &cs.Source, &cs.Pinned, &cs.MessageCount, &cs.FirstTS, &cs.LastTS, &cs.LastTSUnix)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if cs.MessageCount > 0 {
		if err := s.fillLastMessage(ctx, &cs); err != nil {
			return nil, err
		}
		if err := s.fillCounts(ctx, &cs); err != nil {
			return nil, err
		}
	}
	// Cross-source handles and AI facts travel with the single-conversation view
	// so every render path (transcript, jump-to-context) gets them without each
	// handler remembering a separate fetch.
	idents, err := s.ConversationIdentifiers(ctx, id)
	if err != nil {
		return nil, err
	}
	cs.Identifiers = idents
	facts, err := s.ContactFactsByConversation(ctx, id)
	if err != nil {
		return nil, err
	}
	cs.Facts = facts
	return &cs, nil
}

// GetMessages returns a chronological page of a conversation's messages using a
// keyset cursor on (ts_unix, id). Pass afterTSUnix=0, afterID=0 for the first
// (oldest) page; pass the returned NextTSUnix/NextID for subsequent pages.
func (s *Store) GetMessages(ctx context.Context, convID, afterTSUnix, afterID int64, limit int) (*Page, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	// Fetch limit+1 to detect whether more pages exist.
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, hash, sender, is_system, ts, ts_unix, body
		   FROM messages
		  WHERE conversation_id = ?
		    AND (ts_unix > ? OR (ts_unix = ? AND id > ?))
		  ORDER BY ts_unix ASC, id ASC
		  LIMIT ?`, convID, afterTSUnix, afterTSUnix, afterID, limit+1)
	if err != nil {
		return nil, fmt.Errorf("get messages: %w", err)
	}
	defer rows.Close()

	var msgs []MessageView
	for rows.Next() {
		var m MessageView
		var isSystem int
		if err := rows.Scan(&m.ID, &m.Hash, &m.Sender, &isSystem, &m.TS, &m.TSUnix, &m.Body); err != nil {
			return nil, err
		}
		m.IsSystem = isSystem == 1
		m.IsOwner = m.Sender == signal.OwnerSender
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	page := &Page{}
	if len(msgs) > limit {
		page.HasMore = true
		msgs = msgs[:limit]
	}
	if err := s.attachChildren(ctx, msgs); err != nil {
		return nil, err
	}
	page.Messages = msgs
	if n := len(msgs); n > 0 {
		page.NextTSUnix = msgs[n-1].TSUnix
		page.NextID = msgs[n-1].ID
	}
	return page, nil
}

// ConversationTranscript returns a conversation's messages in chronological
// order, optionally bounded by a unix-time range, up to limit. It is the
// transcript-retrieval primitive for the MCP get_conversation tool.
func (s *Store) ConversationTranscript(ctx context.Context, convID, startUnix, endUnix int64, limit int) ([]MessageView, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	where := []string{"conversation_id = ?"}
	args := []any{convID}
	if startUnix > 0 {
		where = append(where, "ts_unix >= ?")
		args = append(args, startUnix)
	}
	if endUnix > 0 {
		where = append(where, "ts_unix <= ?")
		args = append(args, endUnix)
	}
	q := `SELECT id, hash, sender, is_system, ts, ts_unix, body FROM messages
	       WHERE ` + strings.Join(where, " AND ") + `
	       ORDER BY ts_unix ASC, id ASC LIMIT ?`
	args = append(args, limit)

	msgs, err := s.queryMessages(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("conversation transcript: %w", err)
	}
	if err := s.attachChildren(ctx, msgs); err != nil {
		return nil, err
	}
	return msgs, nil
}

// GetContext returns up to `window` messages on each side of the message with the
// given internal id (for jump-to-context from search results). The target itself
// is included.
func (s *Store) GetContext(ctx context.Context, messageID int64, window int) ([]MessageView, error) {
	if window < 0 {
		window = 0
	}
	var convID, tsUnix int64
	err := s.db.QueryRowContext(ctx,
		`SELECT conversation_id, ts_unix FROM messages WHERE id = ?`, messageID).Scan(&convID, &tsUnix)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// `window` older (inclusive of target via >=), then `window` newer.
	before, err := s.queryMessages(ctx,
		`SELECT id, hash, sender, is_system, ts, ts_unix, body FROM messages
		  WHERE conversation_id = ? AND (ts_unix < ? OR (ts_unix = ? AND id <= ?))
		  ORDER BY ts_unix DESC, id DESC LIMIT ?`,
		convID, tsUnix, tsUnix, messageID, window+1)
	if err != nil {
		return nil, err
	}
	after, err := s.queryMessages(ctx,
		`SELECT id, hash, sender, is_system, ts, ts_unix, body FROM messages
		  WHERE conversation_id = ? AND (ts_unix > ? OR (ts_unix = ? AND id > ?))
		  ORDER BY ts_unix ASC, id ASC LIMIT ?`,
		convID, tsUnix, tsUnix, messageID, window)
	if err != nil {
		return nil, err
	}
	// before is newest-first; reverse to chronological, then append after.
	reverse(before)
	all := append(before, after...)
	if err := s.attachChildren(ctx, all); err != nil {
		return nil, err
	}
	return all, nil
}

// queryMessages runs a message SELECT with the standard column list and scans
// the rows into MessageViews (without children).
func (s *Store) queryMessages(ctx context.Context, q string, args ...any) ([]MessageView, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageView
	for rows.Next() {
		var m MessageView
		var isSystem int
		if err := rows.Scan(&m.ID, &m.Hash, &m.Sender, &isSystem, &m.TS, &m.TSUnix, &m.Body); err != nil {
			return nil, err
		}
		m.IsSystem = isSystem == 1
		m.IsOwner = m.Sender == signal.OwnerSender
		out = append(out, m)
	}
	return out, rows.Err()
}

// attachChildren populates Attachments and Links for the given messages in two
// batched queries (avoids N+1).
func (s *Store) attachChildren(ctx context.Context, msgs []MessageView) error {
	if len(msgs) == 0 {
		return nil
	}
	idx := make(map[int64]int, len(msgs))
	ids := make([]any, len(msgs))
	for i := range msgs {
		idx[msgs[i].ID] = i
		ids[i] = msgs[i].ID
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")

	attRows, err := s.db.QueryContext(ctx,
		`SELECT message_id, kind, rel_path, original_name FROM attachments
		  WHERE message_id IN (`+placeholders+`) ORDER BY id`, ids...)
	if err != nil {
		return err
	}
	defer attRows.Close()
	for attRows.Next() {
		var mid int64
		var a AttachmentView
		if err := attRows.Scan(&mid, &a.Kind, &a.RelPath, &a.OriginalName); err != nil {
			return err
		}
		if i, ok := idx[mid]; ok {
			msgs[i].Attachments = append(msgs[i].Attachments, a)
		}
	}
	if err := attRows.Err(); err != nil {
		return err
	}

	linkRows, err := s.db.QueryContext(ctx,
		`SELECT message_id, url, domain FROM links
		  WHERE message_id IN (`+placeholders+`) ORDER BY id`, ids...)
	if err != nil {
		return err
	}
	defer linkRows.Close()
	for linkRows.Next() {
		var mid int64
		var l LinkView
		if err := linkRows.Scan(&mid, &l.URL, &l.Domain); err != nil {
			return err
		}
		if i, ok := idx[mid]; ok {
			msgs[i].Links = append(msgs[i].Links, l)
		}
	}
	if err := linkRows.Err(); err != nil {
		return err
	}

	return s.attachReactions(ctx, msgs)
}

// attachReactions loads reactions for the given messages and aggregates them into
// per-emoji ReactionViews. Reactions key by the STABLE message hash (not rowid;
// see schemaV6), so the join is on messages.hash, batched to avoid N+1.
func (s *Store) attachReactions(ctx context.Context, msgs []MessageView) error {
	if len(msgs) == 0 {
		return nil
	}
	hashIdx := make(map[string]int, len(msgs))
	hashes := make([]any, len(msgs))
	for i := range msgs {
		hashIdx[msgs[i].Hash] = i
		hashes[i] = msgs[i].Hash
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(hashes)), ",")

	rows, err := s.db.QueryContext(ctx,
		`SELECT message_hash, emoji, actor FROM reactions
		  WHERE message_hash IN (`+placeholders+`) ORDER BY id`, hashes...)
	if err != nil {
		return err
	}
	defer rows.Close()

	// Aggregate per message → per emoji, preserving first-seen emoji order.
	type agg struct {
		order []string
		byKey map[string]*ReactionView
	}
	perMsg := make(map[int]*agg)
	for rows.Next() {
		var hash, emoji, actor string
		if err := rows.Scan(&hash, &emoji, &actor); err != nil {
			return err
		}
		i, ok := hashIdx[hash]
		if !ok {
			continue
		}
		a := perMsg[i]
		if a == nil {
			a = &agg{byKey: map[string]*ReactionView{}}
			perMsg[i] = a
		}
		rv, ok := a.byKey[emoji]
		if !ok {
			rv = &ReactionView{Emoji: emoji}
			a.byKey[emoji] = rv
			a.order = append(a.order, emoji)
		}
		rv.Count++
		if actor != "" {
			rv.Actors = append(rv.Actors, actor)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i, a := range perMsg {
		views := make([]ReactionView, 0, len(a.order))
		for _, emoji := range a.order {
			views = append(views, *a.byKey[emoji])
		}
		msgs[i].Reactions = views
	}
	return nil
}

// LatestIngestRun returns the most recent ingest run summary, or nil if none.
func (s *Store) LatestIngestRun(ctx context.Context) (*IngestRun, error) {
	var (
		r                 IngestRun
		started, finished string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT started_at, finished_at, duration_ms, conversations_scanned, conversations_changed,
		        messages_total, messages_added, snapshots_seen, skipped_lines, errors
		   FROM ingest_runs ORDER BY id DESC LIMIT 1`).
		Scan(&started, &finished, &r.DurationMS, &r.ConversationsScanned, &r.ConversationsChanged,
			&r.MessagesTotal, &r.MessagesAdded, &r.SnapshotsSeen, &r.SkippedLines, &r.Errors)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.StartedAt = parseRFC3339(started)
	r.FinishedAt = parseRFC3339(finished)
	return &r, nil
}

// ListSnapshots returns the snapshot inventory ordered newest first.
func (s *Store) ListSnapshots(ctx context.Context) ([]Snapshot, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT filename, taken_at, taken_unix, size_bytes, tier FROM snapshots ORDER BY taken_unix DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		var sn Snapshot
		var takenAt string
		var takenUnix int64
		if err := rows.Scan(&sn.Filename, &takenAt, &takenUnix, &sn.SizeBytes, &sn.Tier); err != nil {
			return nil, err
		}
		sn.TakenAt = parseLayout(takenAt)
		out = append(out, sn)
	}
	return out, rows.Err()
}

// ContactIdentifier is one source-side handle for a contact (e.g. a phone
// number or email from iMessage, or a Signal display name).
type ContactIdentifier struct {
	Source     string
	Identifier string
}

// ConversationIdentifiers returns the identifiers of the contact linked to the
// given conversation, ordered by source then identifier. Returns nil when the
// conversation has no linked contact (e.g. a group). Only the conversation's
// own source-side identity — the (source, name) row created at ingest — is
// excluded, so a Signal conversation doesn't echo its own display name back.
// The exclusion is keyed on both source and value (not value alone) so a
// genuine cross-source handle that happens to share the conversation's name
// (e.g. a phone-named Signal contact merged with the same iMessage number) is
// still surfaced rather than collaterally hidden.
func (s *Store) ConversationIdentifiers(ctx context.Context, convID int64) ([]ContactIdentifier, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT ci.source, ci.identifier
  FROM conversations c
  JOIN contact_identifiers ci ON ci.contact_id = c.contact_id
 WHERE c.id = ? AND NOT (ci.source = c.source AND ci.identifier = c.name)
 ORDER BY ci.source, ci.identifier`, convID)
	if err != nil {
		return nil, fmt.Errorf("conversation identifiers: %w", err)
	}
	defer rows.Close()
	var out []ContactIdentifier
	for rows.Next() {
		var ci ContactIdentifier
		if err := rows.Scan(&ci.Source, &ci.Identifier); err != nil {
			return nil, err
		}
		out = append(out, ci)
	}
	return out, rows.Err()
}

// MessageConversationID returns the id of the conversation that owns the given
// message, or (0, false) if no such message exists. Used to verify that a
// jump-to-context request's message actually belongs to the URL's conversation.
func (s *Store) MessageConversationID(ctx context.Context, messageID int64) (int64, bool, error) {
	var convID int64
	err := s.db.QueryRowContext(ctx,
		`SELECT conversation_id FROM messages WHERE id = ?`, messageID).Scan(&convID)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return convID, true, nil
}

// NewestMessageTS returns the latest message timestamp across all conversations
// ("" if the database is empty) — used to show export freshness. The row is
// picked by ts_unix through idx_messages_ts_unix: a sub-ms probe where the old
// MAX(ts) string aggregation full-scanned 405k rows (430ms) AND sorted iMessage
// month-name strings alphabetically (SPEC-0008 REQ-0008-002/004).
func (s *Store) NewestMessageTS(ctx context.Context) (string, error) {
	var ts string
	err := s.db.QueryRowContext(ctx,
		`SELECT ts FROM messages ORDER BY ts_unix DESC LIMIT 1`).Scan(&ts)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return ts, nil
}

// ConversationSourceName is the minimal single-row lookup for handlers that
// only need a conversation's source and name — media serving, infinite-scroll
// pages — where GetConversationByID's counts/identifiers/facts aggregation is
// wasted work on a hot path (SPEC-0008 REQ-0008-005). Returns sql.ErrNoRows
// when the conversation does not exist.
func (s *Store) ConversationSourceName(ctx context.Context, id int64) (source, name string, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT source, name FROM conversations WHERE id = ?`, id).Scan(&source, &name)
	if err != nil && err != sql.ErrNoRows {
		return "", "", fmt.Errorf("conversation source/name: %w", err)
	}
	return source, name, err
}
