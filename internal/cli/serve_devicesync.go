//go:build devicesync

// Device-sync wiring for `msgbrowse serve`, compiled ONLY under the `devicesync`
// build tag (ADR-0021 / SPEC-0014). The feature is not release-ready, so the
// default build excludes this file — and with it the internal/devsync and
// internal/syncthing packages — entirely; serve_nodevicesync.go supplies the
// no-op seam instead. Everything the pre-gate serve.go did inline lives here.
package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/devsync"
	"github.com/joestump/msgbrowse/internal/onboard"
	"github.com/joestump/msgbrowse/internal/store"
	"github.com/joestump/msgbrowse/internal/syncthing"
	"github.com/joestump/msgbrowse/internal/web"
)

// deviceSyncCompiledIn reports that this binary was built with the device-sync
// feature; the web UI renders the Device sync surface accordingly.
const deviceSyncCompiledIn = true

// wireDeviceSync starts the device-sync engine (when device_sync.enabled) and
// wires its pairing manager, status/roles monitor, and Logs event feed into the
// web server. It returns a waiter the caller blocks on during shutdown to drain
// the supervisor and folder-watch worker; with sync disabled the waiter is a
// no-op. This is the seam serve.go calls; serve_nodevicesync.go is the stub.
func wireDeviceSync(ctx context.Context, srv *web.Server, cfg *config.Config, st *store.Store, runner *onboard.Runner) (func() error, error) {
	worker, err := startDeviceSync(ctx, cfg, st, runner)
	if err != nil {
		return nil, err
	}
	if worker == nil {
		return func() error { return nil }, nil
	}
	srv.SetPairingSource(worker.Manager)
	// Status + roles + the Logs event feed (#158; SPEC-0014 REQ "Status and
	// Doctor Surfacing", REQ "Importer and Replica Roles"): the same Manager
	// backs all three seams.
	srv.SetSyncMonitor(worker.Manager)
	srv.SetSyncNotes(worker.Notes.Snapshot)
	return worker.Wait, nil
}

// deviceSyncWorker is the running device-sync engine started by
// startDeviceSync: the supervised Syncthing daemon's loopback REST address,
// the pairing manager the web layer renders /settings from, and a Wait that
// blocks until the supervisor AND the folder-watch worker have fully drained
// (child stopped, no orphan, no leaked goroutine).
type deviceSyncWorker struct {
	// Addr is the daemon's loopback REST API address (host:port).
	Addr string
	// Manager is the pairing surface wired into web.SetPairingSource, and —
	// as the SyncMonitor — the status/roles source behind Settings, /status,
	// and the Providers cards (#158).
	Manager *devsync.Manager
	// Notes is the device-sync event feed the Logs page renders (#158).
	Notes   *devsync.Notes
	watcher *devsync.Watcher
	done    <-chan error
}

// Wait blocks until the supervision worker and the folder-watch worker have
// exited and returns the supervisor's error.
func (w *deviceSyncWorker) Wait() error {
	err := <-w.done
	w.watcher.Wait()
	return err
}

// startDeviceSync starts the supervised Syncthing engine as a context-managed
// worker when device_sync.enabled is true, then layers the msgbrowse-owned
// pieces on top (issue #157): the pairing manager (internal/devsync.Manager,
// the /settings PairingSource) and the folder-watch → re-ingest worker
// (devsync.Watcher), which dispatches incremental imports through the shared
// onboard Runner so sync imports respect the same per-source job guard and
// surface in the same Logs view. With device sync disabled — the default —
// it returns (nil, nil) and starts NO process and NO socket, keeping the
// process's socket inventory exactly the loopback web UI (SPEC-0014 "Device
// sync disabled means no Syncthing process").
//
// Paired peers persist in the repurposed paired_devices table and are folded
// into the generated config on every start (devsync.ApplyPeers), so pairing
// survives restarts even though msgbrowse regenerates Syncthing's config.xml
// each launch.
//
// Binary resolution here is the bring-your-own path (config key, then $PATH),
// mirroring the exporters: only the desktop .app bundles a pinned binary
// (SPEC-0014 REQ "Bundled Syncthing Runtime"; resolution in
// cmd/msgbrowse-desktop). It starts eagerly and fails fast: the operator
// explicitly enabled sync, so a missing engine or a failed start aborts
// serve rather than degrading silently (SPEC-0014 REQ "Error Handling
// Standards").
//
// Governing: ADR-0021, SPEC-0014 REQ "Supervised Daemon Lifecycle", REQ
// "Pairing via Device ID and QR", REQ "Re-ingest Trigger".
func startDeviceSync(ctx context.Context, cfg *config.Config, st *store.Store, runner *onboard.Runner) (*deviceSyncWorker, error) {
	if !cfg.DeviceSync.Enabled {
		return nil, nil
	}
	bin, err := resolveSyncthingBin(cfg)
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
		BinPath:    bin,
		DataDir:    cfg.DataDir,
		ListenAddr: cfg.DeviceSync.ListenAddr,
		DeviceName: deviceName(cfg),
		Folders:    folders,
		Devices:    peerDevices,
		Logger:     slog.Default(),
	})
	if err != nil {
		return nil, err
	}
	if err := sup.Start(ctx); err != nil {
		return nil, err
	}

	client := sup.Client()
	// One shared event ring: the Manager (pair/unpair) and Watcher (imports,
	// accepted offers, peer connects) record into it; the Logs page reads it
	// (#158; SPEC-0014 REQ "Status and Doctor Surfacing").
	notes := devsync.NewNotes(0)
	manager := devsync.NewManager(client, st, deviceName(cfg), folderSet, slog.Default())
	manager.SetNotes(notes)
	watcher, err := devsync.NewWatcher(devsync.WatcherOptions{
		API:      client,
		Store:    st,
		Importer: runner,
		Folders:  folderSet,
		Notes:    notes,
		Logger:   slog.Default(),
	})
	if err != nil {
		return nil, fmt.Errorf("device sync start failed: %w", err)
	}
	watcher.Start(ctx)

	done := make(chan error, 1)
	go func() {
		err := sup.Wait()
		if err != nil && ctx.Err() == nil {
			slog.Error("device-sync supervisor failed", "error", err)
		}
		done <- err
	}()
	return &deviceSyncWorker{Addr: sup.APIAddr(), Manager: manager, Notes: notes, watcher: watcher, done: done}, nil
}

// deviceName resolves this node's friendly device name: the configured
// device_sync.device_name, else the hostname.
func deviceName(cfg *config.Config) string {
	if cfg.DeviceSync.DeviceName != "" {
		return cfg.DeviceSync.DeviceName
	}
	if host, err := os.Hostname(); err == nil && host != "" {
		return host
	}
	return "msgbrowse"
}

// resolveSyncthingBin resolves the Syncthing binary for `serve`'s
// bring-your-own path: the device_sync.syncthing_bin config key when set,
// otherwise a $PATH lookup of `syncthing` — exactly the resolution shape the
// exporter *_bin keys use. A miss is the typed ErrBinaryNotFound with
// guidance, never a silent no-op.
func resolveSyncthingBin(cfg *config.Config) (string, error) {
	if bin := cfg.DeviceSync.SyncthingBin; bin != "" {
		return bin, nil
	}
	bin, err := exec.LookPath("syncthing")
	if err != nil {
		return "", fmt.Errorf("device sync start failed: %w: install syncthing or set device_sync.syncthing_bin (the desktop app bundles its own copy)",
			syncthing.ErrBinaryNotFound)
	}
	return bin, nil
}
