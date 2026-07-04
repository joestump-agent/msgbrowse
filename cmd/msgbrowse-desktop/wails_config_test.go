// Guard tests for wails.json, the Wails CLI project config the desktop CI
// packaging matrix builds from (SPEC-0010 REQ "Release packaging via CI
// matrix", .github/workflows/desktop.yml). The matrix's artifact paths are
// derived from the name/output filename here — build/bin/msgbrowse on Linux,
// build/bin/msgbrowse.app on macOS — so a rename in one place but not the
// other must fail fast, headless, with CGO_ENABLED=0.
//
// Deliberately untagged (no `desktop` build tag): `make desktop-test` runs it
// on every platform without a webview toolchain.
package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestWailsConfigMatchesCIMatrix(t *testing.T) {
	raw, err := os.ReadFile("wails.json")
	if err != nil {
		t.Fatalf("read wails.json: %v", err)
	}

	var cfg struct {
		Name            string `json:"name"`
		OutputFilename  string `json:"outputfilename"`
		FrontendInstall string `json:"frontend:install"`
		FrontendBuild   string `json:"frontend:build"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("wails.json is not valid JSON: %v", err)
	}

	// The CI matrix zips build/bin/msgbrowse.app (darwin) and uploads
	// build/bin/msgbrowse (linux); both names flow from these fields. The app
	// is named `msgbrowse` — the same product as the CLI, in a native window.
	const want = "msgbrowse"
	if cfg.Name != want {
		t.Errorf("wails.json name = %q, want %q (the darwin .app bundle is <name>.app)", cfg.Name, want)
	}
	if cfg.OutputFilename != want {
		t.Errorf("wails.json outputfilename = %q, want %q (CI artifact paths depend on it)", cfg.OutputFilename, want)
	}

	// The shell has no JS frontend — the webview loads a Go-served trampoline
	// (bootstrap.go) and then plain loopback HTTP. CI builds with `-s` (skip
	// frontend build), which only stays valid while these commands are empty.
	if cfg.FrontendInstall != "" || cfg.FrontendBuild != "" {
		t.Errorf("wails.json frontend:install=%q frontend:build=%q, want both empty (no JS frontend; CI passes -s)",
			cfg.FrontendInstall, cfg.FrontendBuild)
	}
}
