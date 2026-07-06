package llm

import (
	"context"
	"errors"
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
	a := NewApplier(h, "", 0, func(s Settings) error {
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
	a := NewApplier(h, "", 0, func(Settings) error { return errors.New("disk full") })

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
