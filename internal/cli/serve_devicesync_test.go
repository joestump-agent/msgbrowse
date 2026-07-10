//go:build devicesync

// Tests for the device-sync wiring compiled only under the `devicesync` build
// tag (serve_devicesync.go). They exercise startDeviceSync's disabled/fail-fast
// behavior and the Syncthing binary resolution; the untagged build has no such
// code, so these live behind the same tag.
package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/syncthing"
)

// testDeviceCfg builds a device-sync-enabled config over a temp data dir.
func testDeviceCfg(t *testing.T, name string) *config.Config {
	t.Helper()
	return &config.Config{
		DataDir:    t.TempDir(),
		ListenAddr: "127.0.0.1:8787",
		LogLevel:   "error",
		DeviceSync: config.DeviceSyncConfig{
			Enabled:    true,
			ListenAddr: "127.0.0.1:0",
			DeviceName: name,
		},
	}
}

// TestStartDeviceSyncDisabled: with device_sync.enabled=false (the default),
// startDeviceSync starts nothing — no Syncthing process, no worker — keeping
// the socket inventory exactly the loopback web UI (SPEC-0014 "Device sync
// disabled means no Syncthing process"). Governing: ADR-0021.
func TestStartDeviceSyncDisabled(t *testing.T) {
	cfg := testDeviceCfg(t, "disabled-test")
	cfg.DeviceSync.Enabled = false
	w, err := startDeviceSync(context.Background(), cfg, nil, nil)
	if err != nil {
		t.Fatalf("startDeviceSync: %v", err)
	}
	if w != nil {
		t.Fatal("startDeviceSync started a worker with device sync disabled")
	}
	if _, statErr := os.Stat(filepath.Join(cfg.DataDir, syncthing.HomeDirName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("a syncthing home dir was provisioned with device sync disabled")
	}
}

// TestStartDeviceSyncMissingBinaryFailsFast: the operator explicitly enabled
// sync, so an unresolvable Syncthing binary aborts serve with the typed
// sentinel rather than degrading silently (SPEC-0014 REQ "Error Handling
// Standards"). The supervised lifecycle itself (start/stop/no-orphan/drain)
// is proven against a fake binary in internal/syncthing's suite.
func TestStartDeviceSyncMissingBinaryFailsFast(t *testing.T) {
	cfg := testDeviceCfg(t, "missing-bin-test")
	t.Setenv("PATH", t.TempDir()) // no syncthing anywhere
	w, err := startDeviceSync(context.Background(), cfg, nil, nil)
	if !errors.Is(err, syncthing.ErrBinaryNotFound) {
		t.Fatalf("err = %v, want syncthing.ErrBinaryNotFound", err)
	}
	if w != nil {
		t.Fatal("worker returned despite the missing binary")
	}
}

// TestResolveSyncthingBin: the device_sync.syncthing_bin config key wins;
// with it empty, `syncthing` is looked up on $PATH (the bring-your-own CLI
// path, mirroring the exporter *_bin keys — the desktop .app never resolves
// via $PATH). Governing: ADR-0021, SPEC-0014 REQ "Bundled Syncthing Runtime".
func TestResolveSyncthingBin(t *testing.T) {
	t.Run("config key wins", func(t *testing.T) {
		cfg := testDeviceCfg(t, "bin-key")
		cfg.DeviceSync.SyncthingBin = "/opt/custom/syncthing"
		got, err := resolveSyncthingBin(cfg)
		if err != nil || got != "/opt/custom/syncthing" {
			t.Fatalf("resolveSyncthingBin = %q, %v", got, err)
		}
	})
	t.Run("path fallback", func(t *testing.T) {
		dir := t.TempDir()
		fake := filepath.Join(dir, "syncthing")
		if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", dir)
		cfg := testDeviceCfg(t, "bin-path")
		got, err := resolveSyncthingBin(cfg)
		if err != nil || got != fake {
			t.Fatalf("resolveSyncthingBin = %q, %v (want %q)", got, err, fake)
		}
	})
	t.Run("miss is typed", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		cfg := testDeviceCfg(t, "bin-miss")
		_, err := resolveSyncthingBin(cfg)
		if !errors.Is(err, syncthing.ErrBinaryNotFound) {
			t.Fatalf("err = %v, want syncthing.ErrBinaryNotFound", err)
		}
	})
}
