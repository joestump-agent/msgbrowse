// The folder-watch worker: a context-managed pair of goroutines (an events
// long-poll pump and a dispatcher) that turns Syncthing's event stream into
// two msgbrowse actions:
//
//  1. Re-ingest trigger (SPEC-0014 REQ "Re-ingest Trigger"): FolderSummary /
//     FolderCompletion events for a managed folder mark its source dirty; a
//     debounce window coalesces a sync burst into ONE import; when the window
//     closes the watcher confirms via /rest/db/completion that the folder is
//     100% complete — never importing a mid-transfer tree — and dispatches
//     the incremental import through the onboard Runner, whose per-source job
//     guard serializes it against any Enable/Refresh (SPEC-0014 REQ
//     "Concurrency Safety": "overlapping folder events do not double-import").
//
//  2. Scoped auto-accept (issue #157): PendingDevicesChanged /
//     PendingFoldersChanged events are resolved ONLY for device IDs the
//     operator explicitly paired (rows in paired_devices). A pending device
//     in the registry is (re-)added to the daemon config; a pending folder
//     offer from a registry peer is accepted only for that folder id within
//     the locally managed set. Anything else is logged and ignored — never a
//     blanket accept (SPEC-0014 "A device ID alone does not grant sync").
//
// Worker hygiene mirrors internal/onboard's runner: one lifecycle owner, a
// cancellable context propagated everywhere, Start/Wait with a clean drain,
// no leaked goroutines (SPEC-0014 REQ "Concurrency Safety").
//
// Governing: ADR-0021 ("re-ingest trigger" via the events API), SPEC-0014
// REQ "Re-ingest Trigger", REQ "Concurrency Safety", REQ "Error Handling
// Standards"; design.md "Folder-watch trigger: REST events with an fsnotify
// fallback" (events are primary; the hourly folder rescan configured by
// configgen is the convergence backstop).
package devsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/joestump/msgbrowse/internal/onboard"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/syncthing"
)

// Importer dispatches the incremental import for one source. It is the seam
// onto *onboard.Runner.SyncImport: the runner's per-source job registry is
// the concurrency guard, and its structured Progress feeds the Logs surface.
type Importer interface {
	SyncImport(src string) (onboard.Progress, error)
}

// watchEventTypes are the event types the long-poll subscribes to: the two
// folder-completion signals design.md names, plus the pending-introduction
// events the scoped auto-accept resolves.
var watchEventTypes = []string{
	"FolderSummary",
	"FolderCompletion",
	"PendingDevicesChanged",
	"PendingFoldersChanged",
}

// Defaults for the watcher's timing knobs; overridable in WatcherOptions
// (tests use tiny values).
const (
	defaultQuiet       = 3 * time.Second
	defaultPollTimeout = 60 * time.Second
	defaultRetryMin    = time.Second
	defaultRetryMax    = 30 * time.Second
)

// WatcherOptions configures a Watcher. API, Store, Importer, and Folders are
// required.
type WatcherOptions struct {
	// API is the daemon's REST client (events, completion, config).
	API API
	// Store is the paired-peer registry the auto-accept consults and the
	// re-ingest bookkeeping sink.
	Store PeerStore
	// Importer runs the incremental import (the onboard Runner).
	Importer Importer
	// Folders are the locally managed archive folders; only their events are
	// acted on.
	Folders []syncthing.Folder
	// Quiet is the debounce window: a burst of folder events within it
	// coalesces into one import check. 0 means the 3s default.
	Quiet time.Duration
	// PollTimeout is the events long-poll hold time. 0 means 60s.
	PollTimeout time.Duration
	// Logger receives worker logs; nil uses slog.Default().
	Logger *slog.Logger
}

// Watcher is one running folder-watch worker. Construct with NewWatcher,
// start with Start, and drain with Wait after cancelling the Start context.
type Watcher struct {
	api      API
	st       PeerStore
	importer Importer
	quiet    time.Duration
	pollWait time.Duration
	log      *slog.Logger

	// folderSource maps managed folder id → source id, precomputed from
	// Folders so an event for any other folder is ignored outright.
	folderSource map[string]string

	events chan syncthing.Event
	wg     sync.WaitGroup
	done   chan struct{}
}

