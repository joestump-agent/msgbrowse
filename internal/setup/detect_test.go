package setup

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/joestump/msgbrowse/internal/source"
)

// mkfile creates an empty file (and parents), failing the test on error.
func mkfile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// mkdir creates a directory (and parents), failing the test on error.
func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

// fakeHome lays out a temp HOME with the requested well-known stores present.
// It returns the HOME path; absent stores are simply not created, so the
// detector reports NotDetected for them — the non-macOS reality on this Linux
// box, faked deterministically.
func fakeHome(t *testing.T, signal, imessage, whatsapp bool) string {
	t.Helper()
	home := t.TempDir()
	if signal {
		mkdir(t, filepath.Join(home, signalRel))
	}
	if imessage {
		mkfile(t, filepath.Join(home, imessageRel))
	}
	if whatsapp {
		container := filepath.Join(home, whatsappGroupContainersRel, "group.net.whatsapp.WhatsApp.shared")
		mkfile(t, filepath.Join(container, whatsappDBName))
	}
	return home
}

func detectionFor(dets []Detection, src string) Detection {
	for _, d := range dets {
		if d.Source == src {
			return d
		}
	}
	return Detection{}
}

func TestDetectAll(t *testing.T) {
	cases := []struct {
		name                 string
		signal, imsg, wapp   bool
		wantSignal, wantIMsg SourceState
		wantWApp             SourceState
	}{
		{
			name:       "all three present",
			signal:     true,
			imsg:       true,
			wapp:       true,
			wantSignal: Detected,
			wantIMsg:   Detected,
			wantWApp:   Detected,
		},
		{
			// The SPEC-0013 headline scenario: Signal + iMessage present, no
			// WhatsApp → two detected, one not-detected.
			name:       "signal and imessage, no whatsapp",
			signal:     true,
			imsg:       true,
			wapp:       false,
			wantSignal: Detected,
			wantIMsg:   Detected,
			wantWApp:   NotDetected,
		},
		{
			name:       "only whatsapp",
			wapp:       true,
			wantSignal: NotDetected,
			wantIMsg:   NotDetected,
			wantWApp:   Detected,
		},
		{
			// A non-macOS machine (this one): none of the ~/Library stores
			// exist, so everything is NotDetected and nothing errors.
			name:       "none present (non-macOS reality)",
			wantSignal: NotDetected,
			wantIMsg:   NotDetected,
			wantWApp:   NotDetected,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			home := fakeHome(t, c.signal, c.imsg, c.wapp)
			dets := Detector{Home: home}.DetectAll()

			if len(dets) != len(source.All) {
				t.Fatalf("DetectAll returned %d detections, want %d", len(dets), len(source.All))
			}
			// Order must be stable (source.All order) so the UI can render one
			// card per source deterministically.
			for i, want := range []string{source.Signal, source.IMessage, source.WhatsApp} {
				if dets[i].Source != want {
					t.Errorf("dets[%d].Source = %q, want %q", i, dets[i].Source, want)
				}
			}
			if got := detectionFor(dets, source.Signal).State; got != c.wantSignal {
				t.Errorf("signal = %v, want %v", got, c.wantSignal)
			}
			if got := detectionFor(dets, source.IMessage).State; got != c.wantIMsg {
				t.Errorf("imessage = %v, want %v", got, c.wantIMsg)
			}
			if got := detectionFor(dets, source.WhatsApp).State; got != c.wantWApp {
				t.Errorf("whatsapp = %v, want %v", got, c.wantWApp)
			}
		})
	}
}

func TestDetectSignalRequiresDirectory(t *testing.T) {
	// A regular file at the Signal path is not the app-support dir → not detected.
	home := t.TempDir()
	mkfile(t, filepath.Join(home, signalRel))
	if got := (Detector{Home: home}).DetectSignal().State; got != NotDetected {
		t.Errorf("signal (file, not dir) = %v, want NotDetected", got)
	}
}

func TestDetectIMessageRequiresFile(t *testing.T) {
	// A directory at the chat.db path is not the database → not detected.
	home := t.TempDir()
	mkdir(t, filepath.Join(home, imessageRel))
	if got := (Detector{Home: home}).DetectIMessage().State; got != NotDetected {
		t.Errorf("imessage (dir, not file) = %v, want NotDetected", got)
	}
}

