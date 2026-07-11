package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/llm"
)

// TestNewLLMHolderMarksEnvKey: when MSGBROWSE_LLM_API_KEY is set, the boot
// holder records APIKeyFromEnv so the save path knows the key is env-provided.
func TestNewLLMHolderMarksEnvKey(t *testing.T) {
	t.Setenv(llmAPIKeyEnvVar, "sk-from-env")
	cfg := &config.Config{}
	cfg.LLM.APIKey = "sk-from-env" // viper's AutomaticEnv would have folded this in
	if got := newLLMHolder(cfg).Settings(); !got.APIKeyFromEnv || got.APIKey != "sk-from-env" {
		t.Errorf("holder settings = %+v, want env-sourced key", got)
	}

	os.Unsetenv(llmAPIKeyEnvVar)
	cfg2 := &config.Config{}
	cfg2.LLM.APIKey = "sk-in-config"
	if got := newLLMHolder(cfg2).Settings(); got.APIKeyFromEnv {
		t.Errorf("holder settings = %+v, want APIKeyFromEnv=false for a config key", got)
	}
}

// TestNewLLMApplierSuppressesEnvKeyOnDisk is the end-to-end env-key-leak proof:
// applying settings whose key is env-sourced writes a BLANK api_key to the
// config file (the env stays the sole home of the secret) while the live client
// still runs on the key. A non-env key is written normally.
func TestNewLLMApplierSuppressesEnvKeyOnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := &config.Config{SourceFile: path}

	holder := newLLMHolder(cfg)
	applier := newLLMApplier(cfg, holder)

	// Env-sourced key: used live, never persisted.
	if err := applier.ApplyLLM(llm.Settings{
		BaseURL: "http://127.0.0.1:4000/v1", APIKey: "sk-from-env", APIKeyFromEnv: true,
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if out := string(raw); strings.Contains(out, "sk-from-env") {
		t.Errorf("env key leaked to config file:\n%s", out)
	}
	if got := holder.Settings().APIKey; got != "sk-from-env" {
		t.Errorf("live key = %q, want the env key kept live", got)
	}

	// A user-typed (non-env) key IS persisted.
	if err := applier.ApplyLLM(llm.Settings{
		BaseURL: "http://127.0.0.1:4000/v1", APIKey: "sk-typed", APIKeyFromEnv: false,
	}); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if out := string(raw); !strings.Contains(out, "sk-typed") {
		t.Errorf("typed key not persisted:\n%s", out)
	}
}
