package onboard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/setup"
	"github.com/joestump/msgbrowse/internal/source"
)

// waitFor polls Status until the predicate holds or the deadline passes. The
// runner is asynchronous, so tests observe terminal state by polling rather than
// sleeping a fixed duration.
func waitFor(t *testing.T, r *Runner, src string, pred func(Progress) bool) Progress {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p, ok := r.Status(src); ok && pred(p) {
			return p
		}
		time.Sleep(2 * time.Millisecond)
	}
	p, _ := r.Status(src)
	t.Fatalf("timed out waiting for source %s; last state phase=%q msg=%q err=%v", src, p.Phase, p.Message, p.Err)
	return Progress{}
}

// fakeSignalExporter writes a minimal valid signal-export archive into the
// staging dir's export/<conv>/chat.md — the layout internal/ingest scans — so a
// real import can land conversations in the store. It records the argv it was
// called with so tests can assert the app-owned command line.
func fakeSignalExporter(calls *[][]string, mu *sync.Mutex) ExecRunner {
	return func(ctx context.Context, name string, env []string, args ...string) (string, error) {
		mu.Lock()
		*calls = append(*calls, append([]string{name}, args...))
		mu.Unlock()
		// sigexport's positional arg is <dest>/export.
		dest := args[len(args)-1]
		convDir := filepath.Join(dest, "Alice")
		if err := os.MkdirAll(convDir, 0o755); err != nil {
			return "", err
		}
		chat := "[2022-01-01 10:00:00] Alice: Hello from the fake exporter\n" +
			"[2022-01-01 10:01:00] Me: Hi back\n"
		return "", os.WriteFile(filepath.Join(convDir, "chat.md"), []byte(chat), 0o644)
	}
}

// countingImporter records the archive roots it was asked to import and returns
// a fixed result. It stands in for the real store-backed importer so the runner
// pipeline is exercised without a store.
func countingImporter(seen *[]string, mu *sync.Mutex) Importer {
	return ImporterFunc(func(ctx context.Context, src, root string) (ImportResult, error) {
		mu.Lock()
		*seen = append(*seen, root)
		mu.Unlock()
		return ImportResult{ConversationsChanged: 1, MessagesAdded: 2, MessagesTotal: 2}, nil
	})
}

func staticResolver(path string) ToolResolver {
	return ToolResolverFunc(func(ctx context.Context, src string) (string, error) {
		return path, nil
	})
}

// envResolver is a ToolResolver that ALSO implements EnvResolver, returning a
// fixed tool path and a fixed subprocess env. It stands in for the desktop's
// bundled resolver so the runner's env-threading (issue #147) is exercised on
// Linux without a real .app.
type envResolver struct {
	path string
	env  []string
}

func (e envResolver) ResolveTool(context.Context, string) (string, error) { return e.path, nil }
func (e envResolver) EnvForTool(context.Context, string, string) ([]string, error) {
	return e.env, nil
}

