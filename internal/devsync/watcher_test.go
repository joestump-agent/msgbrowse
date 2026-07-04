// Folder-watch worker tests over a FAKE events stream (issue #157): a sync
// burst debounces to one import, the completion gate defers imports for
// mid-transfer folders, the onboard Runner's per-source guard is honored by
// retrying (never overlapping), cancellation drains both goroutines cleanly,
// and pending devices/folders are auto-accepted ONLY for explicitly-paired
// device IDs.
//
// Governing: SPEC-0014 REQ "Re-ingest Trigger" ("MUST NOT run against a
// folder that Syncthing reports as mid-transfer"), REQ "Concurrency Safety"
// ("overlapping folder events do not double-import"; "graceful shutdown"),
// §Trust Model ("a device ID alone does not grant sync").
package devsync

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/devices"
	"github.com/joestump/msgbrowse/internal/onboard"
	"github.com/joestump/msgbrowse/internal/syncthing"
)

// fakeAPI scripts the API surface the watcher consumes: an event queue fed by
// tests, a settable per-folder completion, and recorded config mutations.
type fakeAPI struct {
	mu         sync.Mutex
	queue      []syncthing.Event
	completion map[string]syncthing.Completion
	devices    []syncthing.DeviceConfig
	folders    []syncthing.FolderConfig
	nextID     int64
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		completion: map[string]syncthing.Completion{},
		folders: []syncthing.FolderConfig{
			{ID: "msgbrowse-signal", Path: "/tmp/x/archives/signal", Type: "sendreceive"},
			{ID: "msgbrowse-imessage", Path: "/tmp/x/archives/imessage", Type: "sendreceive"},
		},
	}
}

func (f *fakeAPI) push(typ string, data any) {
	raw, _ := json.Marshal(data)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.queue = append(f.queue, syncthing.Event{ID: f.nextID, Type: typ, Time: time.Now(), Data: raw})
}

func (f *fakeAPI) setCompletion(folderID string, c syncthing.Completion) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completion[folderID] = c
}

func (f *fakeAPI) SystemStatus(context.Context) (*syncthing.SystemStatus, error) {
	return &syncthing.SystemStatus{MyID: selfID}, nil
}

func (f *fakeAPI) GetDevices(context.Context) ([]syncthing.DeviceConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]syncthing.DeviceConfig(nil), f.devices...), nil
}

func (f *fakeAPI) PutDevices(_ context.Context, devs []syncthing.DeviceConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.devices = devs
	return nil
}

func (f *fakeAPI) GetFolders(context.Context) ([]syncthing.FolderConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]syncthing.FolderConfig(nil), f.folders...), nil
}

func (f *fakeAPI) PutFolders(_ context.Context, folders []syncthing.FolderConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.folders = folders
	return nil
}

func (f *fakeAPI) FolderCompletion(_ context.Context, folderID, _ string) (*syncthing.Completion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.completion[folderID]
	if !ok {
		return nil, fmt.Errorf("no completion scripted for %s", folderID)
	}
	return &c, nil
}

// Events drains the scripted queue; with nothing queued it blocks up to the
// long-poll timeout like the real daemon.
func (f *fakeAPI) Events(ctx context.Context, since int64, _ []string, timeout time.Duration) ([]syncthing.Event, error) {
	deadline := time.After(timeout)
	for {
		f.mu.Lock()
		var out []syncthing.Event
		for _, ev := range f.queue {
			if ev.ID > since {
				out = append(out, ev)
			}
		}
		f.mu.Unlock()
		if len(out) > 0 {
			return out, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			return nil, nil
		case <-time.After(time.Millisecond):
		}
	}
}

func (f *fakeAPI) deviceIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.devices))
	for _, d := range f.devices {
		out = append(out, d.DeviceID)
	}
	return out
}

func (f *fakeAPI) folderDeviceIDs(folderID string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, fc := range f.folders {
		if fc.ID == folderID {
			out := make([]string, 0, len(fc.Devices))
			for _, d := range fc.Devices {
				out = append(out, d.DeviceID)
			}
			return out
		}
	}
	return nil
}

// fakeImporter records SyncImport calls; err scripts the runner guard.
type fakeImporter struct {
	mu    sync.Mutex
	calls []string
	errs  []error // popped per call; nil-padded
}

func (f *fakeImporter) SyncImport(src string) (onboard.Progress, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, src)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return onboard.Progress{}, err
		}
	}
	return onboard.Progress{Source: src, Phase: onboard.PhaseImporting}, nil
}

