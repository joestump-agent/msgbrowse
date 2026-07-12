package embed

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/joestump/msgbrowse/internal/llm"
)

// TestIndexerReindexResetRebuilds drives the concrete web.Indexer end-to-end
// (issue #191): a first pass embeds the corpus, a reset-and-rebuild clears the
// index (vectors + run log) and re-embeds from scratch, and EmbedModel reflects
// the live holder.
func TestIndexerReindexRebuilds(t *testing.T) {
	st := newStore(t)
	seed(t, st)
	ctx := context.Background()
	fc := &fakeClient{}
	holder := llm.NewHolder(fc, llm.Settings{EmbedModel: "test-embed"})
	ix := NewIndexer(st, holder, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if ix.EmbedModel() != "test-embed" {
		t.Fatalf("EmbedModel = %q, want test-embed", ix.EmbedModel())
	}

	// Build: embeds the two embeddable messages and records a run.
	if err := ix.RunEmbed(ctx, false); err != nil {
		t.Fatal(err)
	}
	if cov, err := st.EmbeddingCoverage(ctx, "test-embed"); err != nil || cov.Embedded != 2 {
		t.Fatalf("post-build coverage = %+v, %v; want 2 embedded", cov, err)
	}

	// Reset & rebuild: clears then re-embeds; coverage lands at 2 again and the
	// client was asked to embed once more (2 more inputs).
	before := fc.embedded
	if err := ix.RunEmbed(ctx, true); err != nil {
		t.Fatal(err)
	}
	if fc.embedded != before+2 {
		t.Errorf("reset rebuild embedded %d new inputs, want 2", fc.embedded-before)
	}
	cov, err := st.EmbeddingCoverage(ctx, "test-embed")
	if err != nil || cov.Embedded != 2 {
		t.Fatalf("post-reset coverage = %+v, %v; want 2 embedded", cov, err)
	}
}

// TestIndexerEmbedModelFollowsHolder: a live Swap on the holder changes the
// reported embed model (the Settings → LLM save applying to a later Build).
func TestIndexerEmbedModelFollowsHolder(t *testing.T) {
	holder := llm.NewHolder(&fakeClient{}, llm.Settings{EmbedModel: "  test-embed  "})
	ix := NewIndexer(newStore(t), holder, nil)
	if ix.EmbedModel() != "test-embed" {
		t.Errorf("EmbedModel not trimmed: %q", ix.EmbedModel())
	}
	holder.Swap(&fakeClient{}, llm.Settings{EmbedModel: ""})
	if ix.EmbedModel() != "" {
		t.Errorf("EmbedModel = %q after clearing model, want empty", ix.EmbedModel())
	}
}
