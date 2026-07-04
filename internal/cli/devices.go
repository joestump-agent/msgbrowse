// The `msgbrowse devices` namespace under the Syncthing sync engine
// (ADR-0021 supersedes ADR-0018). The SPEC-0011 surface this file used to
// hold — pairing windows, token payloads, the mTLS listener client, unpair
// by fingerprint — is retired: identity, transport, and discovery belong to
// the supervised Syncthing engine, and pairing is the /settings device-ID QR
// flow (issue #157). The CLI verbs are rebuilt on the Syncthing model (#158):
//
//   - list:   the paired-peer registry (node-local database read).
//   - unpair: delete the registry row, then — when a supervised daemon is
//     reachable via its persisted loopback REST address — remove the device
//     and unshare its folders live, so sync stops immediately (SPEC-0014 REQ
//     "Unpair and Revoke": "from the CLI and from the settings page").
//   - status: engine up?, per-peer connection, per-folder completion/health,
//     read from the registry plus the daemon's REST API (SPEC-0014 REQ
//     "Status and Doctor Surfacing").
//
// The REST reach-back is loopback-only: the daemon's address and API key are
// the supervisor-persisted files under <data_dir>/syncthing/, so the CLI
// drives exactly the daemon a running serve/desktop session supervises —
// never a system Syncthing. An unreachable daemon degrades cleanly: unpair
// still removes the registry row (the durable revocation; the next engine
// start regenerates config without the peer) and status reports the engine
// as not running with the registry rows intact.
//
// Governing: ADR-0021 ("retire or repurpose the merged work"), SPEC-0014 REQ
// "Migration from SPEC-0011", REQ "Unpair and Revoke", REQ "Status and
// Doctor Surfacing", REQ "Error Handling Standards".
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/joestump/msgbrowse/internal/devices"
	"github.com/joestump/msgbrowse/internal/devsync"
	"github.com/joestump/msgbrowse/internal/store"
	"github.com/joestump/msgbrowse/internal/syncthing"
	"github.com/spf13/cobra"
)

func newDevicesCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "devices",
		Short: "Manage device-sync peers (Syncthing archive sync)",
		Long: "devices manages multi-device archive synchronization peers (ADR-0021).\n" +
			"Trust is Syncthing's device-ID mutual TLS: pair devices from the web UI's\n" +
			"Settings page by exchanging device-ID QR codes — each device must accept\n" +
			"the other before any archive data flows. Device sync is strictly opt-in:\n" +
			"set device_sync.enabled in the config first.",
	}
	cmd.AddCommand(
		newDevicesListCommand(),
		newDevicesUnpairCommand(),
		newDevicesStatusCommand(),
	)
	return cmd
}

func newDevicesListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List paired device-sync peers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig()
			if err != nil {
				return err
			}
			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()
			return runDevicesList(cmd.Context(), st, os.Stdout)
		},
	}
}

func newDevicesUnpairCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "unpair <device-id-or-prefix>",
		Short: "Unpair a device: stop syncing to it (local archives stay)",
		Long: "unpair severs a paired device (SPEC-0014 \"Unpair and Revoke\"): its registry\n" +
			"row is deleted and — when the sync engine is running — its device entry and\n" +
			"folder shares are removed live, so archive data stops flowing to it\n" +
			"immediately. Archives already synced to THIS machine stay on disk and\n" +
			"remain browsable; unpairing severs only future synchronization.\n" +
			"\n" +
			"The device may be named by its full Syncthing device ID or any unique\n" +
			"prefix of it (e.g. the short ID shown by `devices list`).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolveConfig()
			if err != nil {
				return err
			}
			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()
			return runDevicesUnpair(cmd.Context(), st, dialSyncEngine(cfg.DataDir), args[0], os.Stdout)
		},
	}
}

func newDevicesStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show sync engine, peer connection, and folder completion state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig()
			if err != nil {
				return err
			}
			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()
			return runDevicesStatus(cmd.Context(), st, dialSyncEngine(cfg.DataDir), os.Stdout)
		},
	}
}

// devicesStore is the store seam the devices verbs read/write through
// (*store.Store satisfies it; tests substitute fakes and error scripts).
type devicesStore interface {
	ListSyncPeers(ctx context.Context) ([]devices.SyncPeer, error)
	DeleteSyncPeer(ctx context.Context, deviceID string) error
	SyncImportStates(ctx context.Context) ([]store.SyncImportState, error)
}

// engineDialer resolves a REACHABLE supervised daemon's REST client, or an
// error when none is running (wrapping syncthing.ErrNotRunning for the
// no-daemon case). Injected so tests script both outcomes.
type engineDialer func(ctx context.Context) (devsync.API, error)

