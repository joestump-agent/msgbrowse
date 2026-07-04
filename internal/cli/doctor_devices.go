// doctor's device-sync checks under the Syncthing engine (ADR-0021).
// Disabled remains the healthy default (pass, not warn). Enabled gets the
// listener-config sanity check and the paired-peer inventory from the
// repurposed registry. The SPEC-0011 identity/certificate check is GONE with
// the msgbrowse-issued identity itself (SPEC-0014 REQ "Migration from
// SPEC-0011": no msgbrowse-issued certificate is generated or pinned for
// device sync); the live-daemon checks — engine running, peer connection
// state, folder completion/errors read from the REST API — ride the
// status/doctor story (#158). doctor stays network-silent except behind
// --check-llm.
//
// Governing: ADR-0021, SPEC-0014 REQ "Status and Doctor Surfacing" (the #158
// slice), REQ "Migration from SPEC-0011".
package cli

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/store"
)

// checkDeviceSync reports the device-sync posture. Disabled is the healthy
// default (pass, not warn — unlike archive roots, most installs never enable
// this). Enabled gets two checks: listener config sanity and the paired-peer
// inventory.
func checkDeviceSync(ctx context.Context, r *report, cfg *config.Config, st *store.Store) {
	if !cfg.DeviceSync.Enabled {
		r.add(statusPass, "device sync disabled (no sync engine; loopback-only posture per ADR-0010)", "")
		return
	}

	checkDeviceSyncPorts(r, cfg)
	checkDeviceSyncPeers(ctx, r, st)
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

// checkDeviceSyncPeers lists the paired peers from the repurposed registry
// (Syncthing device IDs, SPEC-0014 "Schema tables carry Syncthing
// identifiers").
func checkDeviceSyncPeers(ctx context.Context, r *report, st *store.Store) {
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
	names := make([]string, len(peers))
	for i, p := range peers {
		names[i] = fmt.Sprintf("%s (%s)", p.Name, p.ShortID())
	}
	r.add(statusPass, fmt.Sprintf("%s paired: %s", plural(len(peers), "device"), strings.Join(names, ", ")), "")
}
