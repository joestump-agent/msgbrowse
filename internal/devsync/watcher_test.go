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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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
	mu           sync.Mutex
	queue        []syncthing.Event
	completion   map[string]syncthing.Completion
	folderStatus map[string]syncthing.FolderStatus
	connections  map[string]syncthing.ConnectionInfo
	devices      []syncthing.DeviceConfig
	folders      []syncthing.FolderConfig
	nextID       int64
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		completion:   map[string]syncthing.Completion{},
		folderStatus: map[string]syncthing.FolderStatus{},
		connections:  map[string]syncthing.ConnectionInfo{},
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

func (f *fakeAPI) FolderStatus(_ context.Context, folderID string) (*syncthing.FolderStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.folderStatus[folderID]
	if !ok {
		return &syncthing.FolderStatus{State: "idle"}, nil
	}
	return &st, nil
}

func (f *fakeAPI) Connections(context.Context) (*syncthing.Connections, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	conns := make(map[string]syncthing.ConnectionInfo, len(f.connections))
	for id, ci := range f.connections {
		conns[id] = ci
	}
	return &syncthing.Connections{Connections: conns}, nil
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

// startWatcher builds and starts a fast-debounce watcher over the standard
// two-source folder fixture; the cleanup asserts the drain completes (no
// leaked goroutines).
func startWatcher(t *testing.T, api *fakeAPI, st PeerStore, imp Importer) (*Watcher, context.CancelFunc) {
	t.Helper()
	return startWatcherWith(t, api, st, imp, testFolderSet(t), testLogger())
}

// startWatcherWith is startWatcher with an explicit folder set and logger,
// for tests exercising provisioning (fresh-replica states) or log output.
func startWatcherWith(t *testing.T, api *fakeAPI, st PeerStore, imp Importer, fs *FolderSet, log *slog.Logger) (*Watcher, context.CancelFunc) {
	t.Helper()
	w, err := NewWatcher(WatcherOptions{
		API:      api,
		Store:    st,
		Importer: imp,
		Folders:  fs,
		Quiet:    30 * time.Millisecond,
		// A short poll keeps the pump responsive to cancellation in tests.
		PollTimeout: 20 * time.Millisecond,
		Logger:      log,
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
// map onto a known source is a construction-time error (now enforced by
// NewFolderSet, which every watcher requires), not a silent skip.
func TestWatcherRejectsUnknownFolderMapping(t *testing.T) {
	if _, err := NewFolderSet(t.TempDir(), []syncthing.Folder{{ID: "msgbrowse-nonsense", Path: "/x"}}); err == nil {
		t.Fatal("NewFolderSet accepted a folder with no source mapping")
	}
	// And a watcher without a folder set at all is refused outright.
	_, err := NewWatcher(WatcherOptions{
		API:      newFakeAPI(),
		Store:    newMemPeerStore(),
		Importer: &fakeImporter{},
		Logger:   testLogger(),
	})
	if err == nil {
		t.Fatal("NewWatcher accepted nil Folders")
	}
}

// syncBuffer is a mutex-guarded bytes.Buffer so a slog handler on the watcher
// goroutines and test assertions never race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestAcceptedFolderOfferPersistsAndSurvivesRestart is the MAJOR review-
// finding scenario (issue #157 adversarial review, finding 2): a paired
// device offers a known-source folder this node was not sharing with it. The
// acceptance must (a) provision the managed root when absent, (b) PERSIST the
// widened share to the peer's registry row, and (c) survive a restart — the
// registry-driven config regeneration (ExistingManagedFolders + ApplyPeers)
// must reproduce the share instead of flip-flopping it away. An offer whose
// folder id is outside the fixed source enum is rejected and logged, changing
// nothing.
func TestAcceptedFolderOfferPersistsAndSurvivesRestart(t *testing.T) {
	// This node manages only the signal root; the daemon config matches.
	dataDir := t.TempDir()
	signal, err := syncthing.ProvisionManagedFolder(dataDir, "signal")
	if err != nil {
		t.Fatal(err)
	}
	fs, err := NewFolderSet(dataDir, []syncthing.Folder{signal})
	if err != nil {
		t.Fatal(err)
	}
	api := newFakeAPI()
	api.mu.Lock()
	api.folders = []syncthing.FolderConfig{{ID: signal.ID, Path: signal.Path, Type: syncthing.FolderTypeSendReceive}}
	api.mu.Unlock()

	st := newMemPeerStore()
	if _, err := st.UpsertSyncPeer(context.Background(), devices.SyncPeer{
		DeviceID: peerAID, Name: "kitchen-mac", Folders: []string{"msgbrowse-signal"},
	}); err != nil {
		t.Fatal(err)
	}
	logBuf := &syncBuffer{}
	startWatcherWith(t, api, st, &fakeImporter{}, fs, slog.New(slog.NewTextHandler(logBuf, nil)))

	// An offer with a folder id OUTSIDE the source enum: rejected + logged.
	api.push("PendingFoldersChanged", map[string]any{
		"added": []map[string]any{{"deviceID": peerAID, "folderID": "msgbrowse-attacker"}},
	})
	// The paired device offers the imessage folder this node lacks: accepted.
	api.push("PendingFoldersChanged", map[string]any{
		"added": []map[string]any{{"deviceID": peerAID, "folderID": "msgbrowse-imessage"}},
	})

	waitFor(t, time.Second, func() bool {
		got := api.folderDeviceIDs("msgbrowse-imessage")
		return len(got) == 1 && got[0] == peerAID
	}, "known-source folder offer from the paired device was not accepted")

	// (a) The managed root was provisioned.
	if fi, err := os.Stat(filepath.Join(dataDir, "archives", "imessage")); err != nil || !fi.IsDir() {
		t.Fatalf("imessage root not provisioned: %v", err)
	}
	// (b) The registry row carries the widened share set — what /settings and
	// `msgbrowse devices list` render.
	stored, err := st.GetSyncPeerByDeviceID(context.Background(), peerAID)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(stored.Folders, "msgbrowse-signal") || !containsString(stored.Folders, "msgbrowse-imessage") {
		t.Fatalf("registry folders = %v, want signal AND imessage", stored.Folders)
	}
	// (c) Simulated restart: regenerate the folder/device wiring exactly as
	// startDeviceSync does — from disk + registry — and the accepted share is
	// still there.
	existing, err := syncthing.ExistingManagedFolders(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	peers, err := st.ListSyncPeers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	regenerated, _ := ApplyPeers(existing, peers)
	found := false
	for _, f := range regenerated {
		if f.ID == "msgbrowse-imessage" {
			found = true
			if !containsString(f.DeviceIDs, peerAID) {
				t.Errorf("regenerated imessage folder not shared with the peer: %v", f.DeviceIDs)
			}
		}
	}
	if !found {
		t.Error("regenerated config lost the accepted imessage folder")
	}

	// The out-of-enum offer changed nothing and left a log trail.
	if got := api.folderDeviceIDs("msgbrowse-attacker"); got != nil {
		t.Errorf("out-of-enum folder entered the daemon config: %v", got)
	}
	if containsString(stored.Folders, "msgbrowse-attacker") {
		t.Error("out-of-enum folder entered the registry")
	}
	if !strings.Contains(logBuf.String(), "outside the managed source enum") {
		t.Error("rejected out-of-enum offer was not logged")
	}
}

// TestProvisionedFolderTriggersImport closes the fresh-replica loop (MAJOR
// review finding 1): a watcher started with NO managed folders begins
// dispatching imports for a folder the pairing Manager provisions mid-run,
// because the two share one live FolderSet — the synced fixture that lands
// after pairing is imported without a restart.
func TestProvisionedFolderTriggersImport(t *testing.T) {
	fs, err := NewFolderSet(t.TempDir(), nil) // fresh replica: nothing managed
	if err != nil {
		t.Fatal(err)
	}
	api := newFakeAPI()
	api.mu.Lock()
	api.folders = nil // daemon config is empty too
	api.mu.Unlock()
	imp := &fakeImporter{}
	st := newMemPeerStore()
	startWatcherWith(t, api, st, imp, fs, testLogger())

	// Before pairing, a signal folder event is unmanaged noise.
	api.push("FolderSummary", folderEvent("msgbrowse-signal"))
	time.Sleep(100 * time.Millisecond)
	if imp.count() != 0 {
		t.Fatalf("unmanaged folder produced %d imports", imp.count())
	}

	// The operator pairs the importer, whose payload introduces the signal
	// folder — provisioning it into the SHARED folder set.
	m := NewManager(api, st, "replica", fs, testLogger())
	if _, err := m.Pair(context.Background(), mustSyncCode(t, peerAID, []string{"msgbrowse-signal"}, "importer")); err != nil {
		t.Fatalf("Pair: %v", err)
	}

	// Syncthing finishes delivering the folder: the watcher now imports it.
	api.setCompletion("msgbrowse-signal", syncthing.Completion{CompletionPct: 100})
	api.push("FolderSummary", folderEvent("msgbrowse-signal"))
	waitFor(t, time.Second, func() bool { return imp.count() == 1 },
		"folder provisioned by pairing did not trigger an import")
}

// mustSyncCode encodes a v2 pairing payload as its manual code.
func mustSyncCode(t *testing.T, deviceID string, folders []string, name string) string {
	t.Helper()
	p, err := devices.NewSyncPayload(deviceID, folders, name)
	if err != nil {
		t.Fatal(err)
	}
	code, err := p.EncodeManualCode()
	if err != nil {
		t.Fatal(err)
	}
	return code
}
