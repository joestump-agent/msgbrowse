package onboardsvc

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/joestump/msgbrowse/internal/config"
	"github.com/joestump/msgbrowse/internal/imageconv"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// TestImportRunsTranscode is the issue-#160 acceptance for the onboard import
// path: the Enable/Refresh Importer runs the imageconv transcode step after
// loading the store — the same step `msgbrowse import` runs — so a
// desktop-onboarded HEIC gets its JPEG derivative. A FAKE converter (a `magick`
// shell script that copies bytes) stands in on PATH, so the test runs on any
// CI box, and the transcode summary rides the ImportResult into the job log.
func TestImportRunsTranscode(t *testing.T) {
	// Fake converter: the ONLY thing on PATH, so imageconv.Detect picks it up
	// deterministically (on macOS the real sips would otherwise win). /bin/cp
	// is absolute because the stripped PATH cannot resolve `cp`.
	bin := t.TempDir()
	script := "#!/bin/sh\nexec /bin/cp \"$1\" \"$2\"\n"
	if err := os.WriteFile(filepath.Join(bin, "magick"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	// A managed signal root with one HEIC image attachment.
	dataDir := t.TempDir()
	managed := filepath.Join(dataDir, "archives", "signal")
	mediaDir := filepath.Join(managed, "export", "Trip", "media")
	if err := os.MkdirAll(mediaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	heic := filepath.Join(mediaDir, "IMG_0001.heic")
	if err := os.WriteFile(heic, []byte("heic fixture bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	chat := "[2022-03-01 09:00:00] Harper: pic ![img](media/IMG_0001.heic)\n"
	if err := os.WriteFile(filepath.Join(managed, "export", "Trip", "chat.md"), []byte(chat), 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "t.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	im := &storeImporter{st: st, cfg: &config.Config{DataDir: dataDir}, log: log}
	res, err := im.Import(context.Background(), source.Signal, managed)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.MessagesAdded != 1 || res.ConversationsChanged != 1 {
		t.Fatalf("import result = %+v, want 1 conversation / 1 message", res)
	}

	// The transcode ran: one conversion reported (into the job-log summary)…
	if res.MediaConverted != 1 || res.MediaFailed != 0 {
		t.Errorf("media summary = converted %d / skipped %d / failed %d, want 1/0/0",
			res.MediaConverted, res.MediaSkipped, res.MediaFailed)
	}
	// …and the derivative exists exactly where the web layer will look for it.
	derived := imageconv.DerivedPath(imageconv.DerivedDir(dataDir), heic)
	if _, err := os.Stat(derived); err != nil {
		t.Errorf("derivative %s missing after onboard import: %v", derived, err)
	}

	// A second import is incremental: nothing re-imports, the derivative is
	// cached (skipped), not re-converted.
	res, err = im.Import(context.Background(), source.Signal, managed)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if res.MediaConverted != 0 || res.MediaSkipped != 1 {
		t.Errorf("second-run media summary = converted %d / skipped %d, want 0/1",
			res.MediaConverted, res.MediaSkipped)
	}
}
