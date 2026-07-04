// Importer/replica role tests (#158; SPEC-0014 REQ "Importer and Replica
// Roles"): pairing records WHO the importer is per source from the one
// observable that matters — whether the share's managed root existed here
// before the pair — and ReplicaSources projects that onto the Providers
// cards and the Enable-conflict guard. Unpair releases the claim.
package devsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/devices"
	"github.com/joestump/msgbrowse/internal/syncthing"
)

// TestPairFreshReplicaRecordsPeerAsImporter: a fresh replica (no local roots)
// pairing with an importer whose payload introduces a folder records that
// peer as the source's IMPORTER — this node becomes the replica, and
// ReplicaSources reports it for the Providers card + Enable guard.
func TestPairFreshReplicaRecordsPeerAsImporter(t *testing.T) {
	daemon := newStubDaemon(t, nil)
	st := newMemPeerStore()
	m := NewManager(daemon.client(), st, "replica", emptyFolderSet(t), testLogger())

	if _, err := m.Pair(context.Background(), mustSyncCode(t, peerAID, []string{"msgbrowse-signal"}, "importer-mac")); err != nil {
		t.Fatalf("Pair: %v", err)
	}

	stored, err := st.GetSyncPeerByDeviceID(context.Background(), peerAID)
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.Roles["signal"]; got != devices.RoleImporter {
		t.Errorf("peer role for signal = %q, want %q", got, devices.RoleImporter)
	}
	if !stored.ImporterFor("signal") {
		t.Error("ImporterFor(signal) = false on the recorded importer")
	}

	reps, err := m.ReplicaSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rep, ok := reps["signal"]
	if !ok {
		t.Fatal("ReplicaSources missing signal — the synced-in source is not role-constrained")
	}
	if rep.PeerName != "importer-mac" || rep.PeerShortID != devices.ShortDeviceID(peerAID) {
		t.Errorf("ReplicaSources[signal] = %+v", rep)
	}
}

// TestPairImporterRecordsPeerAsReplica: on the node that already holds the
// managed roots (the importer), pairing records the peer as the REPLICA for
// those sources — and ReplicaSources stays empty, so the importer's own
// Providers cards and Enable are unaffected (the scope's "importer
// unaffected" contract).
func TestPairImporterRecordsPeerAsReplica(t *testing.T) {
	daemon := newStubDaemon(t, stubFolderConfigs())
	st := newMemPeerStore()
	m := NewManager(daemon.client(), st, "importer", testFolderSet(t), testLogger())

	// Introduced folder AND the bare-ID path both mark the peer replica.
	if _, err := m.Pair(context.Background(), mustSyncCode(t, peerAID, []string{"msgbrowse-signal"}, "replica-mac")); err != nil {
		t.Fatalf("Pair: %v", err)
	}
	stored, err := st.GetSyncPeerByDeviceID(context.Background(), peerAID)
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.Roles["signal"]; got != devices.RoleReplica {
		t.Errorf("peer role for signal = %q, want %q", got, devices.RoleReplica)
	}

	if _, err := m.Pair(context.Background(), peerBID); err != nil {
		t.Fatalf("Pair(bare id): %v", err)
	}
	storedB, err := st.GetSyncPeerByDeviceID(context.Background(), peerBID)
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{"signal", "imessage"} {
		if got := storedB.Roles[src]; got != devices.RoleReplica {
			t.Errorf("bare-id peer role for %s = %q, want %q", src, got, devices.RoleReplica)
		}
	}

	reps, err := m.ReplicaSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(reps) != 0 {
		t.Errorf("importer's ReplicaSources = %v, want empty (importer unaffected)", reps)
	}
}

// TestRePairPreservesRecordedRoles: after the replica's root exists (the
// archive synced in), re-pairing the SAME importer must not flip its recorded
// role to replica — the recorded role wins, or a re-pair would silently
// unlock a conflicting local Enable.
func TestRePairPreservesRecordedRoles(t *testing.T) {
	daemon := newStubDaemon(t, nil)
	st := newMemPeerStore()
	fs := emptyFolderSet(t)
	m := NewManager(daemon.client(), st, "replica", fs, testLogger())

	code := mustSyncCode(t, peerAID, []string{"msgbrowse-signal"}, "importer-mac")
	if _, err := m.Pair(context.Background(), code); err != nil {
		t.Fatalf("first Pair: %v", err)
	}
	// Second pair: the root now exists (provisioned by the first), so a naive
	// recompute would say RoleReplica. The recorded role must survive.
	if _, err := m.Pair(context.Background(), code); err != nil {
		t.Fatalf("re-Pair: %v", err)
	}
	stored, err := st.GetSyncPeerByDeviceID(context.Background(), peerAID)
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.Roles["signal"]; got != devices.RoleImporter {
		t.Errorf("role after re-pair = %q, want %q preserved", got, devices.RoleImporter)
	}
}

