// Package setup is the shared, UI-free foundation for the desktop guided-setup
// surface (SPEC-0013) and the CLI `doctor`: source detection at well-known
// local locations, OS-permission probe primitives, the app-owned managed
// archive layout, and the pure archive/attachment-health decision logic that
// used to live inside internal/cli/doctor.go.
//
// Everything here is a pure library — no cobra, no HTTP, no Wails. Filesystem
// and OS access is injected (a base directory or HOME, a stat function) so the
// well-known macOS paths can be faked and table-tested on Linux, where those
// paths do not exist. The desktop UI (#134) and the CLI `doctor` both call this
// package rather than duplicating detection.
//
// Governing: ADR-0020 (self-contained desktop onboarding — source detection,
// detect-and-guide OS consent, app-owned managed archive roots), SPEC-0013 REQ
// "Source detection", REQ "Permission detection and guidance", REQ "App-owned,
// hidden data and archive roots".
package setup

import (
	"io"
	"os"
	"path/filepath"

	"github.com/joestump/msgbrowse/internal/source"
)

// SourceState is the detection state of one supported source. It mirrors the
// four card states SPEC-0013 REQ "Source detection" names, but this package
// only ever reports the two that pure filesystem detection can decide —
// Detected (the source's well-known store is present) and NotDetected (it is
// absent, including every non-macOS OS where these paths never exist). The
// Needs-permission and Enabled states are decided by the permission probes
// (see probe.go) and the store respectively, layered on top by the UI; source
// detection alone never fails or errors on a missing path.
type SourceState int

const (
	// NotDetected: the source's well-known local store is absent. This is the
	// expected state for every source on a non-macOS machine (the ~/Library
	// paths do not exist there) — it is reported, never an error.
	NotDetected SourceState = iota
	// Detected: the source's well-known local store exists at its probed path.
	Detected
)

// String renders a stable, lowercase token for logs and tests.
func (s SourceState) String() string {
	switch s {
	case Detected:
		return "detected"
	default:
		return "not-detected"
	}
}

// Detection is one source's detection result: which source, what state, and the
// concrete path that was probed (so the UI and doctor can show WHERE it looked).
type Detection struct {
	// Source is the fixed source identifier (source.Signal, source.IMessage,
	// source.WhatsApp).
	Source string
	// State is Detected or NotDetected.
	State SourceState
	// Path is the well-known location probed. For WhatsApp it is the matched
	// ChatStorage.sqlite when Detected, or the glob's parent hint when not.
	Path string
	// MediaPath is the source's live media directory, populated only for WhatsApp
	// when Detected: the app's Message/Media folder beside ChatStorage.sqlite in
	// the group container. The desktop Enable flow threads it into wtsexporter's
	// iOS-mode `-m <media dir>` argument (issue #150). Empty for every other
	// source and for a NotDetected WhatsApp.
	MediaPath string
}

// well-known source store locations, expressed relative to HOME. These are the
// macOS paths SPEC-0013 REQ "Source detection" pins; on non-macOS they simply
// do not exist, so detection reports NotDetected without erroring.
const (
	// signalRel is Signal Desktop's application-support directory (its presence
	// means Signal Desktop is installed for this user).
	signalRel = "Library/Application Support/Signal"
	// imessageRel is the Messages chat database.
	imessageRel = "Library/Messages/chat.db"
	// whatsappGroupContainersRel is the parent of the per-app group containers;
	// the WhatsApp app's container matches whatsappContainerGlob under it and
	// holds ChatStorage.sqlite.
	whatsappGroupContainersRel = "Library/Group Containers"
	// whatsappContainerGlob matches the WhatsApp group-container directory name
	// (e.g. "group.net.whatsapp.WhatsApp.shared"). Kept broad ("*WhatsApp*") to
	// track container-name changes across WhatsApp releases.
	whatsappContainerGlob = "*WhatsApp*"
	// whatsappDBName is the WhatsApp app's SQLite store inside its container.
	whatsappDBName = "ChatStorage.sqlite"
	// whatsappMediaSubdir is the media directory beside ChatStorage.sqlite in the
	// group container. wtsexporter's iOS mode reads copied media from here via
	// `-m <media dir>` (issue #150); it is joined onto the matched container.
	whatsappMediaSubdir = "Message/Media"
)

