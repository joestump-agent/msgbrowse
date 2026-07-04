// Package onboardsvc wires the pure internal/onboard orchestration to the
// concrete side effects the running app owns — the SQLite store, the real
// exporter subprocess, and the source-appropriate importers — and exposes it as
// a web.Enabler. It is the single place both `msgbrowse serve` and the desktop
// shell construct the Setup Enable runner, so the two modes drive the identical
// export→adopt→import pipeline (SPEC-0013) and differ only in the injected
// ToolResolver (bundled toolchain in the .app; $PATH/config in serve).
//
// It is pure Go with no cgo and no Wails import, so it lives in internal/ and is
// importable by both the CLI (internal/cli) and the desktop embedded server
// (cmd/msgbrowse-desktop/internal/embedded).
//
// Governing: ADR-0020 (the export jobs drive the exporter into the managed
// archives, then the existing importer loads the store), SPEC-0013 REQ
// "One-click enable and import per source" (reuse the existing export
// orchestration + incremental import — neither reimplemented in the UI layer),
// §Security "Subprocess argument safety".
package onboardsvc

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/imageconv"
	"github.com/joestump/msgbrowse/internal/imessage"
	"github.com/joestump/msgbrowse/internal/ingest"
	"github.com/joestump/msgbrowse/internal/onboard"
	"github.com/joestump/msgbrowse/internal/setup"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
	"github.com/joestump/msgbrowse/internal/whatsapp"
)

// Build constructs the onboard.Runner that backs the web Setup Enable flow,
// wiring the pure orchestration to the concrete store, the real exporter
// subprocess runner, and the source-dispatching Importer. resolver decides where
// exporters come from: a bundled resolver (desktop .app) or a $PATH/config one
// (serve). A nil resolver leaves Enable disabled — the runner reports
// ErrToolMissing for every source, and the web layer renders the "unavailable"
// affordance.
//
// The returned *onboard.Runner satisfies web.Enabler directly; wire it with
// (*web.Server).SetEnabler and call Shutdown as part of graceful shutdown so no
// exporter subprocess outlives the app (SPEC-0013 REQ "Concurrency Safety").
func Build(cfg *config.Config, st *store.Store, resolver onboard.ToolResolver, log *slog.Logger) (*onboard.Runner, error) {
	if log == nil {
		log = slog.Default()
	}
	return onboard.NewRunner(onboard.Config{
		Resolver: resolver,
		Exec:     ExecRunner,
		Importer: &storeImporter{st: st, cfg: cfg, log: log},
		// The detected live-source resolver threads WhatsApp's container DB + media
		// dir into wtsexporter's iOS-mode argv (issue #150). It is HOME-rooted like
		// the /setup detector, so serve and desktop resolve the same paths; on a
		// non-macOS box it detects nothing and WhatsApp Enable reports "not
		// detected" as before.
		Sources: DetectSourceResolver{det: setup.NewDetector()},
		DataDir: cfg.DataDir,
		Logger:  log,
	})
}

// DetectSourceResolver resolves the detected live source location (WhatsApp
// container DB + media dir) via internal/setup detection, satisfying
// onboard.SourceResolver. It is the production wiring for issue #150: the runner
// hands wtsexporter the real ChatStorage.sqlite + Message/Media paths so its
// iOS-mode export runs instead of exiting 2. Signal/iMessage read their own
// well-known directories, so they resolve to the zero ExportSource.
type DetectSourceResolver struct {
	det setup.Detector
}

// ResolveSource implements onboard.SourceResolver. Only WhatsApp needs an
// explicit path; every other source returns the zero value.
func (d DetectSourceResolver) ResolveSource(_ context.Context, src string) (onboard.ExportSource, error) {
	if src != source.WhatsApp {
		return onboard.ExportSource{}, nil
	}
	det := d.det.DetectWhatsApp()
	if det.State != setup.Detected {
		// Not detected: return the zero value; ExportArgs then fails with a clear
		// "no ChatStorage.sqlite detected" export error rather than an opaque exit 2.
		return onboard.ExportSource{}, nil
	}
	return onboard.ExportSource{DBPath: det.Path, MediaDir: det.MediaPath}, nil
}

