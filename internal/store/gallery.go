package store

import (
	"context"
	"fmt"
	"strings"
)

// GalleryFilter narrows the media/links gallery by conversation, source, date
// range, and (for links) domain. The zero value means "everything".
//
// Every predicate resolves against the denormalized conversation_id (schemaV7)
// and ts_unix (schemaV8) columns on attachments/links — the messages table is
// never part of filtering (SPEC-0008 REQ-0008-009). Source narrows through the
// tiny conversations table (a conversation's messages all share its source by
// construction: ReplaceConversationMessages stamps one source per replace and
// conversations are UNIQUE(source, name)).
type GalleryFilter struct {
	// ConversationIDs limits results to any of these conversations (issue #6:
	// the Media filter is multi-select). Empty means all conversations; ids
	// bind as an IN(...) parameter set, never string-interpolated.
	ConversationIDs []int64
	Source          string
	Domain          string // links only; exact match after www-stripping (MCP list_links)
	StartUnix       int64
	EndUnix         int64
	Limit           int
	// SortAsc flips the attachment walk to oldest-first; the zero value keeps
	// the newest-first default. Links stay domain-ordered regardless (their
	// display order is grouped, not chronological), so this only steers
	// ListAttachments (Images/Files).
	SortAsc bool
}

// MediaItem is one attachment (image or file) with the provenance needed to
// render it and link back to its message in context.
type MediaItem struct {
	ID               int64 // attachment id: keyset cursor + stable lightbox anchors
	ConversationID   int64
	ConversationName string
	Source           string
	MessageID        int64
	Kind             string // "image" | "file"
	RelPath          string
	OriginalName     string
	TS               string
	TSUnix           int64
}

// MediaPage is one keyset page of attachments plus the cursor for the next
// (older) page. NextTSUnix/NextID are only meaningful when HasMore is true.
type MediaPage struct {
	Items      []MediaItem
	HasMore    bool
	NextTSUnix int64
	NextID     int64
}

// LinkItem is one deduplicated URL with its domain, occurrence count, and the
// earliest message it appeared in (for "jump to source"). DomainTotal is the
// number of distinct URLs sharing the domain across the whole filtered result
// — not just this page — so the gallery's per-domain badge stays truthful when
// a domain spans page boundaries.
type LinkItem struct {
	URL              string
	Domain           string
	Count            int
	DomainTotal      int
	ConversationID   int64
	ConversationName string
	Source           string
	MessageID        int64
	TS               string
	TSUnix           int64
}

// LinkCursor is the keyset cursor into the deduplicated links ordering
// (domain ASC, count DESC, earliest ts ASC, url ASC). URL doubles as the
// presence sentinel: every row has a non-empty URL, so a zero cursor means
// "from the top".
type LinkCursor struct {
	Domain string
	Count  int
	TSUnix int64
	URL    string
}

// LinkPage is one keyset page of deduplicated links plus the cursor for the
// next page. Next is only meaningful when HasMore is true.
type LinkPage struct {
	Links   []LinkItem
	HasMore bool
	Next    LinkCursor
}

// inPlaceholders returns a "?,?,..." list of n bound-parameter placeholders
// for an IN(...) set.
func inPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// attachmentClauses builds WHERE clauses (alias a) for attachments queries.
// Only denormalized columns appear — see GalleryFilter.
func attachmentClauses(f GalleryFilter) ([]string, []any) {
	var where []string
	var args []any
	if n := len(f.ConversationIDs); n > 0 {
		where = append(where, "a.conversation_id IN ("+inPlaceholders(n)+")")
		for _, id := range f.ConversationIDs {
			args = append(args, id)
		}
	}
	if f.Source != "" {
		where = append(where, "a.conversation_id IN (SELECT id FROM conversations WHERE source = ?)")
		args = append(args, f.Source)
	}
	if f.StartUnix > 0 {
		where = append(where, "a.ts_unix >= ?")
		args = append(args, f.StartUnix)
	}
	if f.EndUnix > 0 {
		where = append(where, "a.ts_unix <= ?")
		args = append(args, f.EndUnix)
	}
	return where, args
}

