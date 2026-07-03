//go:build desktop

// Native shell affordances and quit bookkeeping for the desktop window.
//
// Governing: ADR-0017, SPEC-0010 REQ "Native shell affordances" (application
// menu with standard quit semantics, meaningful window title) and REQ
// "Graceful shutdown" (every quit path funnels into one context cancel).
package main

import (
	"context"
	goruntime "runtime"
	"sync"

	"github.com/wailsapp/wails/v2/pkg/menu"
	"github.com/wailsapp/wails/v2/pkg/menu/keys"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// shell tracks the Wails runtime context so quit requests arriving from any
// goroutine (menu callback, signal watcher, dead-server watcher) can close
// the window exactly once, at any point in the app lifecycle.
type shell struct {
	mu         sync.Mutex
	ctx        context.Context // Wails runtime context, set by OnStartup
	down       bool            // the app is already quitting / has quit
	serverDone <-chan struct{} // closed when the embedded server has exited
}

func newShell(serverDone <-chan struct{}) *shell {
	return &shell{serverDone: serverDone}
}

// startup captures the Wails runtime context. If the embedded server already
// died while Wails was still starting, quit immediately rather than present a
// window over nothing.
func (sh *shell) startup(ctx context.Context) {
	sh.mu.Lock()
	sh.ctx = ctx
	sh.mu.Unlock()
	select {
	case <-sh.serverDone:
		sh.quit()
	default:
	}
}

// markDown records that the Wails app has begun shutting down (OnShutdown),
// making any later quit() a no-op.
func (sh *shell) markDown() {
	sh.mu.Lock()
	sh.down = true
	sh.mu.Unlock()
}

// quit closes the window via the Wails runtime, once. Before OnStartup it is
// a no-op (startup re-checks the dead-server case); after OnShutdown it is a
// no-op too.
func (sh *shell) quit() {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.down || sh.ctx == nil {
		return
	}
	sh.down = true
	wailsruntime.Quit(sh.ctx)
}

// menu builds the native application menu. On macOS the standard app menu
// supplies Cmd+Q quit (and the Edit menu makes clipboard shortcuts reach the
// webview); everywhere a File menu carries the platform's conventional quit
// accelerator. All quit paths call sh.quit(), which funnels into the single
// shutdown context in run().
func (sh *shell) menu() *menu.Menu {
	m := menu.NewMenu()
	if goruntime.GOOS == "darwin" {
		m.Append(menu.AppMenu())
		m.Append(menu.EditMenu())
	}
	file := m.AddSubmenu("File")
	file.AddText("Quit msgbrowse", keys.CmdOrCtrl("q"), func(*menu.CallbackData) {
		sh.quit()
	})
	return m
}
