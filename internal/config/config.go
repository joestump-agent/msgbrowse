// Package config defines sigbrowse's configuration model and the Viper binding
// that loads it from (in increasing order of precedence) built-in defaults, a
// YAML config file, SIGBROWSE_* environment variables, and command-line flags.
//
// Secrets (notably the LLM API key) are never read from the config file in a way
// that would encourage committing them; prefer the SIGBROWSE_LLM_API_KEY
// environment variable. See SECURITY.md for the egress and secret-handling model.
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config is the fully-resolved runtime configuration for every sigbrowse
// subcommand. Field tags map each key to its Viper/YAML name.
type Config struct {
	// ArchiveRoot is the path to the signal-export archive. It is treated as
	// strictly read-only; sigbrowse never writes inside it.
	ArchiveRoot string `mapstructure:"archive_root"`

	// DataDir is a writable directory (outside the archive) for the SQLite
	// database, vector index, and caches.
	DataDir string `mapstructure:"data_dir"`

	// ListenAddr is the web UI bind address. It defaults to loopback; binding to
	// a non-loopback interface is an explicit, deliberate choice.
	ListenAddr string `mapstructure:"listen_addr"`

	// LLM configures the single OpenAI-compatible provider used for embeddings,
	// RAG synthesis, and journal digests.
	LLM LLMConfig `mapstructure:"llm"`

	// VectorBackend selects the vector store: "sqlite-vec" (default) or "qdrant".
	VectorBackend string `mapstructure:"vector_backend"`

	// Journal configures journal generation and the LLM digest pass.
	Journal JournalConfig `mapstructure:"journal"`

	// IngestOnStart triggers an ingest pass when `serve` boots.
	IngestOnStart bool `mapstructure:"ingest_on_start"`

	// Watch enables the fsnotify watcher inside `serve` (equivalent to running
	// `sigbrowse watch` alongside the server).
	Watch bool `mapstructure:"watch"`

	// LogLevel is one of debug, info, warn, error.
	LogLevel string `mapstructure:"log_level"`
}

// LLMConfig configures the OpenAI-compatible client. BaseURL is the only network
// egress sigbrowse performs; by default it points at a local LiteLLM proxy.
type LLMConfig struct {
	BaseURL        string        `mapstructure:"base_url"`
	APIKey         string        `mapstructure:"api_key"`
	ChatModel      string        `mapstructure:"chat_model"`
	EmbedModel     string        `mapstructure:"embed_model"`
	MaxConcurrency int           `mapstructure:"max_concurrency"`
	Timeout        time.Duration `mapstructure:"timeout"`
}

// JournalConfig configures `sigbrowse journal`.
type JournalConfig struct {
	// DigestEnabled turns the LLM digest pass on or off. The mechanical journal
	// is always written regardless.
	DigestEnabled bool `mapstructure:"digest_enabled"`

	// DigestPrompt is the system/instruction prompt used for the digest pass.
	// Changing it bumps the effective prompt version and invalidates the cache.
	DigestPrompt string `mapstructure:"digest_prompt"`

	// ExcludeConversations is a denylist of conversation folder names whose
	// content is NEVER sent to the LLM (privacy control).
	ExcludeConversations []string `mapstructure:"exclude_conversations"`

	// MaxDaysPerRun caps how many days a single digest run will process.
	MaxDaysPerRun int `mapstructure:"max_days_per_run"`
}

// DefaultDigestPrompt is the built-in journal digest instruction. Its text is
// part of the digest cache key (prompt version), so edits here are intentional.
const DefaultDigestPrompt = "You are summarizing one day of a personal Signal message archive. " +
	"Write a concise digest with: a 1-3 sentence summary, the key people involved, " +
	"a short list of themes/tags, and any notable decisions or links. " +
	"Be factual and neutral; do not invent details that are not in the transcript."

// SetDefaults registers every default value on the given Viper instance. It is
// the single source of truth for built-in defaults and is also used by tests.
func SetDefaults(v *viper.Viper) {
	v.SetDefault("archive_root", "")
	v.SetDefault("data_dir", "./data")
	v.SetDefault("listen_addr", "127.0.0.1:8787")

	v.SetDefault("llm.base_url", "http://127.0.0.1:4000/v1")
	v.SetDefault("llm.api_key", "")
	v.SetDefault("llm.chat_model", "gpt-4o-mini")
	v.SetDefault("llm.embed_model", "text-embedding-3-small")
	v.SetDefault("llm.max_concurrency", 4)
	v.SetDefault("llm.timeout", 60*time.Second)

	v.SetDefault("vector_backend", "sqlite-vec")

	v.SetDefault("journal.digest_enabled", true)
	v.SetDefault("journal.digest_prompt", DefaultDigestPrompt)
	v.SetDefault("journal.exclude_conversations", []string{})
	v.SetDefault("journal.max_days_per_run", 0) // 0 = unbounded

	v.SetDefault("ingest_on_start", false)
	v.SetDefault("watch", false)
	v.SetDefault("log_level", "info")
}

// Load constructs a *viper.Viper wired for sigbrowse: defaults, optional config
// file, and SIGBROWSE_* environment variables. cfgFile may be empty, in which
// case the standard search paths are used. Flags are bound separately by the CLI
// layer via BindPFlags.
func Load(cfgFile string) (*viper.Viper, error) {
	v := viper.New()
	SetDefaults(v)

	v.SetEnvPrefix("SIGBROWSE")
	// Map e.g. SIGBROWSE_LLM_API_KEY -> llm.api_key.
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if cfgFile != "" {
		v.SetConfigFile(cfgFile)
	} else {
		v.SetConfigName("config")
		v.SetConfigType("yaml")
		v.AddConfigPath(".")
		v.AddConfigPath("$HOME/.config/sigbrowse")
		v.AddConfigPath("/etc/sigbrowse")
	}

	if err := v.ReadInConfig(); err != nil {
		// A missing config file is fine; defaults + env + flags still apply.
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("reading config: %w", err)
		}
	}

	return v, nil
}

// Unmarshal materializes a Config from the given Viper instance.
func Unmarshal(v *viper.Viper) (*Config, error) {
	var c Config
	if err := v.Unmarshal(&c); err != nil {
		return nil, fmt.Errorf("decoding config: %w", err)
	}
	return &c, nil
}

// Validate checks the resolved configuration for the invariants every subcommand
// relies on. It does not require the archive to exist for commands that do not
// read it; callers that need the archive should check ArchiveRoot themselves.
func (c *Config) Validate() error {
	switch c.VectorBackend {
	case "sqlite-vec", "qdrant":
	default:
		return fmt.Errorf("invalid vector_backend %q (want sqlite-vec or qdrant)", c.VectorBackend)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid log_level %q", c.LogLevel)
	}
	if c.DataDir == "" {
		return fmt.Errorf("data_dir must not be empty")
	}
	return nil
}