// TestEnableThreadsResolvedEnvToExporter proves the relocation fix reaches the
// spawn site (issue #147): when the resolver implements EnvResolver, the env it
// returns for the source is handed to the ExecRunner verbatim — the corrected
// PYTHONHOME/PYTHONPATH a bundled Python exporter needs after the .app moves.
func TestEnableThreadsResolvedEnvToExporter(t *testing.T) {
	dataDir := t.TempDir()
	wantEnv := []string{"PYTHONHOME=/App/Contents/Resources/tools/python", "PYTHONPATH=/App/.../site-packages"}
	var (
		mu      sync.Mutex
		gotEnv  []string
		gotName string
	)
	r, err := NewRunner(Config{
		Resolver: envResolver{path: "/App/Contents/Resources/tools/venv/bin/sigexport", env: wantEnv},
		Exec: func(ctx context.Context, name string, env []string, args ...string) (string, error) {
			mu.Lock()
			gotName, gotEnv = name, env
			mu.Unlock()
			// Write a minimal fixture so the import step succeeds.
			dest := args[len(args)-1]
			convDir := filepath.Join(dest, "Alice")
			if err := os.MkdirAll(convDir, 0o755); err != nil {
				return "", err
			}
			return "", os.WriteFile(filepath.Join(convDir, "chat.md"),
				[]byte("[2022-01-01 10:00:00] Alice: hi\n"), 0o644)
		},
		Importer: ImporterFunc(func(context.Context, string, string) (ImportResult, error) {
			return ImportResult{ConversationsChanged: 1, MessagesAdded: 1}, nil
		}),
		DataDir: dataDir,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	defer r.Shutdown()

	if _, err := r.Enable(source.Signal); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	waitFor(t, r, source.Signal, func(p Progress) bool { return p.Phase.Terminal() })

	mu.Lock()
	defer mu.Unlock()
	if gotName != "/App/Contents/Resources/tools/venv/bin/sigexport" {
		t.Errorf("exporter name = %q; want the resolved bundled sigexport path", gotName)
	}
	if len(gotEnv) != len(wantEnv) || gotEnv[0] != wantEnv[0] || gotEnv[1] != wantEnv[1] {
		t.Errorf("exporter env = %v; want the resolver's env %v", gotEnv, wantEnv)
	}
}

// TestEnableWithoutEnvResolverInheritsEnv proves the serve/$PATH path is
// unchanged: a plain ToolResolver (no EnvResolver) yields a nil env, so the
// exporter inherits the process environment exactly as before.
func TestEnableWithoutEnvResolverInheritsEnv(t *testing.T) {
	dataDir := t.TempDir()
	var (
		mu     sync.Mutex
		envSet bool
		gotNil bool
	)
	r, err := NewRunner(Config{
		Resolver: staticResolver("/usr/bin/sigexport"),
		Exec: func(ctx context.Context, name string, env []string, args ...string) (string, error) {
			mu.Lock()
			envSet = true
			gotNil = env == nil
			mu.Unlock()
			dest := args[len(args)-1]
			convDir := filepath.Join(dest, "Alice")
			if err := os.MkdirAll(convDir, 0o755); err != nil {
				return "", err
			}
			return "", os.WriteFile(filepath.Join(convDir, "chat.md"),
				[]byte("[2022-01-01 10:00:00] Alice: hi\n"), 0o644)
		},
		Importer: ImporterFunc(func(context.Context, string, string) (ImportResult, error) {
			return ImportResult{}, nil
		}),
		DataDir: dataDir,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	defer r.Shutdown()

	if _, err := r.Enable(source.Signal); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	waitFor(t, r, source.Signal, func(p Progress) bool { return p.Phase.Terminal() })

	mu.Lock()
	defer mu.Unlock()
	if !envSet {
		t.Fatal("exporter was never called")
	}
	if !gotNil {
		t.Error("exporter env != nil for a non-EnvResolver resolver; serve/$PATH must inherit the environment")
	}
}

// TestEnableEndToEnd drives Enable through the full pipeline with a fake
// exporter and asserts the export ran into a staging dir, the managed root was
// populated by the atomic adopt, and the importer saw the managed root — the
// SPEC-0013 "Enable … end to end" acceptance.
func TestEnableEndToEnd(t *testing.T) {
	dataDir := t.TempDir()
	var (
		mu    sync.Mutex
		calls [][]string
		roots []string
	)
	r, err := NewRunner(Config{
		Resolver: staticResolver("/bundle/sigexport"),
		Exec:     fakeSignalExporter(&calls, &mu),
		Importer: countingImporter(&roots, &mu),
		DataDir:  dataDir,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	defer r.Shutdown()

	if _, err := r.Enable(source.Signal); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	done := waitFor(t, r, source.Signal, func(p Progress) bool { return p.Phase.Terminal() })
	if done.Phase != PhaseDone {
		t.Fatalf("expected PhaseDone, got %q (err=%v)", done.Phase, done.Err)
	}
	if done.Result.ConversationsChanged != 1 || done.Result.MessagesAdded != 2 {
		t.Fatalf("unexpected import result: %+v", done.Result)
	}

	// The managed root was populated (adopt promoted staging), and the staging +
	// trash siblings were cleaned up.
	managedRoot, _ := setup.ManagedRoot(dataDir, source.Signal)
	if _, err := os.Stat(filepath.Join(managedRoot, "export", "Alice", "chat.md")); err != nil {
		t.Fatalf("managed root not populated by adopt: %v", err)
	}
	if _, err := os.Stat(managedRoot + stagingSuffix); !os.IsNotExist(err) {
		t.Fatalf("staging dir was not discarded after adopt")
	}

	// The exporter was invoked with the app-owned argv (name + <staging>/export),
	// not any client input.
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("expected exactly one exporter call, got %d", len(calls))
	}
	if calls[0][0] != "/bundle/sigexport" {
		t.Fatalf("exporter argv[0] = %q, want the resolved bundled tool path", calls[0][0])
	}
	wantDest := managedRoot + stagingSuffix + string(filepath.Separator) + "export"
	if calls[0][len(calls[0])-1] != wantDest {
		t.Fatalf("exporter dest = %q, want staging export dir %q", calls[0][len(calls[0])-1], wantDest)
	}
	if len(roots) != 1 || roots[0] != managedRoot {
		t.Fatalf("importer saw roots %v, want [%s]", roots, managedRoot)
	}
}

// TestCancelMidExportLeavesNoPartialArchive is the SPEC-0013 "Cancel mid-export
// leaves no partial archive" scenario: the exporter blocks until its context is
// cancelled, and the test asserts the job reaches PhaseCancelled, the managed
// root is untouched (never created), the staging tree is discarded, and the
// importer was never called.
func TestCancelMidExportLeavesNoPartialArchive(t *testing.T) {
	dataDir := t.TempDir()
	started := make(chan struct{})
	var (
		mu       sync.Mutex
		imported bool
	)
	blockingExporter := func(ctx context.Context, name string, env []string, args ...string) (string, error) {
		close(started)
		<-ctx.Done() // block until cancelled
		// Write a partial file to prove even a exporter that scribbled output on
		// its way out cannot corrupt the managed root (it is under staging).
		dest := args[len(args)-1]
		_ = os.MkdirAll(dest, 0o755)
		_ = os.WriteFile(filepath.Join(dest, "partial"), []byte("x"), 0o644)
		return "", ctx.Err()
	}
	r, err := NewRunner(Config{
		Resolver: staticResolver("/bundle/sigexport"),
		Exec:     blockingExporter,
		Importer: ImporterFunc(func(ctx context.Context, src, root string) (ImportResult, error) {
			mu.Lock()
			imported = true
			mu.Unlock()
			return ImportResult{}, nil
		}),
		DataDir: dataDir,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	defer r.Shutdown()

	if _, err := r.Enable(source.Signal); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	<-started // exporter is running
	if !r.Cancel(source.Signal) {
		t.Fatal("Cancel returned false for a running job")
	}
	term := waitFor(t, r, source.Signal, func(p Progress) bool { return p.Phase.Terminal() })
	if term.Phase != PhaseCancelled {
		t.Fatalf("expected PhaseCancelled, got %q (err=%v)", term.Phase, term.Err)
	}
	if !errors.Is(term.Err, ErrCancelled) {
		t.Fatalf("terminal error %v does not wrap ErrCancelled", term.Err)
	}

	managedRoot, _ := setup.ManagedRoot(dataDir, source.Signal)
	if _, err := os.Stat(managedRoot); !os.IsNotExist(err) {
		t.Fatalf("managed root exists after cancel — must be untouched: %v", err)
	}
	if _, err := os.Stat(managedRoot + stagingSuffix); !os.IsNotExist(err) {
		t.Fatalf("staging dir survived cancel — must be discarded")
	}
	mu.Lock()
	defer mu.Unlock()
	if imported {
		t.Fatal("importer ran despite a cancelled export")
	}
}

// TestConcurrentEnableRejected is the SPEC-0013 "A second Enable while one is
// running is rejected" scenario: while one job is in flight, a second Enable for
// the same source returns ErrJobInProgress and does not spawn a second exporter.
func TestConcurrentEnableRejected(t *testing.T) {
	dataDir := t.TempDir()
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var (
		mu    sync.Mutex
		calls int
	)
	gatedExporter := func(ctx context.Context, name string, env []string, args ...string) (string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		dest := args[len(args)-1]
		convDir := filepath.Join(dest, "Alice")
		_ = os.MkdirAll(convDir, 0o755)
		return "", os.WriteFile(filepath.Join(convDir, "chat.md"), []byte("[2022-01-01 10:00:00] Me: hi\n"), 0o644)
	}
	r, err := NewRunner(Config{
		Resolver: staticResolver("/bundle/sigexport"),
		Exec:     gatedExporter,
		Importer: ImporterFunc(func(ctx context.Context, src, root string) (ImportResult, error) {
			return ImportResult{}, nil
		}),
		DataDir: dataDir,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	defer r.Shutdown()

	if _, err := r.Enable(source.Signal); err != nil {
		t.Fatalf("first Enable: %v", err)
	}
	<-started // first job is inside the exporter

	_, err = r.Enable(source.Signal)
	if !errors.Is(err, ErrJobInProgress) {
		t.Fatalf("second Enable error = %v, want ErrJobInProgress", err)
	}

	close(release)
	done := waitFor(t, r, source.Signal, func(p Progress) bool { return p.Phase.Terminal() })
	if done.Phase != PhaseDone {
		t.Fatalf("first job ended %q, want done (err=%v)", done.Phase, done.Err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("exporter was spawned %d times, want exactly 1 (no duplicate concurrent export)", calls)
	}
}

// TestToolMissingSentinel: a resolver that reports no tool yields a terminal
// PhaseFailed wrapping ErrToolMissing, and no subprocess is spawned.
func TestToolMissingSentinel(t *testing.T) {
	dataDir := t.TempDir()
	var spawned bool
	r, err := NewRunner(Config{
		Resolver: ToolResolverFunc(func(ctx context.Context, src string) (string, error) {
			return "", ErrToolMissing
		}),
		Exec: func(ctx context.Context, name string, env []string, args ...string) (string, error) {
			spawned = true
			return "", nil
		},
		Importer: ImporterFunc(func(ctx context.Context, src, root string) (ImportResult, error) {
			return ImportResult{}, nil
		}),
		DataDir: dataDir,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	defer r.Shutdown()

	if _, err := r.Enable(source.IMessage); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	term := waitFor(t, r, source.IMessage, func(p Progress) bool { return p.Phase.Terminal() })
	if term.Phase != PhaseFailed || !errors.Is(term.Err, ErrToolMissing) {
		t.Fatalf("expected PhaseFailed wrapping ErrToolMissing, got phase=%q err=%v", term.Phase, term.Err)
	}
	if spawned {
		t.Fatal("exporter subprocess was spawned despite a missing tool")
	}
}

// TestPermissionDeniedSentinel: a permission probe reporting not-granted yields
// PhaseFailed wrapping ErrPermissionDenied and never spawns the exporter.
func TestPermissionDeniedSentinel(t *testing.T) {
	dataDir := t.TempDir()
	var spawned bool
	r, err := NewRunner(Config{
		Resolver: staticResolver("/bundle/imessage-exporter"),
		Exec: func(ctx context.Context, name string, env []string, args ...string) (string, error) {
			spawned = true
			return "", nil
		},
		Importer: ImporterFunc(func(ctx context.Context, src, root string) (ImportResult, error) {
			return ImportResult{}, nil
		}),
		Permission: func(src string) (bool, string) { return false, "~/Library/Messages/chat.db" },
		DataDir:    dataDir,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	defer r.Shutdown()

	if _, err := r.Enable(source.IMessage); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	term := waitFor(t, r, source.IMessage, func(p Progress) bool { return p.Phase.Terminal() })
	if term.Phase != PhaseFailed || !errors.Is(term.Err, ErrPermissionDenied) {
		t.Fatalf("expected PhaseFailed wrapping ErrPermissionDenied, got phase=%q err=%v", term.Phase, term.Err)
	}
	if spawned {
		t.Fatal("exporter spawned despite a denied OS permission")
	}
}

// TestExportFailedSentinel: a non-zero exporter yields PhaseFailed wrapping
// ErrExportFailed, the managed root is untouched (staging discarded), AND the
// exporter's captured combined output + exit status are recorded in the job's
// JobLog so the Logs viewer can show WHY it failed (issue #151), not just "exit
// status N".
func TestExportFailedSentinel(t *testing.T) {
	dataDir := t.TempDir()
	wantErr := errors.New("exit status 2")
	const wantStderr = "usage: wtsexporter [-h] ...\nwtsexporter: error: iOS mode requires -d"
	r, err := NewRunner(Config{
		Resolver: staticResolver("/bundle/imessage-exporter"),
		Exec: func(ctx context.Context, name string, env []string, args ...string) (string, error) {
			// A failing exporter "prints" a diagnostic to its combined output and
			// exits non-zero — exactly the WhatsApp exit-2 argparse case.
			return wantStderr, wantErr
		},
		Importer: ImporterFunc(func(ctx context.Context, src, root string) (ImportResult, error) {
			t.Error("importer must not run after an export failure")
			return ImportResult{}, nil
		}),
		DataDir: dataDir,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	defer r.Shutdown()

	if _, err := r.Enable(source.IMessage); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	term := waitFor(t, r, source.IMessage, func(p Progress) bool { return p.Phase.Terminal() })
	if term.Phase != PhaseFailed || !errors.Is(term.Err, ErrExportFailed) {
		t.Fatalf("expected PhaseFailed wrapping ErrExportFailed, got phase=%q err=%v", term.Phase, term.Err)
	}
	// The captured exporter output + exit status are on the terminal snapshot's
	// JobLog — the diagnostic surface for the Logs viewer.
	if term.Log.Output != wantStderr {
		t.Fatalf("JobLog.Output = %q, want the captured exporter stderr %q", term.Log.Output, wantStderr)
	}
	if term.Log.ExitStatus != wantErr.Error() {
		t.Fatalf("JobLog.ExitStatus = %q, want %q", term.Log.ExitStatus, wantErr.Error())
	}
	if term.Log.Tool != "/bundle/imessage-exporter" {
		t.Fatalf("JobLog.Tool = %q, want the resolved tool path", term.Log.Tool)
	}
	if term.Log.ArgvLine() == "" {
		t.Fatal("JobLog.ArgvLine() is empty; the exporter command line must be recorded")
	}
	managedRoot, _ := setup.ManagedRoot(dataDir, source.IMessage)
	if _, err := os.Stat(managedRoot); !os.IsNotExist(err) {
		t.Fatalf("managed root exists after a failed export — must be untouched")
	}
}

// TestImportFailedSentinel: the export + adopt succeed but the importer fails;
// the job reaches PhaseFailed wrapping ErrImportFailed and the managed root is
// still populated (the adopted archive is valid; a later Refresh re-imports).
func TestImportFailedSentinel(t *testing.T) {
	dataDir := t.TempDir()
	var (
		mu    sync.Mutex
		calls [][]string
	)
	r, err := NewRunner(Config{
		Resolver: staticResolver("/bundle/sigexport"),
		Exec:     fakeSignalExporter(&calls, &mu),
		Importer: ImporterFunc(func(ctx context.Context, src, root string) (ImportResult, error) {
			return ImportResult{}, errors.New("store is read-only")
		}),
		DataDir: dataDir,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	defer r.Shutdown()

	if _, err := r.Enable(source.Signal); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	term := waitFor(t, r, source.Signal, func(p Progress) bool { return p.Phase.Terminal() })
	if term.Phase != PhaseFailed || !errors.Is(term.Err, ErrImportFailed) {
		t.Fatalf("expected PhaseFailed wrapping ErrImportFailed, got phase=%q err=%v", term.Phase, term.Err)
	}
	managedRoot, _ := setup.ManagedRoot(dataDir, source.Signal)
	if _, err := os.Stat(filepath.Join(managedRoot, "export", "Alice", "chat.md")); err != nil {
		t.Fatalf("managed root should hold the adopted archive after an import failure: %v", err)
	}
}

// TestAdoptReplacesExistingArchive proves a re-Enable (or Refresh) atomically
// replaces the prior managed contents rather than merging or corrupting them.
func TestAdoptReplacesExistingArchive(t *testing.T) {
	dataDir := t.TempDir()
	managedRoot, _ := setup.ManagedRoot(dataDir, source.Signal)
	// Seed a stale managed root with a file the new export does not produce.
	if err := os.MkdirAll(managedRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(managedRoot, "stale.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	var (
		mu    sync.Mutex
		calls [][]string
		roots []string
	)
	r, err := NewRunner(Config{
		Resolver: staticResolver("/bundle/sigexport"),
		Exec:     fakeSignalExporter(&calls, &mu),
		Importer: countingImporter(&roots, &mu),
		DataDir:  dataDir,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	defer r.Shutdown()

	if _, err := r.Enable(source.Signal); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	done := waitFor(t, r, source.Signal, func(p Progress) bool { return p.Phase.Terminal() })
	if done.Phase != PhaseDone {
		t.Fatalf("expected PhaseDone, got %q (err=%v)", done.Phase, done.Err)
	}
	// The stale file is gone (replaced, not merged) and the new archive is present.
	if _, err := os.Stat(filepath.Join(managedRoot, "stale.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale file survived adopt — adopt must replace, not merge")
	}
	if _, err := os.Stat(filepath.Join(managedRoot, "export", "Alice", "chat.md")); err != nil {
		t.Fatalf("new archive missing after adopt: %v", err)
	}
	// No trash sibling left behind.
	if _, err := os.Stat(managedRoot + trashSuffix); !os.IsNotExist(err) {
		t.Fatalf("adopt-trash sibling was not cleaned up")
	}
}

// TestUnknownSourceRejected: Enable with a non-enum source is rejected up front.
func TestUnknownSourceRejected(t *testing.T) {
	r, err := NewRunner(Config{
		Resolver: staticResolver("/bundle/x"),
		Exec:     func(ctx context.Context, name string, env []string, args ...string) (string, error) { return "", nil },
		Importer: ImporterFunc(func(ctx context.Context, src, root string) (ImportResult, error) { return ImportResult{}, nil }),
		DataDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	defer r.Shutdown()
	if _, err := r.Enable("../etc/passwd"); !errors.Is(err, ErrUnknownSource) {
		t.Fatalf("Enable(unknown) error = %v, want ErrUnknownSource", err)
	}
}

// TestShutdownCancelsRunningJob: Shutdown cancels an in-flight job and waits for
// the worker to exit — no orphaned goroutine or subprocess (SPEC-0013 "Quitting
// the app tears down running jobs").
func TestShutdownCancelsRunningJob(t *testing.T) {
	dataDir := t.TempDir()
	started := make(chan struct{})
	exited := make(chan struct{})
	blockingExporter := func(ctx context.Context, name string, env []string, args ...string) (string, error) {
		close(started)
		<-ctx.Done()
		close(exited)
		return "", ctx.Err()
	}
	r, err := NewRunner(Config{
		Resolver: staticResolver("/bundle/sigexport"),
		Exec:     blockingExporter,
		Importer: ImporterFunc(func(ctx context.Context, src, root string) (ImportResult, error) { return ImportResult{}, nil }),
		DataDir:  dataDir,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	if _, err := r.Enable(source.Signal); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	<-started
	r.Shutdown() // must cancel the job's context and block until the worker returns
	select {
	case <-exited:
	default:
		t.Fatal("exporter context was not cancelled by Shutdown")
	}
	// A new Enable after shutdown is rejected (runner is stopped).
	if _, err := r.Enable(source.Signal); !errors.Is(err, ErrCancelled) {
		t.Fatalf("Enable after Shutdown = %v, want ErrCancelled", err)
	}
}
