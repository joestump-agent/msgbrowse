// doctor's device-sync checks under the Syncthing engine (ADR-0021), the
// #158 status/doctor story: disabled remains the healthy default (pass, not
// warn); enabled gets the full condition ladder from SPEC-0014 REQ "Status
// and Doctor Surfacing" — listener-config sanity, engine binary resolution
// (the CLI's bring-your-own path; the desktop .app bundles its own), the
// supervised daemon's liveness via its persisted loopback REST address,
// per-peer connection state, and per-folder health/completion with
// remediation hints for paused/errored folders.
//
// Network posture: these checks talk ONLY to the loopback REST API of the
// daemon msgbrowse itself supervises (address + API key from
// <data_dir>/syncthing/, written by the supervisor). No packet leaves the
// machine — doctor stays egress-silent except behind --check-llm.
//
// Governing: ADR-0021, SPEC-0014 REQ "Status and Doctor Surfacing" ("doctor
// MUST report device-sync condition, including: the supervised daemon is
// running when sync is enabled, each paired peer's connection state, folder
// completion and staleness, and any Syncthing-reported folder errors"), REQ
// "Migration from SPEC-0011" (no identity/certificate check exists — no
// msgbrowse-issued certificate does either).
package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/devices"
	"github.com/joestump/msgbrowse/internal/store"
	"github.com/joestump/msgbrowse/internal/syncthing"
)

// checkDeviceSync reports the device-sync posture. Disabled is the healthy
// default (pass, not warn — unlike archive roots, most installs never enable
// this). Enabled runs the condition ladder: config sanity, engine
// resolution, daemon liveness, peer connections, folder health.
func checkDeviceSync(ctx context.Context, r *report, cfg *config.Config, st *store.Store) {
	if !cfg.DeviceSync.Enabled {
		r.add(statusPass, "device sync disabled (no sync engine; loopback-only posture per ADR-0010)", "")
		return
	}

	checkDeviceSyncPorts(r, cfg)
	checkDeviceSyncEngineBinary(r, cfg)
	api := checkDeviceSyncDaemon(ctx, r, cfg)
	checkDeviceSyncPeers(ctx, r, st, api)
	checkDeviceSyncFolders(ctx, r, st, api)
}

// checkDeviceSyncPorts re-validates the sync listen address here so the
// posture is visible in the report even though config.Validate gates it
// earlier: dedicated port, distinct from the web UI (the Syncthing P2P
// listener is the only beyond-loopback surface, SPEC-0014 §Authentication).
func checkDeviceSyncPorts(r *report, cfg *config.Config) {
	_, syncPort, err := net.SplitHostPort(cfg.DeviceSync.ListenAddr)
	if err != nil {
		r.add(statusFail, fmt.Sprintf("device_sync.listen_addr %q is not host:port: %v", cfg.DeviceSync.ListenAddr, err),
			"set device_sync.listen_addr to host:port with a port distinct from the web UI's")
		return
	}
	_, webPort, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		r.add(statusFail, fmt.Sprintf("listen_addr %q is not host:port: %v", cfg.ListenAddr, err), "")
		return
	}
	if syncPort == webPort {
		r.add(statusFail, fmt.Sprintf("device_sync.listen_addr %q shares the web UI port %s", cfg.DeviceSync.ListenAddr, webPort),
			"the sync listener needs its own port; change device_sync.listen_addr")
		return
	}
	r.add(statusPass, fmt.Sprintf("device sync enabled: Syncthing listener on port %s (web UI on %s, device-ID mutual TLS)", syncPort, webPort), "")
}

// checkDeviceSyncEngineBinary reports how the engine binary resolves for the
// CLI's bring-your-own path (config key, then $PATH) — the resolution
// `msgbrowse serve` will use. A miss is a warn, not a fail: the desktop .app
// carries its own bundled, version-pinned copy that this CLI cannot see
// (SPEC-0014 REQ "Bundled Syncthing Runtime").
func checkDeviceSyncEngineBinary(r *report, cfg *config.Config) {
	bin, err := resolveSyncthingBin(cfg)
	if err != nil {
		r.add(statusWarn, "syncthing engine not resolvable for `msgbrowse serve` (no device_sync.syncthing_bin, not on $PATH)",
			"the desktop app bundles its own engine; for `serve`, install syncthing or set device_sync.syncthing_bin")
		return
	}
	how := "bring-your-own: $PATH"
	if cfg.DeviceSync.SyncthingBin != "" {
		how = "bring-your-own: device_sync.syncthing_bin"
	}
	r.add(statusPass, fmt.Sprintf("syncthing engine resolved: %s (%s; the desktop app uses its bundled copy)", bin, how), "")
}

// checkDeviceSyncDaemon probes the supervised daemon through its persisted
// loopback REST address and returns a live client for the peer/folder checks
// (nil when the daemon is not running). Loopback-only, API-keyed — never a
// system Syncthing, never an off-machine packet.
func checkDeviceSyncDaemon(ctx context.Context, r *report, cfg *config.Config) *syncthing.Client {
	addr, key, err := syncthing.RESTInfo(cfg.DataDir)
	if err != nil {
		if errors.Is(err, syncthing.ErrNotRunning) {
			r.add(statusWarn, "sync engine not running (no supervised daemon has started under this data dir)",
				"start `msgbrowse serve` or the desktop app; the supervisor records its REST address for these checks")
			return nil
		}
		r.add(statusWarn, fmt.Sprintf("could not read the sync engine's REST address: %v", err), "")
		return nil
	}
	client := syncthing.NewClient(addr, key)
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx); err != nil {
		r.add(statusWarn, "sync engine not running (its last-known REST address does not answer)",
			"start `msgbrowse serve` or the desktop app; sync resumes automatically once the engine is up")
		return nil
	}
	line := fmt.Sprintf("sync engine running (loopback REST at %s)", addr)
	verCtx, cancel2 := context.WithTimeout(ctx, 2*time.Second)
	defer cancel2()
	if ver, err := client.SystemVersion(verCtx); err == nil && ver.Version != "" {
		line = fmt.Sprintf("sync engine running (syncthing %s, loopback REST at %s)", ver.Version, addr)
	}
	r.add(statusPass, line, "")
	return client
}