// linkClauses builds WHERE clauses (alias l) for links queries. Only
// denormalized columns appear — see GalleryFilter.
func linkClauses(f GalleryFilter) ([]string, []any) {
	var where []string
	var args []any
	if n := len(f.ConversationIDs); n > 0 {
		where = append(where, "l.conversation_id IN ("+inPlaceholders(n)+")")
		for _, id := range f.ConversationIDs {
			args = append(args, id)
		}
	}
	if f.Source != "" {
		where = append(where, "l.conversation_id IN (SELECT id FROM conversations WHERE source = ?)")
		args = append(args, f.Source)
	}
	if f.Domain != "" {
		where = append(where, "l.domain = ?")
		args = append(args, f.Domain)
	}
	if f.StartUnix > 0 {
		where = append(where, "l.ts_unix >= ?")
		args = append(args, f.StartUnix)
	}
	if f.EndUnix > 0 {
		where = append(where, "l.ts_unix <= ?")
		args = append(args, f.EndUnix)
	}
	return where, args
}

// whereSQL joins clauses into a WHERE fragment ("" when unfiltered), with a
// leading space when present so callers can concatenate directly.
func whereSQL(clauses []string) string {
	if len(clauses) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(clauses, " AND ")
}

// andSQL joins clauses as additional AND terms for queries that already have a
// WHERE ("" when there is nothing to add).
func andSQL(clauses []string) string {
	if len(clauses) == 0 {
		return ""
	}
	return " AND " + strings.Join(clauses, " AND ")
}

func galleryLimit(f GalleryFilter, def, max int) int {
	if f.Limit <= 0 || f.Limit > max {
		return def
	}
	return f.Limit
}

// listAttachmentsSQL builds the keyset listing query for one attachment kind.
// It is a standalone builder so tests can EXPLAIN QUERY PLAN the exact SQL the
// store runs (SPEC-0008 REQ-0008-009).
//
// The driving index is pinned explicitly because the two candidate plans have
// asymmetric worst cases and SQLite (without ANALYZE data) picks wrong for
// filtered queries:
//   - Unfiltered / source / date paths walk idx_attachments_kind_ts backward:
//     (kind, ts_unix, rowid) is exactly ORDER BY a.ts_unix DESC, a.id DESC
//     within a kind, so the scan emits rows already in display order and stops
//     at LIMIT — no sort, no messages touch (measured 357 ms → 10 ms).
//   - Conversation-filtered paths seek idx_attachments_conv_kind instead: the
//     candidate set is bounded by the selected conversations' attachments
//     (SQLite runs an IN(...) over the index's leading column as one seek per
//     id), so the residual sort is small; walking the kind_ts index here would
//     degrade to a full-index walk for conversations with few recent
//     attachments (measured 104 ms vs 1 ms on a sparse conversation).
//
// messages and conversations are joined unaliased and only by INTEGER PRIMARY
// KEY for the ≤ limit result rows — plans SEARCH them, never SCAN.
func listAttachmentsSQL(kind string, f GalleryFilter, cursorTSUnix, cursorID int64, limit int) (string, []any) {
	clauses, filterArgs := attachmentClauses(f)
	idx := "idx_attachments_kind_ts"
	if len(f.ConversationIDs) > 0 {
		idx = "idx_attachments_conv_kind"
	}
	q := `
SELECT a.id, a.conversation_id, conversations.name, conversations.source, a.message_id,
       a.kind, a.rel_path, a.original_name, messages.ts, a.ts_unix
  FROM attachments a INDEXED BY ` + idx + `
  JOIN conversations ON conversations.id = a.conversation_id
  JOIN messages      ON messages.id = a.message_id
 WHERE a.kind = ?` + andSQL(clauses)
	args := append([]any{kind}, filterArgs...)
	if cursorTSUnix != 0 || cursorID != 0 {
		// Keyset seek in the walk direction: newest-first pages continue strictly
		// before the cursor, oldest-first strictly after it. Each is spelled with
		// a redundant ts_unix bound so the pinned index seeks straight to the
		// cursor instead of re-walking and discarding every already-shown entry.
		if f.SortAsc {
			q += ` AND a.ts_unix >= ? AND (a.ts_unix > ? OR a.id > ?)`
		} else {
			q += ` AND a.ts_unix <= ? AND (a.ts_unix < ? OR a.id < ?)`
		}
		args = append(args, cursorTSUnix, cursorTSUnix, cursorID)
	}
	// Same pinned index serves both directions — SQLite walks it forward for ASC
	// and backward for DESC, emitting rows already in display order (no sort).
	if f.SortAsc {
		q += `
 ORDER BY a.ts_unix ASC, a.id ASC
 LIMIT ?`
	} else {
		q += `
 ORDER BY a.ts_unix DESC, a.id DESC
 LIMIT ?`
	}
	args = append(args, limit)
	return q, args
}

