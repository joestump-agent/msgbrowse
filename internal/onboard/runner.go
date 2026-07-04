// The supervised worker registry: one mutating job per source, cancellable,
// with observable structured state. This is the SPEC-0013 REQ "Concurrency
// Safety" core — the per-source job map is the only shared mutable state and is
// guarded by a mutex; every job runs under its own cancellable context derived
// from a runner-wide base context, so both a per-job Cancel and a runner-wide
// Shutdown tear down the exporter subprocess promptly with no orphaned process.
//
// Governing: SPEC-0013 REQ "Concurrency Safety" (supervised worker lifecycle,
// one job per source, no orphaned subprocesses, synchronized shared state, job
// teardown part of graceful shutdown), REQ "Error Handling Standards" (the
// export→adopt→import steps propagate context and surface structured terminal
// state), REQ "One-click enable and import per source".
package onboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/joestump/msgbrowse/internal/setup"
	"github.com/joestump/msgbrowse/internal/source"
)

// Progress is a snapshot of one source's job state, safe to hand to the UI. It
// is a value copy — the runner never leaks a pointer into its live job — so the
// web layer can render it without holding the runner lock. State transitions are
// monotonic toward a terminal phase (SPEC-0013 §Accessibility: this is what the
// aria-live region announces).
type Progress struct {
	// Source is the fixed source id this job concerns.
	Source string
	// Phase is the current step (queued → exporting → adopting → importing →
	// done, or a terminal failed/cancelled).
	Phase Phase
	// Message is a human-readable one-line status for the current phase — the
	// aria-live announcement text.
	Message string
	// Result is the import outcome, populated once Phase == PhaseDone.
	Result ImportResult
	// Err is the terminal error when Phase == PhaseFailed or PhaseCancelled; nil
	// otherwise. It wraps one of the package sentinels (errors.Is-matchable).
	Err error
	// StartedAt / UpdatedAt bound the job's lifetime for the UI.
	StartedAt time.Time
	UpdatedAt time.Time
}

// Active reports whether the job is still running (a non-terminal phase). The UI
// polls while Active is true and stops once a terminal phase is reached.
func (p Progress) Active() bool { return p.Phase != "" && !p.Phase.Terminal() }

// ErrText returns the terminal error string, or "" when there is none — a
// template-safe accessor (html/template cannot call methods that return an
// error, and rendering a nil error would print "<nil>").
func (p Progress) ErrText() string {
	if p.Err == nil {
		return ""
	}
	return p.Err.Error()
}

// jobMode distinguishes the two callers of the identical export→adopt→import
// pipeline: an initial Enable of a Ready source, or a Refresh of an
// already-Enabled source (SPEC-0013 REQ "Refresh"). The pipeline is byte-for-byte
// the same — the import is incremental and idempotent, so a Refresh naturally
// adds only the delta — and mode only steers the human-readable phase labels and
// the terminal "Enabled X" vs "Refreshed X" message. Keeping it a mode rather
// than a forked pipeline is the point: Refresh reuses the same staging, atomic
// adopt, cancellation, and concurrency guard as Enable, so the two can never
// diverge.
type jobMode int

const (
	// modeEnable is the first-time Enable of a Ready source.
	modeEnable jobMode = iota
	// modeRefresh re-runs the pipeline on an already-Enabled source, importing
	// only the delta.
	modeRefresh
)

// job is one supervised worker's live state. It is owned by the Runner and only
// ever mutated under the Runner's mutex; the running goroutine publishes updates
// through Runner.update, never by touching this struct directly.
type job struct {
	progress Progress
	cancel   context.CancelFunc // cancels this job's context (Cancel / Shutdown)
}

// Runner supervises per-source Enable/Refresh jobs. Construct it with NewRunner;
// it holds a base context whose cancellation (via Shutdown) tears down every
// in-flight job, the injected side-effect seams, and the guarded job registry.
type Runner struct {
	resolver ToolResolver
	runExec  ExecRunner
	importer Importer
	// permission probes the OS consent gate for a source before spawning the
	// exporter; it returns (granted, resource). The desktop shell injects the
	// genuine macOS detector; tests inject a fake. nil means "skip the probe"
	// (browser mode has no OS gate to check beyond what the exporter itself
	// reports).
	permission func(src string) (granted bool, resource string)
	dataDir    string
	log        *slog.Logger

	baseCtx context.Context
	stop    context.CancelFunc

	mu   sync.Mutex
	jobs map[string]*job // keyed by source id
	wg   sync.WaitGroup  // tracks running job goroutines for Shutdown
}

