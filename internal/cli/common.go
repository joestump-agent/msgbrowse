package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/joestump/msgbrowse/internal/archivepath"
	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/llm"
	"github.com/joestump/msgbrowse/internal/setup"
	"github.com/joestump/msgbrowse/internal/store"
)

// dbFileName is the SQLite database file within the data directory.
const dbFileName = store.DBFileName

// dbPath returns the absolute path to the SQLite database for the given config.
func dbPath(cfg *config.Config) string {
	return filepath.Join(cfg.DataDir, dbFileName)
}

// openStore ensures the data directory exists and opens the database. Callers own
// Close. The data directory is created (the archive is never written to).
func openStore(cfg *config.Config) (*store.Store, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir %q: %w", cfg.DataDir, err)
	}
	return store.Open(dbPath(cfg))
}

// newLLMClient builds the OpenAI-compatible LLM client from config. This is the
// only component that performs network egress.
func newLLMClient(cfg *config.Config) *llm.OpenAIClient {
	return llm.New(llm.Options{
		BaseURL:    cfg.LLM.BaseURL,
		APIKey:     cfg.LLM.APIKey,
		ChatModel:  cfg.LLM.ChatModel,
		EmbedModel: cfg.LLM.EmbedModel,
		Timeout:    cfg.LLM.Timeout,
	})
}

// newLLMHolder wraps the config-built client in the process's swappable
// holder (issue #191): every consumer reads the CURRENT client and model
// names through it, so a Settings → LLM save applies live in `serve` and the
// desktop shell, and `mcp` shares the identical wiring shape (it just never
// swaps — a standalone process re-reads config at start).
func newLLMHolder(cfg *config.Config) *llm.Holder {
	return llm.NewHolder(newLLMClient(cfg), llm.Settings{
		BaseURL:    cfg.LLM.BaseURL,
		EmbedModel: cfg.LLM.EmbedModel,
		ChatModel:  cfg.LLM.ChatModel,
	})
}

// llmConfigSavePath resolves where the Settings → LLM tab persists (#191):
// the config file this process actually loaded, else the standard per-user
// location config.Load searches ($HOME/.config/msgbrowse/config.yaml) so the
// created file is found on the next start. Never "." (cwd-dependent) and
// never /etc (root-owned).
func llmConfigSavePath(cfg *config.Config) (string, error) {
	if cfg.SourceFile != "" {
		return cfg.SourceFile, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir for config save: %w", err)
	}
	return filepath.Join(home, ".config", "msgbrowse", "config.yaml"), nil
}

// newLLMApplier builds the web layer's LLMConfigurator over holder: persist
// the three llm keys into the resolved config file, then swap the live
// client. The API key stays the boot-resolved value (MSGBROWSE_LLM_API_KEY /
// config, per the config posture) — it is not editable from the tab.
func newLLMApplier(cfg *config.Config, holder *llm.Holder) *llm.Applier {
	return llm.NewApplier(holder, cfg.LLM.APIKey, cfg.LLM.Timeout, func(s llm.Settings) error {
		path, err := llmConfigSavePath(cfg)
		if err != nil {
			return err
		}
		return config.SaveLLM(path, s.BaseURL, s.EmbedModel, s.ChatModel)
	})
}

// archiveRoots bundles the EFFECTIVE per-source archive roots for
// archivepath.Resolve callers (media transcoding, doctor sampling): the
// configured root when set, else the app-owned managed root
// (<data_dir>/archives/<source>) when it exists on disk (issue #160 — a
// desktop-onboarded data dir has managed roots and empty cfg roots, and its
// media must still resolve for `msgbrowse media` / `doctor`).
func archiveRoots(cfg *config.Config) archivepath.Roots {
	return setup.EffectiveRoots(cfg)
}

// requireArchive verifies the archive root is configured and present.
func requireArchive(cfg *config.Config) error {
	return requireDir("archive_root", "MSGBROWSE_ARCHIVE_ROOT", cfg.ArchiveRoot)
}

// requireIMessageArchive verifies the iMessage archive root is configured and present.
func requireIMessageArchive(cfg *config.Config) error {
	return requireDir("imessage_archive_root", "MSGBROWSE_IMESSAGE_ARCHIVE_ROOT", cfg.IMessageArchiveRoot)
}

// requireWhatsAppArchive verifies the WhatsApp archive root is configured and present.
func requireWhatsAppArchive(cfg *config.Config) error {
	return requireDir("whatsapp_archive_root", "MSGBROWSE_WHATSAPP_ARCHIVE_ROOT", cfg.WhatsAppArchiveRoot)
}

func requireDir(key, env, path string) error {
	if path == "" {
		return fmt.Errorf("%s is not set (use --%s, config, or %s)", key, strings.ReplaceAll(key, "_", "-"), env)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s %q: %w", key, path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s %q is not a directory", key, path)
	}
	return nil
}
