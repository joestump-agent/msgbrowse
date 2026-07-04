package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	v, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Unmarshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:8787" {
		t.Errorf("ListenAddr = %q, want loopback default", cfg.ListenAddr)
	}
	// Every source root defaults to unset ("source skipped").
	if cfg.ArchiveRoot != "" || cfg.IMessageArchiveRoot != "" || cfg.WhatsAppArchiveRoot != "" {
		t.Errorf("archive roots should default empty, got %q/%q/%q",
			cfg.ArchiveRoot, cfg.IMessageArchiveRoot, cfg.WhatsAppArchiveRoot)
	}
	if cfg.VectorBackend != "sqlite-vec" {
		t.Errorf("VectorBackend = %q, want sqlite-vec", cfg.VectorBackend)
	}
	if cfg.LLM.ChatModel != "local-chat" {
		t.Errorf("LLM.ChatModel = %q, want local-first default local-chat", cfg.LLM.ChatModel)
	}
	if cfg.LLM.EmbedModel != "local-embed" {
		t.Errorf("LLM.EmbedModel = %q, want local-first default local-embed", cfg.LLM.EmbedModel)
	}
	if cfg.LLM.Timeout != 60*time.Second {
		t.Errorf("LLM.Timeout = %v, want 60s", cfg.LLM.Timeout)
	}
	if !cfg.Journal.DigestEnabled {
		t.Error("Journal.DigestEnabled should default true")
	}
	if cfg.Journal.DigestPrompt != DefaultDigestPrompt {
		t.Error("Journal.DigestPrompt should default to DefaultDigestPrompt")
	}
	// Device sync (ADR-0021) MUST default off: absent config means no sync
	// engine, no P2P listener, and the loopback-only posture unchanged
	// (SPEC-0014).
	if cfg.DeviceSync.Enabled {
		t.Error("DeviceSync.Enabled should default false (strictly opt-in)")
	}
	if cfg.DeviceSync.ListenAddr != ":8788" {
		t.Errorf("DeviceSync.ListenAddr = %q, want :8788 (distinct from web UI)", cfg.DeviceSync.ListenAddr)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("MSGBROWSE_LISTEN_ADDR", "127.0.0.1:9999")
	t.Setenv("MSGBROWSE_LLM_API_KEY", "secret-from-env")
	t.Setenv("MSGBROWSE_LOG_LEVEL", "debug")
	t.Setenv("MSGBROWSE_WHATSAPP_ARCHIVE_ROOT", "/wapp-from-env")

	v, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Unmarshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1:9999" {
		t.Errorf("ListenAddr = %q, want env override", cfg.ListenAddr)
	}
	if cfg.LLM.APIKey != "secret-from-env" {
		t.Errorf("LLM.APIKey = %q, want env value", cfg.LLM.APIKey)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	// REQ-0009-001: the WhatsApp root has the same env mapping as the others.
	if cfg.WhatsAppArchiveRoot != "/wapp-from-env" {
		t.Errorf("WhatsAppArchiveRoot = %q, want env value", cfg.WhatsAppArchiveRoot)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"defaults ok", func(*Config) {}, false},
		{"bad vector backend", func(c *Config) { c.VectorBackend = "pinecone" }, true},
		{"bad log level", func(c *Config) { c.LogLevel = "trace" }, true},
		{"empty data dir", func(c *Config) { c.DataDir = "" }, true},
		{"device sync enabled with defaults ok", func(c *Config) { c.DeviceSync.Enabled = true }, false},
		{"device sync empty listen addr", func(c *Config) {
			c.DeviceSync.Enabled = true
			c.DeviceSync.ListenAddr = ""
		}, true},
		{"device sync colliding with web ui addr", func(c *Config) {
			c.DeviceSync.Enabled = true
			c.DeviceSync.ListenAddr = c.ListenAddr
		}, true},
		// Port collisions must be caught across different SPELLINGS of the
		// same port — naive string equality misses these (#115 review).
		{"device sync wildcard host colliding with web ui port", func(c *Config) {
			c.DeviceSync.Enabled = true
			c.ListenAddr = "127.0.0.1:8787"
			c.DeviceSync.ListenAddr = ":8787"
		}, true},
		{"device sync explicit host colliding with web ui port", func(c *Config) {
			c.DeviceSync.Enabled = true
			c.ListenAddr = ":8787"
			c.DeviceSync.ListenAddr = "192.168.1.10:8787"
		}, true},
		{"device sync distinct port on same host ok", func(c *Config) {
			c.DeviceSync.Enabled = true
			c.ListenAddr = "127.0.0.1:8787"
			c.DeviceSync.ListenAddr = "127.0.0.1:8788"
		}, false},
		{"device sync unparseable listen addr", func(c *Config) {
			c.DeviceSync.Enabled = true
			c.DeviceSync.ListenAddr = "not-an-addr"
		}, true},
		{"device sync non-numeric port", func(c *Config) {
			c.DeviceSync.Enabled = true
			c.DeviceSync.ListenAddr = "127.0.0.1:http"
		}, true},
		{"device sync disabled skips its checks", func(c *Config) {
			c.DeviceSync.Enabled = false
			c.DeviceSync.ListenAddr = ""
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, _ := Load("")
			cfg, _ := Unmarshal(v)
			tt.mutate(cfg)
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