// TestFolderOfferRecordsImporterRole: the events-side path (a paired peer's
// folder offer provisioning a new root) records the offering peer as that
// source's importer, exactly like the pair-time path.
func TestFolderOfferRecordsImporterRole(t *testing.T) {
	fs := emptyFolderSet(t)
	api := newFakeAPI()
	api.mu.Lock()
	api.folders = nil
	api.mu.Unlock()
	st := newMemPeerStore()
	if _, err := st.UpsertSyncPeer(context.Background(), devices.SyncPeer{
		DeviceID: peerAID, Name: "importer-mac",
	}); err != nil {
		t.Fatal(err)
	}
	startWatcherWith(t, api, st, &fakeImporter{}, fs, testLogger())

	api.push("PendingFoldersChanged", map[string]any{
		"added": []map[string]any{{"deviceID": peerAID, "folderID": "msgbrowse-signal"}},
	})
	waitFor(t, waitLong, func() bool {
		p, err := st.GetSyncPeerByDeviceID(context.Background(), peerAID)
		return err == nil && p.Roles["signal"] == devices.RoleImporter
	}, "accepted folder offer did not record the offering peer as importer")
}

// TestUnpairReleasesRoleClaim: unpairing the importer peer deletes its
// registry row AND its role claims — the source becomes locally Enable-able
// again (the invariant binds "across a paired set"; the peer left the set).
func TestUnpairReleasesRoleClaim(t *testing.T) {
	daemon := newStubDaemon(t, nil)
	st := newMemPeerStore()
	m := NewManager(daemon.client(), st, "replica", emptyFolderSet(t), testLogger())

	if _, err := m.Pair(context.Background(), mustSyncCode(t, peerAID, []string{"msgbrowse-signal"}, "importer-mac")); err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if reps, _ := m.ReplicaSources(context.Background()); len(reps) != 1 {
		t.Fatalf("precondition: ReplicaSources = %v, want the signal claim", reps)
	}

	if _, err := m.Unpair(context.Background(), peerAID); err != nil {
		t.Fatalf("Unpair: %v", err)
	}
	reps, err := m.ReplicaSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(reps) != 0 {
		t.Errorf("ReplicaSources after unpair = %v, want empty (claim released)", reps)
	}
}

// TestUnpairRemovesDeviceAndSharesKeepsArchives is the SPEC-0014 "Unpair
// stops sync to that device immediately" scenario against the stubbed REST
// daemon: the registry row is deleted, the device leaves the daemon config,
// every folder share to it is removed — and the already-synced local archive
// files stay on disk untouched.
func TestUnpairRemovesDeviceAndSharesKeepsArchives(t *testing.T) {
	dataDir := t.TempDir()
	fs, err := NewFolderSet(dataDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	daemon := newStubDaemon(t, nil)
	st := newMemPeerStore()
	m := NewManager(daemon.client(), st, "replica", fs, testLogger())

	if _, err := m.Pair(context.Background(), mustSyncCode(t, peerAID, []string{"msgbrowse-signal"}, "importer-mac")); err != nil {
		t.Fatalf("Pair: %v", err)
	}
	// A synced archive file lands (what Syncthing would deliver).
	archived := writeArchiveFixture(t, dataDir)

	peer, err := m.Unpair(context.Background(), peerAID)
	if err != nil {
		t.Fatalf("Unpair: %v", err)
	}
	if peer.DeviceID != peerAID {
		t.Errorf("unpaired peer = %+v", peer)
	}
	// Registry row gone.
	if _, err := st.GetSyncPeerByDeviceID(context.Background(), peerAID); !errors.Is(err, devices.ErrUnknownSyncPeer) {
		t.Errorf("registry row survived unpair: %v", err)
	}
	// Daemon device gone, folder unshared.
	if ids := daemon.deviceIDs(); len(ids) != 0 {
		t.Errorf("daemon devices after unpair = %v, want none", ids)
	}
	if got := daemon.folderDeviceIDs("msgbrowse-signal"); len(got) != 0 {
		t.Errorf("folder still shared after unpair: %v", got)
	}
	// Local archives stay (SPEC-0014: "already-synced local archives and
	// database remain intact").
	assertFileIntact(t, archived)

	// Unpairing an unknown device is the typed sentinel.
	if _, err := m.Unpair(context.Background(), peerBID); !errors.Is(err, devices.ErrUnknownSyncPeer) {
		t.Errorf("Unpair(unknown) = %v, want ErrUnknownSyncPeer", err)
	}
}

// TestNotesRecordPairUnpairAndImport: the shared event ring captures the
// significant actions for the Logs page feed.
func TestNotesRecordPairUnpairAndImport(t *testing.T) {
	daemon := newStubDaemon(t, stubFolderConfigs())
	st := newMemPeerStore()
	notes := NewNotes(8)
	m := NewManager(daemon.client(), st, "studio-mac", testFolderSet(t), testLogger())
	m.SetNotes(notes)

	if _, err := m.Pair(context.Background(), mustSyncCode(t, peerAID, []string{"msgbrowse-signal"}, "kitchen-mac")); err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if _, err := m.Unpair(context.Background(), peerAID); err != nil {
		t.Fatalf("Unpair: %v", err)
	}
	snap := notes.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("notes = %d entries, want 2 (pair + unpair): %+v", len(snap), snap)
	}
	if !containsSubstring(snap[0].Message, "Paired kitchen-mac") {
		t.Errorf("note[0] = %q", snap[0].Message)
	}
	if !containsSubstring(snap[1].Message, "Unpaired kitchen-mac") || snap[1].IsError() {
		t.Errorf("note[1] = %+v", snap[1])
	}

	// The ring is bounded: overflow evicts the oldest.
	small := NewNotes(2)
	small.Add(NoteInfo, "one")
	small.Add(NoteInfo, "two")
	small.Add(NoteError, "three")
	got := small.Snapshot()
	if len(got) != 2 || got[0].Message != "two" || got[1].Message != "three" {
		t.Errorf("bounded ring = %+v", got)
	}
	// Nil-safety: recording into and snapshotting a nil ring is a no-op.
	var nilNotes *Notes
	nilNotes.Add(NoteInfo, "ignored")
	if nilNotes.Snapshot() != nil {
		t.Error("nil ring snapshot not nil")
	}
}