// dialSyncEngine returns the production dialer: read the supervisor-persisted
// loopback REST address + API key under dataDir and CONFIRM liveness with a
// short ping — the persisted files outlive a stopped daemon, so the ping is
// what distinguishes "engine running" from stale state.
func dialSyncEngine(dataDir string) engineDialer {
	return func(ctx context.Context) (devsync.API, error) {
		addr, key, err := syncthing.RESTInfo(dataDir)
		if err != nil {
			return nil, err
		}
		client := syncthing.NewClient(addr, key)
		pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := client.Ping(pingCtx); err != nil {
			return nil, fmt.Errorf("%w: %v", syncthing.ErrNotRunning, err)
		}
		return client, nil
	}
}

// syncPeerLister is the store seam runDevicesList reads through.
type syncPeerLister interface {
	ListSyncPeers(ctx context.Context) ([]devices.SyncPeer, error)
}

// runDevicesList renders the paired-peer registry — the same rows /settings
// shows, because both read paired_devices, which records the TRUE share set
// (including folders widened by accepted offers, issue #157 review finding
// 2). The ROLE column names the sources the peer imports FOR this node
// (SPEC-0014 REQ "Importer and Replica Roles"). Extracted from the cobra
// RunE so the CLI surface is testable without a config file or real data dir.
func runDevicesList(ctx context.Context, st syncPeerLister, out io.Writer) error {
	peers, err := st.ListSyncPeers(ctx)
	if err != nil {
		return err
	}
	if len(peers) == 0 {
		fmt.Fprintln(out, "No devices paired. Pair one from Settings in the web UI.")
		return nil
	}
	w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tDEVICE ID\tFOLDERS\tIMPORTS\tPAIRED")
	for _, p := range peers {
		folders := "-"
		if len(p.Folders) > 0 {
			folders = strings.Join(p.Folders, ",")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.Name, p.DeviceID, folders,
			importsColumn(p), p.PairedAt.Local().Format("2006-01-02 15:04"))
	}
	return w.Flush()
}

// importsColumn renders the sources the peer is the recorded importer for
// ("-" when none — the peer only receives from this node).
func importsColumn(p devices.SyncPeer) string {
	var srcs []string
	for src, role := range p.Roles {
		if role == devices.RoleImporter {
			srcs = append(srcs, src)
		}
	}
	if len(srcs) == 0 {
		return "-"
	}
	sort.Strings(srcs)
	return strings.Join(srcs, ",")
}

// runDevicesUnpair resolves the argument against the registry (full canonical
// ID or any unique prefix), deletes the registry row — the durable
// revocation — and then best-effort removes the device from a RUNNING
// daemon's config so sync stops immediately (SPEC-0014 REQ "Unpair and
// Revoke"). With no daemon running the registry removal alone suffices: the
// next engine start regenerates config without the peer.
func runDevicesUnpair(ctx context.Context, st devicesStore, dial engineDialer, arg string, out io.Writer) error {
	peers, err := st.ListSyncPeers(ctx)
	if err != nil {
		return err
	}
	peer, err := resolvePeerArg(peers, arg)
	if err != nil {
		return err
	}
	if err := st.DeleteSyncPeer(ctx, peer.DeviceID); err != nil {
		return fmt.Errorf("unpair %s: %w", peer.ShortID(), err)
	}
	fmt.Fprintf(out, "Unpaired %s (%s): removed from this node's registry.\n", peer.Name, peer.ShortID())

	api, err := dial(ctx)
	if err != nil {
		if errors.Is(err, syncthing.ErrNotRunning) {
			fmt.Fprintln(out, "Sync engine not running — its config is regenerated without this device on the next start.")
			fmt.Fprintln(out, "Local archives and the database are untouched.")
			return nil
		}
		return fmt.Errorf("unpair %s: reach sync engine: %w", peer.ShortID(), err)
	}
	opCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := devsync.RemoveDeviceFromDaemon(opCtx, api, peer.DeviceID); err != nil {
		// The durable revocation already happened; surface the live-config
		// failure rather than swallowing it (SPEC-0014 REQ "Error Handling
		// Standards").
		return fmt.Errorf("unpair %s: registry row removed, but the running engine did not accept the removal (sync to it stops at the next engine start): %w", peer.ShortID(), err)
	}
	fmt.Fprintln(out, "Removed from the running sync engine: folders unshared, sync to it stopped.")
	fmt.Fprintln(out, "Local archives and the database are untouched.")
	return nil
}

