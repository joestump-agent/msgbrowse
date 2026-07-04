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
	"github.com/joestump/msgbrowse/internal/devsync"
	"github.com/joestump/msgbrowse/internal/onboard"
	"github.com/joestump/msgbrowse/internal/store"
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

// deviceSync is the running device-sync stack the embedded server owns: the
// supervised engine, the pairing manager (/settings' PairingSource), and the
// folder-watch → re-ingest worker (issue #157).
type deviceSync struct {
	Sup     *syncthing.Supervisor
	Manager *devsync.Manager
	Watcher *devsync.Watcher
	// Notes is the shared device-sync event feed the Logs page renders (#158).
	Notes *devsync.Notes
}

// startDeviceSync starts the supervised Syncthing engine for the desktop
// shell when device_sync.enabled is true, plus the msgbrowse-owned layers on
// top: the pairing manager behind /settings and the folder-watch worker that
// dispatches incremental imports through the shared onboard Runner (same
// per-source job guard, same Logs surface — SPEC-0014 REQ "Re-ingest
// Trigger", REQ "Concurrency Safety"). Paired peers from the repurposed
// paired_devices registry are folded into the generated config on every
// start (devsync.ApplyPeers), so pairing survives restarts.
//
// With sync disabled (the default) it returns (nil, nil) and starts nothing
// — no process, no P2P listener (SPEC-0014 "Device sync disabled means no
// Syncthing process"). The returned stack is stopped by cancelling ctx;
// Close waits for its drain.
func startDeviceSync(ctx context.Context, cfg *config.Config, st *store.Store, runner *onboard.Runner, log *slog.Logger) (*deviceSync, error) {
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
	existing, err := syncthing.ExistingManagedFolders(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("device sync start failed: %w", err)
	}
	// The LIVE managed-folder set, shared by the pairing manager and the
	// watcher: pairing can provision a managed root a fresh replica lacks,
	// and the watcher must see it immediately (SPEC-0014 REQ "Importer and
	// Replica Roles").
	folderSet, err := devsync.NewFolderSet(cfg.DataDir, existing)
	if err != nil {
		return nil, fmt.Errorf("device sync start failed: %w", err)
	}
	peers, err := st.ListSyncPeers(ctx)
	if err != nil {
		return nil, fmt.Errorf("device sync start failed: load paired devices: %w", err)
	}
	folders, peerDevices := devsync.ApplyPeers(existing, peers)
	sup, err := syncthing.New(syncthing.Options{
		BinPath:       res.Bin,
		PinnedVersion: res.PinnedVersion,
		DataDir:       cfg.DataDir,
		ListenAddr:    cfg.DeviceSync.ListenAddr,
		DeviceName:    cfg.DeviceSync.DeviceName,
		Folders:       folders,
		Devices:       peerDevices,
		Logger:        log,
	})
	if err != nil {
		return nil, err
	}
	if err := sup.Start(ctx); err != nil {
		return nil, err
	}

	client := sup.Client()
	// The payload's friendly name mirrors the supervisor's own-device naming:
	// configured name, else hostname (SPEC-0014 pairing payload "friendly
	// device introduction").
	name := cfg.DeviceSync.DeviceName
	if name == "" {
		if host, herr := os.Hostname(); herr == nil && host != "" {
			name = host
		}
	}
	// One shared event ring: the Manager (pair/unpair) and Watcher (imports,
	// accepted offers, peer connects) record into it; the Logs page reads it
	// (#158; SPEC-0014 REQ "Status and Doctor Surfacing").
	notes := devsync.NewNotes(0)
	manager := devsync.NewManager(client, st, name, folderSet, log)
	manager.SetNotes(notes)
	watcher, err := devsync.NewWatcher(devsync.WatcherOptions{
		API:      client,
		Store:    st,
		Importer: runner,
		Folders:  folderSet,
		Notes:    notes,
		Logger:   log,
	})
	if err != nil {
		return nil, fmt.Errorf("device sync start failed: %w", err)
	}
	watcher.Start(ctx)

	log.Info("device sync engine started", "api_addr", sup.APIAddr(), "bundled", res.Bundled)
	return &deviceSync{Sup: sup, Manager: manager, Watcher: watcher, Notes: notes}, nil
}
