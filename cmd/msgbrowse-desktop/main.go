//go:build desktop

// Command msgbrowse-desktop is the native desktop shell for msgbrowse: a
// Wails v2 window over the real internal/web server, started in-process and
// bound to a loopback ephemeral port. The webview talks plain HTTP to that
// server — the same handlers, templates, middleware, gzip, and security
// headers browser mode serves — so desktop and browser modes cannot diverge.
// The MCP streamable-HTTP handler rides the same listener at /mcp, and a
// menubar status item (fyne.io/systray) keeps the app resident: closing the
// window hides it, quitting is explicit (SPEC-0010 "Menubar residency").
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
// a loopback ephemeral port", REQ "Native shell affordances", REQ "Menubar
// residency", REQ "Menubar quick menu", REQ "Graceful shutdown".
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
	"time"

	"github.com/joestump/msgbrowse/cmd/msgbrowse-desktop/internal/embedded"
	"github.com/joestump/msgbrowse/cmd/msgbrowse-desktop/internal/tray"
	"github.com/joestump/msgbrowse/internal/mcp"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "msgbrowse:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgFile := flag.String("config", "", "config file (default: ./config.yaml or $HOME/.config/msgbrowse/config.yaml)")
	// Menubar-only launch is behind a flag rather than the default (SPEC-0010
	// SHOULD): until the status item is validated on macOS hardware, a
	// default-hidden launch could strand users with no window and no tray.
	// The packaging story flips the default once that validation lands.
	hidden := flag.Bool("hidden", false, "start menubar-only: keep the window hidden until View Messages is chosen from the tray")
	flag.Parse()

	cfg, err := embedded.LoadConfig(*cfgFile)
	if err != nil {
		return err
	}

	// One shutdown code path (SPEC-0010 "Graceful shutdown"): SIGINT/SIGTERM
	// cancel the same context that quitting cancels — exactly the wiring
	// `msgbrowse serve` uses — and the cancelled context drives
	// http.Server.Shutdown inside web.(*Server).ServeHandler.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	es, err := embedded.Start(ctx, cfg, slog.Default())
	if err != nil {
		return err
	}

	sh := newShell(es.URL)

	// Quit when the context is cancelled (signal) or when the embedded server
	// exits on its own — an abnormally dead server must not leave a live
	// window, and an abnormally dead webview must not leave the server
	// running headless. A request arriving before OnStartup is latched by the
	// shell state and replayed once the runtime context exists (the #114
	// startup race: signals in that window used to be dropped).
	go func() {
		select {
		case <-ctx.Done():
		case <-es.Done():
			stop()
		}
		sh.quit()
	}()

	// The menubar quick menu (SPEC-0010): payloads come from the embedded
	// server's real bound address; the config block builder is shared with
	// the future /settings page via internal/mcp.
	trayStart, trayStop := setupTray(&tray.Menu{
		Endpoint:   es.MCPURL,
		ConfigJSON: mcp.ClientConfigJSON(es.MCPURL),
		Actions: tray.Actions{
			ShowWindow:  sh.showWindow,
			OpenPairing: sh.openPairing,
			CopyText:    sh.copyText,
			Quit:        sh.quit,
			Probe: func() bool {
				probeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				return es.Healthy(probeCtx)
			},
		},
	})
	// Register the status item before Wails takes the main loop: on macOS
	// this runs on the main thread (required for NSStatusItem) and its menu
	// updates then ride the NSApplication loop Wails is about to start; on
	// Linux the systray backend is loop-independent D-Bus. See traymenu.go.
	trayStart()

	runErr := runShell(&options.App{
		Title:  "msgbrowse",
		Width:  1280,
		Height: 860,
		// The asset server hosts only the bootstrap trampoline; the app itself
		// is served over loopback HTTP by internal/web (SPEC-0010 design
		// decision: loopback HTTP, not the Wails asset handler).
		AssetServer: &assetserver.Options{Handler: bootstrapHandler(es.URL)},
		Menu:        sh.menu(),
		StartHidden: *hidden,
		// Close-to-tray (SPEC-0010 "Menubar residency"): the native
		// hide-on-close path hides the window and never enters the quit
		// flow. OnBeforeClose is deliberately NOT used for this — in Wails
		// v2 the window-close button and every explicit quit path (tray
		// Quit, Cmd+Q, runtime.Quit) funnel into the same OnBeforeClose
		// callback, so hiding there would swallow Cmd+Q; quitting MUST stay
		// explicit and *working* (recorded in design.md).
		HideWindowOnClose: true,
		OnStartup:         sh.startup,
		OnShutdown:        sh.shutdown,
		Linux: &linux.Options{
			ProgramName: "msgbrowse",
		},
	})

	// Window quit (tray/menu/Cmd+Q, or the shell failed/crashed): tear down
	// the status item, cancel the server context, drain in-flight requests,
	// close the store, release the port.
	trayStop()
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