// TestWatcherRecordsPeerConnection: DeviceConnected for a REGISTRY peer
// touches last_seen_at and lands in the event feed; an unpaired device's
// connection chatter is ignored.
func TestWatcherRecordsPeerConnection(t *testing.T) {
	api := newFakeAPI()
	st := newMemPeerStore()
	if _, err := st.UpsertSyncPeer(context.Background(), devices.SyncPeer{
		DeviceID: peerAID, Name: "kitchen-mac",
	}); err != nil {
		t.Fatal(err)
	}
	notes := NewNotes(8)
	w, err := NewWatcher(WatcherOptions{
		API: api, Store: st, Importer: &fakeImporter{}, Folders: testFolderSet(t),
		Notes: notes, Quiet: 30 * time.Millisecond, PollTimeout: 20 * time.Millisecond,
		Logger: testLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() { cancel(); w.Wait() })

	api.push("DeviceConnected", map[string]any{"id": peerBID, "deviceName": "stranger"})
	api.push("DeviceConnected", map[string]any{"id": peerAID, "deviceName": "kitchen-mac"})
	waitFor(t, waitLong, func() bool {
		p, err := st.GetSyncPeerByDeviceID(context.Background(), peerAID)
		return err == nil && !p.LastSeenAt.IsZero()
	}, "paired peer's connect did not record last_seen")

	found := false
	for _, n := range notes.Snapshot() {
		if containsSubstring(n.Message, "Peer connected: kitchen-mac") {
			found = true
		}
		if containsSubstring(n.Message, "stranger") {
			t.Errorf("unpaired device's connection recorded: %q", n.Message)
		}
	}
	if !found {
		t.Error("peer connect missing from the event feed")
	}
}

// helpers ------------------------------------------------------------------

// waitLong is the convergence deadline for event-driven assertions.
const waitLong = 2 * time.Second

func containsSubstring(s, sub string) bool { return strings.Contains(s, sub) }

// writeArchiveFixture drops one synced-looking file into the provisioned
// signal root and returns its path.
func writeArchiveFixture(t *testing.T, dataDir string) string {
	t.Helper()
	f, err := syncthing.ProvisionManagedFolder(dataDir, "signal")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.Path, "conversation.txt")
	if err := os.WriteFile(path, []byte("[2026-07-04 10:00] alice: hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// assertFileIntact fails unless path still exists with its original content.
func assertFileIntact(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("synced archive file gone after unpair: %v", err)
	}
	if string(b) != "[2026-07-04 10:00] alice: hi" {
		t.Fatalf("synced archive file mutated after unpair: %q", b)
	}
}
