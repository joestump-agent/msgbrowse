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
	goruntime "runtime"
	"syscall"
	"time"

	"github.com/joestump/msgbrowse/cmd/msgbrowse-desktop/internal/bootstrap"
	"github.com/joestump/msgbrowse/cmd/msgbrowse-desktop/internal/embedded"
	"github.com/joestump/msgbrowse/cmd/msgbrowse-desktop/internal/shellnotes"
	"github.com/joestump/msgbrowse/cmd/msgbrowse-desktop/internal/tray"
	"github.com/joestump/msgbrowse/internal/mcp"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/linux"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
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

	// Shell diagnostics ring buffer (issue #167): systray/dock startup notes,
	// mirrored to slog and surfaced on the web app's Logs page below, so a
	// menubar icon that never renders on real hardware is observable in-app.
	notes := shellnotes.New(slog.Default())

	// One shutdown code path (SPEC-0010 "Graceful shutdown"): SIGINT/SIGTERM
	// cancel the same context that quitting cancels — exactly the wiring
	// `msgbrowse serve` uses — and the cancelled context drives
	// http.Server.Shutdown inside web.(*Server).ServeHandler.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The shell is constructed BEFORE the embedded server so its openExternal
	// can be wired into the web layer's /desktop/open-url bridge (issue #179)
	// — the server serves the instant Start returns, and late seam wiring
	// would race. It learns the server URL immediately below; every reader of
	// baseURL (the tray deep links) starts after that write.
	sh := newShell()

	es, err := embedded.Start(ctx, cfg, slog.Default(),
		embedded.WithShellNotes(notes.Snapshot),
		embedded.WithExternalOpener(sh.openExternal))
	if err != nil {
		return err
	}
	sh.baseURL = es.URL

	// Resolve + integrity-check the bundled exporter toolchain (SPEC-0013 REQ
	// "Bundled tool integrity and version check": versions recorded for the About
	// view; a broken bundle is a clear state, not a crash). This runs OFF the
	// launch path in its own goroutine so the window opens immediately: the probe
	// spawns up to four synchronous subprocess version checks (incl. a 1–2s cold
	// wtsexporter --help import) that would otherwise delay wails.Run below. Its
	// result only feeds logs and the About view — nothing on the launch path
	// waits on it, and a corrupt .app surfaces per-source when the user clicks
	// Enable, so deferring it never strands the window. In the non-bundled
	// dev/Linux build it is a quick no-op (Bundled=false, no error).
	go logBundledToolchain(ctx, slog.Default())

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
	trayStart, trayStop, trayReadyCh := setupTray(&tray.Menu{
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
	}, notes)
	// Register the status item per platform (issue #167). On macOS
	// scheduleTrayStart DEFERS start() onto the GCD main queue so the
	// NSStatusItem is created on the main thread after the NSApplication
	// wails.Run is about to start has finished launching — creating it
	// earlier (the #122 wiring) is the documented way to get a status item
	// that silently never renders. On Linux it runs immediately: the systray
	// backend is loop-independent D-Bus. See tray_platform_*.go/traymenu.go.
	notes.Infof("menubar: scheduling status-item registration (%s)", goruntime.GOOS)
	scheduleTrayStart(trayStart)
	// Watchdog (issue #167): systray reports no error, so race its onReady
	// against a deadline and surface the verdict — success arms the dock
	// auto-hide, failure is an error note on the Logs page, never silence.
	go watchTray(ctx, notes, trayReadyCh, *hidden)

	runErr := runShell(&options.App{
		Title:  "msgbrowse",
		Width:  1280,
		Height: 860,
		// Slate window background (issue #166): the window itself paints the
		// app's base-100 (#0f1216, SPEC-0006) behind the webview — paired
		// with the slate trampoline splash (internal/bootstrap) and the
		// transparent-webview setting below, launch never flashes white.
		BackgroundColour: options.NewRGB(0x0f, 0x12, 0x16),
		// The asset server hosts only the bootstrap trampoline; the app itself
		// is served over loopback HTTP by internal/web (SPEC-0010 design
		// decision: loopback HTTP, not the Wails asset handler).
		AssetServer: &assetserver.Options{Handler: bootstrap.Handler(es.URL)},
		Menu:        sh.menu(),
		StartHidden: *hidden,
		// Close-to-tray (SPEC-0010 "Menubar residency"): the native
		// hide-on-close path hides the window and never enters the quit
		// flow. OnBeforeClose is deliberately NOT used for this — in Wails
		// v2 the window-close button and every explicit quit path (tray
		// Quit, Cmd+Q, runtime.Quit) funnel into the same OnBeforeClose
		// callback, so hiding there would swallow Cmd+Q; quitting MUST stay
		// explicit and *working* (recorded in design.md). On macOS this path
		// runs [NSApp hide:], which (once the tray is confirmed present) also
		// drops the Dock icon via the accessory policy — see
		// tray_platform_darwin.go.
		HideWindowOnClose: true,
		OnStartup:         sh.startup,
		OnShutdown:        sh.shutdown,
		Mac: &mac.Options{
			// Truly native header (issue #165, SPEC-0010 "Native shell
			// affordances"): hide the macOS title bar so the traffic lights
			// overlay the web app's unified toolbar — which pads itself past
			// them and declares itself the drag region (desktop-chrome body
			// class + --wails-draggable, see internal/web). HiddenInset gives
			// the standard inset traffic-light placement that matches the
			// toolbar's 52px height.
			TitleBar: mac.TitleBarHiddenInset(),
			// Follow the system appearance; the page paints its own theme.
			Appearance: mac.DefaultAppearance,
			// Transparent WEBVIEW (not window): WKWebView then skips its own
			// white background layer, so the slate window colour above shows
			// through until first paint (#166). The window itself stays
			// opaque — the toolbar's frosted look blurs page content, not the
			// desktop, so window translucency/vibrancy is deliberately off.
			WebviewIsTransparent: true,
			WindowIsTranslucent:  false,
		},
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

// trayReadyTimeout is how long the watchdog waits for systray's onReady
// before declaring the tray unavailable. Registration normally completes on
// the run loop's first turns (well under a second); 30s tolerates a slow
// first launch (Gatekeeper checks, cold caches) without leaving a real
// failure unreported for long.
const trayReadyTimeout = 30 * time.Second

// watchTray races the tray's onReady signal against a deadline and surfaces
// the verdict (issue #167: systray reports no registration error — onReady is
// the only success signal, and silence was how the #122 menubar shipped
// broken on real hardware).
//
// On success it also arms the macOS dock auto-hide (owner preference: the app
// "hides up in the menubar" — accessory activation policy while the window is
// hidden). That coupling is deliberate: with no working tray, dropping the
// Dock icon would strand the user with no way back to the window, so a
// Timeout leaves Dock behavior fully standard. With --hidden (menubar-only
// launch) the Dock icon is dropped immediately once the tray is confirmed.
func watchTray(ctx context.Context, notes *shellnotes.Log, ready <-chan struct{}, hidden bool) {
	deadline := make(chan struct{})
	timer := time.AfterFunc(trayReadyTimeout, func() { close(deadline) })
	defer timer.Stop()

	switch tray.AwaitReady(ready, deadline, ctx.Done()) {
	case tray.Ready:
		notes.Infof("menubar: status item registered (icon + quick menu installed)")
		enableDockAutoHide()
		notes.Infof("dock: auto-hide armed — hiding the window switches to the accessory policy (no Dock icon); View Messages restores it")
		if hidden {
			setDockVisible(false)
			notes.Infof("dock: hidden at launch (menubar-only --hidden mode)")
		}
	case tray.Timeout:
		notes.Errorf("menubar: status item did not register within %s — tray unavailable; window and Dock remain primary (issue #167)", trayReadyTimeout)
	case tray.Cancelled:
		// Shutting down before a verdict; nothing to report.
	}
}
