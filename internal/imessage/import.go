package imessage

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
	"strings"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// ErrArchiveNotFound is returned when the iMessage archive directory is missing.
// It is a sentinel so the CLI can attach an actionable hint.
var ErrArchiveNotFound = errors.New("imessage archive directory not found")

// Options configures an iMessage import run.
type Options struct {
	// ArchiveRoot is the imessage-exporter output directory: a flat set of
	// <ChatName>.txt files plus an attachments/ folder. Read-only.
	ArchiveRoot string
	// Full forces every conversation to be re-parsed, ignoring incremental state.
	Full bool
	// Now supplies the current time; defaults to time.Now.
	Now func() time.Time
	// Logger receives progress; defaults to slog.Default().
	Logger *slog.Logger
}

// Run imports the iMessage archive into st and returns the recorded summary.
// It mirrors the Signal importer's incremental, idempotent contract: each
// <ChatName>.txt is a conversation, re-parsed only when its file changes, and
// messages are replaced atomically. Every row is tagged source="imessage".
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
	run := store.IngestRun{Source: source.IMessage, StartedAt: start}
	log.Info("importing imessage archive", "archive", opts.ArchiveRoot, "full", opts.Full)

	entries, err := os.ReadDir(opts.ArchiveRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return run, fmt.Errorf("%w at %s", ErrArchiveNotFound, opts.ArchiveRoot)
		}
		return run, fmt.Errorf("read imessage archive: %w", err)
	}

	// Diagnose the common "0 imported" case up front: the importer only reads
	// top-level <ChatName>.txt files (imessage-exporter `-f txt` output). If there
	// are none, say so loudly with what we DID see, so a nested or wrong-format
	// archive doesn't silently look like a successful no-op.
	var txtCount, dirCount int
	for _, e := range entries {
		switch {
		case e.IsDir():
			dirCount++
		case strings.HasSuffix(e.Name(), ".txt"):
			txtCount++
		}
	}
	if txtCount == 0 {
		log.Warn("no <ChatName>.txt files found at the top level of the imessage archive — "+
			"point --imessage-archive-root at the directory imessage-exporter wrote with `-f txt` "+
			"(flat .txt files; nested subfolders are not scanned)",
			"archive", opts.ArchiveRoot, "entries", len(entries), "subdirs", dirCount)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".txt")
		path := filepath.Join(opts.ArchiveRoot, e.Name())
		info, statErr := os.Stat(path)
		if statErr != nil {
			continue
		}
		run.ConversationsScanned++

		changed, added, skipped, cerr := importConversation(ctx, st, opts, log, name, path, info, now())
		if cerr != nil {
			log.Error("imessage conversation import failed", "conversation", name, "error", cerr)
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

	total, err := st.CountMessages(ctx)
	if err != nil {
		return run, err
	}
	run.MessagesTotal = total

	run.FinishedAt = now()
	run.DurationMS = run.FinishedAt.Sub(run.StartedAt).Milliseconds()
	if _, err := st.RecordIngestRun(ctx, run); err != nil {
		return run, err
	}
	// Fold re-imported identities back onto their merged person (ADR-0022 /
	// SPEC-0015). Idempotent, local-only; nil resolver = no address book.
	if err := st.ReconcileContacts(ctx, nil); err != nil {
		return run, err
	}
	log.Info("imessage import complete",
		"scanned", run.ConversationsScanned, "changed", run.ConversationsChanged,
		"messages_added", run.MessagesAdded, "skipped_lines", run.SkippedLines,
		"errors", run.Errors, "duration_ms", run.DurationMS)
	return run, nil
}

// importConversation imports one .txt file if it changed since the last run.
func importConversation(
	ctx context.Context, st *store.Store, opts Options, log *slog.Logger,
	name, path string, info os.FileInfo, at time.Time,
) (changed bool, added, skipped int, err error) {
	convID, err := st.UpsertConversation(ctx, source.IMessage, name)
	if err != nil {
		return false, 0, 0, err
	}
	prev, err := st.GetIngestState(ctx, convID)
	if err != nil {
		return false, 0, 0, err
	}
	mtime, size := info.ModTime().Unix(), info.Size()

	// Fast path: unchanged (mtime, size) → skip. `--full` forces a rescan.
	if !opts.Full && prev != nil && prev.MTimeUnix == mtime && prev.SizeBytes == size {
		return false, 0, 0, nil
	}
	contentHash, err := hashFile(path)
	if err != nil {
		return false, 0, 0, err
	}
	if !opts.Full && prev != nil && prev.ContentHash == contentHash {
		prev.MTimeUnix, prev.SizeBytes, prev.LastIngestedAt = mtime, size, at
		if serr := st.SetIngestState(ctx, *prev); serr != nil {
			return false, 0, 0, serr
		}
		return false, 0, 0, nil
	}

	msgs, skips, perr := parseFile(name, path, log)
	if perr != nil {
		return false, 0, 0, perr
	}
	added, err = st.ReplaceConversationMessages(ctx, convID, source.IMessage, msgs)
	if err != nil {
		return false, 0, 0, err
	}
	if err = st.SetIngestState(ctx, store.IngestState{
		ConversationID: convID,
		RelPath:        name + ".txt",
		MTimeUnix:      mtime,
		SizeBytes:      size,
		ContentHash:    contentHash,
		MessageCount:   added,
		LastIngestedAt: at,
	}); err != nil {
		return false, 0, 0, err
	}
	return true, added, skips, nil
}

// parseFile streams a .txt file and returns its parsed messages plus a count of
// skipped (pre-timestamp) lines.
func parseFile(name, path string, log *slog.Logger) ([]signal.Message, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	var (
		msgs    []signal.Message
		skipped int
	)
	err = Parse(name, f,
		func(m signal.Message) error { msgs = append(msgs, m); return nil },
		func(line int, text string) {
			skipped++
			log.Warn("skipped pre-timestamp line", "conversation", name, "line", line)
		},
	)
	return msgs, skipped, err
}

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
