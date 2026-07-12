package llm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// markerClient is a fake Client whose Embed result identifies the instance,
// so tests can prove which client a Holder delegates to.
type markerClient struct{ marker float32 }

func (m markerClient) Embed(_ context.Context, in []string) ([][]float32, error) {
	out := make([][]float32, len(in))
	for i := range in {
		out[i] = []float32{m.marker}
	}
	return out, nil
}
func (m markerClient) Chat(context.Context, ChatRequest) (string, error) { return "", nil }
func (m markerClient) Transcribe(context.Context, []byte, string) (string, error) {
	return "", nil
}
func (m markerClient) Vision(context.Context, []byte, string, string) (string, error) {
	return "", nil
}

// TestHolderSwapChangesClientAndSettings is the live-swap contract (#191):
// calls delegate to client A, then — after Swap — to client B, and the model
// getters follow. This is what makes a Settings → LLM save apply with no
// restart.
func TestHolderSwapChangesClientAndSettings(t *testing.T) {
	h := NewHolder(markerClient{marker: 1}, Settings{
		BaseURL: "http://a.invalid/v1", EmbedModel: "embed-a", ChatModel: "chat-a",
	})

	vecs, err := h.Embed(context.Background(), []string{"x"})
	if err != nil {
		t.Fatalf("embed via holder: %v", err)
	}
	if vecs[0][0] != 1 {
		t.Fatalf("holder delegated to the wrong client: got marker %v, want 1", vecs[0][0])
	}
	if h.EmbedModel() != "embed-a" || h.ChatModel() != "chat-a" {
		t.Fatalf("getters = %q/%q, want embed-a/chat-a", h.EmbedModel(), h.ChatModel())
	}

	h.Swap(markerClient{marker: 2}, Settings{
		BaseURL: "http://b.invalid/v1", EmbedModel: "embed-b", ChatModel: "chat-b",
	})

	vecs, err = h.Embed(context.Background(), []string{"x"})
	if err != nil {
		t.Fatalf("embed after swap: %v", err)
	}
	if vecs[0][0] != 2 {
		t.Fatalf("holder still delegates to the old client after Swap: marker %v", vecs[0][0])
	}
	if h.EmbedModel() != "embed-b" || h.ChatModel() != "chat-b" {
		t.Errorf("getters after swap = %q/%q, want embed-b/chat-b", h.EmbedModel(), h.ChatModel())
	}
	if got := h.Settings().BaseURL; got != "http://b.invalid/v1" {
		t.Errorf("Settings().BaseURL after swap = %q", got)
	}
}

// TestApplierPersistsThenSwaps: ApplyLLM persists FIRST and swaps only on
// persist success, so a failed config write leaves the running provider
// untouched.
func TestApplierPersistsThenSwaps(t *testing.T) {
	h := NewHolder(markerClient{marker: 1}, Settings{BaseURL: "http://old.invalid/v1", EmbedModel: "old-embed"})

	var persisted []Settings
	a := NewApplier(h, 0, func(s Settings) error {
		persisted = append(persisted, s)
		return nil
	})

	next := Settings{BaseURL: "http://new.invalid/v1", EmbedModel: "new-embed", ChatModel: "new-chat"}
	if err := a.ApplyLLM(next); err != nil {
		t.Fatalf("ApplyLLM: %v", err)
	}
	if len(persisted) != 1 || persisted[0] != next {
		t.Fatalf("persisted = %+v, want exactly the applied settings", persisted)
	}
	if a.CurrentLLM() != next {
		t.Fatalf("CurrentLLM = %+v, want %+v", a.CurrentLLM(), next)
	}
	// The rebuilt client targets the new endpoint (the OpenAIClient the
	// Applier constructs), not the old marker client.
	if _, ok := h.current().(markerClient); ok {
		t.Error("holder still holds the pre-apply client after a successful ApplyLLM")
	}
}

