// CLI tests for the rebuilt `msgbrowse devices` namespace (ADR-0021): the
// read-only `devices list` over the paired_devices registry — empty state,
// seeded rows through a REAL store, and the error path. The pairing flow
// itself lives behind /settings (SPEC-0014 REQ "Pairing via Device ID and
// QR") and is covered by internal/devsync and internal/web; what the CLI owns
// is rendering the registry truthfully, including folder shares widened by
// accepted offers (issue #157 review finding 2).
package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/devices"
	"github.com/joestump/msgbrowse/internal/store"
)

// A valid Syncthing-format device ID (Luhn check digits intact) — the store
// canonicalizes device IDs on write, so fixtures must be real ones.
const testSyncDeviceID = "XW4UY46-VHRCAEN-OTRLIUX-BIIMJVP-KPVFKQW-4H5TU2H-MYSYKFX-S53S7AL"

// openTestStore opens a real store in a temp dir; the CLI path under test is
// exactly what `devices list` reads after openStore.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "msgbrowse.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// TestDevicesListEmpty: with no paired peers the command prints the guidance
// line (pairing lives in the web UI) and no table.
func TestDevicesListEmpty(t *testing.T) {
	st := openTestStore(t)
	out := &bytes.Buffer{}
	if err := runDevicesList(context.Background(), st, out); err != nil {
		t.Fatalf("runDevicesList: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "No devices paired") {
		t.Errorf("empty-state output = %q, want the no-devices guidance", got)
	}
	if strings.Contains(out.String(), "DEVICE ID") {
		t.Error("empty state rendered the table header")
	}
}

// TestDevicesListRendersPeers: seeded peers come out as tabwriter rows —
// header plus name, the full device ID (whose prefix is the ShortID shown in
// /settings), and the comma-joined folder share set from the registry.
func TestDevicesListRendersPeers(t *testing.T) {
	st := openTestStore(t)
	if _, err := st.UpsertSyncPeer(context.Background(), devices.SyncPeer{
		DeviceID: testSyncDeviceID,
		Name:     "kitchen-mac",
		Folders:  []string{"msgbrowse-signal", "msgbrowse-imessage"},
		PairedAt: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed peer: %v", err)
	}

	out := &bytes.Buffer{}
	if err := runDevicesList(context.Background(), st, out); err != nil {
		t.Fatalf("runDevicesList: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"NAME", "DEVICE ID", "FOLDERS", "PAIRED", // tabwriter header
		"kitchen-mac",
		testSyncDeviceID,
		devices.ShortDeviceID(testSyncDeviceID), // the ID prefix, part of the full ID
		"msgbrowse-signal,msgbrowse-imessage",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

// errPeerLister scripts the registry read failing (e.g. a corrupt table).
type errPeerLister struct{ err error }

func (e errPeerLister) ListSyncPeers(context.Context) ([]devices.SyncPeer, error) {
	return nil, e.err
}

// TestDevicesListErrorPath: a registry read failure is returned to the caller
// (cobra prints it and exits non-zero), never swallowed into an empty state.
func TestDevicesListErrorPath(t *testing.T) {
	boom := errors.New("paired_devices unreadable")
	out := &bytes.Buffer{}
	err := runDevicesList(context.Background(), errPeerLister{err: boom}, out)
	if !errors.Is(err, boom) {
		t.Fatalf("runDevicesList error = %v, want %v", err, boom)
	}
	if out.Len() != 0 {
		t.Errorf("error path wrote output: %q", out.String())
	}
}
