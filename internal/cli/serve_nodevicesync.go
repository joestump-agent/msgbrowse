//go:build !devicesync

// Default build: the device-sync feature is NOT compiled in. This stub replaces
// serve_devicesync.go so `msgbrowse serve` links WITHOUT internal/devsync or
// internal/syncthing — device sync (ADR-0021 / SPEC-0014) is unfinished and must
// not ship in release binaries. Build with `-tags devicesync` to include it.
package cli

import (
	"context"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/onboard"
	"github.com/joestump/msgbrowse/internal/store"
	"github.com/joestump/msgbrowse/internal/web"
)

// deviceSyncCompiledIn reports that this binary was built WITHOUT the
// device-sync feature; the web UI hides the entire Device sync surface.
const deviceSyncCompiledIn = false

// wireDeviceSync is the no-op seam for builds without the `devicesync` tag: it
// wires nothing and returns an immediately-returning waiter, so serve.go's
// shutdown path is identical whether or not the feature is compiled in. The
// parameters match the real implementation's signature exactly.
func wireDeviceSync(_ context.Context, _ *web.Server, _ *config.Config, _ *store.Store, _ *onboard.Runner) (func() error, error) {
	return func() error { return nil }, nil
}
