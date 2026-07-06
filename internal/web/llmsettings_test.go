package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/llm"
)

// fakeLLMConfigurator is a test double for the LLMConfigurator seam. It
// records every ApplyLLM call so the security tests can assert a rejected
// POST applied NOTHING (the checkSetupPOST contract).
type fakeLLMConfigurator struct {
	cur      llm.Settings
	applied  []llm.Settings
	applyErr error
}

func (f *fakeLLMConfigurator) CurrentLLM() llm.Settings { return f.cur }
func (f *fakeLLMConfigurator) ApplyLLM(s llm.Settings) error {
	if f.applyErr != nil {
		return f.applyErr
	}
	f.applied = append(f.applied, s)
	f.cur = s
	return nil
}

// llmPOST builds a POST /settings/llm with the given origin, token, and form
// values (empty entries omitted), mirroring enablePOST.
func llmPOST(t *testing.T, srv *Server, origin, token string, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	for k, v := range fields {
		form.Set(k, v)
	}
	if token != "" {
		form.Set(setupTokenField, token)
	}
	req := httptest.NewRequest(http.MethodPost, "/settings/llm", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// validLLMForm is a happy-path form body.
func validLLMForm() map[string]string {
	return map[string]string{
		"base_url":    "http://127.0.0.1:11434/v1",
		"embed_model": "nomic-embed-text",
		"facts_model": "llama3",
	}
}

// TestLLMTabRenders is the template acceptance (#191): the tab renders with
// exactly the three fields showing the current effective values, the API-key
// HINT (env var) with no API-key input, and the local-first posture line.
func TestLLMTabRenders(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{cur: llm.Settings{
		BaseURL: "http://llm.test:4000/v1", EmbedModel: "test-embed", ChatModel: "test-chat",
	}}
	srv.SetLLMConfig(fc)

	rec := get(t, srv, "/settings/llm")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()

	// The sub-nav renders with the LLM tab active.
	if !contains(body, `href="/settings/llm" class="settings-tab settings-tab-active"`) {
		t.Error("LLM tab not active on its own page")
	}
	// Exactly the three fields, pre-filled with the current effective values.
	for _, want := range []string{
		`name="base_url"`, `name="embed_model"`, `name="facts_model"`,
		`value="http://llm.test:4000/v1"`, `value="test-embed"`, `value="test-chat"`,
	} {
		if !contains(body, want) {
			t.Errorf("LLM tab missing %q", want)
		}
	}
	// The facts-model hint says what it maps to.
	if !contains(body, "llm.chat_model") {
		t.Error("facts-model field missing its llm.chat_model hint")
	}
	// NO API-key input — only the env-var hint (config.go posture).
	for _, forbidden := range []string{`name="api_key"`, `type="password"`} {
		if contains(body, forbidden) {
			t.Errorf("LLM tab must not render an API-key field, found %q", forbidden)
		}
	}
	if !contains(body, "MSGBROWSE_LLM_API_KEY") {
		t.Error("LLM tab missing the MSGBROWSE_LLM_API_KEY env-var hint")
	}
	// The quiet local-first posture line (ADR-0010).
	if !contains(body, "only network egress") || !contains(body, "ADR-0010") {
		t.Error("LLM tab missing the local-first posture line")
	}
	// The save form is armed with a live setup token.
	if !contains(body, `name="setup_token"`) {
		t.Error("LLM save form missing the setup token")
	}
}

// TestLLMTabShowsBootConfigWithoutConfigurator: with no configurator wired
// (plain NewServer), the tab still renders the boot config's effective
// values — file + defaults merged.
func TestLLMTabShowsBootConfigWithoutConfigurator(t *testing.T) {
	st, cfg, _ := newTestStoreAndConfig(t)
	cfg.LLM.BaseURL = "http://boot.test/v1"
	cfg.LLM.EmbedModel = "boot-embed"
	cfg.LLM.ChatModel = "boot-chat"
	srv, err := NewServer(st, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}

	body := get(t, srv, "/settings/llm").Body.String()
	for _, want := range []string{`value="http://boot.test/v1"`, `value="boot-embed"`, `value="boot-chat"`} {
		if !contains(body, want) {
			t.Errorf("boot-config fallback missing %q", want)
		}
	}
}

// TestLLMTabBoostedPartial: an HX-Request gets the *_content partial —
// <title> + #main-content, no full-document shell — per REQ-0008-006.
func TestLLMTabBoostedPartial(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/settings/llm", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, `id="main-content"`) || !contains(body, "<title>") {
		t.Error("boosted response missing the swap unit")
	}
	if contains(body, "<!doctype html") || contains(body, "<html") {
		t.Error("boosted response carried the full document shell")
	}
}

// TestLLMSaveCrossOriginRejected: a cross-origin POST — even with a valid
// token — is rejected 403 and applies nothing.
func TestLLMSaveCrossOriginRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{}
	srv.SetLLMConfig(fc)

	tok := mintToken(t, srv)
	rec := llmPOST(t, srv, "http://evil.example", tok, validLLMForm())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST status = %d, want 403", rec.Code)
	}
	if len(fc.applied) != 0 {
		t.Fatalf("cross-origin POST applied %d settings, want 0", len(fc.applied))
	}
}

