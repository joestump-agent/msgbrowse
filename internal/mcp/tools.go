package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/joestump/msgbrowse/internal/store"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerTools adds every msgbrowse tool to the underlying MCP server.
func (s *Server) registerTools() {
	mcpsdk.AddTool(s.srv, &mcpsdk.Tool{
		Name:        "list_conversations",
		Description: "List every conversation (person or group) with message counts and date ranges.",
	}, s.listConversations)

	mcpsdk.AddTool(s.srv, &mcpsdk.Tool{
		Name: "get_conversation",
		Description: "Retrieve a conversation's transcript in chronological order, optionally " +
			"bounded by a date range. Use list_conversations to discover names.",
	}, s.getConversation)

	mcpsdk.AddTool(s.srv, &mcpsdk.Tool{
		Name: "search_messages",
		Description: "Hybrid keyword + semantic search across all messages. Returns ranked " +
			"snippets with exact provenance (conversation, sender, timestamp, message_id). " +
			"Filterable by conversation, sender, source, and date range.",
	}, s.searchMessages)

	mcpsdk.AddTool(s.srv, &mcpsdk.Tool{
		Name: "semantic_search",
		Description: "Pure vector (meaning-based) search over message embeddings. Returns the " +
			"k most semantically similar messages with provenance and a similarity score.",
	}, s.semanticSearch)

	mcpsdk.AddTool(s.srv, &mcpsdk.Tool{
		Name: "get_context",
		Description: "Return the messages surrounding a given message_id (window on each side) " +
			"for assembling RAG context around a search hit.",
	}, s.getContext)

	mcpsdk.AddTool(s.srv, &mcpsdk.Tool{
		Name:        "list_media",
		Description: "List image or file attachments, optionally filtered by conversation and source.",
	}, s.listMedia)

	mcpsdk.AddTool(s.srv, &mcpsdk.Tool{
		Name:        "list_links",
		Description: "List deduplicated links, optionally filtered by domain, conversation, and source.",
	}, s.listLinks)
}

// --- list_conversations ---

type listConversationsIn struct{}

type conversationInfo struct {
	Name         string `json:"name"`
	MessageCount int    `json:"message_count"`
	FirstTS      string `json:"first_timestamp,omitempty"`
	LastTS       string `json:"last_timestamp,omitempty"`
	Images       int    `json:"images"`
	Files        int    `json:"files"`
	Links        int    `json:"links"`
}

type listConversationsOut struct {
	Conversations []conversationInfo `json:"conversations"`
}

func (s *Server) listConversations(ctx context.Context, _ *mcpsdk.CallToolRequest, _ listConversationsIn) (*mcpsdk.CallToolResult, listConversationsOut, error) {
	convs, err := s.store.ListConversations(ctx)
	if err != nil {
		return nil, listConversationsOut{}, err
	}
	out := listConversationsOut{Conversations: make([]conversationInfo, 0, len(convs))}
	for _, c := range convs {
		out.Conversations = append(out.Conversations, conversationInfo{
			Name: c.Name, MessageCount: c.MessageCount, FirstTS: c.FirstTS, LastTS: c.LastTS,
			Images: c.ImageCount, Files: c.FileCount, Links: c.LinkCount,
		})
	}
	return nil, out, nil
}

// --- get_conversation ---

type getConversationIn struct {
	Name  string `json:"name" jsonschema:"the conversation name (person or group)"`
	Start string `json:"start,omitempty" jsonschema:"earliest date inclusive, YYYY-MM-DD"`
	End   string `json:"end,omitempty" jsonschema:"latest date inclusive, YYYY-MM-DD"`
	Limit int    `json:"limit,omitempty" jsonschema:"max messages to return (default 100, max 500)"`
}

type getConversationOut struct {
	Conversation string       `json:"conversation"`
	Messages     []messageHit `json:"messages"`
}

