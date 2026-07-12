// The live-settings seam (issue #191): a thread-safe, swappable LLM provider
// so the Settings → LLM tab can repoint the app's single egress endpoint with
// NO restart. Consumers (the MCP server's semantic search today; facts and the
// journal digest as they land on desktop) hold one *Holder for the process
// lifetime and read the CURRENT client + model names through it per call; a
// save swaps the client behind the same handle.
package llm

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Settings are the user-configurable LLM endpoint values the Settings → LLM
// tab edits: the base URL, the two model names, and the API key. The key is
// editable from the tab and persisted to the config file (a deliberate
// product choice — a desktop user has no convenient env var; the config file
// is 0600, ADR-0010's loopback single-user trust).
type Settings struct {
	// BaseURL is the OpenAI-compatible endpoint — the only network egress
	// msgbrowse performs (ADR-0010).
	BaseURL string
	// EmbedModel embeds messages and queries for semantic search. Empty means
	// semantic search is off.
	EmbedModel string
	// ChatModel is the completion model ("Facts model" in the UI: the facts
	// feature consumes it today, the journal digest later).
	ChatModel string
	// APIKey authenticates to a keyed endpoint. Empty for a local proxy that
	// needs none. Held in memory and persisted to the config file; it is never
	// rendered back into the tab's HTML (the form shows only whether one is set).
	APIKey string
}

// Holder is a swappable Client: it implements the Client interface by
// delegating every call to the client it currently holds, and Swap replaces
// that client (plus the Settings it was built from) atomically. All methods
// are safe for concurrent use, so in-flight requests finish against the old
// client while new calls see the new one — the live-apply contract of the
// Settings → LLM tab.
type Holder struct {
	mu       sync.RWMutex
	client   Client
	settings Settings
}

// NewHolder wraps client (which must be non-nil) and the settings it was
// built from.
func NewHolder(client Client, s Settings) *Holder {
	return &Holder{client: client, settings: s}
}

// Swap atomically replaces the held client and settings. In-flight calls on
// the previous client are unaffected; every subsequent call goes to client.
func (h *Holder) Swap(client Client, s Settings) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.client = client
	h.settings = s
}

// Settings returns the settings the current client was built from.
func (h *Holder) Settings() Settings {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.settings
}

// EmbedModel returns the CURRENT embedding model name — the per-call getter
// the MCP server reads so semantic search follows a live settings swap
// (mcp.Options.EmbedModelFunc). Empty means semantic search is off.
func (h *Holder) EmbedModel() string { return h.Settings().EmbedModel }

// ChatModel returns the current completion-model name.
func (h *Holder) ChatModel() string { return h.Settings().ChatModel }

// current snapshots the held client under the read lock.
func (h *Holder) current() Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.client
}

// Embed implements Client by delegating to the current client.
func (h *Holder) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	return h.current().Embed(ctx, inputs)
}

// Chat implements Client by delegating to the current client.
func (h *Holder) Chat(ctx context.Context, req ChatRequest) (string, error) {
	return h.current().Chat(ctx, req)
}

// Transcribe implements Client by delegating to the current client.
func (h *Holder) Transcribe(ctx context.Context, audio []byte, filename string) (string, error) {
	return h.current().Transcribe(ctx, audio, filename)
}

// Vision implements Client by delegating to the current client.
func (h *Holder) Vision(ctx context.Context, image []byte, mimeType, prompt string) (string, error) {
	return h.current().Vision(ctx, image, mimeType, prompt)
}

// Applier binds a Holder to a persistence function: it is the object the web
// layer's Settings → LLM tab drives (web.LLMConfigurator). ApplyLLM persists
// the settings FIRST and only then swaps the live client, so a failed write
// leaves the running provider untouched and the page can report the error
// honestly.
//
// timeout is a process-lifetime value captured at wiring time, reused for
// every rebuilt client. The API key now travels in Settings (editable from
// the tab), so each swap rebuilds the client with the settings' own key.
type Applier struct {
	holder  *Holder
	timeout time.Duration
	persist func(Settings) error
}

// NewApplier builds an Applier over holder. persist writes the settings to
// the mode-appropriate config file (config.SaveLLM behind a path the wiring
// layer resolved); a nil persist skips persistence (tests).
func NewApplier(holder *Holder, timeout time.Duration, persist func(Settings) error) *Applier {
	return &Applier{holder: holder, timeout: timeout, persist: persist}
}

// CurrentLLM returns the settings behind the live client.
func (a *Applier) CurrentLLM() Settings { return a.holder.Settings() }

// ApplyLLM persists s and then swaps the live client to one built from it —
// including its API key. On a persist error nothing is swapped.
func (a *Applier) ApplyLLM(s Settings) error {
	if a.persist != nil {
		if err := a.persist(s); err != nil {
			return err
		}
	}
	a.holder.Swap(a.build(s), s)
	return nil
}

// llmTestTimeout caps a TestLLM probe so a wrong or dead endpoint fails fast
// rather than hanging the Settings tab for the Applier's full request timeout.
const llmTestTimeout = 5 * time.Second

// TestLLM probes the endpoint described by s WITHOUT persisting or swapping the
// live client — the Settings → LLM tab's "Test connection" affordance, so a
// user can verify a LiteLLM/Ollama endpoint before saving. It builds a
// transient client from s (same builder ApplyLLM uses) and makes one cheap real
// call to prove reachability + model validity: a single-string embed when an
// embed model is set, else a 1-token chat when only the facts model is set.
// Returns nil on success, the underlying error otherwise (the web layer maps it
// to a fixed-enum banner and never echoes it into the page).
func (a *Applier) TestLLM(ctx context.Context, s Settings) error {
	ctx, cancel := context.WithTimeout(ctx, llmTestTimeout)
	defer cancel()
	c := a.build(s)
	if s.EmbedModel != "" {
		_, err := c.Embed(ctx, []string{"ping"})
		return err
	}
	if s.ChatModel != "" {
		_, err := c.Chat(ctx, ChatRequest{
			Messages:  []Message{{Role: RoleUser, Content: "ping"}},
			MaxTokens: 1,
		})
		return err
	}
	return fmt.Errorf("llm: no embed or facts model configured to test")
}

// build constructs a fresh client from s, reused by ApplyLLM's live swap and
// TestLLM's transient probe so both go through identical Options.
func (a *Applier) build(s Settings) *OpenAIClient {
	return New(Options{
		BaseURL:    s.BaseURL,
		APIKey:     s.APIKey,
		ChatModel:  s.ChatModel,
		EmbedModel: s.EmbedModel,
		Timeout:    a.timeout,
	})
}
