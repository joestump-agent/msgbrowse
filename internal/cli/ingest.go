package cli

import (
	"github.com/spf13/cobra"
)

func newIngestCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ingest",
		Short: "Scan the archive and (incrementally) populate the database",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := resolveConfig(); err != nil {
				return err
			}
			return errNotImplemented
		},
	}
	cmd.Flags().Bool("full", false, "ignore incremental state and re-scan every conversation")
	return cmd
}
