package web_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/onboardsvc"
	"github.com/joestump/msgbrowse/internal/setup"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
	"github.com/joestump/msgbrowse/internal/web"
)

// signalReadyDetector lays out a temp HOME with Signal Desktop present and an
// unencrypted config.json (no sealed key → keychain probe reports OK), so the
// Signal card renders Ready and /setup emits a live Enable button + token.
func signalReadyDetector(t *testing.T) setup.Detector {
	t.Helper()
	home := t.TempDir()
	sigDir := filepath.Join(home, "Library", "Application Support", "Signal")
	if err := os.MkdirAll(sigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// No encryptedKey → ProbeSignalKeychain returns OK → Ready.
	if err := os.WriteFile(filepath.Join(sigDir, "config.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return setup.Detector{Home: home}
}

// fakeExporterResolver points every source at a stub executable this test writes
// — a tiny shell script that reproduces the exporter's I/O contract (writing a
// fixture archive into the staging dir passed as its destination). This drives
// the REAL onboard.Runner + real internal/ingest importer through the REAL web
// HTTP handler, off macOS, with no bundled toolchain.
type fakeExporterResolver struct{ bin string }

func (f fakeExporterResolver) ResolveTool(_ context.Context, _ string) (string, error) {
	return f.bin, nil
}

// writeFakeSignalExporter writes an executable shell stub that mimics
// `sigexport <dest>`: it creates <dest>/Alice/chat.md. (ExportArgs passes
// <staging>/export as the positional arg, so $1 is the export dir the importer
// scans.) Returns the stub's path.
func writeFakeSignalExporter(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-sigexport")
	script := "#!/bin/sh\n" +
		"dest=\"$1\"\n" +
		"mkdir -p \"$dest/Alice\"\n" +
		"printf '[2022-01-01 10:00:00] Alice: hi from the stub\\n[2022-01-01 10:01:00] Me: hello\\n' > \"$dest/Alice/chat.md\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// TestEnableHTTPEndToEnd is the full-stack acceptance for SPEC-0013 REQ
// "One-click enable and import per source", driven entirely through the HTTP
// surface: POST /setup/enable (same-origin + token) starts a real background job
// that runs a stub exporter into staging, adopts it into the managed root, and
// imports it with internal/ingest; polling GET /setup/status/signal over HTTP
// reaches "done"; and the conversation is then present in the store the same
// server renders. No cgo, no macOS, no bundled tools.
func TestEnableHTTPEndToEnd(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.Open(filepath.Join(dataDir, store.DBFileName))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := &config.Config{DataDir: dataDir}
	srv, err := web.NewServer(st, cfg, nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	runner, err := onboardsvc.Build(cfg, st, fakeExporterResolver{bin: writeFakeSignalExporter(t)}, nil)
	if err != nil {
		t.Fatalf("build runner: %v", err)
	}
	t.Cleanup(runner.Shutdown)
	srv.SetEnabler(runner)
	srv.SetDetector(signalReadyDetector(t)) // Signal Ready → live Enable button + token

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	client := ts.Client()

	// 1. Render /providers (the renamed Setup surface) to obtain a live
	//    per-session token from the page.
	setupResp, err := client.Get(ts.URL + "/providers")
	if err != nil {
		t.Fatalf("GET /providers: %v", err)
	}
	setupBody := readBody(t, setupResp)
	token := extractToken(t, setupBody)

	// 2. POST /setup/enable (same-origin Origin + token) to start the job.
	form := url.Values{}
	form.Set("source", source.Signal)
	form.Set("setup_token", token)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/setup/enable", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", ts.URL) // same-origin: the test server's own base URL
	enableResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /setup/enable: %v", err)
	}
	if enableResp.StatusCode != http.StatusOK {
		t.Fatalf("enable status = %d, want 200", enableResp.StatusCode)
	}
	_ = readBody(t, enableResp)

	// 3. Poll GET /setup/status/signal until a terminal phase.
	deadline := time.Now().Add(5 * time.Second)
	var lastBody string
	for time.Now().Before(deadline) {
		resp, err := client.Get(ts.URL + "/setup/status/" + source.Signal)
		if err != nil {
			t.Fatalf("GET status: %v", err)
		}
		lastBody = readBody(t, resp)
		// A terminal fragment no longer carries the active poller.
		if !strings.Contains(lastBody, `hx-get="/setup/status/signal"`) &&
			(strings.Contains(lastBody, "setup-progress-ok") ||
				strings.Contains(lastBody, "setup-progress-err")) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(lastBody, "setup-progress-ok") {
		t.Fatalf("job did not reach a successful terminal state; last fragment:\n%s", lastBody)
	}
	if !strings.Contains(lastBody, "Enabled Signal") {
		t.Errorf("terminal fragment missing the enabled message; got:\n%s", lastBody)
	}

	// 4. The conversation is in the store the server renders.
	convs, err := st.ListConversations(context.Background())
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	if len(convs) != 1 || convs[0].Source != source.Signal {
		t.Fatalf("store has %d conversations (want 1 signal): %+v", len(convs), convs)
	}

	// 5. The managed root holds the adopted archive; staging is gone.
	managedRoot := filepath.Join(dataDir, "archives", "signal")
	if _, err := os.Stat(filepath.Join(managedRoot, "export", "Alice", "chat.md")); err != nil {
		t.Fatalf("managed root not populated: %v", err)
	}
	if _, err := os.Stat(managedRoot + ".staging"); !os.IsNotExist(err) {
		t.Errorf("staging dir survived a successful adopt")
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

// extractToken pulls the per-session token out of the rendered /setup page's
// hx-headers attribute (X-Setup-Token) on the live Enable button.
func extractToken(t *testing.T, body string) string {
	t.Helper()
	const marker = `"X-Setup-Token": "`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("no X-Setup-Token in /setup body (is an Enabler wired and a source Ready?)")
	}
	rest := body[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		t.Fatal("malformed X-Setup-Token attribute")
	}
	return rest[:j]
}
