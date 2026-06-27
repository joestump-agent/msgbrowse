// Command sigbrowse is a self-hosted, local-only browser, search engine, and
// MCP server over a signal-export archive.
//
// It exposes several subcommands:
//
//	sigbrowse ingest   scan the archive and (incrementally) populate the database
//	sigbrowse serve    run the local HTMX web UI
//	sigbrowse mcp      run the Model Context Protocol server
//	sigbrowse watch    re-ingest automatically when the archive changes
//	sigbrowse journal  (re)build the day-by-day journal and optional LLM digests
//
// Everything runs against an on-disk, already-decrypted signal-export tree that
// is treated as strictly read-only. See README.md and ARCHITECTURE.md for the
// full design.
package main

import (
	"fmt"
	"os"

	"github.com/joestump/sigbrowse/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "sigbrowse:", err)
		os.Exit(1)
	}
}
