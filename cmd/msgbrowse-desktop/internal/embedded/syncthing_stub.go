//go:build !devicesync

// Default desktop build: device sync is NOT compiled in. This stub replaces
// syncthing.go so the embedded server links WITHOUT internal/devsync or
// internal/syncthing — device sync (ADR-0021 / SPEC-0014) is unfinished and must
// not ship. Build with `-tags devicesync` to include it.
package embedded

import (
	"context"
	"log/slog"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/onboard"
	"github.com/joestump/msgbrowse/internal/store"
	"github.com/joestump/msgbrowse/internal/web"
)

// deviceSyncCompiledIn reports that this desktop binary was built WITHOUT the
// device-sync feature; the web UI hides the entire Device sync surface.
const deviceSyncCompiledIn = false

// wireDeviceSync is the no-op seam for builds without the `devicesync` tag: it
// starts nothing and returns a nil handle, so embedded.go's serve and drain
// paths are identical whether or not the feature is compiled in.
func wireDeviceSync(_ context.Context, _ *web.Server, _ *config.Config, _ *store.Store, _ *onboard.Runner, _ *slog.Logger) (deviceSyncHandle, error) {
	return nil, nil
}