func (s *Server) getConversation(ctx context.Context, _ *mcpsdk.CallToolRequest, in getConversationIn) (*mcpsdk.CallToolResult, getConversationOut, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, getConversationOut{}, fmt.Errorf("name is required")
	}
	convID, err := s.resolveConversation(ctx, in.Name)
	if err != nil {
		return nil, getConversationOut{}, err
	}
	msgs, err := s.store.ConversationTranscript(ctx, convID, dayStart(in.Start), dayEnd(in.End), in.Limit)
	if err != nil {
		return nil, getConversationOut{}, err
	}
	out := getConversationOut{Conversation: in.Name, Messages: make([]messageHit, 0, len(msgs))}
	for _, m := range msgs {
		out.Messages = append(out.Messages, hitFromView(in.Name, m))
	}
	return nil, out, nil
}

// --- search_messages (hybrid) ---

type searchMessagesIn struct {
	Query        string `json:"query" jsonschema:"search query"`
	Conversation string `json:"conversation,omitempty"`
	Sender       string `json:"sender,omitempty"`
	Source       string `json:"source,omitempty" jsonschema:"signal or imessage"`
	Start        string `json:"start,omitempty" jsonschema:"earliest date inclusive, YYYY-MM-DD"`
	End          string `json:"end,omitempty" jsonschema:"latest date inclusive, YYYY-MM-DD"`
	Limit        int    `json:"limit,omitempty" jsonschema:"max results (default 20, max 100)"`
}

type searchMessagesOut struct {
	Hits []messageHit `json:"hits"`
}

func (s *Server) searchMessages(ctx context.Context, _ *mcpsdk.CallToolRequest, in searchMessagesIn) (*mcpsdk.CallToolResult, searchMessagesOut, error) {
	if strings.TrimSpace(in.Query) == "" {
		return nil, searchMessagesOut{}, fmt.Errorf("query is required")
	}
	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	convID, err := s.resolveConversation(ctx, in.Conversation)
	if err != nil {
		return nil, searchMessagesOut{}, err
	}

	// Keyword half (always available).
	fts, err := s.store.SearchMessages(ctx, store.SearchOptions{
		Query: in.Query, ConversationID: convID, Source: in.Source, Sender: in.Sender,
		StartUnix: dayStart(in.Start), EndUnix: dayEnd(in.End), Limit: limit,
	})
	if err != nil {
		return nil, searchMessagesOut{}, err
	}

	// Vector half (best-effort): embed the query and retrieve. If embeddings or
	// the LLM are unavailable, fall back to keyword-only rather than failing —
	// but log the reason so a silently-degraded hybrid search is diagnosable.
	// The embed model is read per call (live settings, #191).
	var sem []store.ScoredMessage
	if embedModel := s.embedModel(); s.llm != nil && embedModel != "" {
		if vec := s.embedQuery(ctx, in.Query); vec != nil {
			var serr error
			sem, serr = s.store.SemanticSearch(ctx, vec, embedModel, store.SemanticOptions{
				ConversationID: convID, Source: in.Source, Sender: in.Sender,
				StartUnix: dayStart(in.Start), EndUnix: dayEnd(in.End), K: limit,
			})
			if serr != nil {
				s.log.Warn("semantic half of hybrid search failed; returning keyword results only", "error", serr)
			}
		}
	}

	hits := fuseResults(fts, sem, limit)
	return nil, searchMessagesOut{Hits: hits}, nil
}

// --- semantic_search ---

type semanticSearchIn struct {
	Query        string `json:"query" jsonschema:"natural-language query"`
	K            int    `json:"k,omitempty" jsonschema:"number of results (default 20, max 100)"`
	Conversation string `json:"conversation,omitempty"`
	Source       string `json:"source,omitempty"`
}

func (s *Server) semanticSearch(ctx context.Context, _ *mcpsdk.CallToolRequest, in semanticSearchIn) (*mcpsdk.CallToolResult, searchMessagesOut, error) {
	if strings.TrimSpace(in.Query) == "" {
		return nil, searchMessagesOut{}, fmt.Errorf("query is required")
	}
	// Read the embed model per call so a live Settings → LLM change (an
	// endpoint appearing, or the embed model being cleared) applies to the
	// very next tool call (#191).
	embedModel := s.embedModel()
	if s.llm == nil || embedModel == "" {
		return nil, searchMessagesOut{}, fmt.Errorf("semantic search unavailable: no embedding model configured")
	}
	convID, err := s.resolveConversation(ctx, in.Conversation)
	if err != nil {
		return nil, searchMessagesOut{}, err
	}
	vec := s.embedQuery(ctx, in.Query)
	if vec == nil {
		return nil, searchMessagesOut{}, fmt.Errorf("failed to embed query")
	}
	k := in.K
	if k <= 0 || k > 100 {
		k = 20
	}
	scored, err := s.store.SemanticSearch(ctx, vec, embedModel, store.SemanticOptions{
		ConversationID: convID, Source: in.Source, K: k,
	})
	if err != nil {
		return nil, searchMessagesOut{}, err
	}
	out := searchMessagesOut{Hits: make([]messageHit, 0, len(scored))}
	for _, m := range scored {
		out.Hits = append(out.Hits, hitFromScored(m))
	}
	return nil, out, nil
}

