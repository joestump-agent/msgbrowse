// Status-projection tests (#158; SPEC-0014 REQ "Status and Doctor
// Surfacing"): scripted REST payloads must map TRUTHFULLY onto the fixed
// health enum — healthy, syncing, paused, errored, unknown — and an
// unreachable engine must degrade to Running=false with the registry rows
// intact, never a fake state and never a page-killing error.
package devsync

import (
	"context"
	"testing"

	"github.com/joestump/msgbrowse/internal/devices"
	"github.com/joestump/msgbrowse/internal/syncthing"
)

// statusFixture builds a Manager over the scriptable fakeAPI with one paired
// peer and the standard two managed folders.
func statusFixture(t *testing.T) (*fakeAPI, *memPeerStore, *Manager) {
	t.Helper()
	api := newFakeAPI()
	st := newMemPeerStore()
	if _, err := st.UpsertSyncPeer(context.Background(), devices.SyncPeer{
		DeviceID: peerAID, Name: "kitchen-mac", Folders: []string{"msgbrowse-signal"},
	}); err != nil {
		t.Fatal(err)
	}
	m := NewManager(api, st, "studio-mac", testFolderSet(t), testLogger())
	return api, st, m
}

// TestStatusMapsFolderHealthTruthfully scripts each daemon condition and
// asserts the mapped health token — the doctor/status rows must never report
// a paused or errored folder as healthy.
func TestStatusMapsFolderHealthTruthfully(t *testing.T) {
	cases := []struct {
		name       string
		state      string
		errs       int
		paused     bool
		completion syncthing.Completion
		want       string
		wantPct    float64
	}{
		{"healthy idle at 100%", "idle", 0, false, syncthing.Completion{CompletionPct: 100}, HealthHealthy, 100},
		{"syncing mid-transfer", "syncing", 0, false, syncthing.Completion{CompletionPct: 62, NeedItems: 9}, HealthSyncing, 62},
		{"idle but items still needed", "idle", 0, false, syncthing.Completion{CompletionPct: 91, NeedItems: 2}, HealthSyncing, 91},
		{"paused by config", "idle", 0, true, syncthing.Completion{CompletionPct: 100}, HealthPaused, 100},
		{"error state", "error", 0, false, syncthing.Completion{CompletionPct: 80}, HealthError, 80},
		{"failed items count as error", "idle", 3, false, syncthing.Completion{CompletionPct: 100}, HealthError, 100},
		{"unrecognized daemon state", "quantum-flux", 0, false, syncthing.Completion{CompletionPct: 100}, HealthUnknown, 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api, _, m := statusFixture(t)
			api.mu.Lock()
			api.folderStatus["msgbrowse-signal"] = syncthing.FolderStatus{State: c.state, Errors: c.errs}
			for i := range api.folders {
				if api.folders[i].ID == "msgbrowse-signal" {
					api.folders[i].Paused = c.paused
				}
			}
			api.mu.Unlock()
			api.setCompletion("msgbrowse-signal", c.completion)
			api.setCompletion("msgbrowse-imessage", syncthing.Completion{CompletionPct: 100})

			st, err := m.Status(context.Background())
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if !st.Running {
				t.Fatal("Running = false against a live fake")
			}
			var got *FolderStatus
			for i := range st.Folders {
				if st.Folders[i].ID == "msgbrowse-signal" {
					got = &st.Folders[i]
				}
			}
			if got == nil {
				t.Fatal("signal folder missing from status")
			}
			if got.Health != c.want {
				t.Errorf("health = %q, want %q", got.Health, c.want)
			}
			if got.Completion != c.wantPct {
				t.Errorf("completion = %v, want %v", got.Completion, c.wantPct)
			}
			if got.Source != "signal" || got.Label == "" {
				t.Errorf("folder identity = %+v", got)
			}
		})
	}
}

// TestStatusJoinsPeerConnections: connected/paused/disconnected peers come
// out exactly as the daemon reports, joined onto the registry rows.
func TestStatusJoinsPeerConnections(t *testing.T) {
	api, st, m := statusFixture(t)
	if _, err := st.UpsertSyncPeer(context.Background(), devices.SyncPeer{
		DeviceID: peerBID, Name: "attic-mac",
	}); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.connections[peerAID] = syncthing.ConnectionInfo{Connected: true, Address: "192.168.1.20:22000"}
	// peerB has no connection entry: disconnected.
	api.mu.Unlock()
	api.setCompletion("msgbrowse-signal", syncthing.Completion{CompletionPct: 100})
	api.setCompletion("msgbrowse-imessage", syncthing.Completion{CompletionPct: 100})

	status, err := m.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]PeerStatus{}
	for _, p := range status.Peers {
		byID[p.DeviceID] = p
	}
	a := byID[peerAID]
	if !a.StateKnown || !a.Connected || a.Address != "192.168.1.20:22000" {
		t.Errorf("connected peer = %+v", a)
	}
	b := byID[peerBID]
	if !b.StateKnown || b.Connected {
		t.Errorf("disconnected peer = %+v", b)
	}
}

// TestStatusEngineDownDegradesTruthfully: with the engine unreachable the
// snapshot reports Running=false, peers keep StateKnown=false (unknown, not
// "disconnected"), folders read HealthUnknown — and no error kills the page.
func TestStatusEngineDownDegradesTruthfully(t *testing.T) {
	daemon := newStubDaemon(t, nil)
	st := newMemPeerStore()
	if _, err := st.UpsertSyncPeer(context.Background(), devices.SyncPeer{
		DeviceID: peerAID, Name: "kitchen-mac",
	}); err != nil {
		t.Fatal(err)
	}
	m := NewManager(daemon.client(), st, "studio-mac", testFolderSet(t), testLogger())
	daemon.srv.Close() // engine down

	status, err := m.Status(context.Background())
	if err != nil {
		t.Fatalf("Status with engine down must degrade, not error: %v", err)
	}
	if status.Running {
		t.Error("Running = true against a dead engine")
	}
	if len(status.Peers) != 1 || status.Peers[0].StateKnown {
		t.Errorf("peers = %+v, want the registry row with StateKnown=false", status.Peers)
	}
	for _, f := range status.Folders {
		if f.Health != HealthUnknown {
			t.Errorf("folder %s health = %q with engine down, want %q", f.ID, f.Health, HealthUnknown)
		}
	}
}

// TestStatusCarriesLastImport: the sync_state bookkeeping surfaces as each
// folder's LastImportAt (the staleness signal doctor and /status show).
func TestStatusCarriesLastImport(t *testing.T) {
	api, st, m := statusFixture(t)
	api.setCompletion("msgbrowse-signal", syncthing.Completion{CompletionPct: 100})
	api.setCompletion("msgbrowse-imessage", syncthing.Completion{CompletionPct: 100})
	if err := st.RecordSyncImport(context.Background(), "msgbrowse-signal", "signal"); err != nil {
		t.Fatal(err)
	}

	status, err := m.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range status.Folders {
		switch f.ID {
		case "msgbrowse-signal":
			if f.LastImportAt.IsZero() {
				t.Error("signal folder missing its last-import time")
			}
		case "msgbrowse-imessage":
			if !f.LastImportAt.IsZero() {
				t.Error("imessage folder has a last-import time it never earned")
			}
		}
	}
}