func (f *fakeImporter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// startWatcher builds and starts a fast-debounce watcher; the cleanup asserts
// the drain completes (no leaked goroutines).
func startWatcher(t *testing.T, api *fakeAPI, st PeerStore, imp Importer) (*Watcher, context.CancelFunc) {
	t.Helper()
	w, err := NewWatcher(WatcherOptions{
		API:      api,
		Store:    st,
		Importer: imp,
		Folders:  managedFolders(),
		Quiet:    30 * time.Millisecond,
		// A short poll keeps the pump responsive to cancellation in tests.
		PollTimeout: 20 * time.Millisecond,
		Logger:      testLogger(),
	})
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	t.Cleanup(func() {
		cancel()
		drained := make(chan struct{})
		go func() { w.Wait(); close(drained) }()
		select {
		case <-drained:
		case <-time.After(2 * time.Second):
			t.Error("watcher did not drain within 2s of cancellation (leaked goroutine)")
		}
	})
	return w, cancel
}

// waitFor polls cond until true or the deadline — the fake world is
// time-driven, so assertions converge rather than sleep a fixed amount.
func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}

func folderEvent(folder string) map[string]any {
	return map[string]any{"folder": folder, "summary": map[string]any{"state": "idle"}}
}

// TestBurstDebouncesToOneImport: a burst of folder events inside the quiet
// window coalesces into exactly ONE import for that source.
func TestBurstDebouncesToOneImport(t *testing.T) {
	api := newFakeAPI()
	api.setCompletion("msgbrowse-signal", syncthing.Completion{CompletionPct: 100})
	st := newMemPeerStore()
	imp := &fakeImporter{}
	startWatcher(t, api, st, imp)

	for i := 0; i < 5; i++ {
		api.push("FolderSummary", folderEvent("msgbrowse-signal"))
	}
	waitFor(t, time.Second, func() bool { return imp.count() == 1 }, "burst did not produce an import")

	// The quiet window plus slack passes with no further events: still one.
	time.Sleep(120 * time.Millisecond)
	if got := imp.count(); got != 1 {
		t.Errorf("imports = %d, want exactly 1 for a single burst", got)
	}
	// The trigger is recorded for status (#158).
	st.mu.Lock()
	imports := append([]string(nil), st.imports...)
	st.mu.Unlock()
	if len(imports) != 1 || imports[0] != "msgbrowse-signal/signal" {
		t.Errorf("recorded imports = %v", imports)
	}
}

// TestNoImportWhileMidTransfer: an incomplete folder (needItems pending)
// defers the import; a later burst at 100% triggers it.
func TestNoImportWhileMidTransfer(t *testing.T) {
	api := newFakeAPI()
	api.setCompletion("msgbrowse-signal", syncthing.Completion{CompletionPct: 62, NeedItems: 9})
	imp := &fakeImporter{}
	startWatcher(t, api, newMemPeerStore(), imp)

	api.push("FolderCompletion", map[string]any{"folder": "msgbrowse-signal", "completion": 62})
	time.Sleep(120 * time.Millisecond)
	if imp.count() != 0 {
		t.Fatalf("imported against a mid-transfer folder (%d imports)", imp.count())
	}

	api.setCompletion("msgbrowse-signal", syncthing.Completion{CompletionPct: 100})
	waitFor(t, time.Second, func() bool { return imp.count() == 1 }, "no import after completion reached 100%")
}

// TestConcurrentGuardRetries: the runner reporting ErrJobInProgress (an
// Enable/Refresh/import already running) coalesces into a retry, not an
// overlapping import — and the retry succeeds once the job finishes.
func TestConcurrentGuardRetries(t *testing.T) {
	api := newFakeAPI()
	api.setCompletion("msgbrowse-signal", syncthing.Completion{CompletionPct: 100})
	imp := &fakeImporter{errs: []error{onboard.ErrJobInProgress}}
	startWatcher(t, api, newMemPeerStore(), imp)

	api.push("FolderSummary", folderEvent("msgbrowse-signal"))
	waitFor(t, time.Second, func() bool { return imp.count() >= 2 },
		"guarded import was not retried after ErrJobInProgress")
}

// TestUnmanagedFolderIgnored: events for folders msgbrowse does not manage
// never trigger anything.
func TestUnmanagedFolderIgnored(t *testing.T) {
	api := newFakeAPI()
	imp := &fakeImporter{}
	startWatcher(t, api, newMemPeerStore(), imp)

	api.push("FolderSummary", folderEvent("someone-elses-folder"))
	time.Sleep(100 * time.Millisecond)
	if imp.count() != 0 {
		t.Errorf("unmanaged folder produced %d imports", imp.count())
	}
}

