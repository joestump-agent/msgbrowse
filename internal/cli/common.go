package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/llm"
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

// requireArchive verifies the archive root is configured and present.
func requireArchive(cfg *config.Config) error {
	return requireDir("archive_root", "MSGBROWSE_ARCHIVE_ROOT", cfg.ArchiveRoot)
}

// requireIMessageArchive verifies the iMessage archive root is configured and present.
func requireIMessageArchive(cfg *config.Config) error {
	return requireDir("imessage_archive_root", "MSGBROWSE_IMESSAGE_ARCHIVE_ROOT", cfg.IMessageArchiveRoot)
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
