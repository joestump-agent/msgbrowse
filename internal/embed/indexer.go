package embed

import (
	"context"
	"log/slog"
	"strings"

	"github.com/joestump/msgbrowse/internal/llm"
	"github.com/joestump/msgbrowse/internal/store"
)

// Indexer is the concrete web.Indexer (issue #191): the object `msgbrowse
// serve` and the desktop shell wire behind the Status page's Build /
// Reset-&-rebuild controls so an in-app click runs the same embedding pass the
// `msgbrowse embed` CLI does — over the LIVE store and the LIVE LLM client.
//
// It holds the process's shared *llm.Holder (not a fixed client) so a job
// started right after a Settings → LLM save picks up the new endpoint and embed
// model: EmbedModel and RunEmbed both read the holder's CURRENT settings at
// call time. The web layer owns the single-flight guard and the detached
// goroutine; this type just supplies the store + client + the reset primitive.
type Indexer struct {
	store  *store.Store
	holder *llm.Holder
	log    *slog.Logger
}

// NewIndexer builds an Indexer over the shared store and live LLM holder.
func NewIndexer(st *store.Store, holder *llm.Holder, log *slog.Logger) *Indexer {
	if log == nil {
		log = slog.Default()
	}
	return &Indexer{store: st, holder: holder, log: log}
}

// EmbedModel returns the CURRENTLY configured embedding model (trimmed), "" when
// unset. The web layer refuses to start a run when this is empty, so a
// Reset-&-rebuild can never clear the index and then no-op into "0 of N".
func (ix *Indexer) EmbedModel() string { return strings.TrimSpace(ix.holder.EmbedModel()) }

// RunEmbed executes one embedding pass. When reset is true it first clears every
// vector and the run log (store.ResetEmbeddings) so the rebuild starts from
// scratch, then embeds every embeddable message under the live model. It blocks
// until the pass finishes or aborts; the web layer calls it in a background
// goroutine, so ctx is a detached (non-request) context that outlives the HTTP
// call. Prune is on: a rebuild should not carry vectors for messages a
// re-ingest removed.
func (ix *Indexer) RunEmbed(ctx context.Context, reset bool) error {
	if reset {
		if err := ix.store.ResetEmbeddings(ctx); err != nil {
			return err
		}
	}
	_, err := Run(ctx, ix.store, ix.holder, Options{
		EmbedModel: ix.holder.EmbedModel(),
		Prune:      true,
		Logger:     ix.log,
	})
	return err
}
