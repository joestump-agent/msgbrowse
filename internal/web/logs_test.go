package web

import (
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/onboard"
	"github.com/joestump/msgbrowse/internal/source"
)

// TestLogsViewerSurfacesCapturedStderr is the issue #151 acceptance for the web
// layer: a failing exporter's captured combined stdout+stderr — the diagnostic
// detail that used to be discarded, leaving only "exit status N" — is rendered in
// the Logs view along with the exporter command line and exit status. The fake
// enabler reports a failed job whose JobLog carries the captured argparse error,
// standing in for the real WhatsApp exit-2 case.
func TestLogsViewerSurfacesCapturedStderr(t *testing.T) {
	srv := newEmptyStoreServer(t)
	const stderr = "usage: wtsexporter [-h] ...\nwtsexporter: error: the following arguments are required: -d/--db"
	fe := &fakeEnabler{progress: onboard.Progress{
		Phase:   onboard.PhaseFailed,
		Message: "WhatsApp export failed: exit status 2",
		Err:     onboard.ErrExportFailed,
		Log: onboard.JobLog{
			Tool:       "/bundle/venv/bin/wtsexporter",
			Args:       []string{"-i", "-d", "/Users/j/…/ChatStorage.sqlite", "-o", "/data/x.staging", "--no-html"},
			Output:     stderr,
			ExitStatus: "exit status 2",
		},
	}}
	srv.SetEnabler(fe)

	body := get(t, srv, "/logs").Body.String()

	// The captured exporter stderr is visible — the whole point of the Logs view.
	if !strings.Contains(body, "wtsexporter: error: the following arguments are required") {
		t.Error("/logs did not surface the captured exporter stderr")
	}
	// The exporter command line (tool + argv) is shown.
	if !strings.Contains(body, "/bundle/venv/bin/wtsexporter") || !strings.Contains(body, "--no-html") {
		t.Error("/logs did not render the exporter command line")
	}
	// The exit status is shown.
	if !strings.Contains(body, "exit status 2") {
		t.Error("/logs did not render the exit status")
	}
	// A failed job carries the Failed badge (state as text, not color alone).
	if !strings.Contains(body, "log-badge-failed") {
		t.Error("/logs failed job missing the Failed badge")
	}
}

// TestLogsViewerLinkedFromSettings confirms the Logs viewer is reachable from the
// Settings page (issue #151: "a Logs view reachable from Settings"), and that the
// moved Status & backups link is there too.
func TestLogsViewerLinkedFromSettings(t *testing.T) {
	srv := newEmptyStoreServer(t)
	body := get(t, srv, "/settings").Body.String()
	if !strings.Contains(body, `href="/logs"`) {
		t.Error("Settings page missing the Logs link")
	}
	if !strings.Contains(body, `href="/status"`) {
		t.Error("Settings page missing the moved Status & backups link")
	}
}

// TestBuiltCSSCarriesLogsComponents guards the ADR-0012 drift rule for the new
// Logs viewer classes: the committed, go:embed-served app.css must carry the log
// panel + badge + output rules (rebuild: rm -rf .tools && make css).
func TestBuiltCSSCarriesLogsComponents(t *testing.T) {
	css, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatalf("read app.css: %v", err)
	}
	out := string(css)
	for _, want := range []string{
		".log-entry-head",
		".log-entry-title",
		".log-badge",
		".log-badge-failed",
		".log-badge-done",
		".log-badge-active",
		".log-status-line",
		".log-meta",
		".log-meta-row",
		".log-output",
		".log-empty",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("built app.css missing %q (rebuild: rm -rf .tools && make css)", want)
		}
	}
}

// TestLogsViewerEmptyWithNoJobs renders cleanly with no jobs run: one placeholder
// entry per source, no crash.
func TestLogsViewerEmptyWithNoJobs(t *testing.T) {
	srv := newEmptyStoreServer(t)
	fe := &fakeEnabler{} // Status returns ok=false for every source
	srv.SetEnabler(fe)
	rec := get(t, srv, "/logs")
	if rec.Code != 200 {
		t.Fatalf("/logs status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, src := range source.All {
		if !strings.Contains(body, source.Label(src)) {
			t.Errorf("/logs missing entry for %s", source.Label(src))
		}
	}
	if !strings.Contains(body, "log-empty") {
		t.Error("/logs missing the no-runs-yet placeholder")
	}
}