// checkDeviceSyncPeers lists the paired peers from the repurposed registry
// (Syncthing device IDs, SPEC-0014 "Schema tables carry Syncthing
// identifiers") and — when the daemon answers — each peer's live connection
// state. A disconnected peer is a warn with the peer named; sync simply
// resumes when both ends are up, but the operator should know.
func checkDeviceSyncPeers(ctx context.Context, r *report, st *store.Store, api *syncthing.Client) {
	if st == nil {
		r.add(statusWarn, "cannot list paired devices (no database yet)",
			"pair a device from Settings in the web UI; the registry lives in the database")
		return
	}
	peers, err := st.ListSyncPeers(ctx)
	if err != nil {
		r.add(statusWarn, fmt.Sprintf("could not list paired devices: %v", err), "")
		return
	}
	if len(peers) == 0 {
		r.add(statusWarn, "device sync enabled but no devices paired yet",
			"open Settings in the web UI and scan the device-ID QR (or paste the code) from the other device")
		return
	}

	var conns map[string]syncthing.ConnectionInfo
	if api != nil {
		opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if c, err := api.Connections(opCtx); err == nil {
			conns = c.Connections
		} else {
			r.add(statusWarn, fmt.Sprintf("could not read peer connection state: %v", err), "")
		}
	}
	var described []string
	var disconnected []string
	for _, p := range peers {
		state := "state unknown; engine not running"
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
			if state != "connected" {
				disconnected = append(disconnected, fmt.Sprintf("%s (%s)", p.Name, p.ShortID()))
			}
		}
		described = append(described, fmt.Sprintf("%s (%s, %s)", p.Name, p.ShortID(), state))
	}
	line := fmt.Sprintf("%s paired: %s", plural(len(peers), "device"), strings.Join(described, ", "))
	if conns != nil && len(disconnected) > 0 {
		r.add(statusWarn, line,
			"disconnected peers sync nothing; make sure the other device is on this LAN with msgbrowse (and device sync) running")
		return
	}
	r.add(statusPass, line, "")
}

// checkDeviceSyncFolders reports each managed folder's health from the
// daemon: completion percentage, paused state, and Syncthing-reported errors
// — the "paused or errored sync shows in msgbrowse's status" scenario, with
// a remediation hint per condition. Also reports the roles this node holds:
// a source synced in from a peer is named with its importer (SPEC-0014 REQ
// "Importer and Replica Roles").
func checkDeviceSyncFolders(ctx context.Context, r *report, st *store.Store, api *syncthing.Client) {
	if st != nil {
		if peers, err := st.ListSyncPeers(ctx); err == nil {
			for _, p := range peers {
				var srcs []string
				for src, role := range p.Roles {
					if role == devices.RoleImporter {
						srcs = append(srcs, src)
					}
				}
				if len(srcs) > 0 {
					r.add(statusPass, fmt.Sprintf("replica for %s — imported on %s (%s); this node ingests each completed sync",
						strings.Join(srcs, ", "), p.Name, p.ShortID()), "")
				}
			}
		}
	}
	if api == nil {
		return // daemon down already reported; no folder state to assert
	}
	opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	folders, err := api.GetFolders(opCtx)
	if err != nil {
		r.add(statusWarn, fmt.Sprintf("could not read the engine's folder config: %v", err), "")
		return
	}
	if len(folders) == 0 {
		r.add(statusWarn, "sync engine has no folders configured yet",
			"enable a source on the importer (Providers page) or pair with a device that shares one")
		return
	}
	for _, f := range folders {
		status, serr := api.FolderStatus(opCtx, f.ID)
		comp, cerr := api.FolderCompletion(opCtx, f.ID, "")
		if serr != nil || cerr != nil {
			r.add(statusWarn, fmt.Sprintf("folder %s: could not read status/completion", f.ID), "")
			continue
		}
		errCount := status.Errors + status.PullErrors
		switch {
		case f.Paused || status.State == "paused":
			r.add(statusWarn, fmt.Sprintf("folder %s is paused (%.0f%% complete)", f.ID, comp.CompletionPct),
				"a paused folder syncs nothing; unpause it by restarting msgbrowse (config is regenerated on start)")
		case errCount > 0 || status.State == "error" || status.State == "outofsync":
			r.add(statusFail, fmt.Sprintf("folder %s reports errors (state %s, %d failed items, %.0f%% complete)",
				f.ID, status.State, errCount, comp.CompletionPct),
				"check file permissions under the archive root and see the Logs page for the engine's output")
		case comp.NeedItems > 0 || comp.NeedDeletes > 0 || comp.CompletionPct < 100:
			r.add(statusPass, fmt.Sprintf("folder %s syncing: %.0f%% complete (%d items to go)", f.ID, comp.CompletionPct, comp.NeedItems), "")
		default:
			r.add(statusPass, fmt.Sprintf("folder %s healthy: 100%% complete (state %s)", f.ID, status.State), "")
		}
	}
}
