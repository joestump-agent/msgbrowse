package cli

import (
	"log/slog"
	"os/signal"
	"syscall"

	"github.com/joestump/msgbrowse/internal/mcp"
	"github.com/spf13/cobra"
)

func newMCPCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run the Model Context Protocol server (stdio by default)",
		Long: "mcp runs the Model Context Protocol server so an MCP client (Claude\n" +
			"Desktop / Claude Code) can query your archive with citation-faithful\n" +
			"retrieval tools. It serves over stdio by default; pass --http to serve\n" +
			"streamable HTTP on --listen-addr instead.\n" +
			"\n" +
			"Semantic search embeds the query via llm.base_url; run `msgbrowse embed`\n" +
			"first so message embeddings exist.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := resolveConfig()
			if err != nil {
				return err
			}
			useHTTP, err := cmd.Flags().GetBool("http")
			if err != nil {
				return err
			}
			addr, _ := cmd.Flags().GetString("listen-addr")
			if addr == "" {
				addr = "127.0.0.1:8788"
			}

			st, err := openStore(cfg)
			if err != nil {
				return err
			}
			defer st.Close()

			// stdio is the default transport; logs must go to stderr (slog already
			// writes there) so they never corrupt the stdio JSON-RPC stream.
			// The client rides the shared swappable holder (#191) so this
			// wiring is byte-for-byte the desktop shell's; standalone `mcp`
			// simply never swaps it.
			holder := newLLMHolder(cfg)
			srv := mcp.NewServer(st, holder, mcp.Options{
				EmbedModelFunc: holder.EmbedModel,
				Version:        Version,
				Logger:         slog.Default(),
			})

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			if useHTTP {
				return srv.RunHTTP(ctx, addr)
			}
			return srv.RunStdio(ctx)
		},
	}
	cmd.Flags().Bool("http", false, "serve over streamable HTTP instead of stdio")
	cmd.Flags().String("listen-addr", "", "HTTP listen address when --http is set (default 127.0.0.1:8788)")
	return cmd
}