// --- get_context ---

type getContextIn struct {
	MessageID int64 `json:"message_id" jsonschema:"the message id to center on"`
	Window    int   `json:"window,omitempty" jsonschema:"messages on each side (default 5, max 50)"`
}

type getContextOut struct {
	Messages []messageHit `json:"messages"`
}

func (s *Server) getContext(ctx context.Context, _ *mcpsdk.CallToolRequest, in getContextIn) (*mcpsdk.CallToolResult, getContextOut, error) {
	if in.MessageID <= 0 {
		return nil, getContextOut{}, fmt.Errorf("message_id is required")
	}
	window := in.Window
	if window <= 0 || window > 50 {
		window = 5
	}
	convID, found, err := s.store.MessageConversationID(ctx, in.MessageID)
	if err != nil {
		return nil, getContextOut{}, err
	}
	if !found {
		return nil, getContextOut{}, fmt.Errorf("message %d not found", in.MessageID)
	}
	conv, err := s.store.GetConversationByID(ctx, convID)
	if err != nil {
		return nil, getContextOut{}, err
	}
	name := ""
	if conv != nil {
		name = conv.Name
	}
	msgs, err := s.store.GetContext(ctx, in.MessageID, window)
	if err != nil {
		return nil, getContextOut{}, err
	}
	out := getContextOut{Messages: make([]messageHit, 0, len(msgs))}
	for _, m := range msgs {
		out.Messages = append(out.Messages, hitFromView(name, m))
	}
	return nil, out, nil
}

// --- list_media ---

type listMediaIn struct {
	Conversation string `json:"conversation,omitempty"`
	Kind         string `json:"kind,omitempty" jsonschema:"image or file (default both)"`
	Source       string `json:"source,omitempty"`
	Limit        int    `json:"limit,omitempty"`
}

type mediaInfo struct {
	Conversation string `json:"conversation"`
	Source       string `json:"source"`
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	Path         string `json:"path"`
	Timestamp    string `json:"timestamp"`
	MessageID    int64  `json:"message_id"`
}

type listMediaOut struct {
	Media []mediaInfo `json:"media"`
}

func (s *Server) listMedia(ctx context.Context, _ *mcpsdk.CallToolRequest, in listMediaIn) (*mcpsdk.CallToolResult, listMediaOut, error) {
	convID, err := s.resolveConversation(ctx, in.Conversation)
	if err != nil {
		return nil, listMediaOut{}, err
	}
	filter := store.GalleryFilter{ConversationID: convID, Source: in.Source, Limit: in.Limit}
	var kinds []string
	switch in.Kind {
	case "image", "file":
		kinds = []string{in.Kind}
	default:
		kinds = []string{"image", "file"}
	}
	var out listMediaOut
	for _, kind := range kinds {
		page, err := s.store.ListAttachments(ctx, kind, filter, 0, 0)
		if err != nil {
			return nil, listMediaOut{}, err
		}
		for _, it := range page.Items {
			out.Media = append(out.Media, mediaInfo{
				Conversation: it.ConversationName, Source: it.Source, Kind: it.Kind,
				Name: it.OriginalName, Path: it.RelPath, Timestamp: it.TS, MessageID: it.MessageID,
			})
		}
	}
	return nil, out, nil
}

// --- list_links ---

type listLinksIn struct {
	Domain       string `json:"domain,omitempty" jsonschema:"filter to this domain (e.g. example.com)"`
	Conversation string `json:"conversation,omitempty"`
	Source       string `json:"source,omitempty"`
	Limit        int    `json:"limit,omitempty"`
}

