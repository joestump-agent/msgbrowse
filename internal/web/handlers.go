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
	"github.com/joestump/msgbrowse/internal/mcp"
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
	Title    string
	NavTitle string
	// NavTab marks the active header tab on full-page renders (#190):
	// navTabMessages on home and every /c/* transcript, navTabMedia on the
	// gallery/media surface, "" everywhere else (Search, Settings, …) so
	// neither tab claims an unrelated page. Boosted swaps never re-render the
	// shell; /static/shell.js re-syncs the same rule from location — keep the
	// two in lockstep.
	NavTab        string
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

// Header-tab tokens for baseData.NavTab (#190). Values match the templates'
// data-nav-tab attributes and shell.js's location-sync names.
const (
	navTabMessages = "messages"
	navTabMedia    = "media"
)

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
	// Providers is the per-provider freshness breakdown (issue #1): counts +
	// last-synced stamp per source, beside the global strip above.
	Providers []providerStat
	// Embedding is the semantic-search index status card (issue #1).
	Embedding embedStatusData
	// The MCP connection card (issue #1) — the same request-derived endpoint
	// and copy blocks /settings shows, rendered by the shared mcp_connect_card
	// define so the two surfaces can never drift.
	MCPEndpointURL string
	MCPConfigJSON  string
	MCPAddCommand  string
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
	NewestTS          string
	// DeviceSyncEnabled mirrors config device_sync.enabled for the Device
	// sync card's disabled state; Sync is the live snapshot (nil when sync is
	// disabled, no monitor is wired, or the registry read failed) — SPEC-0014
	// REQ "Status and Doctor Surfacing" (#158).
	DeviceSyncEnabled bool
	Sync              *devsync.Status
	// DeviceSyncFeature gates the entire Device sync card: false (the default
	// release build, feature not compiled in behind the `devicesync` tag) omits
	// it so /status carries no dead surface.
	DeviceSyncFeature bool
	// Embedding drives the "Semantic search index" card (#191): the same
	// coverage + last-run + in-progress data the Overview card shows, assembled
	// by overviewEmbedding.
	Embedding embedStatusData
	// IndexAvailable reports whether an Indexer is wired: false (browser / no-op
	// mode) hides the Build controls and shows the unavailable note.
	IndexAvailable bool
	// IndexResult is the post-POST banner state after a Build / Reset-&-rebuild:
	// "" (no action), "started", "reset", "inprogress", "nomodel",
	// "unavailable", or "error" — a fixed enum mapped to prose by the template.
	IndexResult string
	// SetupToken arms the Build / Reset forms with the same per-session token
	// gate the other privileged POSTs use; "" when no Indexer is wired (the
	// forms are not rendered then).
	SetupToken string
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
	// The Overview consolidation (issue #1): per-provider freshness and the
	// semantic-index status ride every home render — full and boosted alike
	// (they live inside #main-content); all are cheap aggregates, so the
	// REQ-0008-006 sidebar-listing exemption above is untouched.
	providers, err := s.overviewProviders(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	embedding, err := s.overviewEmbedding(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	endpoint := mcpEndpointURL(r)
	// Home is the Messages surface: the header's Messages tab reads active.
	base.NavTab = navTabMessages
	s.render(w, r, "index", indexData{
		baseData:          base,
		ConversationCount: convCount,
		NewestTS:          newest,
		HasArchive:        convCount > 0,
		Providers:         providers,
		Embedding:         embedding,
		MCPEndpointURL:    endpoint,
		MCPConfigJSON:     mcp.ClientConfigJSON(endpoint),
		MCPAddCommand:     mcp.ClaudeMCPAddCommand(endpoint),
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
	// A transcript is a Messages surface (#190): the Messages tab reads active.
	base.NavTitle = humanName(active.Name)
	base.NavTab = navTabMessages
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

// pinnedSidebarTrigger is the event a boosted pin toggle response emits via
// HX-Trigger-After-Settle: its OOB swap replaces the sidebar's conversation
// rows, staling sidebar.js's captured filter row list, so the client re-inits.
// It must be the After-Settle variant — plain HX-Trigger dispatches BEFORE the
// swap, so sidebar.js would re-capture the about-to-be-replaced rows and the
// filter would break after every pin.
const pinnedSidebarTrigger = "msgbrowse:pinned"

// handlePin toggles a conversation's pinned flag (REQ-0006-010). It is a
// CSP-clean form POST (no inline JS; form-action 'self' already permits it) and
// idempotent enough for the back button. The toggle is a single direct UPDATE
// in the store — no summary fetch first (SPEC-0008 REQ-0008-005); a missing
// conversation surfaces as the UPDATE matching no row.
//
// A plain (no-JS) POST 303-redirects back to the conversation. A boosted POST
// is answered directly with the flipped conversation view plus an out-of-band
// re-render of the sidebar's PINNED + CONVERSATIONS sections — htmx never
// processes a redirect's body, so the 303 path could not carry the sidebar
// refresh and the pin read as a visual no-op (#176).
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
	if !isPartialRequest(r) {
		http.Redirect(w, r, "/c/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
		return
	}
	s.renderPinToggle(w, r, id)
}

// renderPinToggle answers a boosted pin/unpin POST: the conversation_content
// partial (the #main-content swap, with the pin button flipped) followed by the
// sidebar_lists_oob fragment (#176). Unlike the boosted GET path — which stays
// listing-free per SPEC-0008 REQ-0008-006 — this runs the full baseData listing
// by design: a pin toggle is a rare click, and the OOB sidebar sections are
// exactly the markup the listing feeds.
func (s *Server) renderPinToggle(w http.ResponseWriter, r *http.Request, id int64) {
	ctx := r.Context()
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
	base.NavTitle = humanName(active.Name)
	// Newest-first default order, matching the /c/{id} landing the no-JS 303
	// redirects to.
	page, err := s.store.GetMessages(ctx, id, 0, 0, pageSize, true)
	if err != nil {
		s.serverError(w, err)
		return
	}
	var buf bytes.Buffer
	err = s.tmpl.ExecuteTemplate(&buf, "conversation_content", conversationData{
		baseData: base,
		Active:   active,
		List: messageListData{
			ActiveID:   id,
			Source:     active.Source,
			ConvName:   active.Name,
			Sort:       sortDesc,
			Messages:   page.Messages,
			HasMore:    page.HasMore,
			NextTSUnix: page.NextTSUnix,
			NextID:     page.NextID,
		},
	})
	if err != nil {
		s.serverError(w, err)
		return
	}
	// Best-effort like the Setup flows: an OOB render failure degrades to a
	// stale sidebar, not a failed toggle.
	if oob, oerr := s.renderOOB("sidebar_lists_oob", base); oerr != nil {
		s.log.Warn("pin: could not render sidebar refresh", "error", oerr)
	} else {
		buf.WriteString(string(oob))
	}
	// History must record the conversation URL the no-JS 303 lands on — never
	// the POST-only /pin route the form targeted.
	w.Header().Set("HX-Push-Url", "/c/"+strconv.FormatInt(id, 10))
	// After-Settle, NOT plain HX-Trigger: the plain header fires before htmx
	// performs the swap, so sidebar.js's re-init would capture the doomed rows.
	w.Header().Set("HX-Trigger-After-Settle", pinnedSidebarTrigger)
	w.Header().Add("Vary", "HX-Request")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
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
	s.renderStatus(w, r, "")
}

// handleStatusIndex is POST /status/index — the privileged "Build" control that
// embeds every message still missing a vector. Gate FIRST (checkSetupPOST:
// same-origin + per-session token + body cap, 403 before any work), then start
// the detached single-flight job and re-render the Status page with the
// fixed-enum result banner (the LLM-save pattern). reset=false: existing
// vectors are kept and only the delta is embedded.
func (s *Server) handleStatusIndex(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return // 403 already written; no job started
	}
	s.renderStatus(w, r, s.startReindex(false))
}

// handleStatusIndexReset is POST /status/index/reset — the privileged "Reset &
// rebuild" control: it clears every stored vector and the run log, then
// re-embeds the whole corpus from scratch. Same gate and re-render shape as
// handleStatusIndex; reset=true. The single guarded job does the clear inside
// the detached goroutine (store.ResetEmbeddings) so a from-scratch rebuild is
// one click, not a clear-then-build two-step.
func (s *Server) handleStatusIndexReset(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return // 403 already written; nothing cleared, no job started
	}
	s.renderStatus(w, r, s.startReindex(true))
}

// renderStatus assembles the Status page and renders it (full document or
// boosted #main-content partial). indexResult is the fixed-enum banner from a
// just-completed Build / Reset POST, "" on a plain GET. The semantic-index card
// reflects coverage as of THIS render — a manual refresh shows the count climb
// while a job runs (no hx-poll, keeping the page CSP-clean and free of a
// background request the user did not ask for).
func (s *Server) renderStatus(w http.ResponseWriter, r *http.Request, indexResult string) {
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
	newest, err := s.store.NewestMessageTS(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	embedding, err := s.overviewEmbedding(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	data := statusData{
		baseData:          base,
		ConversationCount: convCount,
		Run:               run,
		NewestTS:          newest,
		DeviceSyncEnabled: s.deviceSyncEnabled,
		DeviceSyncFeature: s.deviceSyncFeature,
		Sync:              s.syncStatusSnapshot(ctx),
		Embedding:         embedding,
		IndexAvailable:    s.indexer != nil,
		IndexResult:       indexResult,
	}
	// The Build / Reset forms are privileged POSTs: arm them with a live token,
	// but only when there is an Indexer to drive (browser mode renders the
	// unavailable note, no forms).
	if s.indexer != nil {
		tok, err := s.setupTokens.mint()
		if err != nil {
			s.serverError(w, err)
			return
		}
		data.SetupToken = tok
	}
	s.render(w, r, "status", data)
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
