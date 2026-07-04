// msgbrowse-owned Syncthing config generation. msgbrowse writes config.xml
// before every daemon start, so the daemon's folders are always exactly the
// managed archive roots (<data_dir>/archives/<source>, send-receive), its
// devices are exactly the paired peers, its REST/GUI API is loopback-bound
// with the msgbrowse-generated API key, and its network posture is LAN-only:
// global discovery OFF, relaying OFF, NAT traversal OFF, local (LAN)
// discovery ON. The user never edits this file or opens Syncthing's GUI.
//
// Generating the file (rather than driving /rest/config after first start)
// makes the invariants assertable in golden tests and removes the
// chicken-and-egg of needing a running, API-keyed daemon to configure the
// daemon; the REST client still owns post-start mutation (device naming now,
// pairing in the follow-up stories). Syncthing migrates the written config
// schema version forward in memory as needed; regenerating on each start
// keeps msgbrowse the single owner.
//
// Governing: ADR-0021 ("msgbrowse owns config generation"), SPEC-0014 REQ
// "msgbrowse-Owned Config Generation", SPEC-0014 Security "Relay and
// Discovery Posture" (LAN + local discovery only; global discovery and
// relaying OFF by default).
package syncthing

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/joestump/msgbrowse/internal/setup"
	"github.com/joestump/msgbrowse/internal/source"
)

// Folder is one Syncthing folder msgbrowse manages: a managed archive root
// shared (send-receive) with zero or more paired peer devices. IDs are
// deterministic ("msgbrowse-<source>") so both peers of a pair agree on the
// folder identity without negotiation.
type Folder struct {
	// ID is the Syncthing folder id, stable across peers.
	ID string
	// Label is the human-readable folder label shown in diagnostics.
	Label string
	// Path is the absolute managed archive root. It MUST be inside
	// <data_dir>/archives/ — ValidateManagedFolderPath enforces this.
	Path string
	// DeviceIDs are the paired peer device IDs this folder is shared with.
	// Empty (this foundation story) means the folder syncs with no one yet.
	DeviceIDs []string
}

// Device is one paired peer device (a Syncthing device ID plus its friendly
// name). None exist in this foundation story; the pairing story populates
// them from the repurposed paired_devices table.
type Device struct {
	// ID is the peer's Syncthing device ID (SHA-256 of its TLS certificate).
	ID string
	// Name is the peer's human-readable device name.
	Name string
	// Addresses are the peer's addresses; empty means "dynamic" (local
	// discovery), the LAN-only default.
	Addresses []string
}

// ConfigSpec is everything msgbrowse decides about the daemon's config.xml.
// The LAN-only posture is NOT a knob here — it is pinned in the generated
// XML so no caller can accidentally produce an internet-egressing config
// (an owner-gated relay opt-in is a deliberate future change to this
// generator, per SPEC-0014 "Relay and Discovery Posture").
type ConfigSpec struct {
	// GUIAddress is the loopback host:port the REST/GUI API binds.
	GUIAddress string
	// APIKey is the msgbrowse-generated REST API key.
	APIKey string
	// ListenAddress is the sync (P2P) listen address, e.g. "tcp://:8788".
	ListenAddress string
	// Folders are the managed archive-root folders.
	Folders []Folder
	// Devices are the paired peers.
	Devices []Device
}

// configVersion is the config schema version written to config.xml. Syncthing
// migrates older schema versions forward automatically and never refuses
// them, so this pins a well-known baseline rather than tracking the daemon's
// latest.
const configVersion = 37

// folderType is the Syncthing folder type for every managed archive root:
// send-receive, so an importer sends and a replica receives through the same
// config shape (roles are enforced by msgbrowse, not by folder type —
// SPEC-0014 REQ "Importer and Replica Roles" lands with the pairing story).
const folderType = "sendreceive"

// XML shapes matching Syncthing's config.xml schema (the subset msgbrowse
// generates; the daemon fills defaults for everything omitted).
type xmlConfiguration struct {
	XMLName xml.Name    `xml:"configuration"`
	Version int         `xml:"version,attr"`
	Folders []xmlFolder `xml:"folder"`
	Devices []xmlDevice `xml:"device"`
	GUI     xmlGUI      `xml:"gui"`
	Options xmlOptions  `xml:"options"`
}

type xmlFolder struct {
	ID               string            `xml:"id,attr"`
	Label            string            `xml:"label,attr"`
	Path             string            `xml:"path,attr"`
	Type             string            `xml:"type,attr"`
	RescanIntervalS  int               `xml:"rescanIntervalS,attr"`
	FSWatcherEnabled bool              `xml:"fsWatcherEnabled,attr"`
	FSWatcherDelayS  int               `xml:"fsWatcherDelayS,attr"`
	Devices          []xmlFolderDevice `xml:"device"`
}

