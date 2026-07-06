// Package mcp implements msgbrowse's Model Context Protocol server: a set of
// citation-faithful retrieval tools over the unified message store, so an MCP
// client (Claude Desktop / Claude Code) can answer natural-language questions
// about the archive.
//
// Every result carries exact source coordinates (conversation, sender, source,
// timestamp, and the stable message id/hash) so the consuming model can cite
// precisely and a human can jump to the message in the web UI. The server reads
// only; it never mutates the store or the archive. The only egress it performs
// is embedding the query for semantic search, via the same llm.Client the rest
// of msgbrowse uses.
package mcp

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/joestump/msgbrowse/internal/llm"
	"github.com/joestump/msgbrowse/internal/store"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Server wires the msgbrowse store and LLM client into an MCP tool surface.
type Server struct {
	store *store.Store
	llm   llm.Client
	// embedModel returns the CURRENT embedding model per call (never nil after
	// NewServer): a static value in CLI mode, the live llm.Holder getter in
	// desktop mode, so a Settings → LLM save applies to the next semantic
	// search with no restart (#191).
	embedModel func() string
	log        *slog.Logger
	srv        *mcpsdk.Server
}

// Options configures the MCP server.
type Options struct {
	// EmbedModel is the embedding model used to embed queries for semantic
	// search; it must match the model used by `msgbrowse embed`.
	EmbedModel string
	// EmbedModelFunc, when non-nil, supplies the embedding model PER CALL and
	// takes precedence over EmbedModel. The desktop shell passes the shared
	// llm.(*Holder).EmbedModel getter so semantic search follows a live
	// Settings → LLM swap (#191); it must be safe for concurrent use.
	EmbedModelFunc func() string
	Version        string
	Logger         *slog.Logger
}

// NewServer builds the MCP server and registers every tool. llmClient may be nil
// if only keyword tools are needed, but semantic_search and the vector half of
// search_messages then return an error.
func NewServer(st *store.Store, llmClient llm.Client, opts Options) *Server {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	version := opts.Version
	if version == "" {
		version = "dev"
	}
	embedModel := opts.EmbedModelFunc
	if embedModel == nil {
		static := opts.EmbedModel
		embedModel = func() string { return static }
	}
	s := &Server{store: st, llm: llmClient, embedModel: embedModel, log: log}
	s.srv = mcpsdk.NewServer(&mcpsdk.Implementation{Name: "msgbrowse", Version: version}, nil)
	s.registerTools()
	return s
}

// RunStdio serves the MCP protocol over stdio until ctx is cancelled.
func (s *Server) RunStdio(ctx context.Context) error {
	return s.srv.Run(ctx, &mcpsdk.StdioTransport{})
}

// HTTPHandler returns the streamable-HTTP handler for this server, for
// callers that mount MCP on an existing listener instead of a dedicated one:
// the desktop shell mounts it on the embedded web server's loopback listener
// (SPEC-0010's bind-surface requirement allows no listener beyond the
// embedded server). RunHTTP remains the standalone-transport path used by
// `msgbrowse mcp --http`.
func (s *Server) HTTPHandler() http.Handler {
	return mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return s.srv }, nil)
}

// RunHTTP serves the MCP protocol over streamable HTTP on addr until ctx is
// cancelled. It binds loopback by default (the caller chooses addr).
func (s *Server) RunHTTP(ctx context.Context, addr string) error {
	httpSrv := &http.Server{Addr: addr, Handler: s.HTTPHandler(), ReadHeaderTimeout: 10 * time.Second}

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()
	s.log.Info("MCP server listening (streamable HTTP)", "addr", addr)
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

// --- shared result shapes (citation-faithful) ---

// messageHit is the common message result with full provenance.
type messageHit struct {
	MessageID    int64   `json:"message_id"`
	Hash         string  `json:"hash"`
	Conversation string  `json:"conversation"`
	Source       string  `json:"source"`
	Sender       string  `json:"sender"`
	Timestamp    string  `json:"timestamp"`
	Text         string  `json:"text"`
	Score        float64 `json:"score,omitempty"`
}

func hitFromSearch(h store.SearchHit) messageHit {
	return messageHit{
		MessageID: h.MessageID, Hash: h.Hash, Conversation: h.ConversationName,
		Source: h.Source, Sender: displaySender(h.Sender, h.IsOwner),
		Timestamp: h.TS, Text: h.Snippet,
	}
}

func hitFromScored(m store.ScoredMessage) messageHit {
	return messageHit{
		MessageID: m.MessageID, Hash: m.Hash, Conversation: m.ConversationName,
		Source: m.Source, Sender: displaySender(m.Sender, m.IsOwner),
		Timestamp: m.TS, Text: m.Body, Score: round4(m.Score),
	}
}

func hitFromView(convName string, m store.MessageView) messageHit {
	return messageHit{
		MessageID: m.ID, Hash: m.Hash, Conversation: convName,
		Sender: displaySender(m.Sender, m.IsOwner), Timestamp: m.TS, Text: m.Body,
	}
}

func displaySender(sender string, isOwner bool) string {
	if isOwner {
		return "Me"
	}
	return sender
}

func round4(f float64) float64 {
	return float64(int(f*10000+0.5)) / 10000
}

// resolveConversation maps an optional conversation name to its id. Returns
// (0, nil) when name is empty (no filter); an error when the name is unknown.
func (s *Server) resolveConversation(ctx context.Context, name string) (int64, error) {
	if name == "" {
		return 0, nil
	}
	c, err := s.store.GetConversation(ctx, name)
	if err != nil {
		return 0, err
	}
	if c == nil {
		return 0, fmt.Errorf("conversation %q not found", name)
	}
	return c.ID, nil
}
