package onboard_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/ingest"
	"github.com/joestump/msgbrowse/internal/onboard"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// TestEnableLandsConversationsInStore is the end-to-end acceptance for
// SPEC-0013 REQ "One-click enable and import per source": a fake exporter writes
// a fixture Signal archive into staging, the runner adopts it into the managed
// root, and the REAL internal/ingest importer lands the conversations in a real
// store — asserted by querying the store afterward. This exercises the whole
// export→adopt→import seam against production import code, off macOS, with no cgo.
func TestEnableLandsConversationsInStore(t *testing.T) {
	dataDir := t.TempDir()
	st, err := store.Open(filepath.Join(dataDir, store.DBFileName))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	// The real importer: dispatch by source into internal/ingest (Signal) — the
	// same wiring the CLI/desktop layer uses.
	importer := onboard.ImporterFunc(func(ctx context.Context, src, root string) (onboard.ImportResult, error) {
		run, err := ingest.Run(ctx, st, ingest.Options{ArchiveRoot: root})
		if err != nil {
			return onboard.ImportResult{}, err
		}
		return onboard.ImportResult{
			ConversationsChanged: run.ConversationsChanged,
			MessagesAdded:        run.MessagesAdded,
			MessagesTotal:        run.MessagesTotal,
		}, nil
	})

	var mu sync.Mutex
	exporter := onboard.ExecRunner(func(ctx context.Context, name string, env []string, args ...string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		dest := args[len(args)-1] // <staging>/export
		convDir := filepath.Join(dest, "Alice")
		if err := os.MkdirAll(convDir, 0o755); err != nil {
			return "", err
		}
		chat := "[2022-01-01 10:00:00] Alice: Hello from the fixture\n" +
			"[2022-01-01 10:01:00] Me: Hi back\n"
		return "", os.WriteFile(filepath.Join(convDir, "chat.md"), []byte(chat), 0o644)
	})

	r, err := onboard.NewRunner(onboard.Config{
		Resolver: onboard.ToolResolverFunc(func(ctx context.Context, src string) (string, error) {
			return "/bundle/sigexport", nil
		}),
		Exec:     exporter,
		Importer: importer,
		DataDir:  dataDir,
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	defer r.Shutdown()

	if _, err := r.Enable(source.Signal); err != nil {
		t.Fatalf("Enable: %v", err)
	}

	// Poll to terminal.
	deadline := time.Now().Add(5 * time.Second)
	var done onboard.Progress
	for time.Now().Before(deadline) {
		if p, ok := r.Status(source.Signal); ok && p.Phase.Terminal() {
			done = p
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if done.Phase != onboard.PhaseDone {
		t.Fatalf("expected PhaseDone, got %q (err=%v)", done.Phase, done.Err)
	}

	// The conversation landed in the store.
	convs, err := st.ListConversations(context.Background())
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}
	if len(convs) != 1 {
		t.Fatalf("expected 1 conversation in the store, got %d", len(convs))
	}
	if convs[0].Source != source.Signal {
		t.Fatalf("conversation source = %q, want signal", convs[0].Source)
	}
	if done.Result.MessagesAdded != 2 {
		t.Fatalf("expected 2 messages added, got %d", done.Result.MessagesAdded)
	}
}

// TestExportArgs asserts the app-owned argv per source stays in lockstep with
// the proven command lines: copy-mode iMessage, Signal into export/, and — the
// issue #150 fix — WhatsApp's iOS-mode invocation against the detected live
// container DB + media dir (`-i -d <DB> -m <media> -o <dest> -j …/result.json
// --no-html`). The detected paths thread through as ExportSource.
func TestExportArgs(t *testing.T) {
	dest := "/data/archives/x.staging"
	const waDB = "/Users/j/Library/Group Containers/group.net.whatsapp.WhatsApp.shared/ChatStorage.sqlite"
	const waMedia = "/Users/j/Library/Group Containers/group.net.whatsapp.WhatsApp.shared/Message/Media"
	cases := []struct {
		src  string
		srcp onboard.ExportSource
		want []string
	}{
		{source.Signal, onboard.ExportSource{}, []string{filepath.Join(dest, "export")}},
		{source.IMessage, onboard.ExportSource{}, []string{"-f", "txt", "-c", "clone", "-o", dest}},
		{
			source.WhatsApp,
			onboard.ExportSource{DBPath: waDB, MediaDir: waMedia},
			[]string{"-i", "-d", waDB, "-m", waMedia, "-o", dest, "-j", filepath.Join(dest, "result.json"), "--no-html"},
		},
	}
	for _, tc := range cases {
		got, err := onboard.ExportArgs(tc.src, dest, tc.srcp)
		if err != nil {
			t.Fatalf("ExportArgs(%s): %v", tc.src, err)
		}
		if len(got) != len(tc.want) {
			t.Fatalf("ExportArgs(%s) = %v, want %v", tc.src, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("ExportArgs(%s)[%d] = %q, want %q", tc.src, i, got[i], tc.want[i])
			}
		}
	}
	// A WhatsApp Enable with no detected container DB must error rather than
	// invoke wtsexporter without `-d` (which exits 2).
	if _, err := onboard.ExportArgs(source.WhatsApp, dest, onboard.ExportSource{}); err == nil {
		t.Fatal("ExportArgs(whatsapp) with no detected DB should error")
	}
	if _, err := onboard.ExportArgs("bogus", dest, onboard.ExportSource{}); err == nil {
		t.Fatal("ExportArgs(bogus) should error")
	}
}
