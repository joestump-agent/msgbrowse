package whatsapp

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

// ErrArchiveNotFound is returned when the WhatsApp archive root or its
// result.json is missing. It is a sentinel so the CLI can attach an
// actionable hint (run whatsapp-chat-exporter with --json into the root).
var ErrArchiveNotFound = errors.New("whatsapp archive not found")

// Options configures a WhatsApp import run.
type Options struct {
	// ArchiveRoot is the WhatsApp-Chat-Exporter output directory containing
	// result.json plus any media directories the tool copied. Read-only.
	ArchiveRoot string
	// Full forces every conversation to be re-written, ignoring incremental state.
	Full bool
	// Now supplies the current time; defaults to time.Now.
	Now func() time.Time
	// Logger receives progress; defaults to slog.Default().
	Logger *slog.Logger
	// Location renders epoch timestamps as wall-clock strings; defaults to
	// time.Local (see Options in parser.go).
	Location *time.Location
}

// Run imports the WhatsApp archive into st and returns the recorded summary.
// It mirrors the Signal/iMessage importers' incremental, idempotent contract:
// the whole export lives in one result.json, so the file is parsed once per
// run and each chat is replaced only when its parsed content changed since
// the last run. Every row is tagged source="whatsapp".
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
	run := store.IngestRun{Source: source.WhatsApp, StartedAt: start}
	log.Info("importing whatsapp archive", "archive", opts.ArchiveRoot, "full", opts.Full)

	path := filepath.Join(opts.ArchiveRoot, ResultFile)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return run, fmt.Errorf("%w: no %s at %s (run whatsapp-chat-exporter with --json)",
				ErrArchiveNotFound, ResultFile, opts.ArchiveRoot)
		}
		return run, fmt.Errorf("open whatsapp export: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return run, err
	}
	mtime, size := info.ModTime().Unix(), info.Size()

	convs, skips, err := ParseAll(f, ParseOptions{ArchiveRoot: opts.ArchiveRoot, Location: opts.Location})
	if err != nil {
		return run, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, e := range skips {
		log.Warn("skipped malformed whatsapp entry", "chat", e.Chat, "message", e.MessageID, "error", e.Err)
	}
	run.SkippedLines = len(skips)

	for _, conv := range convs {
		run.ConversationsScanned++
		changed, added, cerr := importConversation(ctx, st, opts, conv, mtime, size, now())
		if cerr != nil {
			log.Error("whatsapp conversation import failed", "conversation", conv.Name, "error", cerr)
			run.Errors++
			continue
		}
		if changed {
			run.ConversationsChanged++
			run.MessagesAdded += added
			log.Info("imported conversation", "conversation", conv.Name, "messages", added)
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
	log.Info("whatsapp import complete",
		"scanned", run.ConversationsScanned, "changed", run.ConversationsChanged,
		"messages_added", run.MessagesAdded, "skipped_entries", run.SkippedLines,
		"errors", run.Errors, "duration_ms", run.DurationMS)
	return run, nil
}

// importConversation replaces one chat's rows if its parsed content changed.
// All chats share one source file, so the per-conversation change check is a
// content hash over the parsed messages (bodies, attachments, reactions)
// rather than per-file mtime/size — result.json's mtime/size are recorded for
// observability only.
func importConversation(
	ctx context.Context, st *store.Store, opts Options,
	conv Conversation, mtime, size int64, at time.Time,
) (changed bool, added int, err error) {
	convID, err := st.UpsertConversation(ctx, source.WhatsApp, conv.Name)
	if err != nil {
		return false, 0, err
	}
	prev, err := st.GetIngestState(ctx, convID)
	if err != nil {
		return false, 0, err
	}
	contentHash := conversationHash(conv.Messages)
	if !opts.Full && prev != nil && prev.ContentHash == contentHash {
		return false, 0, nil
	}

	added, err = st.ReplaceConversationMessages(ctx, convID, source.WhatsApp, conv.Messages)
	if err != nil {
		return false, 0, err
	}
	if err = st.SetIngestState(ctx, store.IngestState{
		ConversationID: convID,
		RelPath:        ResultFile + "#" + conv.JID,
		MTimeUnix:      mtime,
		SizeBytes:      size,
		ContentHash:    contentHash,
		MessageCount:   added,
		LastIngestedAt: at,
	}); err != nil {
		return false, 0, err
	}
	return true, added, nil
}

// conversationHash fingerprints a chat's parsed content. The per-message
// content hash covers (conversation, ts, sender, body, seq); attachments,
// links, and reactions are folded in explicitly so e.g. a reaction added to
// an old message still marks the chat changed.
func conversationHash(msgs []signal.Message) string {
	h := sha256.New()
	for i := range msgs {
		m := &msgs[i]
		_, _ = io.WriteString(h, m.ID())
		for _, a := range m.Attachments {
			_, _ = io.WriteString(h, "\x00a"+string(a.Kind)+"\x00"+a.RelPath+"\x00"+a.OriginalName)
		}
		for _, l := range m.Links {
			_, _ = io.WriteString(h, "\x00l"+l.URL)
		}
		for _, r := range m.Reactions {
			_, _ = io.WriteString(h, "\x00r"+r.Emoji+"\x00"+r.Actor)
		}
		_, _ = io.WriteString(h, "\x00\n")
	}
	return hex.EncodeToString(h.Sum(nil))
}