// Config configures a Runner. Every side effect is injected so the whole
// orchestration is exercised on Linux with fakes.
type Config struct {
	// Resolver resolves the exporter tool path per source (bundled or $PATH).
	Resolver ToolResolver
	// Exec spawns the exporter subprocess. Required.
	Exec ExecRunner
	// Importer imports an adopted managed root into the store. Required.
	Importer Importer
	// DataDir is the app-owned data dir; managed roots are computed from it
	// (<DataDir>/archives/<source>). Required.
	DataDir string
	// Permission optionally probes the OS consent gate before export; nil skips
	// the probe.
	Permission func(src string) (granted bool, resource string)
	// Logger receives job lifecycle logs; nil uses slog.Default().
	Logger *slog.Logger
}

// NewRunner builds a Runner over the injected seams. The returned Runner holds a
// base context; call Shutdown to cancel every in-flight job and wait for the
// worker goroutines to exit (part of the app's graceful shutdown).
func NewRunner(cfg Config) (*Runner, error) {
	if cfg.Exec == nil {
		return nil, errors.New("onboard: Exec runner is required")
	}
	if cfg.Importer == nil {
		return nil, errors.New("onboard: Importer is required")
	}
	if cfg.DataDir == "" {
		return nil, errors.New("onboard: DataDir is required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	base, stop := context.WithCancel(context.Background())
	return &Runner{
		resolver:   cfg.Resolver,
		runExec:    cfg.Exec,
		importer:   cfg.Importer,
		permission: cfg.Permission,
		dataDir:    cfg.DataDir,
		log:        log,
		baseCtx:    base,
		stop:       stop,
		jobs:       make(map[string]*job),
	}, nil
}

// Enable starts a supervised export→adopt→import job for a source and returns
// its initial Progress. It is the entry point the /setup/enable handler calls.
// If a job for the source is already running, it returns ErrJobInProgress and
// starts nothing (SPEC-0013 REQ "Concurrency Safety"). Enable returns as soon as
// the job is registered — the work runs in a background goroutine and the caller
// polls Status.
//
// An unknown source is rejected with ErrUnknownSource before any work; the web
// layer already constrains the source to the fixed enum, but the runner guards
// too so no client string can drive a filesystem path.
func (r *Runner) Enable(src string) (Progress, error) {
	return r.start(src, modeEnable)
}

// Refresh re-runs the SAME export→adopt→import pipeline on an already-Enabled
// source (SPEC-0013 REQ "Refresh"). It is the entry point the /setup/refresh
// handler calls. Because the import is incremental and idempotent, re-running the
// pipeline adds only the messages that arrived since the last import — there is
// no separate "refresh" code path, only a distinct terminal label. It shares the
// per-source concurrency guard with Enable through start(), so a Refresh while an
// Enable (or another Refresh) for the same source is in flight returns
// ErrJobInProgress and spawns no duplicate exporter (SPEC-0013 REQ "Concurrency
// Safety": "a second Enable/Refresh while one is in flight MUST be rejected").
func (r *Runner) Refresh(src string) (Progress, error) {
	return r.start(src, modeRefresh)
}

// start registers and launches a supervised job for src in the given mode. It is
// the shared body behind Enable and Refresh — one concurrency guard, one job
// registry, one goroutine lifecycle — so the two entry points cannot drift in
// their concurrency or teardown semantics; they differ only in the phase labels
// execute renders for the mode.
func (r *Runner) start(src string, mode jobMode) (Progress, error) {
	if !source.IsKnown(src) {
		return Progress{}, fmt.Errorf("%w: %q", ErrUnknownSource, src)
	}

	r.mu.Lock()
	if existing, ok := r.jobs[src]; ok && existing.progress.Active() {
		p := existing.progress
		r.mu.Unlock()
		return p, fmt.Errorf("%w: %s", ErrJobInProgress, src)
	}

	// Guard against a runner already shutting down: a base context that is done
	// means no new work should start.
	select {
	case <-r.baseCtx.Done():
		r.mu.Unlock()
		return Progress{}, fmt.Errorf("%w: runner shutting down", ErrCancelled)
	default:
	}

	now := time.Now()
	jobCtx, cancel := context.WithCancel(r.baseCtx)
	initial := Progress{
		Source:    src,
		Phase:     PhaseQueued,
		Message:   "Queued",
		StartedAt: now,
		UpdatedAt: now,
	}
	r.jobs[src] = &job{progress: initial, cancel: cancel}
	r.wg.Add(1)
	r.mu.Unlock()

	go func() {
		defer r.wg.Done()
		defer cancel()
		r.execute(jobCtx, src, mode)
	}()

	// Return the captured initial snapshot (a value copy taken under the lock),
	// never a read of j.progress after unlock — the worker goroutine mutates
	// j.progress concurrently, so reading it here would race (caught by
	// `go test -race`).
	return initial, nil
}

// Status returns the current Progress for a source, and ok=false when no job has
// ever run for it. It takes a value copy under the lock so the UI never sees a
// torn read.
func (r *Runner) Status(src string) (Progress, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[src]
	if !ok {
		return Progress{}, false
	}
	return j.progress, true
}

// Cancel requests cancellation of a source's in-flight job. It cancels the job's
// context (terminating the exporter subprocess) and returns true if a job was
// running. A cancelled job reaches PhaseCancelled and promotes no partial output
// (SPEC-0013 "Cancel mid-export leaves no partial archive").
func (r *Runner) Cancel(src string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[src]
	if !ok || !j.progress.Active() {
		return false
	}
	j.cancel()
	return true
}

// Shutdown cancels every in-flight job and blocks until all worker goroutines
// have exited, so no exporter subprocess outlives the app (SPEC-0013 "Quitting
// the app tears down running jobs"). It is idempotent and part of the graceful-
// shutdown path the desktop shell and `msgbrowse serve` already run.
func (r *Runner) Shutdown() {
	r.stop()
	r.wg.Wait()
}

// update publishes a phase/message transition for a source under the lock. It is
// the only writer of live job progress, so all mutation is serialized. A
// terminal phase records Err; the timestamp always advances.
func (r *Runner) update(src string, phase Phase, msg string, res ImportResult, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[src]
	if !ok {
		return
	}
	j.progress.Phase = phase
	j.progress.Message = msg
	j.progress.Result = res
	j.progress.Err = err
	j.progress.UpdatedAt = time.Now()
}

// execute runs the full pipeline for a source under jobCtx: resolve tool → probe
// permission → export into staging → adopt → import. It backs BOTH Enable and
// Refresh (mode) — the steps are identical because a Refresh is just the same
// pipeline over the already-Enabled source, and the incremental importer adds
// only the delta. Every step checks for cancellation, and any failure records a
// terminal PhaseFailed/PhaseCancelled with a sentinel-wrapped error and discards
// staging so the managed root is untouched. It never panics out to the goroutine
// (a panic would leave the job non-terminal); all exits publish a terminal phase.
func (r *Runner) execute(jobCtx context.Context, src string, mode jobMode) {
	// managedRoot is app-computed from the fixed source enum — never client input.
	managedRoot, err := setup.ManagedRoot(r.dataDir, src)
	if err != nil {
		r.fail(src, ErrUnknownSource, fmt.Sprintf("cannot resolve archive location: %v", err))
		return
	}

	// 1. Resolve the exporter tool. A missing tool is a clear terminal state, not
	//    a silent no-op or a $PATH fallback in the bundled build.
	tool, err := r.resolveTool(jobCtx, src)
	if err != nil {
		if errors.Is(err, ErrToolMissing) {
			r.fail(src, ErrToolMissing, fmt.Sprintf("%s exporter is not available", source.Label(src)))
		} else {
			r.fail(src, wrapSentinel(err, ErrToolMissing), fmt.Sprintf("could not resolve %s exporter: %v", source.Label(src), err))
		}
		return
	}
	if r.cancelled(jobCtx) {
		r.cancel(src)
		return
	}

	// 2. Probe the OS consent gate (detect-and-guide only). A missing grant is a
	//    permission error surfaced as guidance — the export never silently fails
	//    or produces an empty archive.
	if r.permission != nil {
		granted, resource := r.permission(src)
		if !granted {
			r.fail(src, ErrPermissionDenied,
				fmt.Sprintf("%s needs macOS permission to read %s — grant access in System Settings, then try again", source.Label(src), resource))
			return
		}
	}

	// 3. Export into a fresh staging dir beside the managed root.
	r.update(src, PhaseExporting, fmt.Sprintf("Exporting %s…", source.Label(src)), ImportResult{}, nil)
	staging, err := newStaging(managedRoot)
	if err != nil {
		r.fail(src, wrapSentinel(err, ErrExportFailed), fmt.Sprintf("could not prepare staging: %v", err))
		return
	}
	args, err := ExportArgs(src, staging)
	if err != nil {
		_ = discardStaging(staging)
		r.fail(src, ErrUnknownSource, fmt.Sprintf("cannot build export command: %v", err))
		return
	}
	if err := r.runExec(jobCtx, tool, args...); err != nil {
		_ = discardStaging(staging)
		if r.cancelled(jobCtx) {
			r.cancel(src)
			return
		}
		r.fail(src, wrapSentinel(err, ErrExportFailed), fmt.Sprintf("%s export failed: %v", source.Label(src), err))
		return
	}
	if r.cancelled(jobCtx) {
		// A cancellation that raced past a clean exporter exit still discards
		// staging: nothing is promoted.
		_ = discardStaging(staging)
		r.cancel(src)
		return
	}

	// 4. Atomically adopt staging into the managed root.
	r.update(src, PhaseAdopting, "Finalizing archive…", ImportResult{}, nil)
	if err := adopt(staging, managedRoot); err != nil {
		_ = discardStaging(staging)
		r.fail(src, wrapSentinel(err, ErrExportFailed), fmt.Sprintf("could not finalize %s archive: %v", source.Label(src), err))
		return
	}

	// 5. Import the adopted archive into the store.
	if r.cancelled(jobCtx) {
		// The archive is already adopted (a complete, valid export); a cancel here
		// simply skips the import — a later Refresh imports it idempotently. Report
		// cancelled so the UI is honest.
		r.cancel(src)
		return
	}
	r.update(src, PhaseImporting, fmt.Sprintf("Importing %s…", source.Label(src)), ImportResult{}, nil)
	res, err := r.importer.Import(jobCtx, src, managedRoot)
	if err != nil {
		if r.cancelled(jobCtx) {
			r.cancel(src)
			return
		}
		r.fail(src, wrapSentinel(err, ErrImportFailed), fmt.Sprintf("%s import failed: %v", source.Label(src), err))
		return
	}

	// The terminal message is the only mode-specific text: an Enable reports the
	// source is now live ("Enabled Signal — …"); a Refresh reports the delta it
	// just added ("Refreshed Signal — …"). Both carry the same counts so the
	// aria-live region announces exactly what changed (SPEC-0013 REQ "Refresh":
	// "reports the number of new conversations/messages").
	verb := "Enabled"
	logMsg := "onboard enable complete"
	if mode == modeRefresh {
		verb = "Refreshed"
		logMsg = "onboard refresh complete"
	}
	r.update(src, PhaseDone,
		fmt.Sprintf("%s %s — %d conversations, %d messages added", verb, source.Label(src), res.ConversationsChanged, res.MessagesAdded),
		res, nil)
	r.log.Info(logMsg, "source", src,
		"conversations_changed", res.ConversationsChanged, "messages_added", res.MessagesAdded)
}

// resolveTool resolves the exporter path for a source, translating a nil
// resolver into ErrToolMissing (a build with no resolver has no tool for any
// source).
func (r *Runner) resolveTool(ctx context.Context, src string) (string, error) {
	if r.resolver == nil {
		return "", ErrToolMissing
	}
	tool, err := r.resolver.ResolveTool(ctx, src)
	if err != nil {
		return "", err
	}
	if tool == "" {
		return "", ErrToolMissing
	}
	return tool, nil
}

// fail records a terminal PhaseFailed with a sentinel-wrapped error and logs it
// (never swallowed — SPEC-0013 REQ "Error Handling Standards").
func (r *Runner) fail(src string, sentinel error, msg string) {
	err := fmt.Errorf("%s: %w", msg, sentinel)
	r.update(src, PhaseFailed, msg, ImportResult{}, err)
	r.log.Warn("onboard enable failed", "source", src, "error", err)
}

// cancel records a terminal PhaseCancelled. Called when the job context is done
// before completion; no partial output has been promoted.
func (r *Runner) cancel(src string) {
	err := fmt.Errorf("%s enable cancelled: %w", source.Label(src), ErrCancelled)
	r.update(src, PhaseCancelled, "Cancelled", ImportResult{}, err)
	r.log.Info("onboard enable cancelled", "source", src)
}

// cancelled reports whether the job context is done (cancelled or shutting down).
func (r *Runner) cancelled(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// wrapSentinel joins an underlying error with a sentinel so callers can match
// the failure mode with errors.Is while still seeing the concrete cause. The
// underlying error stays wrapped for %w chains; the sentinel is added for
// classification.
func wrapSentinel(underlying, sentinel error) error {
	return fmt.Errorf("%w (%w)", sentinel, underlying)
}