// TestLLMSaveMissingTokenRejected: same-origin but tokenless → 403, nothing
// applied.
func TestLLMSaveMissingTokenRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{}
	srv.SetLLMConfig(fc)

	rec := llmPOST(t, srv, selfOrigin, "", validLLMForm())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing-token POST status = %d, want 403", rec.Code)
	}
	if len(fc.applied) != 0 {
		t.Fatalf("missing-token POST applied %d settings, want 0", len(fc.applied))
	}
}

// TestLLMSaveInvalidTokenRejected: a well-formed but never-minted token → 403.
func TestLLMSaveInvalidTokenRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{}
	srv.SetLLMConfig(fc)

	rec := llmPOST(t, srv, selfOrigin, strings.Repeat("ab", 32), validLLMForm())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("invalid-token POST status = %d, want 403", rec.Code)
	}
	if len(fc.applied) != 0 {
		t.Fatalf("invalid-token POST applied %d settings, want 0", len(fc.applied))
	}
}

// TestLLMSaveGETNeverMutates: the GET route renders and applies nothing, even
// when a token and form-shaped query values ride along.
func TestLLMSaveGETNeverMutates(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{cur: llm.Settings{BaseURL: "http://keep.test/v1"}}
	srv.SetLLMConfig(fc)

	tok := mintToken(t, srv)
	rec := get(t, srv, "/settings/llm?base_url=http://evil.example/v1&setup_token="+tok)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(fc.applied) != 0 {
		t.Fatalf("GET applied %d settings, want 0", len(fc.applied))
	}
	if fc.cur.BaseURL != "http://keep.test/v1" {
		t.Fatalf("GET changed the settings: %+v", fc.cur)
	}
}

// TestLLMSaveValidationRejections: bad scheme, control characters, and
// oversized values re-render with a field error and apply NOTHING.
func TestLLMSaveValidationRejections(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]string
		marker string
	}{
		{"scheme", map[string]string{"base_url": "ftp://files.example/v1"}, "must start with http:// or https://"},
		{"empty base URL", map[string]string{"base_url": ""}, "required"},
		{"no host", map[string]string{"base_url": "http://"}, "does not look like a valid URL"},
		{"control chars in URL", map[string]string{"base_url": "http://ok.test/v1\x00"}, "does not look like a valid URL"},
		{"oversize base URL", map[string]string{"base_url": "http://ok.test/" + strings.Repeat("a", llmBaseURLMaxLen)}, "too long"},
		{"control chars in embed model", map[string]string{"base_url": "http://ok.test/v1", "embed_model": "bad\x1bmodel"}, "characters that are not allowed"},
		{"oversize facts model", map[string]string{"base_url": "http://ok.test/v1", "facts_model": strings.Repeat("m", llmModelMaxLen+1)}, "too long"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _, _ := newTestServer(t)
			fc := &fakeLLMConfigurator{}
			srv.SetLLMConfig(fc)

			tok := mintToken(t, srv)
			rec := llmPOST(t, srv, selfOrigin, tok, c.fields)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (re-rendered form)", rec.Code)
			}
			if len(fc.applied) != 0 {
				t.Fatalf("invalid input applied %d settings, want 0", len(fc.applied))
			}
			body := rec.Body.String()
			if !contains(body, "Nothing was saved.") {
				t.Error("missing the nothing-saved banner")
			}
			if !contains(body, c.marker) {
				t.Errorf("missing field error %q", c.marker)
			}
		})
	}
}

