// Embedding-run bookkeeping and coverage (issue #1): the store-level queries
// behind the Overview's "Semantic search index" card. internal/embed records a
// row per indexing run here (begin → per-batch heartbeat → finish), and the
// web layer reads the latest row plus the coverage aggregate to show
// embedded-vs-total, the last completed run, and a live in-progress marker
// — the embed CLI and `msgbrowse serve` are separate processes sharing one
// SQLite file, so this table is their only communication channel.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// EmbedRun is one semantic-search indexing run. FinishedAt is the zero time
// while the run is still in flight (or died before its terminal write);
// UpdatedAt is the per-batch heartbeat readers use to tell a live run from a
// crashed one. Embedded/Batches are live counters during a run and the final
// totals after it. Error carries the abort reason for a failed run ("" on
// success).
type EmbedRun struct {
	ID         int64
	Model      string
	StartedAt  time.Time
	UpdatedAt  time.Time
	FinishedAt time.Time
	DurationMS int64
	Embedded   int
	Pruned     int64
	Batches    int
	Error      string
}

// InFlight reports whether the run has not recorded its terminal write.
func (r EmbedRun) InFlight() bool { return r.FinishedAt.IsZero() }

// BeginEmbedRun records the start of an embedding run and returns the row id
// the run's later progress/finish writes target. The heartbeat (updated_at)
// starts equal to startedAt.
func (s *Store) BeginEmbedRun(ctx context.Context, model string, startedAt time.Time) (int64, error) {
	ts := startedAt.UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO embed_runs (model, started_at, updated_at) VALUES (?, ?, ?)`,
		model, ts, ts)
	if err != nil {
		return 0, fmt.Errorf("begin embed run: %w", err)
	}
	return res.LastInsertId()
}

// UpdateEmbedRunProgress refreshes a run's live counters and heartbeat after a
// batch. Readers treat an unfinished row with a fresh heartbeat as "indexing
// in progress".
func (s *Store) UpdateEmbedRunProgress(ctx context.Context, id int64, embedded, batches int, at time.Time) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE embed_runs SET embedded = ?, batches = ?, updated_at = ? WHERE id = ?`,
		embedded, batches, at.UTC().Format(time.RFC3339), id); err != nil {
		return fmt.Errorf("update embed run progress: %w", err)
	}
	return nil
}

// FinishEmbedRun records a run's terminal state: finished_at (which flips the
// row out of "in flight"), the final totals, and the abort error when the run
// failed. r.ID selects the row; r.UpdatedAt is stamped to r.FinishedAt.
func (s *Store) FinishEmbedRun(ctx context.Context, r EmbedRun) error {
	ts := r.FinishedAt.UTC().Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE embed_runs
		    SET finished_at = ?, updated_at = ?, duration_ms = ?, embedded = ?, pruned = ?, batches = ?, error = ?
		  WHERE id = ?`,
		ts, ts, r.DurationMS, r.Embedded, r.Pruned, r.Batches, r.Error, r.ID); err != nil {
		return fmt.Errorf("finish embed run: %w", err)
	}
	return nil
}

// LatestEmbedRun returns the most recently started embedding run, or nil when
// none has ever been recorded. The caller decides what an unfinished row
// means by its heartbeat age (live vs crashed).
func (s *Store) LatestEmbedRun(ctx context.Context) (*EmbedRun, error) {
	var (
		r                          EmbedRun
		started, updated, finished string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, model, started_at, updated_at, finished_at, duration_ms, embedded, pruned, batches, error
		   FROM embed_runs ORDER BY id DESC LIMIT 1`).
		Scan(&r.ID, &r.Model, &started, &updated, &finished,
			&r.DurationMS, &r.Embedded, &r.Pruned, &r.Batches, &r.Error)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest embed run: %w", err)
	}
	r.StartedAt = parseRFC3339(started)
	r.UpdatedAt = parseRFC3339(updated)
	if finished != "" {
		r.FinishedAt = parseRFC3339(finished)
	}
	return &r, nil
}

// EmbeddingCoverage is the semantic-search index's footprint for one model:
// how many embeddable messages exist and how many of them already have a
// stored vector.
type EmbeddingCoverage struct {
	// Embedded is the number of embeddable messages with a vector for the model.
	Embedded int
	// Embeddable is the total number of messages semantic search can index:
	// non-system with a non-blank body (the same predicate the embed pipeline's
	// MessagesNeedingEmbedding/CountMissingEmbeddings use, so
	// Embeddable - Embedded always equals the pending count).
	Embeddable int
}

// EmbeddingCoverage returns the index coverage for model in one pass: a
// single LEFT JOIN over messages ✕ the embeddings PK, counting every
// embeddable message and the subset that already carries a vector. It shares
// its WHERE predicate with CountMissingEmbeddings by construction.
func (s *Store) EmbeddingCoverage(ctx context.Context, model string) (EmbeddingCoverage, error) {
	var c EmbeddingCoverage
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*), COUNT(e.message_hash)
  FROM messages m
  LEFT JOIN embeddings e ON e.message_hash = m.hash AND e.model = ?
 WHERE m.is_system = 0 AND TRIM(m.body) <> ''`, model).
		Scan(&c.Embeddable, &c.Embedded)
	if err != nil {
		return EmbeddingCoverage{}, fmt.Errorf("embedding coverage: %w", err)
	}
	return c, nil
}
