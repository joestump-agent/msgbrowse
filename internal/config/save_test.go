package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// TestSaveLLMCreatesMinimalFile: with no config file present, SaveLLM creates
// one containing ONLY the llm block with the three user-editable keys — never
// a dump of the full default config, and never an api_key.
func TestSaveLLMCreatesMinimalFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	if err := SaveLLM(path, "http://127.0.0.1:11434/v1", "nomic-embed-text", "llama3"); err != nil {
		t.Fatalf("SaveLLM: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse back: %v", err)
	}
	if len(doc) != 1 {
		t.Errorf("fresh file has %d top-level keys, want just llm: %v", len(doc), doc)
	}
	llmBlock, _ := doc["llm"].(map[string]any)
	if llmBlock == nil {
		t.Fatalf("no llm block in %s", b)
	}
	if len(llmBlock) != 3 {
		t.Errorf("llm block has %d keys, want exactly 3: %v", len(llmBlock), llmBlock)
	}
	if llmBlock["base_url"] != "http://127.0.0.1:11434/v1" ||
		llmBlock["embed_model"] != "nomic-embed-text" ||
		llmBlock["chat_model"] != "llama3" {
		t.Errorf("llm block = %v", llmBlock)
	}
	if _, ok := llmBlock["api_key"]; ok {
		t.Error("SaveLLM wrote an api_key — it must never do that")
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("config file mode = %o, want 600", perm)
		}
	}
}

// TestSaveLLMPreservesUnrelatedKeys is the surgical-merge contract: only the
// three llm.* keys change; every unrelated key — top-level and inside the llm
// block (including a pre-existing api_key) — round-trips unchanged.
func TestSaveLLMPreservesUnrelatedKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	existing := "" +
		"data_dir: /var/lib/msgbrowse\n" +
		"listen_addr: 127.0.0.1:9999\n" +
		"llm:\n" +
		"  base_url: http://old.invalid/v1\n" +
		"  api_key: synthetic-placeholder\n" +
		"  max_concurrency: 7\n" +
		"journal:\n" +
		"  digest_enabled: false\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	// Empty embed model is allowed and means semantic search off.
	if err := SaveLLM(path, "https://llm.example/v1", "", "facts-model"); err != nil {
		t.Fatalf("SaveLLM: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse back: %v", err)
	}

	if doc["data_dir"] != "/var/lib/msgbrowse" || doc["listen_addr"] != "127.0.0.1:9999" {
		t.Errorf("unrelated top-level keys not preserved: %v", doc)
	}
	journal, _ := doc["journal"].(map[string]any)
	if journal == nil || journal["digest_enabled"] != false {
		t.Errorf("journal block not preserved: %v", doc["journal"])
	}
	llmBlock, _ := doc["llm"].(map[string]any)
	if llmBlock == nil {
		t.Fatalf("llm block missing: %s", b)
	}
	if llmBlock["base_url"] != "https://llm.example/v1" || llmBlock["embed_model"] != "" || llmBlock["chat_model"] != "facts-model" {
		t.Errorf("llm keys not updated: %v", llmBlock)
	}
	// Pre-existing keys INSIDE the llm block survive too.
	if llmBlock["api_key"] != "synthetic-placeholder" {
		t.Errorf("pre-existing llm.api_key not preserved: %v", llmBlock["api_key"])
	}
	if llmBlock["max_concurrency"] != 7 {
		t.Errorf("pre-existing llm.max_concurrency not preserved: %v", llmBlock["max_concurrency"])
	}
}

// TestSaveLLMRoundTripsThroughLoad: a saved file loads back through the real
// config loader with the saved values effective — including the empty embed
// model overriding the built-in default (semantic search off).
func TestSaveLLMRoundTripsThroughLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := SaveLLM(path, "http://127.0.0.1:8080/v1", "", "my-chat"); err != nil {
		t.Fatalf("SaveLLM: %v", err)
	}
	v, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg, err := Unmarshal(v)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if cfg.LLM.BaseURL != "http://127.0.0.1:8080/v1" {
		t.Errorf("BaseURL = %q", cfg.LLM.BaseURL)
	}
	if cfg.LLM.EmbedModel != "" {
		t.Errorf("EmbedModel = %q, want empty (explicitly saved empty must beat the default)", cfg.LLM.EmbedModel)
	}
	if cfg.LLM.ChatModel != "my-chat" {
		t.Errorf("ChatModel = %q", cfg.LLM.ChatModel)
	}
	// Untouched keys keep their defaults — the file froze nothing else.
	if cfg.LLM.MaxConcurrency != 4 {
		t.Errorf("MaxConcurrency = %d, want default 4", cfg.LLM.MaxConcurrency)
	}
	// And the loader recorded which file it read (the save-back target).
	if cfg.SourceFile != path {
		t.Errorf("SourceFile = %q, want %q", cfg.SourceFile, path)
	}
}

// TestSaveLLMRejectsMalformedExisting: a file that is not YAML is an error,
// not a silent overwrite.
func TestSaveLLMRejectsMalformedExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("{not yaml: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	err := SaveLLM(path, "http://x.invalid/v1", "e", "c")
	if err == nil {
		t.Fatal("SaveLLM should refuse to overwrite a file it cannot parse")
	}
	if !strings.Contains(err.Error(), "parse existing config") {
		t.Errorf("error = %v, want a parse error", err)
	}
	// The malformed original is untouched.
	b, _ := os.ReadFile(path)
	if string(b) != "{not yaml: [" {
		t.Errorf("original file was modified: %q", b)
	}
}