// exporterLogCap bounds how many bytes of an exporter's combined stdout+stderr
// the Enable/Refresh job retains for the Logs viewer (issue #151). A ring buffer
// keeps only the TAIL — the last N bytes — because that is where an exporter's
// fatal error prints (the WhatsApp argparse exit-2 message, a locked-DB error).
// It is TOOL output only (argv echoes, progress, error text), never message
// content, and lives only in memory. 32 KiB is generous for a diagnostic tail
// while capping a chatty exporter's memory footprint.
const exporterLogCap = 32 << 10 // 32 KiB

// ExecRunner spawns an exporter subprocess with an explicit argv and process
// environment, streaming its stdout/stderr to the process's stdio (these
// exporters print useful progress) AND capturing a bounded tail of the combined
// output for the Logs viewer (issue #151). The context is honored so a Cancel or
// app shutdown terminates the child. name is a resolved absolute tool path and
// args are app-owned constants plus the app-computed staging dir — never client
// input (SPEC-0013 §Security "Subprocess argument safety").
//
// The captured output is returned whether the run succeeded or failed, so a
// non-zero exit's stderr is preserved for diagnosis rather than discarded (the
// old behavior surfaced only "exit status N"). It is bounded to exporterLogCap
// by a ring buffer, and it is TOOL output only — never persisted to disk, never
// message content.
//
// env is the subprocess environment: nil means "inherit the parent process
// environment" (exec.Cmd.Env semantics) — the case for a native exporter or a
// $PATH/BYO tool — while a bundled Python exporter is handed the
// relocation-corrected PYTHONHOME/PYTHONPATH env so the bundled interpreter finds
// its stdlib after the .app has moved (issue #147). Leaving cmd.Env nil rather
// than assigning os.Environ() preserves the exact "inherit" behavior serve has
// always had for BYO exporters.
func ExecRunner(ctx context.Context, name string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env // nil => inherit; non-nil => the corrected bundled-Python env
	// Tee each stream to the operator's stdio AND a shared bounded ring buffer, so
	// progress still shows on the console while the tail is captured for the Logs
	// view. Stdout and stderr are interleaved into one buffer (combined output);
	// the ring is mutex-guarded because the two streams write concurrently.
	ring := newRingBuffer(exporterLogCap)
	cmd.Stdout = io.MultiWriter(os.Stdout, ring)
	cmd.Stderr = io.MultiWriter(os.Stderr, ring)
	err := cmd.Run()
	return ring.String(), err
}

// ringBuffer is a fixed-capacity byte sink that keeps only the LAST cap bytes
// written to it — the tail where an exporter's fatal error lands. It is safe for
// the concurrent stdout+stderr writers exec.Cmd spawns.
type ringBuffer struct {
	mu  sync.Mutex
	buf []byte
	cap int
}

func newRingBuffer(capBytes int) *ringBuffer {
	return &ringBuffer{cap: capBytes}
}

// Write appends p, retaining only the last cap bytes. It never errors and always
// reports the full length written (an io.Writer that silently drops the head).
func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(p)
	if n >= r.cap {
		// This write alone overflows the ring: keep only its own tail.
		r.buf = append(r.buf[:0], p[n-r.cap:]...)
		return n, nil
	}
	if len(r.buf)+n > r.cap {
		// Drop enough of the existing head to make room for the new bytes.
		drop := len(r.buf) + n - r.cap
		r.buf = r.buf[drop:]
	}
	r.buf = append(r.buf, p...)
	return n, nil
}

// String returns the retained tail as a string.
func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}

// storeImporter imports an adopted managed archive root into the store by
// dispatching to the source-appropriate importer — the same import code
// `msgbrowse import` runs, never reimplemented in the UI layer (SPEC-0013 REQ
// "the import MUST reuse the existing incremental ingest").
type storeImporter struct {
	st  *store.Store
	cfg *config.Config
	log *slog.Logger
}

