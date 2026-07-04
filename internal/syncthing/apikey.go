// The msgbrowse-generated REST API key: created once with crypto/rand,
// persisted owner-only under the Syncthing home dir, and injected into the
// generated config.xml on every start. A stable key keeps the REST/events
// clients valid across daemon restarts.
//
// Governing: ADR-0021, SPEC-0014 Authentication ("Syncthing's REST and GUI
// API ... MUST require a msgbrowse-generated API key").
package syncthing

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// apiKeyFile is the file under the Syncthing home dir holding the generated
// REST API key (0600 — it authenticates full control of the daemon).
const apiKeyFile = "apikey"

// apiAddrFile is the file under the Syncthing home dir recording the loopback
// host:port the daemon's REST API bound at the supervisor's last start. The
// port is ephemeral by default, so the supervisor persists it (0600, beside
// the key) to let sibling processes — `msgbrowse devices unpair|status` and
// `msgbrowse doctor` — reach the SAME running daemon instead of guessing.
// Staleness is self-detecting: a reader must Ping before trusting it, and a
// dead address simply fails the ping (SPEC-0014 REQ "Status and Doctor
// Surfacing"; issue #158).
const apiAddrFile = "api-address"

// persistAPIAddr records the daemon's live REST address for sibling-process
// discovery. Owner-only like the key file: the pair leaks nothing the key
// alone would not, but there is no reason to widen it.
func persistAPIAddr(homeDir, addr string) error {
	path := filepath.Join(homeDir, apiAddrFile)
	if err := os.WriteFile(path, []byte(addr+"\n"), 0o600); err != nil {
		return fmt.Errorf("persist syncthing api address %s: %w", path, err)
	}
	return nil
}

// RESTInfo returns the supervised daemon's persisted loopback REST address
// and API key under msgbrowse's dataDir — the CLI-side discovery for
// `msgbrowse devices` and `doctor`. A missing address or key wraps
// ErrNotRunning: no supervisor has started under this data dir (or its state
// was cleaned), so there is no daemon to talk to. Callers MUST still Ping
// before acting: the files persist across daemon stops, so a readable
// address only proves a daemon once ran here.
func RESTInfo(dataDir string) (addr, key string, err error) {
	homeDir := filepath.Join(dataDir, HomeDirName)
	addrBytes, err := os.ReadFile(filepath.Join(homeDir, apiAddrFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", "", fmt.Errorf("%w: no REST address recorded under %s", ErrNotRunning, homeDir)
		}
		return "", "", fmt.Errorf("read syncthing api address: %w", err)
	}
	keyBytes, err := os.ReadFile(filepath.Join(homeDir, apiKeyFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", "", fmt.Errorf("%w: no API key recorded under %s", ErrNotRunning, homeDir)
		}
		return "", "", fmt.Errorf("read syncthing api key: %w", err)
	}
	addr = strings.TrimSpace(string(addrBytes))
	key = strings.TrimSpace(string(keyBytes))
	if addr == "" || key == "" {
		return "", "", fmt.Errorf("%w: empty REST address or API key under %s", ErrNotRunning, homeDir)
	}
	return addr, key, nil
}

// loadOrCreateAPIKey returns the persisted API key under homeDir, generating
// and persisting a fresh 256-bit random key on first use. homeDir must
// already exist.
func loadOrCreateAPIKey(homeDir string) (string, error) {
	path := filepath.Join(homeDir, apiKeyFile)
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if key := strings.TrimSpace(string(b)); key != "" {
			return key, nil
		}
		// An empty key file is corrupt state; regenerate below rather than
		// running an unauthenticated-in-practice daemon.
	case !errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("read syncthing api key %s: %w", path, err)
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate syncthing api key: %w", err)
	}
	key := hex.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("persist syncthing api key %s: %w", path, err)
	}
	return key, nil
}
