// Tests for the modeSyncImport path (issue #157): the device-sync re-ingest
// runs ONLY the import step — no exporter resolution, no permission probe, no
// staging — under the same per-source job guard and structured progress as
// Enable/Refresh.
//
// Governing: SPEC-0014 REQ "Re-ingest Trigger" (incremental import via
// internal/onboardsvc, serialized per source), REQ "Concurrency Safety",
// REQ "Importer and Replica Roles" (a replica holds no exporter — the
// import-only mode must never resolve one).
package onboard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/joestump/msgbrowse/internal/setup"
	"github.com/joestump/msgbrowse/internal/source"
)

// failingResolver fails the test if the runner ever asks it for a tool —
// the replica-safety property of the import-only mode.
type failingResolver struct{ t *testing.T }

func (r failingResolver) ResolveTool(context.Context, string) (string, error) {
	r.t.Error("SyncImport resolved an exporter tool; import-only mode must not")
	return "", ErrToolMissing
}

// TestSyncImportRunsImportOnly: with a materialized managed root, SyncImport
// reaches PhaseDone through the importer alone — the exporter seams are never
// touched, so it works on a replica with no exporter at all.
func TestSyncImportRunsImportOnly(t *testing.T) {
	dataDir := t.TempDir()
	root, err := setup.ManagedRoot(dataDir, source.Signal)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "export"), 0o755); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var roots []string
	execCalled := false
	r, err := NewRunner(Config{
		Resolver: failingResolver{t: t},
		Exec: func(context.Context, string, []string, ...string) (string, error) {
			execCalled = true
			return "", errors.New("exporter must not run")
		},
		Importer: countingImporter(&roots, &mu),
		DataDir:  dataDir,
		Permission: func(string) (bool, string) {
			t.Error("SyncImport probed the OS permission gate; import-only mode must not")
			return false, "nothing"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Shutdown()

	if _, err := r.SyncImport(source.Signal); err != nil {
		t.Fatalf("SyncImport: %v", err)
	}
	p := waitFor(t, r, source.Signal, func(p Progress) bool { return p.Phase.Terminal() })
	if p.Phase != PhaseDone {
		t.Fatalf("phase = %s (%s), want done", p.Phase, p.Message)
	}
	if execCalled {
		t.Error("the exporter Exec seam ran during an import-only job")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(roots) != 1 || roots[0] != root {
		t.Errorf("imported roots = %v, want [%s]", roots, root)
	}
	// The terminal message names the sync-import verb for the Logs surface.
	if want := "Imported synced Signal"; !strings.Contains(p.Message, want) {
		t.Errorf("message = %q, want it to contain %q", p.Message, want)
	}
	if p.Log.Summary != p.Result {
		t.Errorf("JobLog summary %+v != result %+v", p.Log.Summary, p.Result)
	}
}

// TestSyncImportGuardedPerSource: a SyncImport while any job for the source
// is active returns ErrJobInProgress and starts nothing — the SPEC-0014
// "overlapping folder events do not double-import" serialization.
func TestSyncImportGuardedPerSource(t *testing.T) {
	dataDir := t.TempDir()
	root, _ := setup.ManagedRoot(dataDir, source.Signal)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	r, err := NewRunner(Config{
		Exec: func(context.Context, string, []string, ...string) (string, error) { return "", nil },
		Importer: ImporterFunc(func(ctx context.Context, src, root string) (ImportResult, error) {
			once.Do(func() { close(started) })
			select {
			case <-release:
			case <-ctx.Done():
			}
			return ImportResult{}, nil
		}),
		DataDir: dataDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Shutdown()

	if _, err := r.SyncImport(source.Signal); err != nil {
		t.Fatalf("first SyncImport: %v", err)
	}
	<-started
	if _, err := r.SyncImport(source.Signal); !errors.Is(err, ErrJobInProgress) {
		t.Fatalf("second SyncImport = %v, want ErrJobInProgress", err)
	}
	close(release)
	waitFor(t, r, source.Signal, func(p Progress) bool { return p.Phase.Terminal() })
}

// TestSyncImportMissingRootFails: a completion event for a source whose
// managed root never materialized is a clear terminal failure, not a silent
// no-op (SPEC-0014 REQ "Error Handling Standards").
func TestSyncImportMissingRootFails(t *testing.T) {
	var mu sync.Mutex
	var roots []string
	r, err := NewRunner(Config{
		Exec:     func(context.Context, string, []string, ...string) (string, error) { return "", nil },
		Importer: countingImporter(&roots, &mu),
		DataDir:  t.TempDir(), // archives/<source> never created
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Shutdown()

	if _, err := r.SyncImport(source.Signal); err != nil {
		t.Fatalf("SyncImport: %v", err)
	}
	p := waitFor(t, r, source.Signal, func(p Progress) bool { return p.Phase.Terminal() })
	if p.Phase != PhaseFailed || !errors.Is(p.Err, ErrImportFailed) {
		t.Fatalf("phase = %s err = %v, want failed/ErrImportFailed", p.Phase, p.Err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(roots) != 0 {
		t.Errorf("importer ran against a missing root: %v", roots)
	}
}

// TestSyncImportCancellation: cancelling the runner mid-import lands the job
// in PhaseCancelled with the context torn down — the graceful-shutdown
// scenario of SPEC-0014 REQ "Concurrency Safety".
func TestSyncImportCancellation(t *testing.T) {
	dataDir := t.TempDir()
	root, _ := setup.ManagedRoot(dataDir, source.Signal)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	var once sync.Once
	r, err := NewRunner(Config{
		Exec: func(context.Context, string, []string, ...string) (string, error) { return "", nil },
		Importer: ImporterFunc(func(ctx context.Context, src, root string) (ImportResult, error) {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return ImportResult{}, ctx.Err()
		}),
		DataDir: dataDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := r.SyncImport(source.Signal); err != nil {
		t.Fatal(err)
	}
	<-started
	r.Shutdown() // cancels the job context and waits for the worker
	p, _ := r.Status(source.Signal)
	if p.Phase != PhaseCancelled {
		t.Fatalf("phase after shutdown = %s, want cancelled", p.Phase)
	}
}

// TestSyncImportUnknownSource: the fixed source enum guards this entry point
// exactly like Enable/Refresh.
func TestSyncImportUnknownSource(t *testing.T) {
	var mu sync.Mutex
	var roots []string
	r, err := NewRunner(Config{
		Exec:     func(context.Context, string, []string, ...string) (string, error) { return "", nil },
		Importer: countingImporter(&roots, &mu),
		DataDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Shutdown()
	if _, err := r.SyncImport("../../etc"); !errors.Is(err, ErrUnknownSource) {
		t.Fatalf("SyncImport(hostile) = %v, want ErrUnknownSource", err)
	}
}
