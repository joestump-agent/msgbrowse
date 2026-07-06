package cli

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/devsync"
	"github.com/joestump/msgbrowse/internal/ingest"
	"github.com/joestump/msgbrowse/internal/onboard"
	"github.com/joestump/msgbrowse/internal/onboardsvc"
	"github.com/joestump/msgbrowse/internal/setup"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
	"github.com/joestump/msgbrowse/internal/syncthing"
	"github.com/joestump/msgbrowse/internal/web"
	"github.com/spf13/cobra"
)

func newServeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the local HTMX web UI",
		Long: "serve runs the server-rendered HTMX web UI. It binds to loopback by\n" +
			"default; the UI has no authentication, so only expose it on a non-loopback\n" +
			"address behind your own access control.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig()
			if err != nil {
				return err
			}
			addr, err := resolveListenAddr(cmd, cfg.ListenAddr)
			if err != nil {
				return err
			}
			cfg.ListenAddr = addr

			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()

			// Signals cancel the context for graceful shutdown.
			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			if cfg.IngestOnStart {
				if err := ingestOnStart(ctx, st, cfg); err != nil {
					slog.Warn("ingest-on-start failed; serving existing data", "error", err)
				}
			}

			srv, err := web.NewServer(st, cfg, slog.Default())
			if err != nil {
				return err
			}

			// Wire the Setup Enable flow (SPEC-0013): `serve` resolves exporters
			// from config/$PATH (the bring-your-own path — only the .app bundles).
			// A source with no resolvable tool renders "unavailable" rather than a
			// silent no-op. The runner's workers are torn down on shutdown so no
			// exporter subprocess outlives serve (SPEC-0013 REQ "Concurrency
			// Safety").
			onboardRunner, err := onboardsvc.Build(cfg, st, onboardsvc.PathResolverFromConfig(cfg), slog.Default())
			if err != nil {
				return err
			}
			defer onboardRunner.Shutdown()
			srv.SetEnabler(onboardRunner)

			// The Settings → LLM tab (#191): saves persist the three llm
			// keys into the loaded config file and swap the process's live
			// LLM holder, so a changed endpoint applies without a restart.
			srv.SetLLMConfig(newLLMApplier(cfg, newLLMHolder(cfg)))

			// Device sync (ADR-0021): with device_sync.enabled the
			// supervised Syncthing engine runs beside the web UI as a
			// context-managed worker — plus the pairing manager behind
			// /settings and the folder-watch → re-ingest worker (#157);
			// disabled (the default) means no Syncthing process and no P2P
			// listener exist at all.
			devSync, err := startDeviceSync(ctx, cfg, st, onboardRunner)
			if err != nil {
				return err
			}
			if devSync != nil {
				srv.SetPairingSource(devSync.Manager)
				// Status + roles + the Logs event feed (#158; SPEC-0014 REQ
				// "Status and Doctor Surfacing", REQ "Importer and Replica
				// Roles"): the same Manager backs all three seams.
				srv.SetSyncMonitor(devSync.Manager)
				srv.SetSyncNotes(devSync.Notes.Snapshot)
			}

			// Convenience for local use: open the UI in the default browser once
			// the listener is up. Best-effort and easily disabled (--open=false)
			// for headless/server runs.
			if open, _ := cmd.Flags().GetBool("open"); open {
				go openWhenReady(ctx, cfg.ListenAddr, slog.Default())
			}
			err = srv.Run(ctx, cfg.ListenAddr)

			// The web UI has stopped (clean shutdown, or Run failed at bind
			// before any signal arrived). Cancel the shared context so the
			// device-sync worker drains too — otherwise Wait() blocks forever
			// on a still-serving listener when Run returns an early bind error.
			// stop() cancels the NotifyContext; the deferred stop() is a no-op.
			stop()

			// Wait for the sync listener's graceful drain (SPEC-0011
			// "Concurrency Safety": no leaked workers on shutdown).
			if devSync != nil {
				if derr := devSync.Wait(); derr != nil && err == nil {
					err = derr
				}
			}
			return err
		},
	}
	cmd.Flags().String("listen-addr", "", "full listen address host:port (overrides --host/--port and config)")
	cmd.Flags().String("host", "", "bind host (e.g. 127.0.0.1 or 0.0.0.0); default keeps the configured host")
	cmd.Flags().Int("port", 0, "bind port (e.g. 8888); default keeps the configured port")
	cmd.Flags().Bool("open", true, "open the UI in your default browser on start (use --open=false for headless)")
	return cmd
}

// resolveListenAddr layers the serve address flags over the configured default:
// --listen-addr replaces the whole address; otherwise --host / --port override
// just those parts of the configured host:port. Returns the final host:port.
func resolveListenAddr(cmd *cobra.Command, configured string) (string, error) {
	if la, _ := cmd.Flags().GetString("listen-addr"); la != "" {
		return la, nil
	}
	host, port, err := net.SplitHostPort(configured)
	if err != nil {
		return "", fmt.Errorf("invalid configured listen_addr %q: %w", configured, err)
	}
	if h, _ := cmd.Flags().GetString("host"); h != "" {
		host = h
	}
	if p, _ := cmd.Flags().GetInt("port"); p != 0 {
		if p < 1 || p > 65535 {
			return "", fmt.Errorf("invalid --port %d (want 1-65535)", p)
		}
		port = strconv.Itoa(p)
	}
	return net.JoinHostPort(host, port), nil
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

// ingestOnStart runs a best-effort ingest pass before serving, when configured
// and an archive is available. The root is the EFFECTIVE Signal root (the
// configured archive_root, else the managed root a desktop onboarding
// populated — issue #160). The store handle from serve is reused; opening a
// second connection to the same SQLite file works (WAL handles it) but muddles
// ownership.
func ingestOnStart(ctx context.Context, st *store.Store, cfg *config.Config) error {
	root := setup.EffectiveRoot(cfg, source.Signal)
	if root == "" {
		return requireArchive(cfg) // reports the unset archive_root as before
	}
	if err := requireDir("archive_root", "MSGBROWSE_ARCHIVE_ROOT", root); err != nil {
		return err
	}
	_, err := ingest.Run(ctx, st, ingest.Options{ArchiveRoot: root})
	return err
}