func TestDetectWhatsAppContainerVariants(t *testing.T) {
	t.Run("container without database is not detected", func(t *testing.T) {
		home := t.TempDir()
		// The container exists but holds no ChatStorage.sqlite.
		mkdir(t, filepath.Join(home, whatsappGroupContainersRel, "group.net.whatsapp.WhatsApp.shared"))
		det := Detector{Home: home}.DetectWhatsApp()
		if det.State != NotDetected {
			t.Errorf("state = %v, want NotDetected", det.State)
		}
	})

	t.Run("detected path points at the matched database", func(t *testing.T) {
		home := fakeHome(t, false, false, true)
		det := Detector{Home: home}.DetectWhatsApp()
		if det.State != Detected {
			t.Fatalf("state = %v, want Detected", det.State)
		}
		if filepath.Base(det.Path) != whatsappDBName {
			t.Errorf("path = %q, want it to end in %q", det.Path, whatsappDBName)
		}
		// MediaPath is the Message/Media dir beside the database, threaded into
		// wtsexporter's iOS-mode `-m` argument (issue #150).
		wantMedia := filepath.Join(filepath.Dir(det.Path), "Message", "Media")
		if det.MediaPath != wantMedia {
			t.Errorf("media path = %q, want %q (beside the database)", det.MediaPath, wantMedia)
		}
	})

	t.Run("glob error degrades to not-detected", func(t *testing.T) {
		det := Detector{
			Home: t.TempDir(),
			Glob: func(string) ([]string, error) { return nil, os.ErrInvalid },
		}.DetectWhatsApp()
		if det.State != NotDetected {
			t.Errorf("state = %v, want NotDetected on glob error", det.State)
		}
	})
}

func TestDetectEmptyHome(t *testing.T) {
	// No HOME at all: every source is NotDetected with an empty path, never an
	// error (the correct answer when there is no home directory to probe).
	d := Detector{Home: ""}
	for _, det := range d.DetectAll() {
		if det.State != NotDetected {
			t.Errorf("%s with empty home = %v, want NotDetected", det.Source, det.State)
		}
	}
}

func TestNewDetectorUsesHome(t *testing.T) {
	// NewDetector reads the real HOME; on this box the ~/Library stores do not
	// exist, so it must still return without panicking and report NotDetected.
	d := NewDetector()
	for _, det := range d.DetectAll() {
		if det.State != NotDetected {
			t.Errorf("%s on this host = %v, want NotDetected", det.Source, det.State)
		}
	}
}

func TestSourceStateString(t *testing.T) {
	if Detected.String() != "detected" {
		t.Errorf("Detected.String() = %q", Detected.String())
	}
	if NotDetected.String() != "not-detected" {
		t.Errorf("NotDetected.String() = %q", NotDetected.String())
	}
}

// injectedStat exercises the Stat injection path without a real tree: a set of
// paths reported as an existing directory, everything else missing.
func TestDetectorStatInjection(t *testing.T) {
	home := "/faked/home"
	signalPath := filepath.Join(home, signalRel)
	d := Detector{
		Home: home,
		Stat: func(p string) (os.FileInfo, error) {
			if p == signalPath {
				return dirInfo{}, nil
			}
			return nil, os.ErrNotExist
		},
	}
	if got := d.DetectSignal().State; got != Detected {
		t.Errorf("signal via injected stat = %v, want Detected", got)
	}
	if got := d.DetectIMessage().State; got != NotDetected {
		t.Errorf("imessage via injected stat = %v, want NotDetected", got)
	}
}

// dirInfo is a minimal os.FileInfo reporting IsDir()==true for stat injection.
type dirInfo struct{ os.FileInfo }

func (dirInfo) IsDir() bool { return true }

// fileInfo reports IsDir()==false.
type fileInfo struct{ os.FileInfo }

func (fileInfo) IsDir() bool { return false }

// readCloser is a no-op io.ReadCloser for Open injection in probe tests.
type readCloser struct{ io.Reader }

func (readCloser) Close() error { return nil }
