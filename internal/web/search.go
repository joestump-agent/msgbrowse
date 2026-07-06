package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// searchForm holds the raw filter state so the form re-renders with the user's
// selections intact.
type searchForm struct {
	Q              string
	ConversationID int64
	Source         string
	Sender         string
	Start          string // YYYY-MM-DD
	End            string // YYYY-MM-DD
	HasAttachment  bool
	HasLink        bool
}

type searchData struct {
	baseData
	// FilterConversations feeds the conversation dropdown: the lightweight
	// id+name listing, NOT the sidebar summaries, so partial renders never need
	// the expensive listing (SPEC-0008 REQ-0008-006). Ordered alphabetically.
	FilterConversations []store.ConversationRef
	Form                searchForm
	Sources             []string
	Hits                []store.SearchHit
	Ran                 bool // a query was actually executed
	Count               int
}

// searchContextWindow is how many messages on each side of a hit the
// jump-to-context view loads.
const searchContextWindow = 20

// handleSearch renders the full search page (filter form + initial results).
// It degrades without JavaScript: the form submits here via GET and the results
// render server-side.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var base baseData
	if isPartialRequest(r) {
		base = partialBase("Search · msgbrowse", 0)
	} else {
		var err error
		base, err = s.baseData(ctx, "Search · msgbrowse", 0)
		if err != nil {
			s.serverError(w, err)
			return
		}
	}
	// The dropdown uses the cheap id+name listing on BOTH paths so partial and
	// full renders show the identical (alphabetical) option order.
	refs, err := s.store.ConversationRefs(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	form, opts := parseSearchForm(r)
	data := searchData{
		baseData:            base,
		FilterConversations: refs,
		Form:                form,
		Sources:             source.All,
	}
	if opts.Query != "" {
		hits, err := s.store.SearchMessages(ctx, opts)
		if err != nil {
			s.serverError(w, err)
			return
		}
		data.Hits = hits
		data.Count = len(hits)
		data.Ran = true
	}
	s.render(w, r, "search", data)
}

// handleSearchResults renders just the results list for HTMX live search.
func (s *Server) handleSearchResults(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	form, opts := parseSearchForm(r)
	data := searchData{Form: form}
	if opts.Query != "" {
		hits, err := s.store.SearchMessages(ctx, opts)
		if err != nil {
			s.serverError(w, err)
			return
		}
		data.Hits = hits
		data.Count = len(hits)
		data.Ran = true
	}
	s.render(w, r, "search_results", data)
}

// handleConversationAt renders a conversation transcript centered on a specific
// message (the jump-to-context target from a search result). The page anchor
// (#m<id>) scrolls to it; the message is visually marked.
func (s *Server) handleConversationAt(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := parseID(r.PathValue("id"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	mid, ok := parseID(r.PathValue("mid"))
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
	// Verify the target message actually belongs to this conversation. Without
	// this, /c/{id}/at/{mid} would render conversation {id}'s header and sidebar
	// state while GetContext (which derives the conversation from the message
	// itself) shows a *different* conversation's transcript — an
	// information-disclosure / identity-confusion bug for a crafted or mistyped
	// link.
	ownerConv, found, err := s.store.MessageConversationID(ctx, mid)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if !found || ownerConv != id {
		http.NotFound(w, r)
		return
	}
	msgs, err := s.store.GetContext(ctx, mid, searchContextWindow)
	if err != nil {
		s.serverError(w, err)
		return
	}
	if len(msgs) == 0 {
		http.NotFound(w, r)
		return
	}
	list := messageListData{
		ActiveID:    id,
		Source:      active.Source,
		ConvName:    active.Name,
		Sort:        sortAsc, // context reads chronologically regardless of the transcript default
		Messages:    msgs,
		HighlightID: mid,
	}
	// Continue infinite scroll downward from the newest message in the window.
	last := msgs[len(msgs)-1]
	list.HasMore = true
	list.NextTSUnix = last.TSUnix
	list.NextID = last.ID

	// Jump-to-context links are deliberately un-boosted (the #m{id} anchor needs
	// a full-page load), but the partial contract still holds if an HTMX request
	// arrives here (REQ-0008-006).
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
	// Toolbar contextual title is the conversation name on a transcript (#152);
	// jump-to-context is still a Messages surface (#190).
	base.NavTitle = humanName(active.Name)
	base.NavTab = navTabMessages
	s.render(w, r, "conversation", conversationData{
		baseData: base,
		Active:   active,
		List:     list,
	})
}

// parseSearchForm reads the search filters from the request query string into a
// re-renderable form and a store.SearchOptions.
func parseSearchForm(r *http.Request) (searchForm, store.SearchOptions) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	convID, _ := strconv.ParseInt(r.URL.Query().Get("conversation"), 10, 64)
	src := r.URL.Query().Get("source")
	if !source.IsKnown(src) {
		src = ""
	}
	sender := strings.TrimSpace(r.URL.Query().Get("sender"))
	start := r.URL.Query().Get("start")
	end := r.URL.Query().Get("end")
	hasAtt := r.URL.Query().Get("has_attachment") != ""
	hasLink := r.URL.Query().Get("has_link") != ""

	form := searchForm{
		Q: q, ConversationID: convID, Source: src, Sender: sender,
		Start: start, End: end, HasAttachment: hasAtt, HasLink: hasLink,
	}
	opts := store.SearchOptions{
		Query:          q,
		ConversationID: convID,
		Source:         src,
		Sender:         sender,
		StartUnix:      dayStartUnix(start),
		EndUnix:        dayEndUnix(end),
		HasAttachment:  hasAtt,
		HasLink:        hasLink,
	}
	return form, opts
}

// dayStartUnix parses a YYYY-MM-DD date as the start of that UTC day (00:00:00).
// Returns 0 for an empty or unparseable date (no lower bound).
func dayStartUnix(date string) int64 {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return 0
	}
	return t.UTC().Unix()
}

// dayEndUnix parses a YYYY-MM-DD date as the end of that UTC day (23:59:59).
// Returns 0 for an empty or unparseable date (no upper bound).
func dayEndUnix(date string) int64 {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return 0
	}
	return t.UTC().Add(24*time.Hour - time.Second).Unix()
}
