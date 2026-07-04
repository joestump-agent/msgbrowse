package setup

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/source"
)

// TestEffectiveRootPrecedence pins the issue-#160 resolution order: an explicit
// cfg root always wins; with no cfg root the managed root is used only when it
// exists on disk as a directory; otherwise the source stays unconfigured.
func TestEffectiveRootPrecedence(t *testing.T) {
	dataDir := t.TempDir()
	cfgRoot := t.TempDir()

	// Provision only the signal managed root; leave imessage's absent.
	signalManaged, err := ManagedRoot(dataDir, source.Signal)
	if err != nil {
		t.Fatalf("managed root: %v", err)
	}
	if err := os.MkdirAll(signalManaged, 0o700); err != nil {
		t.Fatal(err)
	}

	t.Run("cfg root beats managed", func(t *testing.T) {
		cfg := &config.Config{DataDir: dataDir, ArchiveRoot: cfgRoot}
		if got := EffectiveRoot(cfg, source.Signal); got != cfgRoot {
			t.Errorf("EffectiveRoot = %q, want configured %q", got, cfgRoot)
		}
	})

	t.Run("managed used when cfg empty and dir exists", func(t *testing.T) {
		cfg := &config.Config{DataDir: dataDir}
		if got := EffectiveRoot(cfg, source.Signal); got != signalManaged {
			t.Errorf("EffectiveRoot = %q, want managed %q", got, signalManaged)
		}
	})

	t.Run("managed ignored when missing on disk", func(t *testing.T) {
		cfg := &config.Config{DataDir: dataDir}
		if got := EffectiveRoot(cfg, source.IMessage); got != "" {
			t.Errorf("EffectiveRoot = %q, want empty (managed root absent)", got)
		}
	})

	t.Run("managed ignored when it is a file", func(t *testing.T) {
		fileDataDir := t.TempDir()
		waManaged, err := ManagedRoot(fileDataDir, source.WhatsApp)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(waManaged), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(waManaged, []byte("not a dir"), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := &config.Config{DataDir: fileDataDir}
		if got := EffectiveRoot(cfg, source.WhatsApp); got != "" {
			t.Errorf("EffectiveRoot = %q, want empty (managed root is a file)", got)
		}
	})

	t.Run("unknown source is empty", func(t *testing.T) {
		cfg := &config.Config{DataDir: dataDir}
		if got := EffectiveRoot(cfg, "telegram"); got != "" {
			t.Errorf("EffectiveRoot = %q, want empty for unknown source", got)
		}
	})

	t.Run("empty data dir never guesses", func(t *testing.T) {
		cfg := &config.Config{}
		if got := EffectiveRoot(cfg, source.Signal); got != "" {
			t.Errorf("EffectiveRoot = %q, want empty with no data dir", got)
		}
	})
}

// TestEffectiveRootsBundle covers the Roots bundle: mixed configured + managed
// + absent sources resolve independently.
func TestEffectiveRootsBundle(t *testing.T) {
	dataDir := t.TempDir()
	imRoot := t.TempDir()
	waManaged, err := ManagedRoot(dataDir, source.WhatsApp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(waManaged, 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{DataDir: dataDir, IMessageArchiveRoot: imRoot}
	roots := EffectiveRoots(cfg)
	if roots.Signal != "" {
		t.Errorf("Signal = %q, want empty (no cfg root, no managed dir)", roots.Signal)
	}
	if roots.IMessage != imRoot {
		t.Errorf("IMessage = %q, want configured %q", roots.IMessage, imRoot)
	}
	if roots.WhatsApp != waManaged {
		t.Errorf("WhatsApp = %q, want managed %q", roots.WhatsApp, waManaged)
	}
}
