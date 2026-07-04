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
