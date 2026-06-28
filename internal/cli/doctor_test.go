package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/source"
)

// statOK reports a non-absolute set of paths as existing; everything else
// "missing". It lets classifyAttachment tests run without touching disk.
func statFn(existing map[string]bool) func(string) (os.FileInfo, error) {
	return func(p string) (os.FileInfo, error) {
		if existing[p] {
			return fakeInfo{}, nil
		}
		return nil, os.ErrNotExist
	}
}

type fakeInfo struct{ os.FileInfo }

func TestClassifyAttachment(t *testing.T) {
	const (
		archiveRoot  = "/archive"
		imessageRoot = "/imsg"
		conv         = "Alice"
	)
	// A copy-mode iMessage rel resolves under imessageRoot.
	imsgAbs := filepath.Join(imessageRoot, "attachments", "AB", "IMG.HEIC")
	// A signal rel resolves under <archiveRoot>/export/<conv>/.
	sigAbs := filepath.Join(archiveRoot, "export", conv, "media", "pic.jpg")

	cases := []struct {
		name     string
		src      string
		rel      string
		existing map[string]bool
		want     attachmentClass
	}{
		{
			name: "imessage absolute library path",
			src:  source.IMessage,
			rel:  "/Users/joe/Library/Messages/Attachments/ab/cd/IMG_0001.HEIC",
			want: attachAbsolute,
		},
		{
			name:     "imessage copy-mode present",
			src:      source.IMessage,
			rel:      "attachments/AB/IMG.HEIC",
			existing: map[string]bool{imsgAbs: true},
			want:     attachPresent,
		},
		{
			name:     "imessage copy-mode missing file",
			src:      source.IMessage,
			rel:      "attachments/AB/IMG.HEIC",
			existing: nil,
			want:     attachMissing,
		},
		{
			name:     "signal present",
			src:      source.Signal,
			rel:      "media/pic.jpg",
			existing: map[string]bool{sigAbs: true},
			want:     attachPresent,
		},
		{
			name:     "signal missing",
			src:      source.Signal,
			rel:      "media/gone.jpg",
			existing: nil,
			want:     attachMissing,
		},
		{
			name: "signal absolute path",
			src:  source.Signal,
			rel:  "/var/data/leaked.jpg",
			want: attachAbsolute,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyAttachment(c.src, archiveRoot, imessageRoot, conv, c.rel, statFn(c.existing))
			if got != c.want {
				t.Errorf("classifyAttachment = %v, want %v", got, c.want)
			}
		})
	}
}

func TestClassifyAttachmentUnsetRootIsMissing(t *testing.T) {
	// Non-absolute rel but the relevant archive root is empty → unresolvable →
	// counted as missing (so it still degrades the health score).
	got := classifyAttachment(source.Signal, "", "", "Alice", "media/pic.jpg", statFn(nil))
	if got != attachMissing {
		t.Errorf("classifyAttachment with unset root = %v, want attachMissing", got)
	}
}

func TestAttachmentVerdict(t *testing.T) {
	cases := []struct {
		name       string
		src        string
		stats      attachmentStats
		wantStatus checkStatus
		wantHint   bool // expect a non-empty hint
	}{
		{
			name:       "all present",
			src:        source.IMessage,
			stats:      attachmentStats{present: 50},
			wantStatus: statusPass,
		},
		{
			name:       "empty",
			src:        source.IMessage,
			stats:      attachmentStats{},
			wantStatus: statusPass,
		},
		{
			name:       "imessage majority absolute -> fail",
			src:        source.IMessage,
			stats:      attachmentStats{present: 2, absolute: 98},
			wantStatus: statusFail,
			wantHint:   true,
		},
		{
			name:       "imessage minority absolute -> warn",
			src:        source.IMessage,
			stats:      attachmentStats{present: 90, absolute: 10},
			wantStatus: statusWarn,
			wantHint:   true,
		},
		{
			name:       "signal majority absolute -> fail",
			src:        source.Signal,
			stats:      attachmentStats{present: 1, absolute: 9},
			wantStatus: statusFail,
			wantHint:   true,
		},
		{
			name:       "mostly missing -> warn",
			src:        source.IMessage,
			stats:      attachmentStats{present: 10, missing: 90},
			wantStatus: statusWarn,
			wantHint:   true,
		},
		{
			name:       "few missing -> warn",
			src:        source.Signal,
			stats:      attachmentStats{present: 95, missing: 5},
			wantStatus: statusWarn,
			wantHint:   true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := c.stats
			status, hint := attachmentVerdict(c.src, &s)
			if status != c.wantStatus {
				t.Errorf("status = %v, want %v", status, c.wantStatus)
			}
			if c.wantHint && hint == "" {
				t.Errorf("expected a hint, got empty")
			}
			if !c.wantHint && hint != "" {
				t.Errorf("expected no hint, got %q", hint)
			}
		})
	}
}

func TestAttachmentVerdictImessageHintMentionsCopyMode(t *testing.T) {
	s := attachmentStats{present: 1, absolute: 99}
	status, hint := attachmentVerdict(source.IMessage, &s)
	if status != statusFail {
		t.Fatalf("status = %v, want statusFail", status)
	}
	for _, want := range []string{"copy-mode", "copy-method", "import --full"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint %q missing %q", hint, want)
		}
	}
}

func TestClassifyArchiveRoot(t *testing.T) {
	mkdirs := func(t *testing.T, base string, rels ...string) {
		t.Helper()
		for _, r := range rels {
			if err := os.MkdirAll(filepath.Join(base, r), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}

	t.Run("correct root with export subdir", func(t *testing.T) {
		root := t.TempDir()
		mkdirs(t, root, "export/Alice")
		if got := classifyArchiveRoot(root); got != archiveRootOK {
			t.Errorf("got %v, want archiveRootOK", got)
		}
	})

	t.Run("pointed at export via nested export/export", func(t *testing.T) {
		root := t.TempDir()
		// root/export/export exists -> user passed .../Archive/export as root.
		mkdirs(t, root, "export/export/Alice")
		if got := classifyArchiveRoot(root); got != archiveRootPointsAtExport {
			t.Errorf("got %v, want archiveRootPointsAtExport", got)
		}
	})

	t.Run("root basename is export and has no export subdir", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "export")
		mkdirs(t, root, "Alice") // root/Alice, but no root/export
		if got := classifyArchiveRoot(root); got != archiveRootPointsAtExport {
			t.Errorf("got %v, want archiveRootPointsAtExport", got)
		}
	})

	t.Run("no export subdir and not named export", func(t *testing.T) {
		root := t.TempDir()
		mkdirs(t, root, "random")
		if got := classifyArchiveRoot(root); got != archiveRootNoExport {
			t.Errorf("got %v, want archiveRootNoExport", got)
		}
	})
}

func TestHostPort(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		isErr bool
	}{
		{"http://127.0.0.1:4000/v1", "127.0.0.1:4000", false},
		{"https://api.example.com/v1", "api.example.com:443", false},
		{"http://localhost/v1", "localhost:80", false},
		{"https://host:8443", "host:8443", false},
		{"://nope", "", true},
		{"http://", "", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := hostPort(c.in)
			if c.isErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %q", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("hostPort(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestPlural(t *testing.T) {
	cases := []struct {
		n    int
		noun string
		want string
	}{
		{0, "warning", "0 warnings"},
		{1, "warning", "1 warning"},
		{2, "problem", "2 problems"},
	}
	for _, c := range cases {
		if got := plural(c.n, c.noun); got != c.want {
			t.Errorf("plural(%d, %q) = %q, want %q", c.n, c.noun, got, c.want)
		}
	}
}
