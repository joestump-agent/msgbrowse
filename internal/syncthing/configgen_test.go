// Config-generation tests: a golden file locks the generated config.xml
// byte-for-byte, and targeted assertions pin the LAN-only security posture
// (global discovery OFF, relaying OFF, NAT traversal OFF, local discovery ON)
// so no refactor can silently flip an egress default. Folder validation tests
// prove the database can never be configured into a synced folder.
//
// Governing: ADR-0021, SPEC-0014 REQ "msgbrowse-Owned Config Generation",
// SPEC-0014 Security "Relay and Discovery Posture" ("Default posture stays on
// the LAN").
package syncthing

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// goldenSpec is a fixed, fully-specified config for the golden comparison.
func goldenSpec() ConfigSpec {
	return ConfigSpec{
		GUIAddress:    "127.0.0.1:28384",
		APIKey:        "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ListenAddress: "tcp://:8788",
		Folders: []Folder{
			{ID: "msgbrowse-signal", Label: "msgbrowse Signal archive", Path: "/data/archives/signal"},
			{
				ID:        "msgbrowse-imessage",
				Label:     "msgbrowse iMessage archive",
				Path:      "/data/archives/imessage",
				DeviceIDs: []string{"PEER111-AAAAAAA-BBBBBBB-CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG"},
			},
		},
		Devices: []Device{
			{ID: "PEER111-AAAAAAA-BBBBBBB-CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG", Name: "kitchen-server"},
		},
	}
}

