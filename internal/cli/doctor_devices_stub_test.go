//go:build !devicesync

// Default-build doctor device-sync row: an informational PASS when sync is off,
// but a WARN when device_sync.enabled is set on a binary that cannot honor it —
// a silent PASS there would hide a real misconfiguration (review fix).
package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/config"
)

func TestCheckDeviceSyncStubDisabledIsPass(t *testing.T) {
	var buf bytes.Buffer
	r := &report{w: &buf}
	checkDeviceSync(context.Background(), r, &config.Config{}, nil)
	if r.warnings != 0 || r.fails != 0 {
		t.Errorf("disabled sync should be a clean PASS, got %d warnings / %d fails", r.warnings, r.fails)
	}
	if !strings.Contains(buf.String(), "not built into this binary") {
		t.Errorf("missing the informational line:\n%s", buf.String())
	}
}

func TestCheckDeviceSyncStubEnabledWarns(t *testing.T) {
	var buf bytes.Buffer
	r := &report{w: &buf}
	cfg := &config.Config{}
	cfg.DeviceSync.Enabled = true
	checkDeviceSync(context.Background(), r, cfg, nil)
	if r.warnings != 1 {
		t.Errorf("device_sync.enabled on a no-feature build should WARN once, got %d warnings", r.warnings)
	}
	out := buf.String()
	if !strings.Contains(out, "device_sync.enabled is set") || !strings.Contains(out, "devicesync") {
		t.Errorf("warning missing the misconfiguration explanation + remedy:\n%s", out)
	}
}
