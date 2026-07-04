package onboardsvc

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/onboard"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// growingSignalExporter is a fake exporter whose archive GROWS across runs: the
// first invocation writes N messages, and each subsequent invocation appends
// more. It drives the SPEC-0013 REQ "Refresh" delta acceptance — "WHEN new
// messages have arrived since the last import … an incremental import that adds
// only the new messages" — through the REAL onboard.Runner + the REAL
// store-backed importer, off macOS with no bundled tools.
//
// It writes into the staging dir the runner passes as the exporter's positional
// destination (ExportArgs gives <staging>/export for Signal), reproducing
// sigexport's <dest>/<conversation>/chat.md layout the importer scans.
type growingSignalExporter struct {
	mu   sync.Mutex
	runs int
}

func (g *growingSignalExporter) run(_ context.Context, _ string, args ...string) error {
	g.mu.Lock()
	g.runs++
	runs := g.runs
	g.mu.Unlock()

	dest := args[len(args)-1] // <staging>/export
	convDir := filepath.Join(dest, "Alice")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		return err
	}
	// Run 1 writes 2 messages; run 2 writes the same 2 PLUS 2 new ones. The
	// importer is incremental + idempotent, so the second import re-sees the
	// original 2 (no dupes) and adds only the 2 new. A stable timestamp per line
	// keeps the re-exported prefix byte-identical so the importer dedupes it.
	lines := []string{
		"[2022-01-01 10:00:00] Alice: message one\n",
		"[2022-01-01 10:01:00] Me: message two\n",
	}
	if runs >= 2 {
		lines = append(lines,
			"[2022-01-02 09:00:00] Alice: message three (new since last import)\n",
			"[2022-01-02 09:01:00] Me: message four (new since last import)\n",
		)
	}
	var body []byte
	for _, l := range lines {
		body = append(body, l...)
	}
	return os.WriteFile(filepath.Join(convDir, "chat.md"), body, 0o644)
}

// staticResolver points every source at a fixed (unused) tool path — the fake
// exporter ignores name, so any non-empty path satisfies the resolver.
type staticResolver struct{}

func (staticResolver) ResolveTool(context.Context, string) (string, error) {
	return "/bundle/sigexport", nil
}

// countMessages returns the store-wide message row count — the authoritative
// "no duplicates" signal for the delta assertion.
func countMessages(t *testing.T, st *store.Store) int {
	t.Helper()
	n, err := st.CountMessages(context.Background())
	if err != nil {
		t.Fatalf("CountMessages: %v", err)
	}
	return n
}

// waitTerminal polls the runner's Status until the source reaches a terminal
// phase, so the async job is observed without a fixed sleep.
func waitTerminal(t *testing.T, r *onboard.Runner, src string) onboard.Progress {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p, ok := r.Status(src); ok && p.Phase.Terminal() {
			return p
		}
		time.Sleep(2 * time.Millisecond)
	}
	p, _ := r.Status(src)
	t.Fatalf("timed out waiting for %s to terminate; last phase=%q err=%v", src, p.Phase, p.Err)
	return onboard.Progress{}
}

// TestRefreshAddsOnlyDelta is the SPEC-0013 REQ "Refresh" acceptance: an initial
// Enable imports the baseline archive, then a Refresh over a GROWN archive imports
// only the new messages — no duplicates. It runs the whole real pipeline (export
// into staging → atomic adopt → incremental import into SQLite) via the exported
// Build wiring, and asserts row deltas by message count in the store.
func TestRefreshAddsOnlyDelta(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.Open(filepath.Join(dataDir, store.DBFileName))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := &config.Config{DataDir: dataDir}
	exp := &growingSignalExporter{}
	runner, err := onboard.NewRunner(onboard.Config{
		Resolver: staticResolver{},
		Exec:     exp.run,
		Importer: &storeImporter{st: st, cfg: cfg, log: discardLogger()},
		DataDir:  dataDir,
		Logger:   discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	t.Cleanup(runner.Shutdown)

	// 1. Enable: imports the 2-message baseline.
	if _, err := runner.Enable(source.Signal); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	enabled := waitTerminal(t, runner, source.Signal)
	if enabled.Phase != onboard.PhaseDone {
		t.Fatalf("Enable ended %q (err=%v), want done", enabled.Phase, enabled.Err)
	}
	// After the baseline Enable the store holds exactly the 2 baseline messages.
	if got := countMessages(t, st); got != 2 {
		t.Fatalf("store holds %d messages after baseline Enable, want 2", got)
	}

	// 2. Refresh: the exporter now emits 4 messages (the original 2 + 2 new). The
	// incremental import is idempotent, so the store must end with exactly 4
	// distinct messages — the 2 baseline (NOT duplicated) plus the 2 new ones. This
	// is the SPEC-0013 "only the delta is added, without duplicating existing rows"
	// acceptance, asserted on the authoritative store row count (the importer's
	// idempotent replace re-writes the conversation's rows, so MessagesAdded counts
	// re-inserted rows, not net-new — the store row count is the true delta signal).
	if _, err := runner.Refresh(source.Signal); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	refreshed := waitTerminal(t, runner, source.Signal)
	if refreshed.Phase != onboard.PhaseDone {
		t.Fatalf("Refresh ended %q (err=%v), want done", refreshed.Phase, refreshed.Err)
	}
	if got := countMessages(t, st); got != 4 {
		t.Fatalf("store holds %d messages after Refresh, want exactly 4 (2 baseline + 2 delta, no dupes)", got)
	}
	if refreshed.Result.MessagesTotal != 4 {
		t.Fatalf("refresh reported store total = %d, want 4 (2 baseline + 2 delta)", refreshed.Result.MessagesTotal)
	}
	// The terminal message reports "Refreshed" (not "Enabled"), the only
	// mode-specific text.
	if refreshed.Message == "" || refreshed.Message[:9] != "Refreshed" {
		t.Errorf("refresh terminal message = %q, want it to start with \"Refreshed\"", refreshed.Message)
	}

	// The exporter ran exactly twice — one Enable, one Refresh, never a duplicate
	// concurrent export for the same source.
	exp.mu.Lock()
	runs := exp.runs
	exp.mu.Unlock()
	if runs != 2 {
		t.Fatalf("exporter ran %d times, want exactly 2 (Enable + Refresh)", runs)
	}
}
