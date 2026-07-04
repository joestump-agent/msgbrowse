// The `msgbrowse devices` namespace under the Syncthing sync engine
// (ADR-0021 supersedes ADR-0018). The SPEC-0011 surface this file used to
// hold — pairing windows, token payloads, the mTLS listener client, unpair
// by fingerprint — is retired: identity, transport, and discovery belong to
// the supervised Syncthing engine now, and pairing is the /settings
// device-ID QR flow (issue #157). What remains CLI-side is the read-only
// peer registry listing; unpair and status verbs are rebuilt on the
// Syncthing REST surface by the roles/unpair/doctor story (#158).
//
// Governing: ADR-0021 ("retire or repurpose the merged work"), SPEC-0014 REQ
// "Migration from SPEC-0011" (the token pairing windows and mTLS listener
// no longer exist in the build), REQ "Pairing via Device ID and QR".
package cli

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

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

			peers, err := st.ListSyncPeers(cmd.Context())
			if err != nil {
				return err
			}
			if len(peers) == 0 {
				fmt.Println("No devices paired. Pair one from Settings in the web UI.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tDEVICE ID\tFOLDERS\tPAIRED")
			for _, p := range peers {
				folders := "-"
				if len(p.Folders) > 0 {
					folders = strings.Join(p.Folders, ",")
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.Name, p.DeviceID, folders, p.PairedAt.Local().Format("2006-01-02 15:04"))
			}
			return w.Flush()
		},
	}
}
