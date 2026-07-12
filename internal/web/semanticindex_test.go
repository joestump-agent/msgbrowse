package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/store"
)

// newStatusServer builds a Server over the fixture archive with embedModel
// configured and (optionally) a fake Indexer wired, so the Status page's
// semantic-index card renders its controls. Returns the store for seeding
// embed_runs / vectors to drive the card's states.
func newStatusServer(t *testing.T, embedModel string, withIndexer bool) (*Server, *store.Store) {
	t.Helper()
	st, cfg, _ := newTestStoreAndConfig(t)
	cfg.LLM.EmbedModel = embedModel
	srv, err := NewServer(st, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if withIndexer {
		srv.SetIndexer(newFakeIndexer(embedModel))
	}
	return srv, st
}

// fakeIndexer is a test double for the Indexer seam. It never touches the
// network: RunEmbed blocks on a release channel so a test can hold a job "in
// flight" and prove the single-flight guard coalesces a second start.
type fakeIndexer struct {
	model    string
	release  chan struct{} // RunEmbed blocks until this is closed/received
	started  int32         // RunEmbed invocations (atomic)
	lastRst  atomic.Bool   // reset arg of the most recent RunEmbed
	finished sync.WaitGroup
}

func newFakeIndexer(model string) *fakeIndexer {
	return &fakeIndexer{model: model, release: make(chan struct{})}
}

func (f *fakeIndexer) EmbedModel() string { return f.model }

func (f *fakeIndexer) RunEmbed(ctx context.Context, reset bool) error {
	atomic.AddInt32(&f.started, 1)
	f.lastRst.Store(reset)
	<-f.release // block until the test lets the job finish
	f.finished.Done()
	return nil
}

// starts returns how many times RunEmbed was invoked.
func (f *fakeIndexer) starts() int { return int(atomic.LoadInt32(&f.started)) }

// indexPOST issues a privileged POST to a /status/index* route with the given
// origin + token, mirroring llmPOST.
func indexPOST(t *testing.T, srv *Server, path, origin, token string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	if token != "" {
		form.Set(setupTokenField, token)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// waitFor polls until cond() is true or the deadline elapses (the detached
// goroutine's RunEmbed call is async).
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

// TestStatusIndexBuildStartsJob: a valid same-origin POST with a live token
// starts exactly one detached job and re-renders Status with the "started"
// banner.
func TestStatusIndexBuildStartsJob(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fi := newFakeIndexer("test-embed")
	fi.finished.Add(1)
	srv.SetIndexer(fi)

	rec := indexPOST(t, srv, "/status/index", selfOrigin, mintToken(t, srv))
	if rec.Code != http.StatusOK {
		t.Fatalf("build POST status = %d, want 200", rec.Code)
	}
	if !contains(rec.Body.String(), "Indexing started") {
		t.Errorf("build response missing the started banner:\n%s", rec.Body.String())
	}
	waitFor(t, func() bool { return fi.starts() == 1 })
	if fi.lastRst.Load() {
		t.Error("Build must call RunEmbed with reset=false")
	}
	close(fi.release)
	fi.finished.Wait()
}

// TestStatusIndexResetStartsResetJob: the reset route starts a job with
// reset=true and shows the reset banner.
func TestStatusIndexResetStartsResetJob(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fi := newFakeIndexer("test-embed")
	fi.finished.Add(1)
	srv.SetIndexer(fi)

	rec := indexPOST(t, srv, "/status/index/reset", selfOrigin, mintToken(t, srv))
	if rec.Code != http.StatusOK {
		t.Fatalf("reset POST status = %d, want 200", rec.Code)
	}
	if !contains(rec.Body.String(), "Reset &amp; rebuild started") {
		t.Errorf("reset response missing the reset banner:\n%s", rec.Body.String())
	}
	waitFor(t, func() bool { return fi.starts() == 1 })
	if !fi.lastRst.Load() {
		t.Error("Reset must call RunEmbed with reset=true")
	}
	close(fi.release)
	fi.finished.Wait()
}

// TestStatusIndexSingleFlight: with a job held in flight, a second Build
// coalesces — RunEmbed is invoked exactly once and the second POST returns the
// "in progress" banner, never a duplicate writer.
func TestStatusIndexSingleFlight(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fi := newFakeIndexer("test-embed")
	fi.finished.Add(1)
	srv.SetIndexer(fi)

	// First start: the job begins and blocks on release.
	first := indexPOST(t, srv, "/status/index", selfOrigin, mintToken(t, srv))
	if first.Code != http.StatusOK {
		t.Fatalf("first build status = %d", first.Code)
	}
	waitFor(t, func() bool { return srv.indexJobRunning() })

	// Second start while the first is in flight: coalesced.
	second := indexPOST(t, srv, "/status/index", selfOrigin, mintToken(t, srv))
	if second.Code != http.StatusOK {
		t.Fatalf("second build status = %d", second.Code)
	}
	if !contains(second.Body.String(), "already in progress") {
		t.Errorf("second build missing the in-progress banner:\n%s", second.Body.String())
	}
	if fi.starts() != 1 {
		t.Errorf("RunEmbed called %d times under single-flight, want 1", fi.starts())
	}

	// Let the job finish; the guard clears.
	close(fi.release)
	fi.finished.Wait()
	waitFor(t, func() bool { return !srv.indexJobRunning() })
}

// TestStatusIndexUnavailableWithoutIndexer: no Indexer wired (browser / no-op
// mode) — the POST reports "unavailable" and starts nothing, rather than 500.
func TestStatusIndexUnavailableWithoutIndexer(t *testing.T) {
	srv, _, _ := newTestServer(t)
	// No SetIndexer.
	rec := indexPOST(t, srv, "/status/index", selfOrigin, mintToken(t, srv))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (unavailable is a banner, not an error)", rec.Code)
	}
	if !contains(rec.Body.String(), "not available here") {
		t.Errorf("missing the unavailable banner:\n%s", rec.Body.String())
	}
}

// TestStatusIndexNoModel: an Indexer with an empty embed model refuses to start
// (a Reset would otherwise clear the index and no-op into 0-of-N).
func TestStatusIndexNoModel(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fi := newFakeIndexer("") // no model
	srv.SetIndexer(fi)

	rec := indexPOST(t, srv, "/status/index", selfOrigin, mintToken(t, srv))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !contains(rec.Body.String(), "No embedding model configured") {
		t.Errorf("missing the no-model banner:\n%s", rec.Body.String())
	}
	if fi.starts() != 0 {
		t.Errorf("RunEmbed called %d times with no model, want 0", fi.starts())
	}
}

// TestStatusIndexCrossOriginRejected: a cross-origin POST is 403 and starts
// nothing, even with a valid token (the checkSetupPOST contract).
func TestStatusIndexCrossOriginRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fi := newFakeIndexer("test-embed")
	srv.SetIndexer(fi)

	rec := indexPOST(t, srv, "/status/index", "http://evil.example", mintToken(t, srv))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", rec.Code)
	}
	if fi.starts() != 0 {
		t.Errorf("cross-origin POST started %d jobs, want 0", fi.starts())
	}
}