// Detector probes the well-known local locations for each source. Home is the
// base directory the ~/Library paths are resolved against (a real HOME in
// production, a faked temp tree in tests); Stat is injected so probing can be
// faked without touching disk (nil defaults to os.Stat). Glob is injected the
// same way for the WhatsApp container match (nil defaults to filepath.Glob).
type Detector struct {
	// Home is the base directory the relative well-known paths resolve against.
	// Empty Home means "no HOME" → every source NotDetected (never an error).
	Home string
	// Stat probes file existence; nil uses os.Stat.
	Stat func(string) (os.FileInfo, error)
	// Glob resolves the WhatsApp container glob; nil uses filepath.Glob.
	Glob func(string) ([]string, error)
	// Open opens a file for reading, used by the permission probes (probe.go)
	// to distinguish "present but inaccessible" (Needed) from "accessible"
	// (OK); nil uses os.Open. Injected in tests to fake a permission error.
	Open func(string) (io.ReadCloser, error)
	// KeychainAccessible reports whether the macOS Keychain will release a
	// Signal encryptedKey. The real check is macOS-only; nil defaults to
	// "cannot verify" so a sealed key reads as Needs-permission off macOS. The
	// desktop layer (#134) injects the genuine macOS check.
	KeychainAccessible func(encryptedKey string) bool
}

func (d Detector) stat() func(string) (os.FileInfo, error) {
	if d.Stat != nil {
		return d.Stat
	}
	return os.Stat
}

func (d Detector) glob() func(string) ([]string, error) {
	if d.Glob != nil {
		return d.Glob
	}
	return filepath.Glob
}

// NewDetector builds a Detector rooted at the current user's HOME using the real
// filesystem. An empty HOME env yields a Detector that reports every source
// NotDetected (the correct answer when there is no home directory to probe).
func NewDetector() Detector {
	home, _ := os.UserHomeDir()
	return Detector{Home: home}
}

// DetectAll returns one Detection per supported source, in source.All order, so
// callers get a stable, complete list (the SPEC-0013 "one card per source"
// contract). A source whose well-known store is absent — always the case off
// macOS — is reported NotDetected, never an error.
func (d Detector) DetectAll() []Detection {
	return []Detection{
		d.DetectSignal(),
		d.DetectIMessage(),
		d.DetectWhatsApp(),
	}
}

// DetectSignal reports whether Signal Desktop's application-support directory
// exists under HOME.
func (d Detector) DetectSignal() Detection {
	path := d.path(signalRel)
	return Detection{Source: source.Signal, State: d.dirState(path), Path: path}
}

// DetectIMessage reports whether the Messages chat.db exists under HOME. Note
// this is only presence detection; whether the process can actually READ
// chat.db is a separate Full Disk Access concern (see ProbeFullDiskAccess).
func (d Detector) DetectIMessage() Detection {
	path := d.path(imessageRel)
	return Detection{Source: source.IMessage, State: d.fileState(path), Path: path}
}

// DetectWhatsApp reports whether the WhatsApp app's group container holds a
// ChatStorage.sqlite. The container name varies across releases, so the parent
// Group Containers directory is globbed for *WhatsApp* and each match checked
// for the database. The probed Path is the matched database when Detected, or
// the group-containers directory hint when not.
func (d Detector) DetectWhatsApp() Detection {
	if d.Home == "" {
		return Detection{Source: source.WhatsApp, State: NotDetected, Path: ""}
	}
	groupContainers := d.path(whatsappGroupContainersRel)
	det := Detection{Source: source.WhatsApp, State: NotDetected, Path: groupContainers}
	matches, err := d.glob()(filepath.Join(groupContainers, whatsappContainerGlob))
	if err != nil {
		return det
	}
	for _, container := range matches {
		db := filepath.Join(container, whatsappDBName)
		if d.fileState(db) == Detected {
			det.State = Detected
			det.Path = db
			// The live media directory sits beside the database in the same
			// container. The desktop Enable flow hands it to wtsexporter's iOS-mode
			// `-m` argument (issue #150). It is reported unconditionally when the DB
			// is present; the export only relies on it for WhatsApp.
			det.MediaPath = filepath.Join(container, whatsappMediaSubdir)
			return det
		}
	}
	return det
}

// path joins a HOME-relative well-known path onto Home. An empty Home yields an
// empty result so the state helpers report NotDetected.
func (d Detector) path(rel string) string {
	if d.Home == "" {
		return ""
	}
	return filepath.Join(d.Home, rel)
}

// dirState reports Detected when path is an existing directory, else
// NotDetected. A stat error of any kind (absent, permission) is NotDetected —
// detection never errors.
func (d Detector) dirState(path string) SourceState {
	if path == "" {
		return NotDetected
	}
	info, err := d.stat()(path)
	if err != nil || !info.IsDir() {
		return NotDetected
	}
	return Detected
}

// fileState reports Detected when path is an existing non-directory file, else
// NotDetected.
func (d Detector) fileState(path string) SourceState {
	if path == "" {
		return NotDetected
	}
	info, err := d.stat()(path)
	if err != nil || info.IsDir() {
		return NotDetected
	}
	return Detected
}