// ListAttachments returns one page of attachments of the given kind ("image"
// or "file") matching the filter, ordered newest-first by default or
// oldest-first when f.SortAsc is set. A zero cursor starts from the leading
// edge; passing the previous page's NextTSUnix/NextID continues strictly after
// it in the chosen order. Page size is f.Limit (default 200, max 1000).
func (s *Store) ListAttachments(ctx context.Context, kind string, f GalleryFilter, cursorTSUnix, cursorID int64) (*MediaPage, error) {
	limit := galleryLimit(f, 200, 1000)
	// Fetch limit+1 to detect whether more pages exist.
	q, args := listAttachmentsSQL(kind, f, cursorTSUnix, cursorID, limit+1)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list attachments: %w", err)
	}
	defer rows.Close()
	var items []MediaItem
	for rows.Next() {
		var m MediaItem
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.ConversationName, &m.Source, &m.MessageID,
			&m.Kind, &m.RelPath, &m.OriginalName, &m.TS, &m.TSUnix); err != nil {
			return nil, err
		}
		items = append(items, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	page := &MediaPage{}
	if len(items) > limit {
		page.HasMore = true
		items = items[:limit]
	}
	page.Items = items
	if n := len(items); n > 0 {
		page.NextTSUnix = items[n-1].TSUnix
		page.NextID = items[n-1].ID
	}
	return page, nil
}

// ListImageAttachments returns every image attachment (no filter, no limit) with
// the conversation source/name needed to resolve its on-disk path. Used by the
// image transcoder to find HEIC/TIFF files to convert; ordered oldest-first so a
// resumed run makes steady forward progress.
func (s *Store) ListImageAttachments(ctx context.Context) ([]MediaItem, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT a.id, m.conversation_id, c.name, m.source, m.id, a.kind, a.rel_path, a.original_name, m.ts, m.ts_unix
  FROM attachments a
  JOIN messages m      ON m.id = a.message_id
  JOIN conversations c ON c.id = m.conversation_id
 WHERE a.kind = 'image'
 ORDER BY m.ts_unix ASC, m.id ASC, a.id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list image attachments: %w", err)
	}
	defer rows.Close()
	var out []MediaItem
	for rows.Next() {
		var m MediaItem
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.ConversationName, &m.Source, &m.MessageID,
			&m.Kind, &m.RelPath, &m.OriginalName, &m.TS, &m.TSUnix); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// listLinksSQL builds the keyset listing query for deduplicated links.
// Standalone for the same EXPLAIN QUERY PLAN testability as listAttachmentsSQL.
//
// Dedup layers, innermost out:
//  1. GROUP BY url over links alone computes each URL's occurrence count and —
//     via SQLite's bare-column min() guarantee — the conversation/message of
//     its earliest occurrence. With idx_links_gallery this is one covering
//     index scan: no whole-table row fetches, no messages join (an earlier
//     window-function formulation cost ~450 ms; this runs in ~35 ms on the
//     reference archive).
//  2. A window pass tags every deduplicated row with its domain's total
//     distinct-URL count, computed before the keyset cut so a domain split
//     across pages still shows its full total.
//  3. The keyset predicate + ORDER BY + LIMIT pick one page in display order
//     (domain ASC, count DESC, earliest ts ASC, url ASC — url is the unique
//     tiebreaker that makes the walk resumable).
//
// Only the ≤ limit page rows then join conversations/messages (by INTEGER
// PRIMARY KEY — SEARCH, never SCAN) for the display name, source, and
// timestamp string.
func listLinksSQL(f GalleryFilter, cur LinkCursor, limit int) (string, []any) {
	clauses, args := linkClauses(f)
	q := `
SELECT o.url, o.domain, o.cnt, o.dtotal, o.conversation_id, conversations.name, conversations.source,
       o.message_id, messages.ts, o.ts_unix
  FROM (
    SELECT url, domain, cnt, dtotal, conversation_id, message_id, ts_unix
      FROM (
        SELECT url, domain, cnt, conversation_id, message_id, ts_unix,
               COUNT(*) OVER (PARTITION BY domain) AS dtotal
          FROM (
            SELECT l.url AS url, l.domain AS domain, COUNT(*) AS cnt,
                   MIN(l.ts_unix) AS ts_unix,
                   l.conversation_id AS conversation_id, l.message_id AS message_id
              FROM links l` + whereSQL(clauses) + `
             GROUP BY l.url
          )
      )`
	if cur.URL != "" {
		q += `
     WHERE (domain > ? OR (domain = ? AND cnt < ?)
        OR (domain = ? AND cnt = ? AND ts_unix > ?)
        OR (domain = ? AND cnt = ? AND ts_unix = ? AND url > ?))`
		args = append(args, cur.Domain, cur.Domain, cur.Count,
			cur.Domain, cur.Count, cur.TSUnix,
			cur.Domain, cur.Count, cur.TSUnix, cur.URL)
	}
	q += `
     ORDER BY domain ASC, cnt DESC, ts_unix ASC, url ASC
     LIMIT ?
  ) o
  JOIN conversations ON conversations.id = o.conversation_id
  JOIN messages      ON messages.id = o.message_id
 ORDER BY o.domain ASC, o.cnt DESC, o.ts_unix ASC, o.url ASC`
	args = append(args, limit)
	return q, args
}

