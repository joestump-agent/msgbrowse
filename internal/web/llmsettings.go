// The Settings → LLM tab (issue #191, stage A): the user-facing surface for
// the AI endpoint — base URL, embed model, and "Facts model" (llm.chat_model;
// the facts feature consumes it today, the journal digest later). Exactly
// three fields, and deliberately NO API-key field: per internal/config's
// posture, endpoints that require a key read it from the MSGBROWSE_LLM_API_KEY
// environment variable, so a key never round-trips through a form or the
// config file. Saving applies LIVE — the wired LLMConfigurator persists the
// three keys into the loaded config file and swaps the process's llm.Holder,
// so the MCP server's semantic search uses the new endpoint with no restart.
//
// The save POST is privileged (it changes the app's single network egress,
// ADR-0010) and is gated exactly like the Setup POSTs: same-origin +
// per-session token + MaxBytesReader, rejected 403 before any work
// (checkSetupPOST, SPEC-0013 §Security). Validation failures re-render the
// tab with fixed-enum field errors; the only request-derived strings in the
// render are the echoed form values, which html/template escapes like all
// message content.
package web

import (
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/joestump/msgbrowse/internal/llm"
)

// llmBaseURLMaxLen / llmModelMaxLen bound the accepted field lengths — far
// beyond any real endpoint or model name, small enough to reject garbage.
const (
	llmBaseURLMaxLen = 2048
	llmModelMaxLen   = 128
)

// LLMConfigurator is the live-settings seam behind the LLM tab (the
// SetDetector/SetEnabler pattern): serve and the desktop shell wire an
// llm.Applier over the process's shared llm.Holder; tests wire fakes. With no
// configurator wired the tab still renders (showing the boot config values)
// but a save reports itself unavailable rather than pretending.
type LLMConfigurator interface {
	// CurrentLLM returns the effective settings behind the live client
	// (config file + defaults merged, as loaded or last applied).
	CurrentLLM() llm.Settings
	// ApplyLLM persists s to the config file and swaps the live client.
	// Nothing is swapped when persistence fails.
	ApplyLLM(s llm.Settings) error
}

// SetLLMConfig wires the live LLM settings source. Call it after NewServer
// and before serving begins — handlers read the field without locking, so
// late wiring would race.
func (s *Server) SetLLMConfig(c LLMConfigurator) { s.llmConfig = c }

// llmSettingsData drives the LLM tab. Field errors and SaveResult are fixed
// enums mapped to prose by the template; BaseURL/EmbedModel/FactsModel echo
// the submitted values on a validation failure (html/template-escaped) so the
// user can correct instead of retyping.
type llmSettingsData struct {
	baseData
	BaseURL    string
	EmbedModel string
	FactsModel string
	// SetupToken is the per-session token the form submits through the same
	// checkSetupPOST gate the Setup POSTs use.
	SetupToken string
	// SaveResult is the post-save banner state: "" (no save attempted), "ok",
	// "unavailable" (no configurator wired), or "error" (persist/swap failed).
	SaveResult string
	// Per-field validation errors: "" (valid), "required", "scheme",
	// "invalid", or "toolong".
	ErrBaseURL    string
	ErrEmbedModel string
	ErrFactsModel string
}

// HasErrors reports whether any field failed validation, for the template's
// summary banner.
func (d llmSettingsData) HasErrors() bool {
	return d.ErrBaseURL != "" || d.ErrEmbedModel != "" || d.ErrFactsModel != ""
}

// currentLLM resolves the effective settings for display: the live
// configurator when wired, else the boot-time config snapshot.
func (s *Server) currentLLM() llm.Settings {
	if s.llmConfig != nil {
		return s.llmConfig.CurrentLLM()
	}
	return s.llmBoot
}

// handleSettingsLLM renders the LLM tab (GET /settings/llm) with the current
// effective values. Safe GET: no mutation; the minted token arms the save
// form.
func (s *Server) handleSettingsLLM(w http.ResponseWriter, r *http.Request) {
	cur := s.currentLLM()
	s.renderLLMSettings(w, r, llmSettingsData{
		BaseURL:    cur.BaseURL,
		EmbedModel: cur.EmbedModel,
		FactsModel: cur.ChatModel,
	})
}

