// Package cli wires sigbrowse's Cobra command tree to its Viper configuration.
//
// The root command owns the persistent flags shared by every subcommand and the
// config-loading lifecycle; each subcommand lives in its own file and receives a
// fully-resolved *config.Config via resolveConfig.
package cli

import (
	"log/slog"
	"os"

	"github.com/joestump/sigbrowse/internal/config"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// cfgFile holds the value of the global --config flag.
var cfgFile string

// v is the process-wide Viper instance, initialized in the root
// PersistentPreRunE after flag parsing.
var v *viper.Viper

// NewRootCommand builds the root `sigbrowse` command and attaches every
// subcommand. It is exported so tests can exercise the command tree.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "sigbrowse",
		Short: "Browse, search, and semantically query a local signal-export archive",
		Long: "sigbrowse is a self-hosted, local-only web app and MCP server over a\n" +
			"signal-export archive. It treats the archive as strictly read-only and keeps\n" +
			"all data on the machine; the only network egress is the configured\n" +
			"OpenAI-compatible LLM endpoint.",
		SilenceUsage:  true,
		SilenceErrors: true,
		// PersistentPreRunE loads config and binds flags once, before any
		// subcommand RunE. Using it (instead of cobra.OnInitialize) gives us the
		// invoked command's flag set directly.
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			return initConfig(cmd)
		},
	}

	pf := root.PersistentFlags()
	pf.StringVar(&cfgFile, "config", "", "config file (default: ./config.yaml or $HOME/.config/sigbrowse/config.yaml)")
	pf.String("archive-root", "", "path to the signal-export archive (read-only)")
	pf.String("data-dir", "", "writable directory for the database and caches")
	pf.String("log-level", "", "log level: debug, info, warn, error")

	root.AddCommand(
		newIngestCommand(),
		newServeCommand(),
		newMCPCommand(),
		newWatchCommand(),
		newJournalCommand(),
		newVersionCommand(),
	)
	return root
}

// Execute runs the root command. It is the single entry point used by main.
func Execute() error {
	return NewRootCommand().Execute()
}

// initConfig loads Viper and binds the persistent flags onto their config keys.
// Only flags the user actually changed override file/env values, because Viper
// consults flag.Changed when reading a bound pflag.
func initConfig(cmd *cobra.Command) error {
	var err error
	v, err = config.Load(cfgFile)
	if err != nil {
		return err
	}
	pf := cmd.Root().PersistentFlags()
	if err := v.BindPFlag("archive_root", pf.Lookup("archive-root")); err != nil {
		return err
	}
	if err := v.BindPFlag("data_dir", pf.Lookup("data-dir")); err != nil {
		return err
	}
	if err := v.BindPFlag("log_level", pf.Lookup("log-level")); err != nil {
		return err
	}
	return nil
}

// resolveConfig unmarshals and validates the active configuration and configures
// the default slog logger. Every subcommand calls this at the top of its RunE.
func resolveConfig() (*config.Config, error) {
	cfg, err := config.Unmarshal(v)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	configureLogger(cfg.LogLevel)
	return cfg, nil
}

// configureLogger installs a slog logger at the requested level as the default.
func configureLogger(level string) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	slog.SetDefault(slog.New(h))
}
