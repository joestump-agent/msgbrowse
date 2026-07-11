// Package ingest scans a signal-export archive and populates the SQLite store.
//
// It is incremental and idempotent: per-conversation file metadata (mtime, size,
// content hash) is recorded in ingest_state, and a conversation is re-parsed only
// when its chat.md actually changes. Re-running ingest over an unchanged archive
// is a cheap no-op. The archive is only ever read.
//
// This package is the Signal-side importer; the equivalent iMessage importer
// will live in a sibling internal/imessage package (Slice 2.5). Both write to
// the same store and tag every row with their source so the unified contacts
// and journal layers can blend them.
package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// ErrExportDirNotFound is returned when archive_root has no export/ subdirectory.
// It is a sentinel so the CLI can attach an actionable hint (the most common
// misconfiguration is pointing archive_root at the wrong level).
var ErrExportDirNotFound = errors.New("export directory not found")

// ExportDir is the archive subdirectory holding per-conversation folders.
const ExportDir = "export"

// ChatFile is the conversation transcript filename within each conversation folder.
const ChatFile = "chat.md"

// Options configures an ingest run.
type Options struct {
	// ArchiveRoot is the signal-export archive root (read-only).
	ArchiveRoot string
	// Full forces every conversation to be re-parsed, ignoring incremental state.
	Full bool
	// Now supplies the current time (snapshot tier classification, timestamps).
	// Defaults to time.Now when nil.
	Now func() time.Time
	// Logger receives progress and per-line skip warnings. Defaults to the slog
	// default logger when nil.
	Logger *slog.Logger
}

// Run performs one ingest pass against st and returns the recorded summary. It
// scans conversations and snapshots, writing only what changed. Individual
// malformed lines and transient per-conversation errors are logged and counted,
// never fatal; only unrecoverable store errors abort the run.
func Run(ctx context.Context, st *store.Store, opts Options) (store.IngestRun, error) {
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	start := now()

	run := store.IngestRun{Source: source.Signal, StartedAt: start}
	log.Info("importing signal archive", "archive", opts.ArchiveRoot, "full", opts.Full)

	// 1. Conversations.
	convRoot := filepath.Join(opts.ArchiveRoot, ExportDir)
	entries, err := os.ReadDir(convRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return run, fmt.Errorf("%w at %s", ErrExportDirNotFound, convRoot)
		}
		return run, fmt.Errorf("read export dir: %w", err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		chatPath := filepath.Join(convRoot, name, ChatFile)
		info, statErr := os.Stat(chatPath)
		if statErr != nil {
			// A conversation folder without a chat.md is simply skipped.
			continue
		}
		run.ConversationsScanned++

		changed, added, skipped, cerr := ingestConversation(ctx, st, opts, log, name, chatPath, info, now())
		if cerr != nil {
			// Log and continue: one bad conversation must not abort the run.
			log.Error("conversation ingest failed", "conversation", name, "error", cerr)
			run.Errors++
			continue
		}
		run.SkippedLines += skipped
		if changed {
			run.ConversationsChanged++
			run.MessagesAdded += added
			log.Info("imported conversation", "conversation", name, "messages", added, "skipped_lines", skipped)
		}
	}

	// Total message count after the pass.
	total, err := st.CountMessages(ctx)
	if err != nil {
		return run, err
	}
	run.MessagesTotal = total

	// 2. Snapshots inventory.
	snaps, err := scanSnapshots(opts.ArchiveRoot, now())
	if err != nil {
		log.Error("snapshot scan failed", "error", err)
		run.Errors++
	} else {
		if err := st.ReplaceSnapshots(ctx, snaps); err != nil {
			return run, err
		}
		run.SnapshotsSeen = len(snaps)
	}

	// 3. Finalize and record the run.
	run.FinishedAt = now()
	run.DurationMS = run.FinishedAt.Sub(run.StartedAt).Milliseconds()
	if _, err := st.RecordIngestRun(ctx, run); err != nil {
		return run, err
	}

	// Re-apply durable contact merge/split decisions so a re-ingested identity
	// folds straight back onto its person (ADR-0022 / SPEC-0015). Reconcile is
	// idempotent and local-only; the nil resolver means "no address book",
	// which is all reconcile needs — stored decisions re-apply without it, and
	// address-book hints never auto-merge.
	// Folding is best-effort: the import is already committed and hash-idempotent,
	// and reconcile re-runs on the next import, so a reconcile failure is logged
	// rather than failing an otherwise-successful (and needlessly retried) import.
	if err := st.ReconcileContacts(ctx, nil); err != nil {
		log.Error("contact reconcile failed (import committed; will retry next run)", "error", err)
		run.Errors++
	}

	log.Info("ingest complete",
		"scanned", run.ConversationsScanned,
		"changed", run.ConversationsChanged,
		"messages_total", run.MessagesTotal,
		"messages_added", run.MessagesAdded,
		"snapshots", run.SnapshotsSeen,
		"skipped_lines", run.SkippedLines,
		"errors", run.Errors,
		"duration_ms", run.DurationMS,
	)
	return run, nil
}

