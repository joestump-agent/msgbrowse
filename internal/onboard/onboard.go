// Package onboard is the shared, UI-free orchestration for the desktop guided-
// setup Enable flow (SPEC-0013): resolve the exporter for a source, run it into
// a STAGING directory, atomically adopt the staged output into the app-owned
// managed archive root, then import it into the store — reporting structured
// progress throughout and honoring cancellation at every step.
//
// It is pure Go with every side effect injected: the exporter tool paths come
// from a ToolResolver (the desktop shell backs it with the bundled toolchain;
// `msgbrowse serve` backs it with $PATH), the subprocess spawn comes from an
// ExecRunner, and the import comes from an Importer. That injection is the whole
// point — the Enable→export→adopt→import pipeline is driven end to end on Linux
// with a FAKE exporter (a func that writes a known fixture archive) and NO cgo,
// so the concurrency, staging/atomic-adopt, cancellation, and error-handling
// contracts are all covered by `go test ./internal/onboard`.
//
// Concurrency Safety (SPEC-0013 REQ): the Runner is a supervised worker registry
// keyed by source. Exactly one mutating job runs per source at a time; a second
// Enable/Refresh while one is in flight is rejected with ErrJobInProgress rather
// than spawning a duplicate exporter. All shared state (the per-source job map)
// is guarded by a mutex, and each job carries its own cancellable context so a
// Cancel — or app shutdown — tears the exporter subprocess down promptly.
//
// Error Handling Standards (SPEC-0013 REQ): every failure mode has a sentinel
// (ErrToolMissing, ErrPermissionDenied, ErrExportFailed, ErrImportFailed,
// ErrCancelled) wrapped with context; the staging + atomic-adopt design means a
// cancelled or failed export NEVER promotes partial output into the managed root
// (the store and archive are left import-clean), and errors are surfaced as
// structured job state, never swallowed.
//
// Governing: ADR-0020 (self-contained desktop onboarding — the export jobs drive
// the bundled exporter into the managed archives, then the existing importer
// loads the store), SPEC-0013 REQ "One-click enable and import per source", REQ
// "Error Handling Standards", REQ "Concurrency Safety", and §Security (the
// exporter argv is assembled from app-owned constants + the bundled tool path +
// the computed managed root — no client input reaches the command line).
package onboard

import (
	"context"
	"errors"
	"fmt"

	"github.com/joestump/msgbrowse/internal/source"
)

// Sentinel errors for the known failure modes SPEC-0013 REQ "Error Handling
// Standards" names. Callers (the web layer, tests) match these with errors.Is
// to render a precise per-source message rather than a generic failure.
var (
	// ErrToolMissing: the exporter for the source could not be resolved (no
	// bundled tool and none on $PATH), so no subprocess can be spawned.
	ErrToolMissing = errors.New("onboard: exporter tool not available for source")
	// ErrPermissionDenied: an OS consent gate (macOS Full Disk Access / Signal
	// Keychain / WhatsApp container) is not granted, so the export cannot read
	// the source. Surfaced as guidance, not a hard crash.
	ErrPermissionDenied = errors.New("onboard: os permission not granted for source")
	// ErrExportFailed: the exporter subprocess exited non-zero (e.g. a locked
	// source database). The staged output is discarded; the managed root is
	// untouched.
	ErrExportFailed = errors.New("onboard: exporter failed")
	// ErrImportFailed: the export succeeded and was adopted, but importing the
	// adopted archive into the store failed.
	ErrImportFailed = errors.New("onboard: import failed")
	// ErrCancelled: the caller cancelled the job (or the app is shutting down)
	// before it completed. No partial output is promoted.
	ErrCancelled = errors.New("onboard: job cancelled")
	// ErrJobInProgress: a mutating job for this source is already running; a
	// second Enable/Refresh is rejected rather than spawning a duplicate exporter
	// (SPEC-0013 REQ "Concurrency Safety" — "A second Enable while one is running
	// is rejected").
	ErrJobInProgress = errors.New("onboard: a job is already running for this source")
	// ErrUnknownSource: the source id is not one of the fixed enum values. The web
	// layer rejects these before reaching the runner, but the orchestration guards
	// too so no client string can ever drive a filesystem path.
	ErrUnknownSource = errors.New("onboard: unknown source")
)

