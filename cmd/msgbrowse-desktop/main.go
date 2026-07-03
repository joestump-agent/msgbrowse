//go:build desktop

// Command msgbrowse-desktop is the native desktop shell for msgbrowse: a
// Wails v2 window over the real internal/web server, started in-process and
// bound to a loopback ephemeral port. The webview talks plain HTTP to that
// server — the same handlers, templates, middleware, gzip, and security
// headers browser mode serves — so desktop and browser modes cannot diverge.
//
// This command is the only cgo in the repository (the webview bindings
// require it) and is isolated twice over: it lives in its own Go module so
// Wails' dependency tree never touches the core go.mod/go.sum, and its files
// carry the `desktop` build tag so no default build ever compiles them. Build
// with `make desktop-linux`, or from this directory:
//
//	CGO_ENABLED=1 go build -tags desktop,production,webkit2_41 .
//
// (drop webkit2_41 on distros that still ship webkit2gtk-4.0).
//
// Governing: ADR-0017 (desktop shell via Wails v2 wrapping the embedded
// server), SPEC-0010 REQ "Isolated cgo build target", REQ "Embedded server on
// a loopback ephemeral port", REQ "Native shell affordances", REQ "Graceful
// shutdown".
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/joestump/msgbrowse/cmd/msgbrowse-desktop/internal/embedded"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "msgbrowse-desktop:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgFile := flag.String("config", "", "config file (default: ./config.yaml or $HOME/.config/msgbrowse/config.yaml)")
	flag.Parse()

	cfg, err := embedded.LoadConfig(*cfgFile)
	if err != nil {
		return err
	}

	// One shutdown code path (SPEC-0010 "Graceful shutdown"): SIGINT/SIGTERM
	// cancel the same context that window close cancels — exactly the wiring
	// `msgbrowse serve` uses — and the cancelled context drives
	// http.Server.Shutdown inside web.(*Server).Serve.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	es, err := embedded.Start(ctx, cfg, slog.Default())
	if err != nil {
		return err
	}

	sh := newShell(es.Done())

	// Close the window when the context is cancelled (signal) or when the
	// embedded server exits on its own — an abnormally dead server must not
	// leave a live window, and an abnormally dead webview must not leave the
	// server running headless. On the normal window-close path OnShutdown has
	// already marked the shell down, so quit() is a no-op.
	go func() {
		select {
		case <-ctx.Done():
		case <-es.Done():
			stop()
		}
		sh.quit()
	}()

	runErr := runShell(&options.App{
		Title:  "msgbrowse",
		Width:  1280,
		Height: 860,
		// The asset server hosts only the bootstrap trampoline; the app itself
		// is served over loopback HTTP by internal/web (SPEC-0010 design
		// decision: loopback HTTP, not the Wails asset handler).
		AssetServer: &assetserver.Options{Handler: bootstrapHandler(es.URL)},
		Menu:        sh.menu(),
		OnStartup:   sh.startup,
		OnShutdown:  func(context.Context) { sh.markDown() },
		Linux: &linux.Options{
			ProgramName: "msgbrowse",
		},
	})

	// Window closed (or Quit, or the shell failed/crashed): cancel the server
	// context, drain in-flight requests, close the store, release the port.
	stop()
	return errors.Join(runErr, es.Close())
}

// runShell runs the Wails app, converting a webview panic (e.g. GTK failing
// to initialize on a machine without a display) into an error so run() still
// walks the graceful shutdown path — abnormal webview termination must not
// leave the server running headless (SPEC-0010 "Graceful shutdown").
func runShell(opts *options.App) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("desktop shell crashed: %v", r)
		}
	}()
	return wails.Run(opts)
}
