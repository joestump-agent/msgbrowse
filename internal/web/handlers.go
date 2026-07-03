package web

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/joestump/msgbrowse/internal/store"
)

// baseData is embedded in every full-page view; it drives the chrome (navbar +
// sidebar). It carries the global counts the navbar shows (REQ-0006-002) and the
// full conversation list the sidebar renders (REQ-0006-003).
type baseData struct {
	Title         string
	Conversations []store.ConversationSummary
	ActiveID      int64
	TotalMessages int // global message count for the navbar
}

// PinnedConversations are the conversations the sidebar renders in its PINNED
// section (REQ-0006-010), preserving Conversations' most-recent-first order.
func (b baseData) PinnedConversations() []store.ConversationSummary {
	out := make([]store.ConversationSummary, 0)
	for _, c := range b.Conversations {
		if c.Pinned {
			out = append(out, c)
		}
	}
	return out
}

// UnpinnedConversations are the rest, shown under the CONVERSATIONS section.
func (b baseData) UnpinnedConversations() []store.ConversationSummary {
	out := make([]store.ConversationSummary, 0)
	for _, c := range b.Conversations {
		if !c.Pinned {
			out = append(out, c)
		}
	}
	return out
}

// baseData loads the shell context shared by every full-page view: the
// conversation list (sidebar) and the global message count (navbar). activeID is
// the currently-open conversation (0 when none), used to mark the selected row.
//
// The navbar total is summed from the listing's per-conversation counts instead
// of a standalone COUNT(*) over all messages — that global aggregate measured
// 133ms per render on the reference archive for a number the sidebar query
// already knows (SPEC-0008 REQ-0008-004).
func (s *Server) baseData(ctx context.Context, title string, activeID int64) (baseData, error) {
	convs, err := s.store.ListConversations(ctx)
	if err != nil {
		return baseData{}, err
	}
	total := 0
	for i := range convs {
		total += convs[i].MessageCount
	}
	return baseData{
		Title:         title,
		Conversations: convs,
		ActiveID:      activeID,
		TotalMessages: total,
	}, nil
}

// messageListData drives the transcript message list and its infinite-scroll
// sentinel (used both in the full page and the HTMX partial).
type messageListData struct {
	ActiveID    int64
	Source      string // active conversation's source (for media renderability checks)
	ConvName    string // active conversation's name (for media path resolution)
	Messages    []store.MessageView
	HasMore     bool
	NextTSUnix  int64
	NextID      int64
	HighlightID int64 // marks the jump-to-context target message (0 = none)
}

type indexData struct {
	baseData
	NewestTS   string
	HasArchive bool
}

type conversationData struct {
	baseData
	Active *store.ConversationSummary
	List   messageListData
}

type statusData struct {
	baseData
	Run               *store.IngestRun
	Snapshots         []store.Snapshot
	NewestTS          string
	SnapshotFootprint int64
}

// pageSize is the number of messages per transcript page.
const pageSize = 50

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	base, err := s.baseData(ctx, "msgbrowse", 0)
	if err != nil {
		s.serverError(w, err)
		return
	}
	newest, err := s.store.NewestMessageTS(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, "index", indexData{
		baseData:   base,
		NewestTS:   newest,
		HasArchive: len(base.Conversations) > 0,
	})
}

func (s *Server) handleConversation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	active, err := s.store.GetConversationByID(ctx, id)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if active == nil {
		http.NotFound(w, r)
		return
	}
	base, err := s.baseData(ctx, active.Name+" · msgbrowse", id)
	if err != nil {
		s.serverError(w, err)
		return
	}
	page, err := s.store.GetMessages(ctx, id, 0, 0, pageSize)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, "conversation", conversationData{
		baseData: base,
		Active:   active,
		List: messageListData{
			ActiveID:   id,
			Source:     active.Source,
			ConvName:   active.Name,
			Messages:   page.Messages,
			HasMore:    page.HasMore,
			NextTSUnix: page.NextTSUnix,
			NextID:     page.NextID,
		},
	})
}

// handlePin toggles a conversation's pinned flag and redirects back to it
// (REQ-0006-010). It is a plain form POST + 303 redirect — CSP-clean (no inline
// JS; form-action 'self' already permits the POST) and idempotent enough for the
// back button. The toggle is a single direct UPDATE in the store — no summary
// fetch first (SPEC-0008 REQ-0008-005); a missing conversation surfaces as the
// UPDATE matching no row.
func (s *Server) handlePin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	found, err := s.store.TogglePinned(ctx, id)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/c/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	afterTS, _ := strconv.ParseInt(r.URL.Query().Get("after_ts"), 10, 64)
	afterID, _ := strconv.ParseInt(r.URL.Query().Get("after_id"), 10, 64)
	page, err := s.store.GetMessages(ctx, id, afterTS, afterID, pageSize)
	if err != nil {
		s.serverError(w, err)
		return
	}
	// The conversation's source/name drive media renderability checks in the
	// partial; use the minimal single-row lookup — this runs once per scroll
	// page, and GetConversationByID's count/identifier/fact aggregation is
	// wasted here (SPEC-0008 REQ-0008-005). A missing conversation just renders
	// without media; a real store error is logged, not swallowed.
	src, convName, err := s.store.ConversationSourceName(ctx, id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.log.Error("conversation lookup failed", "error", err, "conversation", id)
	}
	s.render(w, "message_list", messageListData{
		ActiveID:   id,
		Source:     src,
		ConvName:   convName,
		Messages:   page.Messages,
		HasMore:    page.HasMore,
		NextTSUnix: page.NextTSUnix,
		NextID:     page.NextID,
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	base, err := s.baseData(ctx, "Status · msgbrowse", 0)
	if err != nil {
		s.serverError(w, err)
		return
	}
	run, err := s.store.LatestIngestRun(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	snaps, err := s.store.ListSnapshots(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	newest, err := s.store.NewestMessageTS(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	var footprint int64
	for _, sn := range snaps {
		footprint += sn.SizeBytes
	}
	s.render(w, "status", statusData{
		baseData:          base,
		Run:               run,
		Snapshots:         snaps,
		NewestTS:          newest,
		SnapshotFootprint: footprint,
	})
}

// render executes a named template into a buffer first, so a template error
// never produces a half-written response.
func (s *Server) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

func (s *Server) serverError(w http.ResponseWriter, err error) {
	s.log.Error("request failed", "error", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

// parseID parses a positive int64 path id.
func parseID(s string) (int64, bool) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