type linkInfo struct {
	URL          string `json:"url"`
	Domain       string `json:"domain"`
	Count        int    `json:"count"`
	Conversation string `json:"conversation"`
	Source       string `json:"source"`
	Timestamp    string `json:"timestamp"`
	MessageID    int64  `json:"message_id"`
}

type listLinksOut struct {
	Links []linkInfo `json:"links"`
}

func (s *Server) listLinks(ctx context.Context, _ *mcpsdk.CallToolRequest, in listLinksIn) (*mcpsdk.CallToolResult, listLinksOut, error) {
	convID, err := s.resolveConversation(ctx, in.Conversation)
	if err != nil {
		return nil, listLinksOut{}, err
	}
	// The domain narrowing runs in SQL (GalleryFilter.Domain hits
	// idx_links_domain) so the tool sees every matching URL even when the
	// archive holds more distinct links than one store page.
	limit := in.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	filter := store.GalleryFilter{
		ConversationID: convID,
		Source:         in.Source,
		Domain:         strings.ToLower(strings.TrimPrefix(in.Domain, "www.")),
		Limit:          limit,
	}
	page, err := s.store.ListLinks(ctx, filter, store.LinkCursor{})
	if err != nil {
		return nil, listLinksOut{}, err
	}
	var out listLinksOut
	for _, l := range page.Links {
		out.Links = append(out.Links, linkInfo{
			URL: l.URL, Domain: l.Domain, Count: l.Count, Conversation: l.ConversationName,
			Source: l.Source, Timestamp: l.TS, MessageID: l.MessageID,
		})
	}
	return nil, out, nil
}

// --- helpers ---

// embedQuery embeds a single query string, returning nil on any error (callers
// fall back to keyword-only where possible).
func (s *Server) embedQuery(ctx context.Context, query string) []float32 {
	vecs, err := s.llm.Embed(ctx, []string{query})
	if err != nil || len(vecs) != 1 {
		s.log.Warn("query embedding failed; using keyword results only", "error", err)
		return nil
	}
	return vecs[0]
}

// fuseResults merges keyword and semantic hits with reciprocal-rank fusion, then
// returns the top `limit`. RRF (score = sum of 1/(k0+rank)) is robust to the two
// lists' incomparable score scales. Keyword hits keep their highlighted snippet;
// semantic-only hits carry the message body.
func fuseResults(fts []store.SearchHit, sem []store.ScoredMessage, limit int) []messageHit {
	const k0 = 60.0
	type agg struct {
		hit   messageHit
		score float64
	}
	byID := map[int64]*agg{}

	for rank, h := range fts {
		a := byID[h.MessageID]
		if a == nil {
			a = &agg{hit: hitFromSearch(h)}
			byID[h.MessageID] = a
		}
		a.score += 1.0 / (k0 + float64(rank+1))
	}
	for rank, m := range sem {
		a := byID[m.MessageID]
		if a == nil {
			a = &agg{hit: hitFromScored(m)}
			byID[m.MessageID] = a
		}
		a.score += 1.0 / (k0 + float64(rank+1))
	}

	out := make([]messageHit, 0, len(byID))
	for _, a := range byID {
		h := a.hit
		h.Score = round4(a.score)
		out = append(out, h)
	}
	sortHitsByScoreDesc(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// sortHitsByScoreDesc sorts hits by descending fused score, breaking ties by
// message_id so identical queries return a stable, reproducible order (the
// fusion map's iteration order is otherwise random).
func sortHitsByScoreDesc(hits []messageHit) {
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].MessageID < hits[j].MessageID
	})
}

// dayStart parses YYYY-MM-DD to the start-of-day UTC unix second; 0 if empty/bad.
func dayStart(date string) int64 {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return 0
	}
	return t.UTC().Unix()
}

// dayEnd parses YYYY-MM-DD to the end-of-day UTC unix second; 0 if empty/bad.
func dayEnd(date string) int64 {
	t, err := time.Parse("2006-01-02", date)
	if err != nil {
		return 0
	}
	return t.UTC().Add(24*time.Hour - time.Second).Unix()
}
