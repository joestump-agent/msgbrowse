package mcp

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/llm"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestSemanticSearchFollowsLiveEmbedModel is the #191 no-restart contract at
// the MCP layer: the server reads the embed model PER CALL through
// Options.EmbedModelFunc, so a Settings → LLM save (an llm.Holder swap)
// changes semantic_search's behavior on its very next invocation — from
// "unavailable" (no embed model) to working results — with no server rebuild.
func TestSemanticSearchFollowsLiveEmbedModel(t *testing.T) {
	st, _ := seedStore(t)

	// Boot shape: an endpoint is configured but the embed model is empty —
	// semantic search off (the desktop default until the user fills the tab).
	holder := llm.NewHolder(embedClient{}, llm.Settings{EmbedModel: ""})
	srv := NewServer(st, holder, Options{
		EmbedModelFunc: holder.EmbedModel,
		Version:        "test",
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ctx := context.Background()
	t1, t2 := mcpsdk.NewInMemoryTransports()
	if _, err := srv.srv.Connect(ctx, t1, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, t2, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })

	call := func() *mcpsdk.CallToolResult {
		res, err := cs.CallTool(ctx, &mcpsdk.CallToolParams{
			Name: "semantic_search", Arguments: map[string]any{"query": "lease"},
		})
		if err != nil {
			t.Fatalf("call semantic_search: %v", err)
		}
		return res
	}

	// Before the "save": no embed model → the tool reports itself unavailable.
	res := call()
	if !res.IsError {
		t.Fatal("semantic_search should error with no embed model configured")
	}

	// The live swap (what a Settings → LLM save performs): same server, same
	// session — only the holder changed.
	holder.Swap(embedClient{}, llm.Settings{EmbedModel: "test-embed"})

	res = call()
	if res.IsError {
		t.Fatalf("semantic_search still failing after the live swap: %+v", res.Content)
	}
	// It found the seeded lease message via the swapped-in model's embeddings.
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcpsdk.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	if !strings.Contains(text.String(), "lease") {
		t.Errorf("semantic_search after swap returned no lease hit: %s", text.String())
	}

	// And clearing the model turns it off again — live in both directions.
	holder.Swap(embedClient{}, llm.Settings{EmbedModel: ""})
	if res := call(); !res.IsError {
		t.Error("semantic_search should be unavailable again after the embed model was cleared")
	}
}
