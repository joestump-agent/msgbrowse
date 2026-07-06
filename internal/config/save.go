// Save-back for the Settings → LLM tab (#191): persist EXACTLY the three
// user-editable llm keys into the YAML config file, touching nothing else.
//
// The write is a surgical merge, never a dump: the existing file is parsed
// into a generic map, only llm.base_url / llm.embed_model / llm.chat_model
// are set, and every other key — including a pre-existing llm.api_key, which
// this code never writes — round-trips unchanged. A missing file is created
// containing only the llm block, so the built-in defaults stay defaults
// instead of being frozen into the file. The write is atomic (temp file +
// rename, 0600) so a crash mid-save can never leave a truncated config.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	yaml "go.yaml.in/yaml/v3"
)

// SaveLLM merges the three user-configurable LLM keys into the YAML config
// file at path, creating the file (and its directory) if absent. It writes
// ONLY llm.base_url, llm.embed_model, and llm.chat_model; unrelated keys and
// any existing llm.api_key are preserved verbatim, and no api_key is ever
// written (the MSGBROWSE_LLM_API_KEY env var is the supported key channel).
func SaveLLM(path, baseURL, embedModel, chatModel string) error {
	doc := map[string]any{}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("parse existing config %q: %w", path, err)
		}
		if doc == nil {
			doc = map[string]any{}
		}
	case os.IsNotExist(err):
		// No file yet: create one holding only the llm block below.
	default:
		return fmt.Errorf("read config %q: %w", path, err)
	}

	llmBlock, ok := doc["llm"].(map[string]any)
	if !ok || llmBlock == nil {
		llmBlock = map[string]any{}
	}
	llmBlock["base_url"] = baseURL
	llmBlock["embed_model"] = embedModel
	llmBlock["chat_model"] = chatModel
	doc["llm"] = llmBlock

	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	return atomicWrite(path, out)
}

// atomicWrite lands data at path via a same-directory temp file + rename, so
// readers only ever see the old or the complete new file. The temp file is
// created 0600 (os.CreateTemp's mode) — the config can carry paths and, if a
// user chose to put one there, an API key.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config dir %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.yaml")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	// Any failure below removes the temp file; the real config is untouched.
	fail := func(op string, err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("%s temp config: %w", op, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail("write", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("sync", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace config %q: %w", path, err)
	}
	return nil
}