// TestStatusIndexMissingTokenRejected: same-origin but tokenless → 403, nothing
// started.
func TestStatusIndexMissingTokenRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fi := newFakeIndexer("test-embed")
	srv.SetIndexer(fi)

	rec := indexPOST(t, srv, "/status/index", selfOrigin, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing-token status = %d, want 403", rec.Code)
	}
	if fi.starts() != 0 {
		t.Errorf("tokenless POST started %d jobs, want 0", fi.starts())
	}
}

// --- Status card render states ---

// TestStatusCardNeverState: embed model configured, an Indexer wired, and
// nothing indexed yet — the card shows 0-of-N coverage, "never", and the enabled
// Build / Reset controls with a live token.
func TestStatusCardNeverState(t *testing.T) {
	srv, _ := newStatusServer(t, "test-embed", true)
	body := get(t, srv, "/status").Body.String()
	for _, want := range []string{
		"Semantic search index",
		"of", "messages (0%)", // coverage line, 0% before any embedding
		"never",
		`action="/status/index"`,
		`action="/status/index/reset"`,
		"Build index",
		"Reset &amp; rebuild",
		`name="setup_token"`,
	} {
		if !contains(body, want) {
			t.Errorf("never-state Status missing %q", want)
		}
	}
	// Buttons enabled: no disabled submit while idle.
	if contains(body, "setup-btn setup-btn-ghost\" disabled") || contains(body, "setup-btn\" disabled") {
		t.Error("Build/Reset buttons disabled while no run is in progress")
	}
}