// TestGenerateConfigXMLGolden compares the generated XML against the checked-
// in golden file. Regenerate deliberately with UPDATE_GOLDEN=1 when the
// config shape changes on purpose.
func TestGenerateConfigXMLGolden(t *testing.T) {
	got, err := GenerateConfigXML(goldenSpec())
	if err != nil {
		t.Fatalf("GenerateConfigXML: %v", err)
	}
	golden := filepath.Join("testdata", "config.golden.xml")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatalf("update golden: %v", err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with UPDATE_GOLDEN=1 to create): %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("generated config.xml drifted from golden.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestGenerateConfigXMLLANOnlyPosture pins each security-relevant element
// individually, independent of the golden file, so the intent is explicit:
// nothing leaves the LAN by default (SPEC-0014 "Relay and Discovery Posture").
func TestGenerateConfigXMLLANOnlyPosture(t *testing.T) {
	out, err := GenerateConfigXML(goldenSpec())
	if err != nil {
		t.Fatalf("GenerateConfigXML: %v", err)
	}
	xml := string(out)
	for _, want := range []string{
		"<globalAnnounceEnabled>false</globalAnnounceEnabled>", // global discovery OFF
		"<relaysEnabled>false</relaysEnabled>",                 // relaying OFF
		"<natEnabled>false</natEnabled>",                       // NAT traversal OFF
		"<localAnnounceEnabled>true</localAnnounceEnabled>",    // LAN discovery ON
		"<urAccepted>-1</urAccepted>",                          // usage reporting declined
		"<crashReportingEnabled>false</crashReportingEnabled>", // crash reporting OFF
		"<autoUpgradeIntervalH>0</autoUpgradeIntervalH>",       // no self-upgrade
		"<startBrowser>false</startBrowser>",                   // never open Syncthing's GUI
		`type="sendreceive"`,                                   // archive folders are send-receive
		"<address>127.0.0.1:28384</address>",                   // REST/GUI on loopback
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("generated config missing %s", want)
		}
	}
	// The generator must refuse a non-loopback REST bind outright.
	bad := goldenSpec()
	bad.GUIAddress = "0.0.0.0:8384"
	if _, err := GenerateConfigXML(bad); err == nil {
		t.Error("GenerateConfigXML accepted a non-loopback GUI address")
	}
	// And an empty API key (an unauthenticated REST API).
	bad = goldenSpec()
	bad.APIKey = ""
	if _, err := GenerateConfigXML(bad); err == nil {
		t.Error("GenerateConfigXML accepted an empty API key")
	}
}

// TestExistingManagedFolders: only sources whose managed root exists get a
// folder, ids are deterministic, and paths are exactly the managed roots.
func TestExistingManagedFolders(t *testing.T) {
	dataDir := t.TempDir()
	signalRoot := makeManagedRoot(t, dataDir, "signal")
	imsgRoot := makeManagedRoot(t, dataDir, "imessage")
	// whatsapp root deliberately absent.

	folders, err := ExistingManagedFolders(dataDir)
	if err != nil {
		t.Fatalf("ExistingManagedFolders: %v", err)
	}
	got := map[string]string{}
	for _, f := range folders {
		got[f.ID] = f.Path
	}
	want := map[string]string{
		"msgbrowse-signal":   signalRoot,
		"msgbrowse-imessage": imsgRoot,
	}
	if len(got) != len(want) {
		t.Fatalf("folders = %v, want %v", got, want)
	}
	for id, path := range want {
		if got[id] != path {
			t.Errorf("folder %s path = %q, want %q", id, got[id], path)
		}
	}
	// Every generated folder passes the managed-path guard by construction.
	for _, f := range folders {
		if err := ValidateManagedFolderPath(dataDir, f.Path); err != nil {
			t.Errorf("managed folder %s failed its own validation: %v", f.ID, err)
		}
	}
}

// TestValidateManagedFolderPath: the data dir itself, the DB's directory, a
// sibling outside archives/, and archives/ itself are all refused — only a
// strict subdirectory of <data_dir>/archives/ may become a synced folder
// (SPEC-0014 "No database file enters a synced folder").
func TestValidateManagedFolderPath(t *testing.T) {
	dataDir := "/home/u/.config/msgbrowse"
	cases := []struct {
		name string
		path string
		ok   bool
	}{
		{"managed root", filepath.Join(dataDir, "archives", "signal"), true},
		{"nested under a managed root", filepath.Join(dataDir, "archives", "signal", "export"), true},
		{"data dir itself (holds the DB)", dataDir, false},
		{"archives dir itself", filepath.Join(dataDir, "archives"), false},
		{"sibling of archives", filepath.Join(dataDir, "cache"), false},
		{"outside data dir entirely", "/home/u/Documents", false},
		{"traversal escape", filepath.Join(dataDir, "archives", "..", "msgbrowse.db"), false},
		{"empty", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateManagedFolderPath(dataDir, c.path)
			if c.ok && err != nil {
				t.Errorf("ValidateManagedFolderPath(%q) = %v, want nil", c.path, err)
			}
			if !c.ok {
				if !errors.Is(err, ErrUnmanagedFolder) {
					t.Errorf("ValidateManagedFolderPath(%q) = %v, want ErrUnmanagedFolder", c.path, err)
				}
			}
		})
	}
}

// TestPrepareFolderWritesGuards: prepareFolder is idempotent and leaves the
// .stignore DB-exclusion patterns and the .stfolder marker in place.
func TestPrepareFolderWritesGuards(t *testing.T) {
	dataDir := t.TempDir()
	root := makeManagedRoot(t, dataDir, "signal")
	f := Folder{ID: "msgbrowse-signal", Label: "l", Path: root}
	for range 2 { // idempotent
		if err := prepareFolder(f); err != nil {
			t.Fatalf("prepareFolder: %v", err)
		}
	}
	ign, err := os.ReadFile(filepath.Join(root, ".stignore"))
	if err != nil {
		t.Fatalf("read .stignore: %v", err)
	}
	for _, pattern := range []string{"*.db", "*.db-wal", "*.db-shm"} {
		if !strings.Contains(string(ign), pattern) {
			t.Errorf(".stignore missing DB exclusion %q", pattern)
		}
	}
	fi, err := os.Stat(filepath.Join(root, ".stfolder"))
	if err != nil || !fi.IsDir() {
		t.Errorf(".stfolder marker missing or not a dir: %v", err)
	}
}
