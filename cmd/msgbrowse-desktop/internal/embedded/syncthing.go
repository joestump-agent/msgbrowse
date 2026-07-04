// Device-sync wiring for the desktop shell: when device_sync.enabled is true,
// the embedded server starts the Syncthing supervisor (internal/syncthing)
// alongside the web UI, resolving the engine binary the ADR-0020 way — from
// the .app bundle (Contents/Resources/tools/syncthing, version-pinned via
// tools/syncthing.version) and NEVER from $PATH. Only the non-bundled build
// (the Linux desktop binary or a dev `go run`) falls back to the
// device_sync.syncthing_bin config key and then $PATH, mirroring the
// exporters' bring-your-own path.
//
// Pure Go (no Wails import, no cgo) so the resolution logic is exercised by
// the desktop module's CGO_ENABLED=0 headless suite on Linux.
//
// Governing: ADR-0021 ("bundle + supervise"), SPEC-0014 REQ "Bundled
// Syncthing Runtime" ("resolve the Syncthing binary from the bundle and MUST
// NOT resolve it from $PATH"), REQ "Supervised Daemon Lifecycle".
package embedded

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"

	"github.com/joestump/msgbrowse/cmd/msgbrowse-desktop/internal/toolchain"
	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/syncthing"
)

// resolvedSyncthing is the outcome of binary resolution: the path to run,
// the pinned version to enforce (bundled only; empty for BYO), and whether
// it came from the bundle.
type resolvedSyncthing struct {
	Bin           string
	PinnedVersion string
	Bundled       bool
}

// resolveSyncthing resolves the Syncthing binary for the desktop shell:
//
//   - In a macOS .app (Locate succeeds): the bundled tools/syncthing plus its
//     build-time version pin. The bundle is the ONLY source — a missing
//     bundled binary or pin is a hard typed error, never a $PATH fallback
//     (SPEC-0014 "Fresh Mac with no Syncthing installed still syncs" — and
//     its inverse: the bundle never silently defers to a system copy).
//   - Not in a .app (ErrNotBundled): the bring-your-own fallback `serve`
//     uses — device_sync.syncthing_bin, then $PATH — so the Linux desktop
//     build and dev runs behave like the CLI.
//
// execPath is injected (os.Executable() in production) so this is
// unit-testable on Linux with a faked bundle.
func resolveSyncthing(execPath string, cfg *config.Config) (resolvedSyncthing, error) {
	r, err := toolchain.Locate(execPath)
	if err == nil {
		bin, perr := r.SyncthingPath()
		if perr != nil {
			return resolvedSyncthing{}, perr
		}
		pin, perr := r.SyncthingVersionPin()
		if perr != nil {
			return resolvedSyncthing{}, perr
		}
		return resolvedSyncthing{Bin: bin, PinnedVersion: pin, Bundled: true}, nil
	}
	if !errors.Is(err, toolchain.ErrNotBundled) {
		return resolvedSyncthing{}, err
	}
	// Non-bundled build: BYO, exactly like `msgbrowse serve`.
	if bin := cfg.DeviceSync.SyncthingBin; bin != "" {
		return resolvedSyncthing{Bin: bin}, nil
	}
	bin, lerr := exec.LookPath("syncthing")
	if lerr != nil {
		return resolvedSyncthing{}, fmt.Errorf("device sync start failed: %w: install syncthing or set device_sync.syncthing_bin",
			syncthing.ErrBinaryNotFound)
	}
	return resolvedSyncthing{Bin: bin}, nil
}

// startDeviceSync starts the supervised Syncthing engine for the desktop
// shell when device_sync.enabled is true; with sync disabled (the default)
// it returns (nil, nil) and starts nothing — no process, no P2P listener
// (SPEC-0014 "Device sync disabled means no Syncthing process"). The
// returned supervisor is stopped by cancelling ctx; Close waits for its
// drain.
func startDeviceSync(ctx context.Context, cfg *config.Config, log *slog.Logger) (*syncthing.Supervisor, error) {
	if !cfg.DeviceSync.Enabled {
		return nil, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("device sync start failed: resolve executable: %w", err)
	}
	res, err := resolveSyncthing(exe, cfg)
	if err != nil {
		return nil, err
	}
	folders, err := syncthing.ExistingManagedFolders(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("device sync start failed: %w", err)
	}
	sup, err := syncthing.New(syncthing.Options{
		BinPath:       res.Bin,
		PinnedVersion: res.PinnedVersion,
		DataDir:       cfg.DataDir,
		ListenAddr:    cfg.DeviceSync.ListenAddr,
		DeviceName:    cfg.DeviceSync.DeviceName,
		Folders:       folders,
		Logger:        log,
	})
	if err != nil {
		return nil, err
	}
	if err := sup.Start(ctx); err != nil {
		return nil, err
	}
	log.Info("device sync engine started", "api_addr", sup.APIAddr(), "bundled", res.Bundled)
	return sup, nil
}
