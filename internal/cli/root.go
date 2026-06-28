// Package cli wires msgbrowse's Cobra command tree to its Viper configuration.
//
// The root command owns the persistent flags shared by every subcommand and the
// config-loading lifecycle; each subcommand lives in its own file and receives a
// fully-resolved *config.Config via resolveConfig.
package cli

import (
	"errors"
	"log/slog"
	"os"

	charmlog "github.com/charmbracelet/log"
	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/imessage"
	"github.com/joestump/msgbrowse/internal/ingest"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// cfgFile holds the value of the global --config flag.
var cfgFile string

// v is the process-wide Viper instance, initialized in the root
// PersistentPreRunE after flag parsing.
var v *viper.Viper

// NewRootCommand builds the root `msgbrowse` command and attaches every
// subcommand. It is exported so tests can exercise the command tree.
func NewRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "msgbrowse",
		Short: "Browse, search, and editorialize your message archives locally",
		Long: "msgbrowse is a self-hosted, local-only web app and MCP server over your\n" +
			"message-archive exports. It treats every archive as strictly read-only and\n" +
			"keeps all data on the machine; the only network egress is the configured\n" +
			"OpenAI-compatible LLM endpoint.\n" +
			"\n" +
			"Sources: signal-export (signal-import) and imessage-exporter (imessage-import).",
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
	pf.StringVar(&cfgFile, "config", "", "config file (default: ./config.yaml or $HOME/.config/msgbrowse/config.yaml)")
	pf.String("archive-root", "", "path to the signal-export archive (read-only)")
	pf.String("imessage-archive-root", "", "path to the imessage-exporter archive (read-only)")
	pf.String("data-dir", "", "writable directory for the database and caches")
	pf.String("log-level", "", "log level: debug, info, warn, error")

	root.AddCommand(
		newImportCommand(),
		newSignalImportCommand(),
		newIngestAliasCommand(),
		newIMessageImportCommand(),
		newDoctorCommand(),
		newEmbedCommand(),
		newFactsCommand(),
		newMediaCommand(),
		newServeCommand(),
		newMCPCommand(),
		newWatchCommand(),
		newJournalCommand(),
		newVersionCommand(),
	)
	return root
}

// Execute runs the root command. It is the single entry point used by main.
//
// It installs a pretty default logger up front (so even config-load failures,
// which happen before per-command config resolution, render nicely) and reports
// the final error through that logger rather than as a bare line.
func Execute() error {
	configureLogger("info")
	if err := NewRootCommand().Execute(); err != nil {
		renderError(err)
		return err
	}
	return nil
}

// renderError prints a command failure as a styled error, appending an
// actionable hint for the failure modes users actually hit.
func renderError(err error) {
	// `doctor` already printed a full human-readable report to stdout; its
	// sentinel only exists to make the process exit non-zero. Don't double-report
	// it as a logger error line.
	if errors.Is(err, errDoctorFailed) {
		return
	}
	if hint := errorHint(err); hint != "" {
		slog.Error(err.Error(), "hint", hint)
		return
	}
	slog.Error(err.Error())
}

// errorHint maps known failures to an actionable next step. Matching is on
// sentinel errors (errors.Is), not strings, so it stays robust as messages
// evolve.
func errorHint(err error) string {
	switch {
	case errors.Is(err, ingest.ErrExportDirNotFound):
		return "archive_root must be the folder that CONTAINS export/ — e.g. .../Signal-Archive, not .../Signal-Archive/export. " +
			"In Docker, point MSGBROWSE_ARCHIVE_HOST in .env at that folder."
	case errors.Is(err, imessage.ErrArchiveNotFound):
		return "imessage_archive_root must point at the imessage-exporter output directory " +
			"(the folder of <ChatName>.txt files). Set --imessage-archive-root or MSGBROWSE_IMESSAGE_ARCHIVE_ROOT."
	}
	return ""
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
	if err := v.BindPFlag("imessage_archive_root", pf.Lookup("imessage-archive-root")); err != nil {
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

// configureLogger installs a charmbracelet/log logger (pretty, colorized,
// leveled) as the slog default. charmbracelet/log implements slog.Handler, so
// every existing slog call across import/serve/mcp gets the styled output with
// no call-site changes. Output goes to STDERR — important for `mcp` over stdio,
// whose JSON-RPC stream must stay on stdout uncorrupted.
func configureLogger(level string) {
	slog.SetDefault(slog.New(newLogHandler(level)))
}

// newLogHandler builds the charmbracelet/log handler at the requested level.
func newLogHandler(level string) slog.Handler {
	lvl := charmlog.InfoLevel
	switch level {
	case "debug":
		lvl = charmlog.DebugLevel
	case "warn":
		lvl = charmlog.WarnLevel
	case "error":
		lvl = charmlog.ErrorLevel
	}
	logger := charmlog.NewWithOptions(os.Stderr, charmlog.Options{
		Level:           lvl,
		ReportTimestamp: true,
		TimeFormat:      "15:04:05",
	})
	// Prefix each level with an emoji so status reads at a glance. We keep the
	// level word (and charm's color) and just swap the rendered label string.
	styles := charmlog.DefaultStyles()
	for level, label := range map[charmlog.Level]string{
		charmlog.DebugLevel: "🔍 DEBUG",
		charmlog.InfoLevel:  "✅ INFO",
		charmlog.WarnLevel:  "⚠️ WARN",
		charmlog.ErrorLevel: "🛑 ERROR",
		charmlog.FatalLevel: "💀 FATAL",
	} {
		// Drop the default 4-cell MaxWidth (it would truncate "✅ INFO" → "✅ I");
		// keep the level's color from DefaultStyles.
		styles.Levels[level] = styles.Levels[level].SetString(label).UnsetMaxWidth()
	}
	logger.SetStyles(styles)
	return logger
}
