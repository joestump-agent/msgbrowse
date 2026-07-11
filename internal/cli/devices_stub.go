//go:build !devicesync

// Default build: the `msgbrowse devices` namespace (device-sync peer
// management) is not compiled in — device sync is gated behind the `devicesync`
// build tag (ADR-0021 / SPEC-0014) and excluded from release binaries. This stub
// keeps root.go's command registration unchanged while linking WITHOUT
// internal/devices, internal/devsync, or internal/syncthing: it registers a
// hidden `devices` command that explains the feature is not built in.
package cli

import (
	"github.com/spf13/cobra"
)

// newDevicesCommand returns a hidden placeholder in builds without the
// `devicesync` tag. Hidden so it does not clutter --help, but present so an
// operator who runs `msgbrowse devices` gets a clear explanation rather than an
// "unknown command" error.
func newDevicesCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "devices",
		Short:  "Manage device-sync peers (not built into this binary)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.Println("device sync is not built into this binary (feature gated behind the `devicesync` build tag).")
			return nil
		},
	}
}
