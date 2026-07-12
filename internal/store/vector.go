package store

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/joestump/msgbrowse/internal/signal"
)

// EmbedTarget is a message that needs an embedding: its stable hash and the text
// to embed.
type EmbedTarget struct {
	Hash string
	Text string
}

// ScoredMessage is a semantic-search result with its cosine similarity.
type ScoredMessage struct {
	MessageID        int64
	Hash             string
	ConversationID   int64
	ConversationName string
	Source           string
	Sender           string
	IsOwner          bool
	IsSystem         bool
	TS               string
	TSUnix           int64
	Body             string
	Score            float64
}

// SemanticOptions filters the candidate set before scoring (same filters as
// keyword search). K caps the number of returned results.
type SemanticOptions struct {
	ConversationID int64
	Source         string
	Sender         string
	StartUnix      int64
	EndUnix        int64
	K              int
}

// MessagesNeedingEmbedding returns up to limit messages that have no embedding
// for the given model (new or model-changed), with non-empty bodies. System
// messages and empty bodies are skipped (nothing to embed).
func (s *Store) MessagesNeedingEmbedding(ctx context.Context, model string, limit int) ([]EmbedTarget, error) {
	if limit <= 0 {
		limit = 256
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT m.hash, m.body
  FROM messages m
  LEFT JOIN embeddings e ON e.message_hash = m.hash AND e.model = ?
 WHERE e.message_hash IS NULL
   AND m.is_system = 0
   AND TRIM(m.body) <> ''
 ORDER BY m.ts_unix DESC, m.id DESC
 LIMIT ?`, model, limit)
	if err != nil {
		return nil, fmt.Errorf("messages needing embedding: %w", err)
	}
	defer rows.Close()
	var out []EmbedTarget
	for rows.Next() {
		var t EmbedTarget
		if err := rows.Scan(&t.Hash, &t.Text); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CountMissingEmbeddings returns how many embeddable messages still lack an
// embedding for the model (for progress reporting).
func (s *Store) CountMissingEmbeddings(ctx context.Context, model string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*)
  FROM messages m
  LEFT JOIN embeddings e ON e.message_hash = m.hash AND e.model = ?
 WHERE e.message_hash IS NULL AND m.is_system = 0 AND TRIM(m.body) <> ''`, model).Scan(&n)
	return n, err
}

// PutEmbedding upserts the embedding for a message hash under the given model.
func (s *Store) PutEmbedding(ctx context.Context, hash, model string, vec []float32) error {
	if len(vec) == 0 {
		return fmt.Errorf("put embedding %s: empty vector", hash)
	}
	blob := encodeVec(vec)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO embeddings(message_hash, model, dim, vec) VALUES (?, ?, ?, ?)
ON CONFLICT(message_hash, model) DO UPDATE SET dim=excluded.dim, vec=excluded.vec`,
		hash, model, len(vec), blob)
	if err != nil {
		return fmt.Errorf("put embedding %s: %w", hash, err)
	}
	return nil
}

// PruneOrphanEmbeddings deletes embeddings whose message hash no longer exists
// (messages removed by re-ingest). Returns the number removed.
func (s *Store) PruneOrphanEmbeddings(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
DELETE FROM embeddings
 WHERE message_hash NOT IN (SELECT hash FROM messages)`)
	if err != nil {
		return 0, fmt.Errorf("prune embeddings: %w", err)
	}
	return res.RowsAffected()
}