// TestStatusCardCompleteState: a finished run makes the last-run line carry the
// stamp, and the card no longer reads "never".
func TestStatusCardCompleteState(t *testing.T) {
	srv, st := newStatusServer(t, "test-embed", true)
	ctx := context.Background()
	fin := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	id, err := st.BeginEmbedRun(ctx, "test-embed", fin.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishEmbedRun(ctx, store.EmbedRun{ID: id, FinishedAt: fin, DurationMS: 1200, Embedded: 3, Batches: 1}); err != nil {
		t.Fatal(err)
	}
	body := get(t, srv, "/status").Body.String()
	if !contains(body, fin.Local().Format("2006-01-02 15:04")) {
		t.Error("complete-state Status missing the finished-run stamp")
	}
	if contains(body, ">never<") {
		t.Error("last-run line still reads never after a completed run")
	}
}

// TestStatusCardInProgressState: a live embed_runs heartbeat renders the
// Indexing… marker AND disables the Build / Reset buttons so a click cannot
// race the running job.
func TestStatusCardInProgressState(t *testing.T) {
	srv, st := newStatusServer(t, "test-embed", true)
	ctx := context.Background()
	id, err := st.BeginEmbedRun(ctx, "test-embed", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateEmbedRunProgress(ctx, id, 42, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	body := get(t, srv, "/status").Body.String()
	if !contains(body, "Indexing…") || !contains(body, "42 messages embedded so far") {
		t.Error("in-progress Status missing the live indexing marker")
	}
	if !contains(body, "disabled") {
		t.Error("Build/Reset buttons not disabled while a run is in progress")
	}
}

// TestStatusCardNotConfigured: no embed model — the card points at Settings →
// LLM and renders no Build controls (even with an Indexer wired).
func TestStatusCardNotConfigured(t *testing.T) {
	srv, _ := newStatusServer(t, "", true)
	body := get(t, srv, "/status").Body.String()
	if !contains(body, "Semantic search is not configured") || !contains(body, "/settings/llm") {
		t.Error("not-configured Status missing the LLM-settings pointer")
	}
	if contains(body, `action="/status/index"`) {
		t.Error("Build form rendered with no embed model configured")
	}
}

// TestStatusCardUnavailable: no Indexer wired (browser / no-op mode) — the card
// renders the unavailable note and no Build controls.
func TestStatusCardUnavailable(t *testing.T) {
	srv, _ := newStatusServer(t, "test-embed", false)
	body := get(t, srv, "/status").Body.String()
	if !contains(body, "Index management is unavailable in this mode") {
		t.Error("no-indexer Status missing the unavailable note")
	}
	if contains(body, `action="/status/index"`) {
		t.Error("Build form rendered with no Indexer wired")
	}
}

// TestOverviewLinksToStatusIndex: the Overview's semantic-search card points at
// the Status page rather than duplicating the controls.
func TestOverviewLinksToStatusIndex(t *testing.T) {
	srv, _ := newStatusServer(t, "test-embed", true)
	body := get(t, srv, "/").Body.String()
	if !contains(body, "Manage index") || !contains(body, `href="/status"`) {
		t.Error("Overview semantic card missing the Manage index link to /status")
	}
	// The buttons live only on Status, not duplicated on the Overview.
	if contains(body, `action="/status/index"`) {
		t.Error("Overview should not duplicate the Build form")
	}
}
