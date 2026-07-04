// Tests for the persisted REST discovery pair (#158): the supervisor records
// its loopback address beside the API key so sibling processes (`msgbrowse
// devices`, `doctor`) can reach the SAME supervised daemon; RESTInfo reads
// them back and reports the no-daemon case as the typed ErrNotRunning.
package syncthing

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRESTInfoRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	homeDir := filepath.Join(dataDir, HomeDirName)
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Nothing recorded yet: the typed not-running sentinel.
	if _, _, err := RESTInfo(dataDir); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("RESTInfo on empty home = %v, want ErrNotRunning", err)
	}

	key, err := loadOrCreateAPIKey(homeDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := persistAPIAddr(homeDir, "127.0.0.1:12345"); err != nil {
		t.Fatal(err)
	}

	addr, gotKey, err := RESTInfo(dataDir)
	if err != nil {
		t.Fatalf("RESTInfo: %v", err)
	}
	if addr != "127.0.0.1:12345" || gotKey != key {
		t.Errorf("RESTInfo = (%q, %q), want (127.0.0.1:12345, the persisted key)", addr, gotKey)
	}

	// The address file is owner-only, like the key it sits beside.
	fi, err := os.Stat(filepath.Join(homeDir, apiAddrFile))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("api-address mode = %v, want 0600", fi.Mode().Perm())
	}

	// An empty address file is not trusted.
	if err := os.WriteFile(filepath.Join(homeDir, apiAddrFile), []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := RESTInfo(dataDir); !errors.Is(err, ErrNotRunning) {
		t.Errorf("RESTInfo with empty address = %v, want ErrNotRunning", err)
	}
}