// TestLLMSaveHappyPathWritesExactlyThreeKeys drives the REAL stack — handler
// → llm.Applier → config.SaveLLM → llm.Holder — against a pre-existing config
// file: exactly the three llm keys are written, the unrelated pre-existing
// key survives, and the live holder swaps (#191's no-restart contract).
func TestLLMSaveHappyPathWritesExactlyThreeKeys(t *testing.T) {
	srv, _, _ := newTestServer(t)

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("data_dir: /custom/data\nllm:\n  max_concurrency: 9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	holder := llm.NewHolder(llm.New(llm.Options{BaseURL: "http://old.test/v1"}), llm.Settings{
		BaseURL: "http://old.test/v1", EmbedModel: "old-embed", ChatModel: "old-chat",
	})
	srv.SetLLMConfig(llm.NewApplier(holder, "", 0, func(s llm.Settings) error {
		return config.SaveLLM(path, s.BaseURL, s.EmbedModel, s.ChatModel)
	}))

	tok := mintToken(t, srv)
	rec := llmPOST(t, srv, selfOrigin, tok, map[string]string{
		"base_url":    "  http://127.0.0.1:11434/v1  ", // trimmed
		"embed_model": "nomic-embed-text",
		"facts_model": "llama3",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "LLM settings saved.") {
		t.Error("missing the saved confirmation")
	}
	// The re-rendered form shows the now-effective (trimmed) values.
	if !contains(body, `value="http://127.0.0.1:11434/v1"`) {
		t.Error("saved render missing the trimmed effective base URL")
	}

	// Live swap: the holder now reports the new settings.
	if got := holder.Settings(); got.BaseURL != "http://127.0.0.1:11434/v1" || got.EmbedModel != "nomic-embed-text" || got.ChatModel != "llama3" {
		t.Errorf("holder after save = %+v", got)
	}

	// The file: three keys written, unrelated keys preserved, no api_key.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	for _, want := range []string{
		"base_url: http://127.0.0.1:11434/v1",
		"embed_model: nomic-embed-text",
		"chat_model: llama3",
		"data_dir: /custom/data", // unrelated top-level key preserved
		"max_concurrency: 9",     // unrelated llm key preserved
	} {
		if !contains(out, want) {
			t.Errorf("config file missing %q:\n%s", want, out)
		}
	}
	if contains(out, "api_key") {
		t.Errorf("config file gained an api_key:\n%s", out)
	}
}

// TestLLMSaveEmptyEmbedModelAllowed: an empty embed model is valid and means
// semantic search off.
func TestLLMSaveEmptyEmbedModelAllowed(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{}
	srv.SetLLMConfig(fc)

	tok := mintToken(t, srv)
	rec := llmPOST(t, srv, selfOrigin, tok, map[string]string{
		"base_url":    "http://127.0.0.1:4000/v1",
		"embed_model": "",
		"facts_model": "local-chat",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(fc.applied) != 1 {
		t.Fatalf("applied %d settings, want 1", len(fc.applied))
	}
	if got := fc.applied[0]; got.EmbedModel != "" || got.BaseURL != "http://127.0.0.1:4000/v1" {
		t.Errorf("applied = %+v", got)
	}
	if !contains(rec.Body.String(), "LLM settings saved.") {
		t.Error("missing the saved confirmation")
	}
}

// TestLLMSaveUnavailableWithoutConfigurator: with no configurator wired the
// gate-passing POST changes nothing and says so.
func TestLLMSaveUnavailableWithoutConfigurator(t *testing.T) {
	srv, _, _ := newTestServer(t)
	tok := mintToken(t, srv)
	rec := llmPOST(t, srv, selfOrigin, tok, validLLMForm())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !contains(rec.Body.String(), "Saving is not available here.") {
		t.Error("missing the unavailable banner")
	}
}

// TestLLMSaveApplyErrorReported: a failed persist/swap renders the error
// banner and the running config stays visible as submitted (no fake success).
func TestLLMSaveApplyErrorReported(t *testing.T) {
	srv, _, _ := newTestServer(t)
	fc := &fakeLLMConfigurator{applyErr: errors.New("disk full")}
	srv.SetLLMConfig(fc)

	tok := mintToken(t, srv)
	rec := llmPOST(t, srv, selfOrigin, tok, validLLMForm())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "Saving failed.") {
		t.Error("missing the save-error banner")
	}
	if contains(body, "LLM settings saved.") {
		t.Error("rendered success despite an apply error")
	}
}