// Phase is the coarse step a job is in, for structured progress. It is reported
// as a stable lowercase token so the UI's aria-live region can announce it and
// tests can assert the lifecycle.
type Phase string

const (
	// PhaseQueued: the job is registered but has not started work yet.
	PhaseQueued Phase = "queued"
	// PhaseExporting: the exporter subprocess is running into the staging dir.
	PhaseExporting Phase = "exporting"
	// PhaseAdopting: the export finished; the staged output is being promoted
	// into the managed archive root.
	PhaseAdopting Phase = "adopting"
	// PhaseImporting: the adopted archive is being imported into the store.
	PhaseImporting Phase = "importing"
	// PhaseDone: the job finished successfully; the source is Enabled.
	PhaseDone Phase = "done"
	// PhaseFailed: the job ended in a terminal error (Err is set).
	PhaseFailed Phase = "failed"
	// PhaseCancelled: the job was cancelled before completion (Err wraps
	// ErrCancelled).
	PhaseCancelled Phase = "cancelled"
)

// Terminal reports whether a phase is an end state (no further transitions).
func (p Phase) Terminal() bool {
	return p == PhaseDone || p == PhaseFailed || p == PhaseCancelled
}

// ImportResult is the subset of an import run the UI announces: how many
// conversations and messages the incremental import added, plus the outcome of
// the post-import media-transcode step. It is deliberately small and
// importer-agnostic so the orchestration never depends on store.IngestRun.
type ImportResult struct {
	// ConversationsChanged is the number of conversations added or updated.
	ConversationsChanged int
	// MessagesAdded is the number of new messages written.
	MessagesAdded int
	// MessagesTotal is the store-wide message count after the import.
	MessagesTotal int
	// MediaConverted / MediaSkipped / MediaFailed summarize the best-effort
	// post-import image transcode (HEIC/TIFF → cached JPEG) the Importer runs
	// after loading the store — the same step `msgbrowse import` runs (issue
	// #160: without it, desktop-onboarded HEICs never got derivatives). They
	// ride the ImportResult into the JobLog Summary so the Logs viewer shows
	// the transcode outcome beside the import counts. All zero when no
	// converter is available (the UI falls back to placeholders, ADR-0014).
	MediaConverted int
	MediaSkipped   int
	MediaFailed    int
}

// ToolResolver resolves the exporter executable for one source. The desktop
// shell backs it with the bundled toolchain (verified, absolute Contents/
// Resources paths — never $PATH); `msgbrowse serve` backs it with a $PATH /
// config lookup; a build with no tool for a source returns ErrToolMissing so
// Enable is a clear "unavailable", never a silent no-op. The returned path is an
// explicit executable used verbatim as argv[0] — no shell, no PATH re-resolution
// inside the runner.
type ToolResolver interface {
	// ResolveTool returns the absolute executable path for src's exporter. A
	// source with no available tool returns ErrToolMissing (wrapped is fine —
	// callers match with errors.Is).
	ResolveTool(ctx context.Context, src string) (string, error)
}

// ToolResolverFunc adapts a plain func to ToolResolver (the common case: a
// closure over a resolver struct).
type ToolResolverFunc func(ctx context.Context, src string) (string, error)

// ResolveTool implements ToolResolver.
func (f ToolResolverFunc) ResolveTool(ctx context.Context, src string) (string, error) {
	return f(ctx, src)
}