// Import loads root into the store for src and returns the onboard-shaped result,
// then runs a best-effort media transcode (non-fatal, like `msgbrowse import`).
func (im *storeImporter) Import(ctx context.Context, src, root string) (onboard.ImportResult, error) {
	var run store.IngestRun
	var err error
	switch src {
	case source.Signal:
		run, err = ingest.Run(ctx, im.st, ingest.Options{ArchiveRoot: root})
	case source.IMessage:
		run, err = imessage.Run(ctx, im.st, imessage.Options{ArchiveRoot: root})
	case source.WhatsApp:
		run, err = whatsapp.Run(ctx, im.st, whatsapp.Options{ArchiveRoot: root})
	default:
		return onboard.ImportResult{}, fmt.Errorf("onboardsvc: unknown source %q", src)
	}
	if err != nil {
		return onboard.ImportResult{}, err
	}

	// Best-effort media transcode scoped to the source's managed root, so an
	// Enable/Refresh only transcodes what it just imported — the same step
	// `msgbrowse import` runs after loading the store (issue #160: without it,
	// desktop-onboarded HEICs never got derivatives and the gallery showed
	// placeholders forever). A missing converter falls back to gallery
	// placeholders (not an error). The summary rides the ImportResult into the
	// job's JobLog so the Logs viewer shows the transcode outcome (issue #151's
	// surface), and is logged here for the serve/desktop console.
	opts := imageconv.Options{DataDir: im.cfg.DataDir, Logger: im.log}
	switch src {
	case source.Signal:
		opts.ArchiveRoot = root
	case source.IMessage:
		opts.IMessageArchiveRoot = root
	case source.WhatsApp:
		opts.WhatsAppArchiveRoot = root
	}
	res := onboard.ImportResult{
		ConversationsChanged: run.ConversationsChanged,
		MessagesAdded:        run.MessagesAdded,
		MessagesTotal:        run.MessagesTotal,
	}
	if msum, cerr := imageconv.Run(ctx, im.st, opts); cerr != nil {
		im.log.Warn("onboardsvc: image transcode step failed; gallery may show placeholders", "source", src, "error", cerr)
	} else if !msum.NoConverter {
		res.MediaConverted = msum.Converted
		res.MediaSkipped = msum.Skipped
		res.MediaFailed = msum.Failed
		im.log.Info("onboardsvc: image transcode complete", "source", src,
			"converted", msum.Converted, "cached", msum.Skipped, "failed", msum.Failed)
	}

	return res, nil
}

// Default console command names looked up on $PATH when no override is given,
// mirroring internal/cli/export.go (the console script `sigexport`, not the pip
// package name; likewise `wtsexporter`).
const (
	defaultSignalExportBin     = "sigexport"
	defaultIMessageExporterBin = "imessage-exporter"
	defaultWhatsAppExporterBin = "wtsexporter"
)

// PathToolResolver resolves each source's exporter from an explicit override or
// $PATH — the bring-your-own-exporter path `msgbrowse serve` uses (ADR-0020:
// only the .app bundles; the CLI is unchanged). It returns onboard.ErrToolMissing
// when a source's tool is neither configured nor on PATH, so Enable is a clear
// "unavailable" rather than a silent no-op.
type PathToolResolver struct {
	SignalBin   string // explicit override; empty falls back to the default name on PATH
	IMessageBin string
	WhatsAppBin string
}

// PathResolverFromConfig builds a PathToolResolver from the exporter-bin config
// keys `msgbrowse export` already reads, so serve and the CLI resolve the same
// tools.
func PathResolverFromConfig(cfg *config.Config) PathToolResolver {
	return PathToolResolver{
		SignalBin:   cfg.SignalExportBin,
		IMessageBin: cfg.IMessageExporterBin,
		WhatsAppBin: cfg.WhatsAppExporterBin,
	}
}

// ResolveTool implements onboard.ToolResolver.
func (r PathToolResolver) ResolveTool(_ context.Context, src string) (string, error) {
	var override, defaultName string
	switch src {
	case source.Signal:
		override, defaultName = r.SignalBin, defaultSignalExportBin
	case source.IMessage:
		override, defaultName = r.IMessageBin, defaultIMessageExporterBin
	case source.WhatsApp:
		override, defaultName = r.WhatsAppBin, defaultWhatsAppExporterBin
	default:
		return "", fmt.Errorf("%w: %q", onboard.ErrUnknownSource, src)
	}
	if override != "" {
		return override, nil
	}
	path, err := exec.LookPath(defaultName)
	if err != nil {
		return "", fmt.Errorf("%w: %s not found on PATH", onboard.ErrToolMissing, defaultName)
	}
	return path, nil
}
