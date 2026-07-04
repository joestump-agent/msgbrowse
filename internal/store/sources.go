// Per-source aggregate queries and the Disable delete (issue #162): the
// Providers page shows what each enabled source actually imported ("N
// conversations · N messages") and can remove a source's imported data from
// the store while KEEPING its managed archive on disk, so a later re-enable is
// a fast local re-import rather than a fresh export.
package store

import (
	"context"
	"fmt"
)

// SourceCount is one source's imported footprint: how many conversations and
// messages the store holds for it.
type SourceCount struct {
	Conversations int
	Messages      int
}

// SourceCounts returns the imported footprint per source, keyed by source id.
// Sources with nothing imported are absent from the map. Two cheap GROUP BYs
// over indexed columns, combined so it is a single round trip — the Providers
// render calls this once per page (issue #162).
func (s *Store) SourceCounts(ctx context.Context) (map[string]SourceCount, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source, SUM(convs), SUM(msgs) FROM (
			SELECT source, COUNT(*) AS convs, 0 AS msgs FROM conversations GROUP BY source
			UNION ALL
			SELECT source, 0, COUNT(*) FROM messages GROUP BY source
		) GROUP BY source`)
	if err != nil {
		return nil, fmt.Errorf("source counts: %w", err)
	}
	defer rows.Close()
	out := make(map[string]SourceCount)
	for rows.Next() {
		var src string
		var c SourceCount
		if err := rows.Scan(&src, &c.Conversations, &c.Messages); err != nil {
			return nil, fmt.Errorf("scan source count: %w", err)
		}
		out[src] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("source counts rows: %w", err)
	}
	return out, nil
}

// DeleteSourceData removes every imported row for one source from the store —
// the Disable action (issue #162). It deletes the source's conversations and
// lets the schema's ON DELETE CASCADE fan out: messages (whose FTS rows the
// messages_ad trigger cleans), attachments and links (via their message FK),
// reactions, ingest_state, and fact_state all cascade with their conversation.
// It then removes the source's contact_identifiers and any contacts left with
// no identifiers and no conversations, so a disabled source leaves no orphaned
// contact rows behind. Hash-keyed rows (embeddings, contact_facts) are
// deliberately untouched — they carry no FK by design (schemaV3/V4: re-ingest
// must not wipe them) and a re-enable reuses them; `embed --prune` reclaims
// true orphans.
//
// The managed archive on disk is NOT touched: Disable only forgets the
// imported data, so a later Enable re-imports locally without re-exporting.
// Returns the number of conversations removed. Runs in one transaction so a
// failure leaves the store unchanged.
func (s *Store) DeleteSourceData(ctx context.Context, src string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("disable %s: begin: %w", src, err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE source = ?`, src)
	if err != nil {
		return 0, fmt.Errorf("disable %s: delete conversations: %w", src, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("disable %s: rows affected: %w", src, err)
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM contact_identifiers WHERE source = ?`, src); err != nil {
		return 0, fmt.Errorf("disable %s: delete identifiers: %w", src, err)
	}
	// Contacts left with no identifiers and no conversations are pure orphans
	// of this source (cross-source-linked contacts keep their other rows).
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM contacts WHERE
			NOT EXISTS (SELECT 1 FROM contact_identifiers ci WHERE ci.contact_id = contacts.id)
			AND NOT EXISTS (SELECT 1 FROM conversations c WHERE c.contact_id = contacts.id)`); err != nil {
		return 0, fmt.Errorf("disable %s: delete orphaned contacts: %w", src, err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("disable %s: commit: %w", src, err)
	}
	return n, nil
}
