package web

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/joestump/msgbrowse/internal/devsync"
	"github.com/joestump/msgbrowse/internal/store"
)

// baseData is embedded in every full-page view; it drives the chrome (the
// unified toolbar + sidebar). It carries the full conversation list the sidebar
// renders (REQ-0006-003) and the contextual toolbar title (#152, issue #129).
//
// NavTitle is the text the unified toolbar shows on the left, distinct from the
// document <title> in Title: "msgbrowse" on home/global surfaces, the active
// conversation's display name on a transcript page. It is display-only and never
// used in URLs. TotalMessages remains for surfaces that show global counts in
// their body (Home stat strip, Status) — Option A (#152) removed the counts from
// the toolbar itself, so the toolbar no longer reads this field.
type baseData struct {
	Title         string
	NavTitle      string
	Conversations []store.ConversationSummary
	ActiveID      int64
	TotalMessages int // global message count (Home/Status body, not the toolbar)
	// DesktopChrome is true when rendering inside the desktop shell's
	// hidden-title-bar window (SPEC-0010, issue #165): page_start then adds
	// the `desktop-chrome` <body> class (traffic-light inset padding on the
	// unified toolbar) and loads /static/desktop.js (the CSP-safe
	// --wails-draggable drag-region reader). Only full-page renders emit the
	// <body> tag, so partialBase never needs to carry it.
	DesktopChrome bool
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
//
// HTMX partial renders MUST NOT call this: the listing measured 82–316ms per
// boosted click for sidebar markup the swap then discards. Use partialBase
// instead (SPEC-0008 REQ-0008-006).
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
		NavTitle:      defaultNavTitle,
		Conversations: convs,
		ActiveID:      activeID,
		TotalMessages: total,
		DesktopChrome: s.desktopChrome,
	}, nil
}

// defaultNavTitle is the unified toolbar's contextual title on home and every
// global surface (Search, Media, Settings, …); a transcript page overrides it
// with the conversation's display name (#152). The wordmark links home from the
// toolbar, so this doubles as the wordmark text.
const defaultNavTitle = "msgbrowse"

// partialBase is the shell-free baseData for HTMX partial renders: title and
// active id only, zero store work. The *_content defines never touch
// .Conversations, so the sidebar listing is skipped entirely (REQ-0008-006).
func partialBase(title string, activeID int64) baseData {
	return baseData{Title: title, NavTitle: defaultNavTitle, ActiveID: activeID}
}

// isPartialRequest reports whether the request is an HTMX boosted navigation
// that wants only the <title> + #main-content region. History restores
// (HX-History-Restore-Request: true) need the full document — htmx rebuilds
// the whole page from them. The headers are consumed strictly as booleans and
// never echoed into output.
func isPartialRequest(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true" &&
		r.Header.Get("HX-History-Restore-Request") != "true"
}

// messageListData drives the transcript message list and its infinite-scroll
// sentinel (used both in the full page and the HTMX partial).
type messageListData struct {
	ActiveID    int64
	Source      string // active conversation's source (for media renderability checks)
	ConvName    string // active conversation's name (for media path resolution)
	Sort        string // display order: sortDesc (default) or sortAsc; carried on the load-more URL
	Messages    []store.MessageView
	HasMore     bool
	NextTSUnix  int64
	NextID      int64
	HighlightID int64 // marks the jump-to-context target message (0 = none)
}

type indexData struct {
	baseData
	ConversationCount int // stat-strip count; independent of the sidebar listing (REQ-0008-006)
	NewestTS          string
	HasArchive        bool
}

type conversationData struct {
	baseData
	Active *store.ConversationSummary
	List   messageListData
}

type statusData struct {
	baseData
	ConversationCount int // stat-strip count; independent of the sidebar listing (REQ-0008-006)
	Run               *store.IngestRun
	Snapshots         []store.Snapshot
	NewestTS          string
	SnapshotFootprint int64
	// HasSnapshotPipeline gates the Encrypted-DB-snapshots card (issue #164):
	// on a desktop-onboarded machine there IS no snapshot pipeline (that flow
	// is the Cowork/launchd Signal export), so "0 B across 0 snapshots … No
	// snapshots found" read like a failure. True when snapshots are recorded or
	// the signal archive carries a .snapshots directory; false renders one
	// neutral line instead of the card.
	HasSnapshotPipeline bool
	// DeviceSyncEnabled mirrors config device_sync.enabled for the Device
	// sync card's disabled state; Sync is the live snapshot (nil when sync is
	// disabled, no monitor is wired, or the registry read failed) — SPEC-0014
	// REQ "Status and Doctor Surfacing" (#158).
	DeviceSyncEnabled bool
	Sync              *devsync.Status
}

// pageSize is the number of messages per transcript page.
const pageSize = 50

// Transcript sort orders (the ?sort= query value). Newest-first is the default
// so a conversation opens at its most recent messages; oldest-first is the
// legacy chronological walk (and the order jump-to-context always uses).
const (
	sortDesc = "desc"
	sortAsc  = "asc"
)

