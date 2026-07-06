// Headless end-to-end coverage for the desktop LLM settings wiring (issue
// #191): the embedded server serves the Settings → LLM tab, and a gated save
// over the real loopback socket persists the three llm keys into the loaded
// config file. Pure Go, CGO_ENABLED=0, no webview.
package embedded

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// setupTokenRe extracts the per-session token the rendered LLM tab arms its
// save form with (256-bit hex, the internal/web mint format).
var setupTokenRe = regexp.MustCompile(`name="setup_token" value="([0-9a-f]{64})"`)

// TestLLMSettingsSaveEndToEnd drives the full desktop-mode stack: GET the tab
// off the embedded listener, submit the save through the same-origin + token
// gate, and confirm the three keys landed in the config file the process
// loaded (cfg.SourceFile) — the stage-A acceptance for #191.
func TestLLMSettingsSaveEndToEnd(t *testing.T) {
	cfg := testConfig(t)
	cfg.SourceFile = filepath.Join(t.TempDir(), "config.yaml")

	ctx, cancel := context.WithCancel(context.Background())
	es, err := Start(ctx, cfg, testLogger())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := es.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	// The tab renders with its three fields and no API-key input.
	resp, err := http.Get(es.URL + "/settings/llm")
	if err != nil {
		t.Fatalf("GET /settings/llm: %v", err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /settings/llm status = %d", resp.StatusCode)
	}
	for _, want := range []string{`name="base_url"`, `name="embed_model"`, `name="facts_model"`, "MSGBROWSE_LLM_API_KEY"} {
		if !strings.Contains(string(page), want) {
			t.Errorf("LLM tab missing %q", want)
		}
	}
	if strings.Contains(string(page), `name="api_key"`) {
		t.Error("LLM tab rendered an api_key input")
	}
	m := setupTokenRe.FindSubmatch(page)
	if m == nil {
		t.Fatal("LLM tab carries no setup token")
	}

	// Save through the gate: same-origin POST with the minted token.
	form := url.Values{
		"setup_token": {string(m[1])},
		"base_url":    {"http://127.0.0.1:11434/v1"},
		"embed_model": {"nomic-embed-text"},
		"facts_model": {"llama3"},
	}
	req, err := http.NewRequest(http.MethodPost, es.URL+"/settings/llm", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", es.URL)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /settings/llm: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /settings/llm status = %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "LLM settings saved.") {
		t.Fatalf("save did not confirm: %s", body)
	}

	// The loaded config file gained exactly the three llm keys.
	saved, err := os.ReadFile(cfg.SourceFile)
	if err != nil {
		t.Fatalf("read saved config: %v", err)
	}
	for _, want := range []string{
		"base_url: http://127.0.0.1:11434/v1",
		"embed_model: nomic-embed-text",
		"chat_model: llama3",
	} {
		if !strings.Contains(string(saved), want) {
			t.Errorf("saved config missing %q:\n%s", want, saved)
		}
	}
	if strings.Contains(string(saved), "api_key") {
		t.Errorf("saved config gained an api_key:\n%s", saved)
	}

	// And a cross-origin replay with a fresh valid token is rejected 403 —
	// the desktop listener enforces the same gate as `serve`.
	resp, err = http.Get(es.URL + "/settings/llm")
	if err != nil {
		t.Fatal(err)
	}
	page, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	m = setupTokenRe.FindSubmatch(page)
	if m == nil {
		t.Fatal("re-rendered tab carries no setup token")
	}
	form.Set("setup_token", string(m[1]))
	form.Set("base_url", "http://evil.example/v1")
	req, _ = http.NewRequest(http.MethodPost, es.URL+"/settings/llm", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://evil.example")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin save status = %d, want 403", resp.StatusCode)
	}
	if saved2, _ := os.ReadFile(cfg.SourceFile); strings.Contains(string(saved2), "evil.example") {
		t.Error("cross-origin save mutated the config file")
	}
}
