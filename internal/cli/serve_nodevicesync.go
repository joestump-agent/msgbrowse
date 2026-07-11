//go:build !devicesync

// Default build: the device-sync feature is NOT wired in. This stub replaces
// serve_devicesync.go so `msgbrowse serve` never starts a Syncthing engine or
// pairing manager — device sync (ADR-0021 / SPEC-0014) is unfinished and must
// not run in release builds. Build with `-tags devicesync` to activate it.
//
// Scope of the gate (honest boundary): this tag governs RUNTIME wiring and the
// UI surface (deviceSyncCompiledIn=false hides the Device sync pages). It does
// NOT keep the internal/devsync or internal/syncthing packages out of the
// binary — internal/web imports internal/devsync unconditionally (for the
// status/settings/logs types), so both packages remain in the compiled dep
// graph regardless of this tag. True binary-level exclusion would require
// build-tagging the web layer's devsync references as well; see ADR-0021.
package cli

import (
	"context"
	"log/slog"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/onboard"
	"github.com/joestump/msgbrowse/internal/store"
	"github.com/joestump/msgbrowse/internal/web"
)

// deviceSyncCompiledIn reports that this binary was built WITHOUT the
// device-sync feature wired in; the web UI hides the entire Device sync surface.
const deviceSyncCompiledIn = false

// wireDeviceSync is the seam for builds without the `devicesync` tag: it wires
// nothing and returns an immediately-returning waiter, so serve.go's shutdown
// path is identical whether or not the feature is active. The parameters match
// the real implementation's signature exactly.
//
// If the config explicitly asks for device sync (device_sync.enabled: true)
// this build cannot honor it, so it WARNS loudly rather than silently doing
// nothing — the operator flipped a switch that has no effect here and needs to
// know they must run a `-tags devicesync` build (or the desktop app).
func wireDeviceSync(_ context.Context, _ *web.Server, cfg *config.Config, _ *store.Store, _ *onboard.Runner) (func() error, error) {
	if cfg != nil && cfg.DeviceSync.Enabled {
		slog.Warn("device_sync.enabled is set, but this binary was built without the device-sync feature; " +
			"no sync engine will start. Rebuild with `-tags devicesync` or use the desktop app to enable it.")
	}
	return func() error { return nil }, nil
}