// parseSort normalizes a ?sort= query value: "asc" selects the legacy
// oldest-first order, anything else (including absent) the newest-first
// default.
func parseSort(r *http.Request) string {
	if r.URL.Query().Get("sort") == sortAsc {
		return sortAsc
	}
	return sortDesc
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var (
		base      baseData
		convCount int
	)
	if isPartialRequest(r) {
		// The home stat strip still needs the global counts, but the one-row
		// ArchiveStats aggregate is far cheaper than the full summary listing
		// the sidebar needs (REQ-0008-006).
		stats, err := s.store.ArchiveStats(ctx)
		if err != nil {
			s.serverError(w, err)
			return
		}
		base = partialBase("msgbrowse", 0)
		base.TotalMessages = stats.Messages
		convCount = stats.Conversations
	} else {
		var err error
		base, err = s.baseData(ctx, "msgbrowse", 0)
		if err != nil {
			s.serverError(w, err)
			return
		}
		convCount = len(base.Conversations)
	}
	// First-run routing (SPEC-0013 REQ "First-run wizard versus returning
	// launch"): an empty store (no imported conversations) lands on the Providers
	// wizard (the renamed Setup surface) instead of the empty transcript home. A
	// configured store falls through to the transcript UI below, with Providers
	// reachable from the nav. The 303 is followed transparently by both a plain
	// browser and an htmx boosted navigation, so first launch opens on Providers in
	// either mode.
	if convCount == 0 {
		http.Redirect(w, r, "/providers", http.StatusSeeOther)
		return
	}
	newest, err := s.store.NewestMessageTS(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, "index", indexData{
		baseData:          base,
		ConversationCount: convCount,
		NewestTS:          newest,
		HasArchive:        convCount > 0,
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
	// Boosted clicks skip the sidebar listing entirely: the partial response
	// carries no sidebar markup, so its 82–316ms would be pure waste
	// (SPEC-0008 REQ-0008-006).
	var base baseData
	if isPartialRequest(r) {
		base = partialBase(active.Name+" · msgbrowse", id)
	} else {
		base, err = s.baseData(ctx, active.Name+" · msgbrowse", id)
		if err != nil {
			s.serverError(w, err)
			return
		}
	}
	// The unified toolbar shows the active conversation's display name as its
	// contextual title on a transcript page (#152), not the "msgbrowse" wordmark.
	base.NavTitle = humanName(active.Name)
	sort := parseSort(r)
	page, err := s.store.GetMessages(ctx, id, 0, 0, pageSize, sort == sortDesc)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, r, "conversation", conversationData{
		baseData: base,
		Active:   active,
		List: messageListData{
			ActiveID:   id,
			Source:     active.Source,
			ConvName:   active.Name,
			Sort:       sort,
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
	// The keyset cursor param matches the walk direction: ascending pages
	// continue strictly after it, descending pages strictly before it.
	sort := parseSort(r)
	cursorTSParam, cursorIDParam := "after_ts", "after_id"
	if sort == sortDesc {
		cursorTSParam, cursorIDParam = "before_ts", "before_id"
	}
	cursorTS, _ := strconv.ParseInt(r.URL.Query().Get(cursorTSParam), 10, 64)
	cursorID, _ := strconv.ParseInt(r.URL.Query().Get(cursorIDParam), 10, 64)
	page, err := s.store.GetMessages(ctx, id, cursorTS, cursorID, pageSize, sort == sortDesc)
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
	s.render(w, r, "message_list", messageListData{
		ActiveID:   id,
		Source:     src,
		ConvName:   convName,
		Sort:       sort,
		Messages:   page.Messages,
		HasMore:    page.HasMore,
		NextTSUnix: page.NextTSUnix,
		NextID:     page.NextID,
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var (
		base      baseData
		convCount int
	)
	if isPartialRequest(r) {
		// Same shape as handleIndex: the freshness stat strip needs the global
		// counts, but never the full sidebar listing (REQ-0008-006).
		stats, err := s.store.ArchiveStats(ctx)
		if err != nil {
			s.serverError(w, err)
			return
		}
		base = partialBase("Status · msgbrowse", 0)
		base.TotalMessages = stats.Messages
		convCount = stats.Conversations
	} else {
		var err error
		base, err = s.baseData(ctx, "Status · msgbrowse", 0)
		if err != nil {
			s.serverError(w, err)
			return
		}
		convCount = len(base.Conversations)
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
	s.render(w, r, "status", statusData{
		baseData:            base,
		ConversationCount:   convCount,
		Run:                 run,
		Snapshots:           snaps,
		NewestTS:            newest,
		SnapshotFootprint:   footprint,
		HasSnapshotPipeline: len(snaps) > 0 || s.signalSnapshotsDirExists(),
		DeviceSyncEnabled:   s.deviceSyncEnabled,
		Sync:                s.syncStatusSnapshot(ctx),
	})
}

// signalSnapshotsDirExists reports whether the signal archive carries a
// .snapshots directory — the on-disk marker of the Cowork/launchd snapshot
// pipeline (issue #164). Recorded snapshots in the store are the primary
// signal; this catches a pipeline whose snapshots have not been ingested yet.
// The effective signal root covers both a configured archive_root and a
// desktop-managed one (issue #160).
func (s *Server) signalSnapshotsDirExists() bool {
	root := s.archiveRoots().Signal
	if root == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(root, ".snapshots"))
	return err == nil && info.IsDir()
}

// render executes a named template into a buffer first, so a template error
// never produces a half-written response.
//
// HTMX boosted navigations (HX-Request: true, not a history restore) get only
// the page's *_content define — <title> + <main id="main-content"> — instead
// of the full document (SPEC-0008 REQ-0008-006). htmx swaps the <main> via
// hx-select="#main-content" and lifts the <title> into history. Fragment
// templates without a *_content sibling (message_list, search_results) render
// unchanged, so the infinite-scroll and live-search contracts are untouched.
// Everything still flows through the same html/template escaping.
func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data any) {
	if isPartialRequest(r) {
		if content := name + "_content"; s.tmpl.Lookup(content) != nil {
			name = content
		}
	}
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.serverError(w, err)
		return
	}
	// The body varies on the HX-Request header (partial vs full document), so
	// any HTTP cache between htmx and the server must key on it — the canonical
	// htmx cache-poisoning footgun. Set on both variants; harmless today
	// (loopback-only, no cache), load-bearing the day a proxy appears.
	w.Header().Add("Vary", "HX-Request")
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
