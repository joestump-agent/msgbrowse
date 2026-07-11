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

// checkDeviceSync reports that device sync is not built into this binary. The
// feature is gated behind the `devicesync` build tag (ADR-0021 / SPEC-0014) and
// excluded from release builds; the full check ladder lives in
// doctor_devices.go under that tag.
func checkDeviceSync(_ context.Context, r *report, _ *config.Config, _ *store.Store) {
	r.add(statusPass, "device sync not built into this binary (feature gated behind the `devicesync` build tag)", "")
}