// ResetEmbeddings clears the semantic-search index: it DELETEs every stored
// vector AND every embed_runs row in one transaction, so afterwards
// EmbeddingCoverage reports 0-embedded and LatestEmbedRun returns nil ("never
// indexed"). It backs the Status page's "Reset & rebuild" control (issue #191):
// a from-scratch rebuild whose UI immediately reads as un-indexed rather than
// showing a stale "last run" from the wiped-out vectors. Both tables are
// cleared together — a run log describing embeddings that no longer exist would
// be misleading — so the two DELETEs share one atomic unit.
func (s *Store) ResetEmbeddings(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("reset embeddings: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM embeddings`); err != nil {
		return fmt.Errorf("reset embeddings: delete vectors: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM embed_runs`); err != nil {
		return fmt.Errorf("reset embeddings: delete run log: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("reset embeddings: commit: %w", err)
	}
	return nil
}

// SemanticSearch returns the top-K messages most cosine-similar to query, after
// applying the metadata filters. It is a brute-force scan: candidate vectors
// (filtered) are loaded and scored in Go. For a personal archive this is fast
// and keeps everything in one SQLite file with no extension; a sqlite-vec
// backend can replace this later behind the same signature (see ADR-0002).
func (s *Store) SemanticSearch(ctx context.Context, query []float32, model string, opts SemanticOptions) ([]ScoredMessage, error) {
	if len(query) == 0 {
		return nil, nil
	}
	k := opts.K
	if k <= 0 || k > 200 {
		k = 20
	}
	qNorm := norm(query)
	if qNorm == 0 {
		return nil, nil
	}

	where := []string{"e.model = ?"}
	args := []any{model}
	if opts.ConversationID > 0 {
		where = append(where, "m.conversation_id = ?")
		args = append(args, opts.ConversationID)
	}
	if opts.Source != "" {
		where = append(where, "m.source = ?")
		args = append(args, opts.Source)
	}
	if opts.Sender != "" {
		where = append(where, "m.sender = ?")
		args = append(args, opts.Sender)
	}
	if opts.StartUnix > 0 {
		where = append(where, "m.ts_unix >= ?")
		args = append(args, opts.StartUnix)
	}
	if opts.EndUnix > 0 {
		where = append(where, "m.ts_unix <= ?")
		args = append(args, opts.EndUnix)
	}

	q := `
SELECT m.id, m.hash, m.conversation_id, c.name, m.source, m.sender, m.is_system,
       m.ts, m.ts_unix, m.body, e.vec, e.dim
  FROM embeddings e
  JOIN messages m      ON m.hash = e.message_hash
  JOIN conversations c ON c.id = m.conversation_id
 WHERE ` + strings.Join(where, " AND ")

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("semantic search: %w", err)
	}
	defer rows.Close()

	var scored []ScoredMessage
	for rows.Next() {
		var (
			m        ScoredMessage
			isSystem int
			blob     []byte
			dim      int
		)
		if err := rows.Scan(&m.MessageID, &m.Hash, &m.ConversationID, &m.ConversationName,
			&m.Source, &m.Sender, &isSystem, &m.TS, &m.TSUnix, &m.Body, &blob, &dim); err != nil {
			return nil, err
		}
		vec, err := decodeVec(blob, dim)
		if err != nil {
			return nil, err
		}
		if len(vec) != len(query) {
			// Dimension mismatch — only possible if a model kept its name but
			// changed its output dimensionality. Skip rather than score garbage;
			// a re-embed under the (effectively new) model repopulates correct
			// vectors.
			continue
		}
		m.IsSystem = isSystem == 1
		m.IsOwner = m.Sender == signal.OwnerSender
		m.Score = cosine(query, vec, qNorm)
		scored = append(scored, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.SliceStable(scored, func(i, j int) bool { return scored[i].Score > scored[j].Score })
	if len(scored) > k {
		scored = scored[:k]
	}
	return scored, nil
}

// --- float32 blob codec + math ---

// encodeVec packs a float32 slice into a little-endian byte blob.
func encodeVec(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

// decodeVec unpacks a little-endian float32 blob of the given dimension.
func decodeVec(b []byte, dim int) ([]float32, error) {
	if len(b) != dim*4 {
		return nil, fmt.Errorf("vec blob length %d does not match dim %d", len(b), dim)
	}
	v := make([]float32, dim)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v, nil
}

// norm returns the Euclidean norm of v.
func norm(v []float32) float64 {
	var sum float64
	for _, f := range v {
		sum += float64(f) * float64(f)
	}
	return math.Sqrt(sum)
}

// cosine returns the cosine similarity of a and b. aNorm is |a| precomputed by
// the caller (the query norm, reused across all candidates).
func cosine(a, b []float32, aNorm float64) float64 {
	var dot, bSum float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		bSum += float64(b[i]) * float64(b[i])
	}
	bNorm := math.Sqrt(bSum)
	if aNorm == 0 || bNorm == 0 {
		return 0
	}
	return dot / (aNorm * bNorm)
}
