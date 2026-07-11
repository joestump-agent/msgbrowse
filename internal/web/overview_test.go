package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/store"
)

// newOverviewServer is newTestServer with llm.embed_model configured, so the
// Overview's semantic-index card renders its coverage state instead of the
// not-configured pointer.
func newOverviewServer(t *testing.T, embedModel string) (*Server, *store.Store) {
	t.Helper()
	st, cfg, _ := newTestStoreAndConfig(t)
	cfg.LLM.EmbedModel = embedModel
	srv, err := NewServer(st, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv, st
}

// TestOverviewProviderBreakdown (issue #1): the Overview expands the global
// stat strip with a per-provider table — provider label, its own
// conversation/message counts, and the last-synced stamp from its most recent
// completed ingest run.
func TestOverviewProviderBreakdown(t *testing.T) {
	srv, st, _ := newTestServer(t)

	// The fixture ingest ran at a fixed instant; its per-source footprint is
	// what the table must echo.
	ctx := context.Background()
	counts, err := st.SourceCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sc, ok := counts["signal"]
	if !ok || sc.Conversations == 0 || sc.Messages == 0 {
		t.Fatalf("fixture signal counts = %+v, %v", sc, ok)
	}
	syncs, err := st.LastSyncTimes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	syncStamp, ok := syncs["signal"]
	if !ok {
		t.Fatal("fixture ingest did not record a signal ingest run")
	}

	body := get(t, srv, "/").Body.String()
	for _, want := range []string{
		"By provider",
		">Signal<",
		"Last synced",
		">" + itoa(int64(sc.Conversations)) + "<",
		">" + itoa(int64(sc.Messages)) + "<",
		syncStamp.Local().Format("2006-01-02 15:04"),
	} {
		if !contains(body, want) {
			t.Errorf("overview missing provider marker %q", want)
		}
	}
	// The fixture is signal-only: no phantom rows for empty providers.
	for _, absent := range []string{">iMessage<", ">WhatsApp<"} {
		if contains(body, absent) {
			t.Errorf("overview shows %q with nothing imported", absent)
		}
	}
}

// TestOverviewEmbeddingNotConfigured: with no embed model set, the
// semantic-index card renders a pointer to Settings → LLM — never fake zeros.
func TestOverviewEmbeddingNotConfigured(t *testing.T) {
	srv, _, _ := newTestServer(t)
	body := get(t, srv, "/").Body.String()
	if !contains(body, "Semantic search index") {
		t.Fatal("overview missing the semantic-index card")
	}
	for _, want := range []string{"Semantic search is not configured", `href="/settings/llm"`} {
		if !contains(body, want) {
			t.Errorf("unconfigured card missing %q", want)
		}
	}
	if contains(body, "Coverage") {
		t.Error("unconfigured card must not render coverage metrics")
	}
}

// TestOverviewEmbeddingCoverageAndLastRun: with a model configured, the card
// shows embedded-vs-total coverage and the latest completed index run's
// stamp, totals, and duration.
func TestOverviewEmbeddingCoverageAndLastRun(t *testing.T) {
	srv, st := newOverviewServer(t, "test-embed")
	ctx := context.Background()

	// Never indexed: full corpus pending, run line reads "never".
	cov, err := st.EmbeddingCoverage(ctx, "test-embed")
	if err != nil || cov.Embeddable == 0 {
		t.Fatalf("coverage = %+v, %v", cov, err)
	}
	body := get(t, srv, "/").Body.String()
	for _, want := range []string{
		"Coverage",
		"0 of " + itoa(int64(cov.Embeddable)) + " messages (0%)",
		"test-embed",
		"never",
	} {
		if !contains(body, want) {
			t.Errorf("pre-index card missing %q", want)
		}
	}

	// Embed one message and record a completed run; both must surface.
	targets, err := st.MessagesNeedingEmbedding(ctx, "test-embed", 1)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets = %v, %v", targets, err)
	}
	if err := st.PutEmbedding(ctx, targets[0].Hash, "test-embed", []float32{1, 2}); err != nil {
		t.Fatal(err)
	}
	fin := time.Date(2026, 7, 2, 9, 30, 0, 0, time.UTC)
	id, err := st.BeginEmbedRun(ctx, "test-embed", fin.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishEmbedRun(ctx, store.EmbedRun{
		ID: id, FinishedAt: fin, DurationMS: 1234, Embedded: 1, Batches: 1,
	}); err != nil {
		t.Fatal(err)
	}

	body = get(t, srv, "/").Body.String()
	pct := 100 / cov.Embeddable // 1 of N
	for _, want := range []string{
		"1 of " + itoa(int64(cov.Embeddable)) + " messages (" + itoa(int64(pct)) + "%)",
		fin.Local().Format("2006-01-02 15:04"),
		"1 embedded in 1,234 ms",
	} {
		if !contains(body, want) {
			t.Errorf("post-index card missing %q", want)
		}
	}
	if contains(body, ">never<") {
		t.Error("run line still reads never after a completed run")
	}
}

// TestOverviewEmbeddingInProgress: an unfinished run with a fresh heartbeat
// renders the live in-progress state with its running total; one whose
// heartbeat went stale renders the interrupted state instead.
func TestOverviewEmbeddingInProgress(t *testing.T) {
	srv, st := newOverviewServer(t, "test-embed")
	ctx := context.Background()

	id, err := st.BeginEmbedRun(ctx, "test-embed", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateEmbedRunProgress(ctx, id, 42, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	body := get(t, srv, "/").Body.String()
	for _, want := range []string{"Indexing…", "42 messages embedded so far", "running now"} {
		if !contains(body, want) {
			t.Errorf("in-progress card missing %q", want)
		}
	}

	// Age the heartbeat past the staleness window: the run reads interrupted.
	if err := st.UpdateEmbedRunProgress(ctx, id, 42, 1, time.Now().Add(-embedRunStaleAfter-time.Minute)); err != nil {
		t.Fatal(err)
	}
	body = get(t, srv, "/").Body.String()
	if !contains(body, "Interrupted") || !contains(body, "stopped before finishing") {
		t.Error("stale in-flight run not rendered as interrupted")
	}
	if contains(body, "Indexing…") {
		t.Error("stale run still renders the live in-progress state")
	}
}

// TestOverviewEmbeddingFailedRun: a completed-but-failed run surfaces its
// abort reason.
func TestOverviewEmbeddingFailedRun(t *testing.T) {
	srv, st := newOverviewServer(t, "test-embed")
	ctx := context.Background()
	id, err := st.BeginEmbedRun(ctx, "test-embed", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishEmbedRun(ctx, store.EmbedRun{
		ID: id, FinishedAt: time.Now(), Error: "provider unreachable",
	}); err != nil {
		t.Fatal(err)
	}
	body := get(t, srv, "/").Body.String()
	if !contains(body, "Last run aborted") || !contains(body, "provider unreachable") {
		t.Error("failed run's abort reason missing from the card")
	}
}

// TestOverviewMCPCard (issue #1): the MCP connection block — endpoint URL,
// client JSON, `claude mcp add` one-liner, each with its copy-button wiring —
// renders on the Overview, shared verbatim with /settings via the
// mcp_connect_card define ( /settings keeps it too: it stays canonical).
func TestOverviewMCPCard(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for _, route := range []string{"/", "/settings"} {
		body := get(t, srv, route).Body.String()
		for _, want := range []string{
			"MCP server",
			`id="mcp-endpoint"`,
			`data-copy-target="mcp-endpoint"`,
			`id="mcp-config-json"`,
			`data-copy-target="mcp-config-json"`,
			`id="mcp-add-command"`,
			`data-copy-target="mcp-add-command"`,
			"http://example.com/mcp", // httptest requests carry Host example.com
			"claude mcp add",
		} {
			if !contains(body, want) {
				t.Errorf("%s missing MCP marker %q", route, want)
			}
		}
	}
	// Device pairing stays on Settings only (issue #1 scope).
	if body := get(t, srv, "/").Body.String(); contains(body, "Pair a device") {
		t.Error("device pairing leaked onto the Overview")
	}
}

// TestOverviewPartialCarriesConsolidatedCards: the boosted-partial render of
// "/" (the REQ-0008-006 cheap path) still carries the consolidated cards —
// they live inside #main-content, not the shell.
func TestOverviewPartialCarriesConsolidatedCards(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := getPartial(t, srv, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("partial status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"By provider", "Semantic search index", "MCP server", `id="mcp-endpoint"`} {
		if !contains(body, want) {
			t.Errorf("overview partial missing %q", want)
		}
	}
}