// resolvePeerArg matches arg — a full device ID or a unique prefix, in any
// transcription Syncthing tolerates — against the registry. Ambiguity and
// misses are errors naming the candidates, never a guess: unpair is
// destructive to future sync (issue #158 Security Checklist: "peer/device
// removal by validated device ID").
func resolvePeerArg(peers []devices.SyncPeer, arg string) (devices.SyncPeer, error) {
	if id, err := devices.CanonicalDeviceID(arg); err == nil {
		for _, p := range peers {
			if p.DeviceID == id {
				return p, nil
			}
		}
		return devices.SyncPeer{}, fmt.Errorf("device %s: %w", devices.ShortDeviceID(id), devices.ErrUnknownSyncPeer)
	}
	// Prefix match over the dashless normalized form, so "P56IOI7" (the short
	// ID `devices list` shows) and a partial paste both resolve.
	norm := normalizePrefix(arg)
	if norm == "" {
		return devices.SyncPeer{}, fmt.Errorf("%q is not a device ID or prefix", arg)
	}
	var matches []devices.SyncPeer
	for _, p := range peers {
		if strings.HasPrefix(normalizePrefix(p.DeviceID), norm) {
			matches = append(matches, p)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return devices.SyncPeer{}, fmt.Errorf("no paired device matches %q: %w", arg, devices.ErrUnknownSyncPeer)
	default:
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = fmt.Sprintf("%s (%s)", m.Name, m.ShortID())
		}
		return devices.SyncPeer{}, fmt.Errorf("%q is ambiguous: matches %s", arg, strings.Join(names, ", "))
	}
}

// normalizePrefix uppercases and strips dashes/whitespace (the device-ID
// transcription tolerance) for prefix comparison.
func normalizePrefix(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(s)) {
		switch r {
		case '-', ' ':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// runDevicesStatus renders the engine/peer/folder state tables from the
// registry joined with a running daemon's REST API (SPEC-0014 REQ "Status
// and Doctor Surfacing"): the CLI face of the same snapshot /settings and
// /status render. Engine-down is a truthful state, not an error.
func runDevicesStatus(ctx context.Context, st devicesStore, dial engineDialer, out io.Writer) error {
	peers, err := st.ListSyncPeers(ctx)
	if err != nil {
		return err
	}
	lastImports := map[string]time.Time{}
	if states, err := st.SyncImportStates(ctx); err == nil {
		for _, s := range states {
			lastImports[s.FolderID] = s.LastImportAt
		}
	}

	api, dialErr := dial(ctx)
	if dialErr != nil {
		if !errors.Is(dialErr, syncthing.ErrNotRunning) {
			return dialErr
		}
		fmt.Fprintln(out, "Engine: not running (start `msgbrowse serve` or the desktop app with device_sync.enabled)")
	}

	var (
		conns   map[string]syncthing.ConnectionInfo
		folders []syncthing.FolderConfig
	)
	if api != nil {
		opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		status, err := api.SystemStatus(opCtx)
		if err != nil {
			return fmt.Errorf("devices status: %w", err)
		}
		fmt.Fprintf(out, "Engine: running (this device: %s, up %s)\n",
			devices.ShortDeviceID(status.MyID), (time.Duration(status.Uptime) * time.Second).String())
		if c, err := api.Connections(opCtx); err == nil {
			conns = c.Connections
		}
		if f, err := api.GetFolders(opCtx); err == nil {
			folders = f
		}
	}

	fmt.Fprintln(out)
	if len(peers) == 0 {
		fmt.Fprintln(out, "No devices paired. Pair one from Settings in the web UI.")
	} else {
		w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "DEVICE\tID\tSTATE\tLAST SEEN")
		for _, p := range peers {
			state := "unknown"
			if conns != nil {
				state = "disconnected"
				if ci, ok := conns[p.DeviceID]; ok {
					switch {
					case ci.Connected:
						state = "connected"
					case ci.Paused:
						state = "paused"
					}
				}
			}
			last := "-"
			if !p.LastSeenAt.IsZero() {
				last = p.LastSeenAt.Local().Format("2006-01-02 15:04")
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.Name, p.ShortID(), state, last)
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}

	if api != nil && len(folders) > 0 {
		fmt.Fprintln(out)
		w := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
		fmt.Fprintln(w, "FOLDER\tSTATE\tCOMPLETE\tERRORS\tLAST IMPORT")
		opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		for _, f := range folders {
			state, complete, errCount := "unknown", "-", "-"
			if fst, err := api.FolderStatus(opCtx, f.ID); err == nil {
				state = fst.State
				errCount = fmt.Sprintf("%d", fst.Errors+fst.PullErrors)
			}
			if f.Paused {
				state = "paused"
			}
			if comp, err := api.FolderCompletion(opCtx, f.ID, ""); err == nil {
				complete = fmt.Sprintf("%.0f%%", comp.CompletionPct)
			}
			last := "-"
			if t, ok := lastImports[f.ID]; ok && !t.IsZero() {
				last = t.Local().Format("2006-01-02 15:04")
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", f.ID, state, complete, errCount, last)
		}
		if err := w.Flush(); err != nil {
			return err
		}
	}
	return nil
}