// TestApplierPersistFailureLeavesHolderUntouched: no swap on a failed write.
func TestApplierPersistFailureLeavesHolderUntouched(t *testing.T) {
	before := Settings{BaseURL: "http://old.invalid/v1", EmbedModel: "old-embed"}
	h := NewHolder(markerClient{marker: 1}, before)
	a := NewApplier(h, 0, func(Settings) error { return errors.New("disk full") })

	err := a.ApplyLLM(Settings{BaseURL: "http://new.invalid/v1"})
	if err == nil {
		t.Fatal("ApplyLLM should surface the persist error")
	}
	if h.Settings() != before {
		t.Errorf("settings changed despite persist failure: %+v", h.Settings())
	}
	if _, ok := h.current().(markerClient); !ok {
		t.Error("client swapped despite persist failure")
	}
}

// TestApplierTestLLMProbesBothModels: with BOTH an embed and a facts model set,
// TestLLM probes EACH — the embeddings endpoint and the chat endpoint — so a
// valid embed model cannot mask a broken facts model. It returns nil on success
// WITHOUT swapping the live client (the holder keeps its marker).
func TestApplierTestLLMProbesBothModels(t *testing.T) {
	hits := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path] = true
		if r.URL.Path == "/v1/chat/completions" {
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[0.1,0.2]}]}`)
	}))
	defer srv.Close()

	h := NewHolder(markerClient{marker: 7}, Settings{BaseURL: "http://old.invalid/v1"})
	a := NewApplier(h, 0, nil)
	err := a.TestLLM(context.Background(), Settings{
		BaseURL: srv.URL + "/v1", EmbedModel: "probe-embed", ChatModel: "probe-chat",
	})
	if err != nil {
		t.Fatalf("TestLLM = %v, want nil", err)
	}
	if !hits["/v1/embeddings"] {
		t.Error("probe did not hit /v1/embeddings")
	}
	if !hits["/v1/chat/completions"] {
		t.Error("probe did not hit /v1/chat/completions")
	}
	// The probe must not swap the live client.
	if _, ok := h.current().(markerClient); !ok {
		t.Error("TestLLM swapped the live client")
	}
	if h.Settings().BaseURL != "http://old.invalid/v1" {
		t.Errorf("TestLLM changed the live settings: %+v", h.Settings())
	}
}

// TestApplierTestLLMFactsModelError: a valid embed model must NOT mask a broken
// facts model — TestLLM surfaces the chat probe's error even when the embed
// probe succeeds.
func TestApplierTestLLMFactsModelError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[0.1,0.2]}]}`)
	}))
	defer srv.Close()

	a := NewApplier(NewHolder(markerClient{marker: 1}, Settings{}), 0, nil)
	if err := a.TestLLM(context.Background(), Settings{
		BaseURL: srv.URL + "/v1", EmbedModel: "probe-embed", ChatModel: "probe-chat",
	}); err == nil {
		t.Error("TestLLM should surface a broken facts model even when the embed model is valid")
	}
}

// TestApplierTestLLMChatProbe: with only a facts (chat) model set, TestLLM
// probes /chat/completions instead of /embeddings.
func TestApplierTestLLMChatProbe(t *testing.T) {
	var hitPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer srv.Close()

	a := NewApplier(NewHolder(markerClient{marker: 1}, Settings{}), 0, nil)
	if err := a.TestLLM(context.Background(), Settings{BaseURL: srv.URL + "/v1", ChatModel: "probe-chat"}); err != nil {
		t.Fatalf("TestLLM = %v, want nil", err)
	}
	if hitPath != "/v1/chat/completions" {
		t.Errorf("probe hit %q, want /v1/chat/completions", hitPath)
	}
}

// TestApplierTestLLMSurfacesError: a 5xx from the endpoint surfaces as an error.
func TestApplierTestLLMSurfacesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := NewApplier(NewHolder(markerClient{marker: 1}, Settings{}), 0, nil)
	if err := a.TestLLM(context.Background(), Settings{BaseURL: srv.URL + "/v1", EmbedModel: "probe-embed"}); err == nil {
		t.Error("TestLLM should surface a 5xx as an error")
	}
}

// TestApplierTestLLMNoModel: with neither model set there is nothing to probe.
func TestApplierTestLLMNoModel(t *testing.T) {
	a := NewApplier(NewHolder(markerClient{marker: 1}, Settings{}), 0, nil)
	if err := a.TestLLM(context.Background(), Settings{BaseURL: "http://x.invalid/v1"}); err == nil {
		t.Error("TestLLM with no models should error")
	}
}
