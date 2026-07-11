// The Overview (the Messages landing screen at "/") consolidation — issue #1:
// beside the original hero + global stat strip, the home page now surfaces
// per-provider archive freshness (store.SourceCounts + store.LastSyncTimes),
// the semantic-search index status (coverage, last index run, live
// in-progress marker via the embed_runs heartbeat), and the MCP connection
// card that also lives on /settings — so "how healthy is my archive / how do
// I connect" no longer requires hopping between /status and /settings.
//
// Deliberate non-changes: /status and /settings stay the canonical URLs (the
// settings_subnav contract from #163 — "nothing redirects" — is preserved;
// the Overview duplicates, it does not replace), device pairing stays on
// Settings, and snapshots stay on /status until they graduate to their own
// Backups tab (tracked separately).
package web

import (
	"context"
	"strings"
	"time"

	"github.com/joestump/msgbrowse/internal/source"
)

// providerStat is one provider row in the Overview's per-provider freshness
// breakdown: what the store holds for that source and when it last completed
// an ingest.
type providerStat struct {
	Label         string
	Conversations int
	Messages      int
	// LastSync is the local-time stamp of the source's most recent completed
	// ingest run; "" when none has ever been recorded (e.g. data synced in
	// from another device, or a pre-ingest_runs import).
	LastSync string
}

// embedStatusData drives the Overview's "Semantic search index" card. All
// display strings are precomputed here (the peers/ingest cards' pattern) so
// the template stays logic-free.
type embedStatusData struct {
	// Configured is false while llm.embed_model is unset — the card then
	// renders a pointer to Settings → LLM instead of fake zeros.
	Configured bool
	Model      string
	// Embedded / Embeddable / Percent are the coverage aggregate: how much of
	// the embeddable corpus (non-system, non-blank messages) carries a vector
	// for the configured model.
	Embedded   int
	Embeddable int
	Percent    int
	// InProgress marks a live indexing run (an unfinished embed_runs row with
	// a fresh heartbeat); RunEmbedded is its progress so far.
	InProgress  bool
	RunEmbedded int
	// Stalled marks an unfinished run whose heartbeat went stale — the embed
	// process died before its terminal write.
	Stalled bool
	// HasLastRun gates the last-completed-run line.
	HasLastRun     bool
	LastFinished   string // local "2006-01-02 15:04"
	LastEmbedded   int
	LastDurationMS int64
	// LastModel is the embed model the last completed run used. LatestEmbedRun
	// is model-agnostic while coverage above is scoped to the configured model,
	// so when a user switches llm.embed_model the two halves can describe
	// different models; the template surfaces LastModel when it differs from
	// Model so the disagreement is explained rather than silent.
	LastModel string
	// LastError is the abort reason when the last run failed ("" on success).
	// Server-owned prose from the embed pipeline, escaped like everything else.
	LastError string
}

// embedRunStaleAfter is how fresh an unfinished run's heartbeat must be to
// read as "indexing in progress". The heartbeat moves once per batch (a
// single /embeddings request), so anything older than this means the embed
// process died before its terminal write; report a crashed run rather than
// spinning forever. The window is generous on purpose: a large batch (up to
// 512) against a slow local embedding endpoint can take many minutes, and
// misreading a live run as "Interrupted" invites the user to launch a second
// concurrent `msgbrowse embed` against the same SQLite file — the more costly
// error than briefly showing "Indexing…" for a run that just died.
const embedRunStaleAfter = 30 * time.Minute

// overviewTimeFormat is the freshness-stamp layout, matching the Settings
// peers' PairedAt/LastSeen stamps.
const overviewTimeFormat = "2006-01-02 15:04"

// overviewProviders builds the per-provider freshness rows in source.All
// order. A provider appears when it has imported data OR a recorded ingest
// run; wholly absent sources render no row (the global strip already covers
// the "nothing at all" story). Two cheap aggregate queries, run on both the
// full and boosted-partial paths (REQ-0008-006 only exempts the sidebar
// listing).
func (s *Server) overviewProviders(ctx context.Context) ([]providerStat, error) {
	counts, err := s.store.SourceCounts(ctx)
	if err != nil {
		return nil, err
	}
	syncs, err := s.store.LastSyncTimes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]providerStat, 0, len(source.All))
	for _, src := range source.All {
		c, hasCounts := counts[src]
		ts, hasSync := syncs[src]
		if !hasCounts && !hasSync {
			continue
		}
		row := providerStat{
			Label:         source.Label(src),
			Conversations: c.Conversations,
			Messages:      c.Messages,
		}
		if hasSync {
			row.LastSync = ts.Local().Format(overviewTimeFormat)
		}
		out = append(out, row)
	}
	return out, nil
}

// overviewEmbedding assembles the semantic-search index status: coverage for
// the CURRENTLY configured embed model (live via the LLM tab's configurator
// when wired, else the boot config) plus the latest recorded index run,
// classified live / stalled / finished by its heartbeat.
//
// Accepted cost: EmbeddingCoverage is a full messages scan (COUNT + LEFT JOIN,
// TRIM(body) predicate, no covering index) and it runs on every "/" render,
// including boosted #main-content partials. Fine for modest archives; at the
// millions-of-messages target this should move to a cached/periodic aggregate
// rather than a per-request scan.
func (s *Server) overviewEmbedding(ctx context.Context) (embedStatusData, error) {
	model := strings.TrimSpace(s.currentLLM().EmbedModel)
	d := embedStatusData{Model: model, Configured: model != ""}
	if !d.Configured {
		return d, nil
	}
	cov, err := s.store.EmbeddingCoverage(ctx, model)
	if err != nil {
		return d, err
	}
	d.Embedded, d.Embeddable = cov.Embedded, cov.Embeddable
	if cov.Embeddable > 0 {
		d.Percent = cov.Embedded * 100 / cov.Embeddable
	}
	run, err := s.store.LatestEmbedRun(ctx)
	if err != nil {
		return d, err
	}
	switch {
	case run == nil:
		// Never indexed; the coverage line plus the template's run-it hint
		// carry the story.
	case run.InFlight() && time.Since(run.UpdatedAt) <= embedRunStaleAfter:
		d.InProgress = true
		d.RunEmbedded = run.Embedded
	case run.InFlight():
		d.Stalled = true
	default:
		d.HasLastRun = true
		d.LastFinished = run.FinishedAt.Local().Format(overviewTimeFormat)
		d.LastEmbedded = run.Embedded
		d.LastDurationMS = run.DurationMS
		d.LastError = run.Error
		d.LastModel = run.Model
	}
	return d, nil
}