// NewWatcher validates options and builds a Watcher. It performs no I/O.
func NewWatcher(o WatcherOptions) (*Watcher, error) {
	if o.API == nil {
		return nil, errors.New("devsync watcher: API is required")
	}
	if o.Store == nil {
		return nil, errors.New("devsync watcher: Store is required")
	}
	if o.Importer == nil {
		return nil, errors.New("devsync watcher: Importer is required")
	}
	if o.Quiet <= 0 {
		o.Quiet = defaultQuiet
	}
	if o.PollTimeout <= 0 {
		o.PollTimeout = defaultPollTimeout
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	fs := make(map[string]string, len(o.Folders))
	for _, f := range o.Folders {
		src := strings.TrimPrefix(f.ID, syncthing.FolderIDPrefix)
		if !source.IsKnown(src) {
			return nil, fmt.Errorf("devsync watcher: folder %q does not map to a known source", f.ID)
		}
		fs[f.ID] = src
	}
	return &Watcher{
		api:          o.API,
		st:           o.Store,
		importer:     o.Importer,
		quiet:        o.Quiet,
		pollWait:     o.PollTimeout,
		log:          log.With("component", "devsync"),
		folderSource: fs,
		events:       make(chan syncthing.Event, 16),
		done:         make(chan struct{}),
	}, nil
}

// Start launches the pump and dispatcher goroutines. ctx governs the whole
// worker lifetime: cancel it, then Wait for the drain. Start must be called
// at most once.
func (w *Watcher) Start(ctx context.Context) {
	w.wg.Add(2)
	go func() {
		defer w.wg.Done()
		w.pump(ctx)
	}()
	go func() {
		defer w.wg.Done()
		w.dispatch(ctx)
	}()
	go func() {
		w.wg.Wait()
		close(w.done)
	}()
}

// Wait blocks until both worker goroutines have exited after context
// cancellation. It mirrors the onboard Runner's shutdown contract: cancel,
// then Wait, and nothing is leaked.
func (w *Watcher) Wait() { <-w.done }

// pump long-polls the daemon's event stream and forwards matching events to
// the dispatcher. Errors are retried with capped backoff (the daemon may be
// restarting under its supervisor); a failure resets the event cursor to 0
// because Syncthing event IDs restart with the daemon. Never silent: every
// failure is logged with context (SPEC-0014 REQ "Error Handling Standards").
func (w *Watcher) pump(ctx context.Context) {
	var since int64
	backoff := defaultRetryMin
	for {
		if ctx.Err() != nil {
			return
		}
		evs, err := w.api.Events(ctx, since, watchEventTypes, w.pollWait)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			w.log.Warn("event long-poll failed; retrying", "error", err, "backoff", backoff)
			since = 0 // daemon restart resets event ids
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, defaultRetryMax)
			continue
		}
		backoff = defaultRetryMin
		for _, ev := range evs {
			if ev.ID > since {
				since = ev.ID
			}
			select {
			case <-ctx.Done():
				return
			case w.events <- ev:
			}
		}
	}
}

// dispatch owns the debounce state: per-source deadlines armed by folder
// events, fired through a single timer. Auto-accept events are handled
// inline (they are rare and cheap).
func (w *Watcher) dispatch(ctx context.Context) {
	due := make(map[string]time.Time) // source → deadline
	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	rearm := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		var next time.Time
		for _, d := range due {
			if next.IsZero() || d.Before(next) {
				next = d
			}
		}
		if !next.IsZero() {
			timer.Reset(time.Until(next))
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-w.events:
			switch ev.Type {
			case "FolderSummary", "FolderCompletion":
				if src, ok := w.eventSource(ev); ok {
					// A sync burst re-arms the deadline each time: one import
					// per quiet period, not one per event.
					due[src] = time.Now().Add(w.quiet)
					rearm()
				}
			case "PendingDevicesChanged":
				w.acceptPendingDevices(ctx, ev)
			case "PendingFoldersChanged":
				w.acceptPendingFolders(ctx, ev)
			}
		case <-timer.C:
			now := time.Now()
			for src, deadline := range due {
				if deadline.After(now) {
					continue
				}
				if w.tryImport(ctx, src) {
					delete(due, src)
				} else {
					// Not complete yet, or an import is already running:
					// re-check after another quiet period rather than spinning.
					due[src] = now.Add(w.quiet)
				}
			}
			rearm()
		}
	}
}

