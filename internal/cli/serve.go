package cli

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/ingest"
	"github.com/joestump/msgbrowse/internal/onboardsvc"
	"github.com/joestump/msgbrowse/internal/setup"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
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

			// Background provider auto-refresh (replaces the retired "Refresh all
			// sources" button): re-import each Enabled source's delta on the
			// configured cadence. No-op when disabled (providers.refresh_interval
			// <= 0); drains with the shared context.
			go srv.StartAutoRefresh(ctx, cfg.Providers.RefreshInterval)

			// The Settings → LLM tab (#191): saves persist the three llm
			// keys into the loaded config file and swap the process's live
			// LLM holder, so a changed endpoint applies without a restart.
			srv.SetLLMConfig(newLLMApplier(cfg, newLLMHolder(cfg)))

			// Device sync (ADR-0021) is gated behind the `devicesync` build
			// tag — it is NOT compiled into release binaries. wireDeviceSync is
			// the real wiring under the tag (supervised Syncthing engine +
			// pairing manager + status/roles monitor + folder-watch re-ingest
			// worker) and a no-op stub without it; deviceSyncCompiledIn tells
			// the web UI whether to render the Device sync surface at all.
			srv.SetDeviceSyncFeature(deviceSyncCompiledIn)
			syncWait, err := wireDeviceSync(ctx, srv, cfg, st, onboardRunner)
			if err != nil {
				return err
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
			// device-sync worker drains too — otherwise its Wait blocks forever
			// on a still-serving listener when Run returns an early bind error.
			// stop() cancels the NotifyContext; the deferred stop() is a no-op.
			stop()

			// Wait for the sync listener's graceful drain (no leaked workers on
			// shutdown); the stub's waiter returns immediately.
			if derr := syncWait(); derr != nil && err == nil {
				err = derr
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