// handleSettingsLLMSave is POST /settings/llm — the privileged save. Gate
// first (checkSetupPOST: same-origin + per-session token + body cap, 403
// before any work), validate second, and only then apply: persist the three
// keys and swap the live client. Success re-renders the tab with the saved
// banner and the now-effective values (the boosted-partial pattern — htmx
// swaps #main-content; a plain form POST gets the full document).
func (s *Server) handleSettingsLLMSave(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return // 403 already written; nothing was validated or applied
	}

	data := llmSettingsData{
		BaseURL:    strings.TrimSpace(r.PostFormValue("base_url")),
		EmbedModel: strings.TrimSpace(r.PostFormValue("embed_model")),
		FactsModel: strings.TrimSpace(r.PostFormValue("facts_model")),
	}
	data.ErrBaseURL = validateLLMBaseURL(data.BaseURL)
	data.ErrEmbedModel = validateLLMModel(data.EmbedModel)
	data.ErrFactsModel = validateLLMModel(data.FactsModel)
	if data.HasErrors() {
		s.renderLLMSettings(w, r, data)
		return
	}

	if s.llmConfig == nil {
		data.SaveResult = "unavailable"
		s.renderLLMSettings(w, r, data)
		return
	}
	if err := s.llmConfig.ApplyLLM(llm.Settings{
		BaseURL:    data.BaseURL,
		EmbedModel: data.EmbedModel,
		ChatModel:  data.FactsModel,
	}); err != nil {
		s.log.Error("LLM settings save failed", "error", err)
		data.SaveResult = "error"
		s.renderLLMSettings(w, r, data)
		return
	}
	// Endpoint/model names are configuration, never message content — safe to
	// log (the API key is not part of Settings at all).
	s.log.Info("LLM settings saved and applied live",
		"base_url", data.BaseURL, "embed_model", data.EmbedModel, "chat_model", data.FactsModel)

	cur := s.llmConfig.CurrentLLM()
	s.renderLLMSettings(w, r, llmSettingsData{
		BaseURL:    cur.BaseURL,
		EmbedModel: cur.EmbedModel,
		FactsModel: cur.ChatModel,
		SaveResult: "ok",
	})
}

// renderLLMSettings finishes any LLM-tab response: shell (full or boosted
// partial), fresh per-session token, render.
func (s *Server) renderLLMSettings(w http.ResponseWriter, r *http.Request, data llmSettingsData) {
	const title = "LLM · msgbrowse"
	if isPartialRequest(r) {
		data.baseData = partialBase(title, 0)
	} else {
		base, err := s.baseData(r.Context(), title, 0)
		if err != nil {
			s.serverError(w, err)
			return
		}
		data.baseData = base
	}
	tok, err := s.setupTokens.mint()
	if err != nil {
		s.serverError(w, err)
		return
	}
	data.SetupToken = tok
	s.render(w, r, "llmsettings", data)
}

// validateLLMBaseURL checks the endpoint URL: required, bounded length, no
// control characters, and it must parse with an http/https scheme and a host.
// Returns "" or a fixed error enum ("required" / "toolong" / "invalid" /
// "scheme").
func validateLLMBaseURL(raw string) string {
	if raw == "" {
		return "required"
	}
	if len(raw) > llmBaseURLMaxLen {
		return "toolong"
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return "invalid"
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid"
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "scheme"
	}
	if u.Host == "" {
		return "invalid"
	}
	return ""
}

// validateLLMModel checks a model name: bounded length, printable runes only.
// Empty is VALID — an empty embed model means semantic search off, an empty
// facts model means the facts/journal features are unconfigured. Returns ""
// or a fixed error enum ("toolong" / "invalid").
func validateLLMModel(m string) string {
	if len(m) > llmModelMaxLen {
		return "toolong"
	}
	for _, r := range m {
		if !unicode.IsPrint(r) {
			return "invalid"
		}
	}
	return ""
}