// ExecRunner runs an exporter subprocess with an explicit argv and process
// environment, blocks until it exits, and returns the exporter's captured
// combined stdout+stderr. It is the seam that makes the whole pipeline testable
// without the real macOS exporters: tests inject a fake that writes a fixture
// archive into the staging dir and returns "" (or a scripted error + the stderr
// it "printed"). Production wires it to a real exec.CommandContext runner (in the
// CLI/desktop layer, which owns the os/exec dependency).
//
// The returned string is the exporter's combined output, BOUNDED by the runner
// (production caps it to a ring buffer — see onboardsvc.ExecRunner) so a chatty
// exporter cannot grow it without limit; it is captured whether the run
// succeeded or failed, so a non-zero exit's stderr — the diagnostic detail the
// Logs viewer surfaces (issue #151) — is preserved rather than discarded. It is
// TOOL output (argv echoes, progress, error text), never message content, and is
// never persisted to disk.
//
// name is the resolved absolute tool path and args are app-owned constants plus
// the app-computed staging path — never client input (SPEC-0013 §Security
// "Subprocess argument safety"). env is the subprocess environment: nil means
// "inherit the parent process environment" (exec.Cmd.Env semantics), which is
// the case for a native exporter; a bundled Python exporter is handed the
// relocation-corrected PYTHONHOME/PYTHONPATH env so it can find its stdlib after
// the .app is moved (issue #147). The context MUST be honored so a Cancel kills
// the subprocess.
type ExecRunner func(ctx context.Context, name string, env []string, args ...string) (output string, err error)

// EnvResolver is an OPTIONAL capability a ToolResolver may also implement to
// supply the process environment a source's exporter subprocess must run with.
// The desktop's bundled resolver implements it to hand back the corrected
// PYTHONHOME/PYTHONPATH env for the Python exporters (Signal, WhatsApp) and nil
// (inherit) for the native imessage-exporter (issue #147). A resolver that does
// not implement it (e.g. the $PATH/BYO resolver `msgbrowse serve` uses) causes
// the runner to inherit the environment — the correct behavior for tools on
// $PATH. toolPath is the path ResolveTool just returned, so the resolver can map
// it back to the specific tool without re-resolving.
type EnvResolver interface {
	EnvForTool(ctx context.Context, src, toolPath string) ([]string, error)
}

// ExportSource is the detected LIVE source location an exporter must read from,
// for the sources whose exporter reads a specific database + media directory
// rather than its own well-known application directory. It is resolved by the
// SourceResolver seam and threaded into ExportArgs.
//
// Signal and iMessage leave it zero: sigexport reads Signal Desktop's own
// application-support directory and imessage-exporter reads ~/Library/Messages/
// chat.db, so neither needs an explicit source path. WhatsApp REQUIRES it: the
// Mac app keeps its history in a live container database, and wtsexporter must
// be pointed at it in iOS mode (`-i -d <DB> -m <media dir>`) or argparse exits 2
// (issue #150). The paths are app-detected (internal/setup), never client input.
type ExportSource struct {
	// DBPath is the source's live database file (WhatsApp's ChatStorage.sqlite).
	DBPath string
	// MediaDir is the source's live media directory (WhatsApp's Message/Media),
	// handed to wtsexporter's `-m`.
	MediaDir string
}

// SourceResolver resolves the detected LIVE source location for a source whose
// exporter reads a specific database + media directory (WhatsApp). It is an
// OPTIONAL Runner seam: the desktop shell backs it with the real
// internal/setup.Detector; tests inject a fake with faked paths; a resolver that
// returns the zero ExportSource (or a nil resolver entirely) means "the exporter
// reads its own well-known directory" (Signal/iMessage). This keeps the pure
// onboard package off the detection internals while still threading the detected
// WhatsApp container DB + media path into the export invocation (issue #150).
type SourceResolver interface {
	// ResolveSource returns the detected live source location for src. A source
	// that needs no explicit path (Signal/iMessage) returns the zero value, nil.
	ResolveSource(ctx context.Context, src string) (ExportSource, error)
}

// SourceResolverFunc adapts a plain func to SourceResolver.
type SourceResolverFunc func(ctx context.Context, src string) (ExportSource, error)

// ResolveSource implements SourceResolver.
func (f SourceResolverFunc) ResolveSource(ctx context.Context, src string) (ExportSource, error) {
	return f(ctx, src)
}