// ingestConversation ingests a single conversation if its chat.md changed since
// the last run (or opts.Full is set). It returns whether the conversation was
// (re)parsed, how many messages were written, and how many malformed lines were
// skipped.
func ingestConversation(
	ctx context.Context, st *store.Store, opts Options, log *slog.Logger,
	name, chatPath string, info os.FileInfo, at time.Time,
) (changed bool, added, skipped int, err error) {
	convID, err := st.UpsertConversation(ctx, source.Signal, name)
	if err != nil {
		return false, 0, 0, err
	}

	prev, err := st.GetIngestState(ctx, convID)
	if err != nil {
		return false, 0, 0, err
	}

	mtime := info.ModTime().Unix()
	size := info.Size()

	// Fast path: unchanged (mtime, size) means unchanged content; skip without
	// hashing. This is a deliberate optimization over the strict "always hash"
	// reading of the brief — real-world edits to a chat.md change at least the
	// mtime and almost always the size. A pathological edit that preserves both
	// (e.g. an in-place byte swap of equal length) will not be re-ingested
	// until `ingest --full` rescues it; that escape hatch is the contract for
	// any caller who suspects out-of-band edits.
	if !opts.Full && prev != nil && prev.MTimeUnix == mtime && prev.SizeBytes == size {
		return false, 0, 0, nil
	}

	contentHash, err := hashFile(chatPath)
	if err != nil {
		return false, 0, 0, err
	}
	// Metadata changed but content is identical (e.g. a touch): refresh state only.
	if !opts.Full && prev != nil && prev.ContentHash == contentHash {
		prev.MTimeUnix = mtime
		prev.SizeBytes = size
		prev.LastIngestedAt = at
		if serr := st.SetIngestState(ctx, *prev); serr != nil {
			return false, 0, 0, serr
		}
		return false, 0, 0, nil
	}

	// Parse and replace.
	msgs, skips, perr := parseChatFile(name, chatPath, log)
	if perr != nil {
		return false, 0, 0, perr
	}
	added, err = st.ReplaceConversationMessages(ctx, convID, source.Signal, msgs)
	if err != nil {
		return false, 0, 0, err
	}
	if err = st.SetIngestState(ctx, store.IngestState{
		ConversationID: convID,
		RelPath:        filepath.Join(ExportDir, name, ChatFile),
		MTimeUnix:      mtime,
		SizeBytes:      size,
		ContentHash:    contentHash,
		MessageCount:   added,
		LastIngestedAt: at,
	}); err != nil {
		return false, 0, 0, err
	}
	return true, added, len(skips), nil
}

// parseChatFile streams chat.md and returns its parsed messages plus any skipped
// (malformed) lines. Malformed lines are logged at warn level.
func parseChatFile(name, chatPath string, log *slog.Logger) ([]signal.Message, []signal.ParseError, error) {
	f, err := os.Open(chatPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open chat.md: %w", err)
	}
	defer f.Close()

	var (
		msgs  []signal.Message
		skips []signal.ParseError
	)
	err = signal.Parse(name, f,
		func(m signal.Message) error { msgs = append(msgs, m); return nil },
		func(e signal.ParseError) {
			skips = append(skips, e)
			log.Warn("skipped malformed line", "conversation", name, "line", e.Line, "error", e.Err)
		},
	)
	if err != nil {
		return nil, nil, err
	}
	return msgs, skips, nil
}

// hashFile returns the hex SHA-256 of a file's contents, read in a streaming
// fashion so large transcripts do not load fully into memory.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