type xmlFolderDevice struct {
	ID string `xml:"id,attr"`
}

type xmlDevice struct {
	ID        string   `xml:"id,attr"`
	Name      string   `xml:"name,attr"`
	Addresses []string `xml:"address"`
}

type xmlGUI struct {
	Enabled bool   `xml:"enabled,attr"`
	TLS     bool   `xml:"tls,attr"`
	Address string `xml:"address"`
	APIKey  string `xml:"apikey"`
}

// xmlOptions pins the LAN-only, no-egress, no-self-mutation posture:
//
//   - globalAnnounceEnabled=false / relaysEnabled=false / natEnabled=false —
//     no connection to any global discovery server, relay, or NAT traversal
//     service; nothing leaves the LAN (SPEC-0014 "Default posture stays on
//     the LAN").
//   - localAnnounceEnabled=true — LAN-local discovery is how paired peers
//     find each other with zero configuration.
//   - urAccepted=-1 / crashReportingEnabled=false — usage reporting and
//     crash reporting are egress; both declined permanently.
//   - autoUpgradeIntervalH=0 — the daemon never self-upgrades; the bundled
//     binary is version-pinned and only a msgbrowse release changes it
//     (ADR-0021 "version-pinning + security-update cadence").
//   - startBrowser=false — the user never sees Syncthing's GUI.
type xmlOptions struct {
	ListenAddresses       []string `xml:"listenAddress"`
	GlobalAnnounceEnabled bool     `xml:"globalAnnounceEnabled"`
	LocalAnnounceEnabled  bool     `xml:"localAnnounceEnabled"`
	RelaysEnabled         bool     `xml:"relaysEnabled"`
	NATEnabled            bool     `xml:"natEnabled"`
	URAccepted            int      `xml:"urAccepted"`
	URSeen                int      `xml:"urSeen"`
	CrashReportingEnabled bool     `xml:"crashReportingEnabled"`
	AutoUpgradeIntervalH  int      `xml:"autoUpgradeIntervalH"`
	StartBrowser          bool     `xml:"startBrowser"`
}

// GenerateConfigXML renders the daemon's full config.xml from spec. It is
// deterministic (golden-testable) and hardcodes the LAN-only posture. It
// validates the loopback GUI bind and the presence of an API key — a config
// that would expose the REST API beyond loopback or without auth is a
// programming error refused here, never written to disk (SPEC-0014
// Authentication: the REST/GUI API MUST bind loopback and MUST require a
// generated API key).
func GenerateConfigXML(spec ConfigSpec) ([]byte, error) {
	if spec.APIKey == "" {
		return nil, errors.New("generate syncthing config: empty API key")
	}
	if err := requireLoopback(spec.GUIAddress); err != nil {
		return nil, fmt.Errorf("generate syncthing config: %w", err)
	}
	if spec.ListenAddress == "" {
		return nil, errors.New("generate syncthing config: empty sync listen address")
	}

	c := xmlConfiguration{
		Version: configVersion,
		GUI: xmlGUI{
			Enabled: true,
			TLS:     false, // loopback-only; the API key is the auth, per SPEC-0014
			Address: spec.GUIAddress,
			APIKey:  spec.APIKey,
		},
		Options: xmlOptions{
			ListenAddresses:       []string{spec.ListenAddress},
			GlobalAnnounceEnabled: false,
			LocalAnnounceEnabled:  true,
			RelaysEnabled:         false,
			NATEnabled:            false,
			URAccepted:            -1,
			URSeen:                3,
			CrashReportingEnabled: false,
			AutoUpgradeIntervalH:  0,
			StartBrowser:          false,
		},
	}
	for _, f := range spec.Folders {
		if f.ID == "" || f.Path == "" {
			return nil, fmt.Errorf("generate syncthing config: folder with empty id or path (%+v)", f)
		}
		xf := xmlFolder{
			ID:    f.ID,
			Label: f.Label,
			Path:  f.Path,
			Type:  folderType,
			// The filesystem watcher notices exporter writes promptly; the
			// hourly rescan is the convergence backstop.
			RescanIntervalS:  3600,
			FSWatcherEnabled: true,
			FSWatcherDelayS:  10,
		}
		for _, id := range f.DeviceIDs {
			xf.Devices = append(xf.Devices, xmlFolderDevice{ID: id})
		}
		c.Folders = append(c.Folders, xf)
	}
	for _, d := range spec.Devices {
		if d.ID == "" {
			return nil, errors.New("generate syncthing config: device with empty id")
		}
		xd := xmlDevice{ID: d.ID, Name: d.Name, Addresses: d.Addresses}
		if len(xd.Addresses) == 0 {
			xd.Addresses = []string{"dynamic"}
		}
		c.Devices = append(c.Devices, xd)
	}

	out, err := xml.MarshalIndent(c, "", "    ")
	if err != nil {
		return nil, fmt.Errorf("generate syncthing config: marshal: %w", err)
	}
	return append(out, '\n'), nil
}