// eventSource extracts the managed source a folder event concerns, ok=false
// for folders msgbrowse does not manage.
func (w *Watcher) eventSource(ev syncthing.Event) (string, bool) {
	var data struct {
		Folder string `json:"folder"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		w.log.Warn("undecodable folder event", "type", ev.Type, "error", err)
		return "", false
	}
	src, ok := w.folderSource[data.Folder]
	return src, ok
}

// tryImport gates on folder completion and dispatches the incremental import.
// It returns true when the source needs no further attention (import started,
// or coalesced onto an already-running job that started after our event), and
// false when it should be re-checked (mid-transfer, engine unreachable, or a
// concurrent job in flight).
func (w *Watcher) tryImport(ctx context.Context, src string) bool {
	folderID := syncthing.FolderIDPrefix + src
	opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	comp, err := w.api.FolderCompletion(opCtx, folderID, "")
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return true
		}
		w.log.Warn("completion check failed; will retry", "folder", folderID, "error", err)
		return false
	}
	if comp.CompletionPct < 100 || comp.NeedItems > 0 || comp.NeedDeletes > 0 {
		// Mid-transfer: never import a partial tree (SPEC-0014 "No re-ingest
		// during an in-flight transfer").
		w.log.Debug("folder not yet complete; deferring import",
			"folder", folderID, "completion", comp.CompletionPct, "need_items", comp.NeedItems)
		return false
	}

	prog, err := w.importer.SyncImport(src)
	if err != nil {
		if errors.Is(err, onboard.ErrJobInProgress) {
			// The runner's per-source guard: an Enable/Refresh/import is
			// already running. Retry after the quiet period so the delta this
			// event announced is not lost (SPEC-0014 "coalesced or queued
			// rather than run concurrently").
			w.log.Info("import already running; will retry", "source", src)
			return false
		}
		w.log.Error("sync import failed to start", "source", src, "error", err)
		return true // a hard start failure is terminal for this event burst
	}
	w.log.Info("sync import dispatched", "source", src, "phase", string(prog.Phase))
	if err := w.st.RecordSyncImport(ctx, folderID, src); err != nil {
		w.log.Warn("could not record sync import", "folder", folderID, "error", err)
	}
	return true
}

// acceptPendingDevices resolves a PendingDevicesChanged event: every added
// pending device whose ID is in the paired registry is (re-)added to the
// daemon config — covering config regeneration races and reconnects — and
// every other ID is logged and left pending. NEVER a blanket accept.
func (w *Watcher) acceptPendingDevices(ctx context.Context, ev syncthing.Event) {
	var data struct {
		Added []struct {
			DeviceID string `json:"deviceID"`
			Name     string `json:"name"`
		} `json:"added"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		w.log.Warn("undecodable PendingDevicesChanged event", "error", err)
		return
	}
	for _, add := range data.Added {
		peer, err := w.st.GetSyncPeerByDeviceID(ctx, add.DeviceID)
		if err != nil {
			w.log.Info("ignoring pending device (not explicitly paired)",
				"device_id", add.DeviceID, "name", add.Name)
			continue
		}
		m := NewManager(w.api, w.st, "", nil, w.log)
		if err := m.ensureDevice(ctx, *peer); err != nil {
			w.log.Warn("could not accept paired pending device", "device_id", peer.DeviceID, "error", err)
			continue
		}
		if err := m.ensureFolderShares(ctx, peer.DeviceID, w.managedIntersect(peer.Folders)); err != nil {
			w.log.Warn("could not re-share folders with paired device", "device_id", peer.DeviceID, "error", err)
			continue
		}
		w.log.Info("accepted pending device (explicitly paired)", "device_id", peer.DeviceID, "name", peer.Name)
	}
}

// acceptPendingFolders resolves a PendingFoldersChanged event: an offered
// folder is accepted only when BOTH the offering device is in the paired
// registry AND the folder id is one this node manages — then the folder is
// shared with that device. Offers from unpaired devices, or for folder ids
// outside the managed set, are logged and ignored.
func (w *Watcher) acceptPendingFolders(ctx context.Context, ev syncthing.Event) {
	var data struct {
		Added []struct {
			DeviceID string `json:"deviceID"`
			FolderID string `json:"folderID"`
		} `json:"added"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		w.log.Warn("undecodable PendingFoldersChanged event", "error", err)
		return
	}
	for _, add := range data.Added {
		if _, managed := w.folderSource[add.FolderID]; !managed {
			w.log.Info("ignoring pending folder offer (not a managed folder)",
				"folder", add.FolderID, "device_id", add.DeviceID)
			continue
		}
		peer, err := w.st.GetSyncPeerByDeviceID(ctx, add.DeviceID)
		if err != nil {
			w.log.Info("ignoring pending folder offer (device not explicitly paired)",
				"folder", add.FolderID, "device_id", add.DeviceID)
			continue
		}
		m := NewManager(w.api, w.st, "", nil, w.log)
		if err := m.ensureFolderShares(ctx, peer.DeviceID, []string{add.FolderID}); err != nil {
			w.log.Warn("could not share offered folder with paired device",
				"folder", add.FolderID, "device_id", peer.DeviceID, "error", err)
			continue
		}
		w.log.Info("accepted pending folder offer from paired device",
			"folder", add.FolderID, "device_id", peer.DeviceID)
	}
}

// managedIntersect filters a peer's recorded folder set down to the folders
// this watcher manages.
func (w *Watcher) managedIntersect(folders []string) []string {
	var out []string
	for _, f := range folders {
		if _, ok := w.folderSource[f]; ok {
			out = append(out, f)
		}
	}
	return out
}
