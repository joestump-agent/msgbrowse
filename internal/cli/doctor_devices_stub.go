//go:build !devicesync

// Default build: device sync is not compiled in (see serve_nodevicesync.go), so
// doctor's device-sync condition ladder is replaced by a single informational
// line. This keeps `msgbrowse doctor` linking without internal/devices or
// internal/syncthing while still telling the operator why there is no sync
// posture to report.
package cli

import (
	"context"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/store"
)

// checkDeviceSync reports on device sync for a binary built WITHOUT the
// `devicesync` feature (ADR-0021 / SPEC-0014); the full check ladder lives in
// doctor_devices.go under that tag. When the config does not ask for sync this
// is an informational PASS. But when device_sync.enabled is true the operator
// asked for something this build cannot deliver — a silent PASS would hide a
// misconfiguration — so it WARNS with the remedy (rebuild with the tag, or use
// the desktop app).
func checkDeviceSync(_ context.Context, r *report, cfg *config.Config, _ *store.Store) {
	if cfg != nil && cfg.DeviceSync.Enabled {
		r.add(statusWarn, "device_sync.enabled is set but this binary was built without the device-sync feature — no sync engine runs",
			"Rebuild with `-tags devicesync` or use the desktop app; otherwise set device_sync.enabled: false to silence this.")
		return
	}
	r.add(statusPass, "device sync not built into this binary (feature gated behind the `devicesync` build tag)", "")
}