// requireLoopback rejects a GUI/REST bind that is not a loopback host. The
// REST API is msgbrowse's control channel and must never be reachable beyond
// the machine (SPEC-0014 Authentication table).
func requireLoopback(addr string) error {
	host, _, err := splitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid GUI address %q: %w", addr, err)
	}
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return nil
	}
	return fmt.Errorf("GUI address %q is not loopback", addr)
}

// splitHostPort is a thin wrapper so configgen has no direct net import churn.
func splitHostPort(addr string) (host, port string, err error) {
	i := strings.LastIndexByte(addr, ':')
	if i < 0 {
		return "", "", fmt.Errorf("missing port in %q", addr)
	}
	host = strings.Trim(addr[:i], "[]")
	port = addr[i+1:]
	if port == "" {
		return "", "", fmt.Errorf("missing port in %q", addr)
	}
	return host, port, nil
}

// FolderIDPrefix prefixes every managed folder id; the full id is
// "msgbrowse-<source>". Deterministic ids let two paired msgbrowse nodes
// agree on folder identity without negotiation.
const FolderIDPrefix = "msgbrowse-"

// ExistingManagedFolders returns a Folder for every known source whose
// managed archive root (<dataDir>/archives/<source>) exists on disk. Sources
// that have never been enabled have no root and get no folder — enabling a
// source is what materializes its root (SPEC-0014 "Enabling a source's sync
// adds exactly that archive folder"; per-source sync toggles arrive with the
// pairing story, so the foundation syncs every managed root that exists).
func ExistingManagedFolders(dataDir string) ([]Folder, error) {
	var out []Folder
	for _, src := range source.All {
		root, err := setup.ManagedRoot(dataDir, src)
		if err != nil {
			return nil, fmt.Errorf("resolve managed root for %s: %w", src, err)
		}
		fi, err := os.Stat(root)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("stat managed root for %s: %w", src, err)
		}
		if !fi.IsDir() {
			continue
		}
		out = append(out, Folder{
			ID:    FolderIDPrefix + src,
			Label: "msgbrowse " + source.Label(src) + " archive",
			Path:  root,
		})
	}
	return out, nil
}

// ValidateManagedFolderPath rejects any folder path that is not strictly
// inside <dataDir>/archives/. This is the config-generation guard that keeps
// the SQLite database, its WAL/SHM files, and all other data_dir state out of
// every synced folder: the archives subtree is the ONLY thing Syncthing may
// ever see (SPEC-0014 "The database is never in a synced folder", "No
// database file enters a synced folder").
func ValidateManagedFolderPath(dataDir, folderPath string) error {
	if dataDir == "" {
		return fmt.Errorf("validate folder path: empty data dir: %w", ErrUnmanagedFolder)
	}
	archives := filepath.Join(dataDir, "archives")
	rel, err := filepath.Rel(archives, folderPath)
	if err != nil {
		return fmt.Errorf("validate folder path %q: %v: %w", folderPath, err, ErrUnmanagedFolder)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("validate folder path %q: not inside %s: %w", folderPath, archives, ErrUnmanagedFolder)
	}
	return nil
}

// stignore is the ignore file msgbrowse writes into every managed folder
// root: defense-in-depth so no database or OS cruft is ever synchronized even
// if a future layout change put such a file inside an archive root (SPEC-0014
// REQ "msgbrowse-Owned Config Generation": ignore patterns keep DB/WAL/SHM
// and non-archive state out of synced folders).
const stignore = `// generated by msgbrowse — do not edit (SPEC-0014)
(?d).DS_Store
*.db
*.db-wal
*.db-shm
*.sqlite
*.sqlite-wal
*.sqlite-shm
`

// prepareFolder makes a managed folder root Syncthing-ready: the root exists,
// carries msgbrowse's .stignore, and has the .stfolder marker Syncthing uses
// to distinguish a healthy root from an unmounted disk. Idempotent; called by
// the Supervisor before every daemon start.
func prepareFolder(f Folder) error {
	if err := os.MkdirAll(f.Path, 0o700); err != nil {
		return fmt.Errorf("prepare folder %s: %w", f.ID, err)
	}
	if err := os.WriteFile(filepath.Join(f.Path, ".stignore"), []byte(stignore), 0o600); err != nil {
		return fmt.Errorf("prepare folder %s: write .stignore: %w", f.ID, err)
	}
	if err := os.MkdirAll(filepath.Join(f.Path, ".stfolder"), 0o700); err != nil {
		return fmt.Errorf("prepare folder %s: create .stfolder marker: %w", f.ID, err)
	}
	return nil
}
