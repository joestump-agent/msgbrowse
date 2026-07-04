package cli

import (
	"fmt"

	"github.com/joestump/msgbrowse/internal/imageconv"
	"github.com/spf13/cobra"
)

func newMediaCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "media",
		Short: "Transcode non-web images (HEIC/TIFF) to cached JPEGs for the gallery",
		Long: "media converts image attachments browsers can't render (Apple HEIC/HEIF, TIFF)\n" +
			"into JPEG derivatives cached under <data_dir>/derived, so the gallery and\n" +
			"transcript display them. It is incremental (skips already-converted files) and\n" +
			"uses whatever converter is on PATH (sips / magick / convert / heif-convert);\n" +
			"with none installed it is a no-op and the UI shows placeholders.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig()
			if err != nil {
				return err
			}
			force, err := cmd.Flags().GetBool("force")
			if err != nil {
				return err
			}
			conc, err := cmd.Flags().GetInt("concurrency")
			if err != nil {
				return err
			}

			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()

			// Effective roots (cfg or existing managed root — issue #160), so a
			// desktop-onboarded data dir transcodes its managed archives too.
			roots := archiveRoots(cfg)
			sum, err := imageconv.Run(cmd.Context(), st, imageconv.Options{
				ArchiveRoot:         roots.Signal,
				IMessageArchiveRoot: roots.IMessage,
				WhatsAppArchiveRoot: roots.WhatsApp,
				DataDir:             cfg.DataDir,
				Concurrency:         conc,
				Force:               force,
			})
			if err != nil {
				return err
			}
			if sum.NoConverter {
				_, err = fmt.Fprintln(cmd.OutOrStdout(),
					"media: no image converter found on PATH (install sips, ImageMagick, or libheif's heif-convert); nothing converted")
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"media: %d converted, %d already cached, %d source-missing, %d failed (of %d candidates) in %dms\n",
				sum.Converted, sum.Skipped, sum.Missing, sum.Failed, sum.Scanned, sum.DurationMS)
			return err
		},
	}
	cmd.Flags().Bool("force", false, "re-convert even if a derivative already exists")
	cmd.Flags().Int("concurrency", 6, "number of images to convert in parallel")
	return cmd
}
