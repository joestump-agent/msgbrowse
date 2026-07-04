package onboardsvc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/onboard"
	"github.com/joestump/msgbrowse/internal/setup"
	"github.com/joestump/msgbrowse/internal/source"
)

// TestDetectSourceResolverThreadsWhatsAppPaths proves the issue #150 wiring: the
// production source resolver hands the runner the detected WhatsApp container DB
// + Message/Media paths, so wtsexporter's iOS-mode argv gets its `-d`/`-m` values
// instead of exiting 2. It fakes the macOS group-container layout on Linux.
func TestDetectSourceResolverThreadsWhatsAppPaths(t *testing.T) {
	home := t.TempDir()
	container := filepath.Join(home, "Library", "Group Containers", "group.net.whatsapp.WhatsApp.shared")
	if err := os.MkdirAll(filepath.Join(container, "Message", "Media"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(container, "ChatStorage.sqlite")
	if err := os.WriteFile(db, []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := DetectSourceResolver{det: setup.Detector{Home: home}}
	got, err := r.ResolveSource(context.Background(), source.WhatsApp)
	if err != nil {
		t.Fatalf("ResolveSource(whatsapp): %v", err)
	}
	if got.DBPath != db {
		t.Errorf("DBPath = %q, want the detected ChatStorage.sqlite %q", got.DBPath, db)
	}
	wantMedia := filepath.Join(container, "Message", "Media")
	if got.MediaDir != wantMedia {
		t.Errorf("MediaDir = %q, want %q", got.MediaDir, wantMedia)
	}

	// And the assembled argv carries iOS mode against those paths.
	args, err := onboard.ExportArgs(source.WhatsApp, "/data/x.staging", got)
	if err != nil {
		t.Fatalf("ExportArgs: %v", err)
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{"-i", "-d " + db, "-m " + wantMedia, "--no-html"} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q missing %q", joined, want)
		}
	}
}

// TestDetectSourceResolverZeroForNonWhatsApp confirms Signal/iMessage resolve to
// the zero ExportSource (their exporters read their own well-known directories).
func TestDetectSourceResolverZeroForNonWhatsApp(t *testing.T) {
	r := DetectSourceResolver{det: setup.Detector{Home: t.TempDir()}}
	for _, src := range []string{source.Signal, source.IMessage} {
		got, err := r.ResolveSource(context.Background(), src)
		if err != nil {
			t.Fatalf("ResolveSource(%s): %v", src, err)
		}
		if got != (onboard.ExportSource{}) {
			t.Errorf("ResolveSource(%s) = %+v, want zero ExportSource", src, got)
		}
	}
}

// TestRingBufferKeepsTail proves the exporter-log ring buffer retains only the
// last cap bytes — the tail where a fatal error prints — so a chatty exporter
// cannot grow the captured log without bound (issue #151).
func TestRingBufferKeepsTail(t *testing.T) {
	rb := newRingBuffer(8)
	// Write more than the cap across multiple writes; only the last 8 bytes survive.
	for _, chunk := range []string{"aaaa", "bbbb", "cccc", "final-err"} {
		if _, err := rb.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	got := rb.String()
	if len(got) > 8 {
		t.Fatalf("ring buffer kept %d bytes, want <= 8", len(got))
	}
	if got != "inal-err" { // last 8 bytes of "…final-err"
		t.Errorf("ring buffer tail = %q, want the last 8 bytes %q", got, "inal-err")
	}
}