// ListLinks returns one page of links matching the filter, deduplicated by
// URL. Each item carries its total occurrence count, its domain's distinct-URL
// total, and the earliest message it appeared in (for "jump to source").
// Results are ordered by domain, then descending occurrence count. A zero
// cursor starts from the top; passing the previous page's Next cursor
// continues strictly after it. Page size is f.Limit (default 200, max 1000).
func (s *Store) ListLinks(ctx context.Context, f GalleryFilter, cur LinkCursor) (*LinkPage, error) {
	limit := galleryLimit(f, 200, 1000)
	// Fetch limit+1 to detect whether more pages exist.
	q, args := listLinksSQL(f, cur, limit+1)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}
	defer rows.Close()

	var links []LinkItem
	for rows.Next() {
		var li LinkItem
		if err := rows.Scan(&li.URL, &li.Domain, &li.Count, &li.DomainTotal, &li.ConversationID,
			&li.ConversationName, &li.Source, &li.MessageID, &li.TS, &li.TSUnix); err != nil {
			return nil, err
		}
		links = append(links, li)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	page := &LinkPage{}
	if len(links) > limit {
		page.HasMore = true
		links = links[:limit]
	}
	page.Links = links
	if n := len(links); n > 0 {
		last := links[n-1]
		page.Next = LinkCursor{Domain: last.Domain, Count: last.Count, TSUnix: last.TSUnix, URL: last.URL}
	}
	return page, nil
}

// MediaCounts is the per-tab totals shown on the gallery (so empty tabs are
// obvious and the active filter's effect is visible).
type MediaCounts struct {
	Images int
	Files  int
	Links  int // distinct URLs
}

// countAttachmentsSQL builds the per-kind attachment count. Unfiltered it is a
// covering scan of idx_attachments_kind; every filter stays on attachments'
// own denormalized columns (REQ-0008-009 — measured 576 ms → 7 ms without the
// messages join).
func countAttachmentsSQL(f GalleryFilter) (string, []any) {
	clauses, args := attachmentClauses(f)
	return `SELECT a.kind, COUNT(*) FROM attachments a` + whereSQL(clauses) + ` GROUP BY a.kind`, args
}

// countLinksSQL builds the distinct-URL count. Unfiltered it is a covering
// scan of idx_links_gallery; filters stay on links' own denormalized columns.
func countLinksSQL(f GalleryFilter) (string, []any) {
	clauses, args := linkClauses(f)
	return `SELECT COUNT(DISTINCT l.url) FROM links l` + whereSQL(clauses), args
}

// CountMedia returns the number of images, files, and distinct links matching
// the filter. No path touches the messages table (SPEC-0008 REQ-0008-009).
func (s *Store) CountMedia(ctx context.Context, f GalleryFilter) (MediaCounts, error) {
	var c MediaCounts

	attQ, attArgs := countAttachmentsSQL(f)
	rows, err := s.db.QueryContext(ctx, attQ, attArgs...)
	if err != nil {
		return c, fmt.Errorf("count attachments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return c, err
		}
		switch kind {
		case "image":
			c.Images = n
		case "file":
			c.Files = n
		}
	}
	if err := rows.Err(); err != nil {
		return c, err
	}

	linkQ, linkArgs := countLinksSQL(f)
	if err := s.db.QueryRowContext(ctx, linkQ, linkArgs...).Scan(&c.Links); err != nil {
		return c, fmt.Errorf("count links: %w", err)
	}
	return c, nil
}