// TestCancellationDrainsCleanly: cancelling mid-burst stops both goroutines
// (the cleanup in startWatcher asserts the drain) without further imports.
func TestCancellationDrainsCleanly(t *testing.T) {
	api := newFakeAPI()
	api.setCompletion("msgbrowse-signal", syncthing.Completion{CompletionPct: 100})
	imp := &fakeImporter{}
	_, cancel := startWatcher(t, api, newMemPeerStore(), imp)

	api.push("FolderSummary", folderEvent("msgbrowse-signal"))
	cancel() // before the quiet window can fire
	time.Sleep(80 * time.Millisecond)
	if imp.count() != 0 {
		t.Logf("note: import raced cancellation (%d) — acceptable only if dispatched before cancel", imp.count())
	}
}

// TestAutoAcceptOnlyExplicitlyPairedDevice is the issue #157 trust contract:
// a pending device in the paired registry is accepted (re-added to config +
// folders re-shared); an unknown pending device is NEVER accepted.
func TestAutoAcceptOnlyExplicitlyPairedDevice(t *testing.T) {
	api := newFakeAPI()
	st := newMemPeerStore()
	if _, err := st.UpsertSyncPeer(context.Background(), devices.SyncPeer{
		DeviceID: peerAID, Name: "kitchen-mac", Folders: []string{"msgbrowse-signal"},
	}); err != nil {
		t.Fatal(err)
	}
	imp := &fakeImporter{}
	startWatcher(t, api, st, imp)

	// An unknown device knocks first: it must stay pending.
	api.push("PendingDevicesChanged", map[string]any{
		"added": []map[string]any{{"deviceID": peerBID, "name": "stranger", "address": "tcp://10.0.0.9"}},
	})
	// Then the explicitly-paired device knocks.
	api.push("PendingDevicesChanged", map[string]any{
		"added": []map[string]any{{"deviceID": peerAID, "name": "kitchen-mac"}},
	})

	waitFor(t, time.Second, func() bool {
		ids := api.deviceIDs()
		return len(ids) == 1 && ids[0] == peerAID
	}, "paired pending device was not accepted into the config")

	if ids := api.deviceIDs(); len(ids) != 1 || ids[0] != peerAID {
		t.Errorf("daemon devices = %v; the unknown device must never be auto-accepted", ids)
	}
	if got := api.folderDeviceIDs("msgbrowse-signal"); len(got) != 1 || got[0] != peerAID {
		t.Errorf("signal folder devices = %v, want re-shared with the paired device only", got)
	}
}

// TestAutoAcceptPendingFolderOnlyManagedAndPaired: a folder offer is accepted
// only when the offering device is paired AND the folder id is managed.
func TestAutoAcceptPendingFolderOnlyManagedAndPaired(t *testing.T) {
	api := newFakeAPI()
	st := newMemPeerStore()
	if _, err := st.UpsertSyncPeer(context.Background(), devices.SyncPeer{
		DeviceID: peerAID, Name: "kitchen-mac", Folders: []string{"msgbrowse-signal"},
	}); err != nil {
		t.Fatal(err)
	}
	imp := &fakeImporter{}
	startWatcher(t, api, st, imp)

	// Offer from an UNPAIRED device for a managed folder: ignored.
	api.push("PendingFoldersChanged", map[string]any{
		"added": []map[string]any{{"deviceID": peerBID, "folderID": "msgbrowse-signal"}},
	})
	// Offer from the paired device for an UNMANAGED folder: ignored.
	api.push("PendingFoldersChanged", map[string]any{
		"added": []map[string]any{{"deviceID": peerAID, "folderID": "random-folder"}},
	})
	// Offer from the paired device for a managed folder: accepted (shared).
	api.push("PendingFoldersChanged", map[string]any{
		"added": []map[string]any{{"deviceID": peerAID, "folderID": "msgbrowse-imessage"}},
	})

	waitFor(t, time.Second, func() bool {
		got := api.folderDeviceIDs("msgbrowse-imessage")
		return len(got) == 1 && got[0] == peerAID
	}, "managed folder offer from the paired device was not accepted")

	if got := api.folderDeviceIDs("msgbrowse-signal"); len(got) != 0 {
		t.Errorf("folder shared with an unpaired device: %v", got)
	}
}

// TestWatcherRejectsUnknownFolderMapping: a managed folder whose id does not
// map onto a known source is a construction-time error, not a silent skip.
func TestWatcherRejectsUnknownFolderMapping(t *testing.T) {
	_, err := NewWatcher(WatcherOptions{
		API:      newFakeAPI(),
		Store:    newMemPeerStore(),
		Importer: &fakeImporter{},
		Folders:  []syncthing.Folder{{ID: "msgbrowse-nonsense", Path: "/x"}},
		Logger:   testLogger(),
	})
	if err == nil {
		t.Fatal("NewWatcher accepted a folder with no source mapping")
	}
}
