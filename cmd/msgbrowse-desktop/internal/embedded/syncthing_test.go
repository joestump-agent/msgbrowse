// Headless tests for the desktop shell's Syncthing binary resolution: bundled
// resolution from a faked .app (never $PATH), the typed error on a bundle
// missing its engine or version pin, and the BYO fallback for the non-bundled
// build. The supervised lifecycle itself is proven in internal/syncthing's
// suite against a fake daemon.
//
// Governing: ADR-0021, SPEC-0014 REQ "Bundled Syncthing Runtime".
package embedded

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/syncthing"
)

// fakeApp materializes Contents/MacOS/msgbrowse + Contents/Resources/tools
// with a syncthing stub and version pin, returning the fake executable path.
func fakeApp(t *testing.T, withBinary, withPin bool) string {
	t.Helper()
	root := t.TempDir()
	macos := filepath.Join(root, "msgbrowse.app", "Contents", "MacOS")
	tools := filepath.Join(root, "msgbrowse.app", "Contents", "Resources", "tools")
	for _, d := range []string{macos, tools} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	exe := filepath.Join(macos, "msgbrowse")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if withBinary {
		if err := os.WriteFile(filepath.Join(tools, "syncthing"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if withPin {
		if err := os.WriteFile(filepath.Join(tools, "syncthing.version"), []byte("v2.1.1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return exe
}

func TestResolveSyncthingBundled(t *testing.T) {
	exe := fakeApp(t, true, true)
	cfg := &config.Config{}
	// Even with a syncthing on $PATH, the bundle must win and $PATH must
	// never be consulted (SPEC-0014: "MUST NOT resolve it from $PATH").
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "syncthing"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	res, err := resolveSyncthing(exe, cfg)
	if err != nil {
		t.Fatalf("resolveSyncthing: %v", err)
	}
	if !res.Bundled {
		t.Error("Bundled = false inside a .app, want true")
	}
	wantBin := filepath.Join(filepath.Dir(filepath.Dir(exe)), "Resources", "tools", "syncthing")
	if res.Bin != wantBin {
		t.Errorf("Bin = %q, want the bundled %q", res.Bin, wantBin)
	}
	if res.PinnedVersion != "v2.1.1" {
		t.Errorf("PinnedVersion = %q, want v2.1.1", res.PinnedVersion)
	}
}

func TestResolveSyncthingBundleMissingPinIsTypedError(t *testing.T) {
	exe := fakeApp(t, true, false)
	if _, err := resolveSyncthing(exe, &config.Config{}); err == nil {
		t.Fatal("resolveSyncthing with no version pin: want error (the pin is part of the bundle integrity contract), got nil")
	}
}

func TestResolveSyncthingNotBundledFallsBackToConfigThenPath(t *testing.T) {
	nonBundled := filepath.Join("/usr", "local", "bin", "msgbrowse")

	t.Run("config key wins", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.DeviceSync.SyncthingBin = "/opt/custom/syncthing"
		res, err := resolveSyncthing(nonBundled, cfg)
		if err != nil || res.Bin != "/opt/custom/syncthing" || res.Bundled {
			t.Fatalf("resolveSyncthing = %+v, %v", res, err)
		}
		if res.PinnedVersion != "" {
			t.Error("BYO resolution must not carry a version pin")
		}
	})
	t.Run("path fallback", func(t *testing.T) {
		dir := t.TempDir()
		fake := filepath.Join(dir, "syncthing")
		if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
		res, err := resolveSyncthing(nonBundled, &config.Config{})
		if err != nil || res.Bin != fake {
			t.Fatalf("resolveSyncthing = %+v, %v (want %q)", res, err, fake)
		}
	})
	t.Run("miss is typed", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, err := resolveSyncthing(nonBundled, &config.Config{})
		if !errors.Is(err, syncthing.ErrBinaryNotFound) {
			t.Fatalf("err = %v, want syncthing.ErrBinaryNotFound", err)
		}
	})
}

// TestStartDeviceSyncDisabledDesktop: the desktop shell starts no engine with
// sync disabled (SPEC-0014 "Device sync disabled means no Syncthing process").
func TestStartDeviceSyncDisabledDesktop(t *testing.T) {
	cfg := &config.Config{DataDir: t.TempDir()}
	sup, err := startDeviceSync(context.Background(), cfg, testLogger())
	if err != nil {
		t.Fatalf("startDeviceSync: %v", err)
	}
	if sup != nil {
		t.Fatal("supervisor started with device sync disabled")
	}
	if _, statErr := os.Stat(filepath.Join(cfg.DataDir, syncthing.HomeDirName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("a syncthing home dir was provisioned with device sync disabled")
	}
}