// Importer imports an adopted managed archive root for a source into the store
// and returns the result. It is injected (rather than importing internal/ingest
// et al. directly) so the orchestration stays pure and off-store: the CLI /
// desktop layer, which holds the concrete *store.Store, supplies an Importer
// that dispatches to internal/ingest, internal/imessage, or internal/whatsapp by
// source. The archiveRoot is always the app-owned managed root — never a client
// path.
type Importer interface {
	// Import loads the source's managed archive root into the store incrementally
	// and returns what changed. A failure is wrapped by the runner as
	// ErrImportFailed.
	Import(ctx context.Context, src, archiveRoot string) (ImportResult, error)
}

// ImporterFunc adapts a plain func to Importer.
type ImporterFunc func(ctx context.Context, src, archiveRoot string) (ImportResult, error)

// Import implements Importer.
func (f ImporterFunc) Import(ctx context.Context, src, archiveRoot string) (ImportResult, error) {
	return f(ctx, src, archiveRoot)
}

// ExportArgs builds the exporter argv for a source, writing into the given
// destination directory. The destination is ALWAYS the app-computed staging dir
// (never the managed root directly, and never a client path): the exporter runs
// into staging and only a clean success is promoted. src is the detected LIVE
// source location (the WhatsApp container DB + media dir); it is the zero value
// for Signal/iMessage, whose exporters read their own well-known directories.
//
// The flags mirror internal/cli/export.go's proven command lines (iMessage
// always copy-mode `-c clone`; Signal writes into <dest>/export) plus the
// real-Mac WhatsApp iOS-mode invocation (issue #150): wtsexporter reads the live
// container database in iOS mode — `-i -d <ChatStorage.sqlite> -m <media dir>` —
// and writes JSON + copied media into <dest>. Without `-i -d`, wtsexporter's
// argparse rejects the invocation with exit 2, which was the whole WhatsApp
// Enable failure.
//
// This is the single place the argv is assembled, and it is assembled only from
// app-owned constants + the computed staging dir + the app-DETECTED source paths
// (internal/setup, never client input) — no request input reaches it (SPEC-0013
// §Security "Subprocess argument safety").
func ExportArgs(src, dest string, source_ ExportSource) ([]string, error) {
	switch src {
	case source.Signal:
		// sigexport <dest>/export → <dest>/export/<conversation>/chat.md, the
		// layout internal/ingest scans (ingest.ExportDir).
		return []string{signalExportSubdir(dest)}, nil
	case source.IMessage:
		// imessage-exporter -f txt -c clone -o <dest>: copy-mode is mandatory so
		// attachments are cloned into the archive, not left as absolute ~/Library
		// references (the non-copy-mode trap doctor diagnoses).
		return []string{"-f", "txt", "-c", "clone", "-o", dest}, nil
	case source.WhatsApp:
		// wtsexporter iOS mode against the live container DB (issue #150). The Mac
		// app has no on-disk backup for the CLI's `-b` path to read; its history is
		// the live ChatStorage.sqlite in the group container, so the export MUST run
		// `-i -d <DB> -m <media dir>`. `-o <dest>` + `-j <dest>/result.json` land the
		// copied media and the JSON under <dest> (the layout internal/whatsapp
		// scans); `--no-html` skips the HTML render the app never reads.
		if source_.DBPath == "" {
			// A WhatsApp Enable with no detected container DB cannot run iOS mode —
			// surface a clear error rather than invoking wtsexporter without `-d`
			// (which exits 2). The card is only actionable when Detected, so this is
			// a guard, not the common path.
			return nil, fmt.Errorf("%w: whatsapp: no ChatStorage.sqlite detected to export", ErrExportFailed)
		}
		args := []string{"-i", "-d", source_.DBPath}
		if source_.MediaDir != "" {
			args = append(args, "-m", source_.MediaDir)
		}
		args = append(args, "-o", dest, "-j", whatsappResultFile(dest), "--no-html")
		return args, nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownSource, src)
	}
}
