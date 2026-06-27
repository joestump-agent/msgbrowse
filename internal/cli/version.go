package cli

import (
	"fmt"
	"runtime"

	"github.com/spf13/cobra"
)

// These are overridden at build time via -ldflags "-X ...". See the Makefile.
var (
	// Version is the semantic version or git describe string.
	Version = "dev"
	// Commit is the short git commit hash.
	Commit = "none"
	// BuildDate is the RFC3339 build timestamp.
	BuildDate = "unknown"
)

func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "sigbrowse %s (commit %s, built %s, %s)\n",
				Version, Commit, BuildDate, runtime.Version())
			return err
		},
	}
}
