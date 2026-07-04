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
//     offer from a registry peer is accepted for any folder id that maps
//     onto the FIXED SOURCE ENUM — provisioning the managed root when this
//     node lacks it (the fresh-replica path, SPEC-0014 REQ "Importer and
//     Replica Roles") and persisting the widened share to the peer's
//     registry row so restarts regenerate the identical config. Anything
//     else — an unpaired device, or a folder id outside the enum — is logged
//     and ignored, never a blanket accept (SPEC-0014 "A device ID alone does
//     not grant sync"; see acceptPendingFolders for the scope decision).
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
	"log/slog"
	"sync"
	"time"

	"github.com/joestump/msgbrowse/internal/devices"
	"github.com/joestump/msgbrowse/internal/onboard"
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
	// Peer connection transitions: recorded into the registry's last_seen_at
	// and the Logs event feed (#158 status surfacing).
	"DeviceConnected",
	"DeviceDisconnected",
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
	// Folders is the LIVE managed-folder set, shared with the pairing
	// Manager: only events for its members trigger imports, and an accepted
	// folder offer provisions into it (so a folder that appears mid-run is
	// watched immediately). Required.
	Folders *FolderSet
	// Notes is the shared device-sync event feed for the Logs page (#158);
	// nil records nothing (Notes methods are nil-safe).
	Notes *Notes
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
	notes    *Notes

	// folders is the live managed-folder set shared with the pairing
	// Manager; an event for any folder outside it is ignored outright.
	folders *FolderSet
	// mgr executes the accept-side config mutations (ensureDevice /
	// ensureFolders / ensureFolderShares) over the same folder set.
	mgr *Manager

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
	if o.Folders == nil {
		return nil, errors.New("devsync watcher: Folders is required")
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
	// The watcher's own manager over the shared folder set; the friendly
	// name is irrelevant here (it only feeds ActivePairing payloads). It
	// shares the notes ring so accept-side actions land in the same feed.
	mgr := NewManager(o.API, o.Store, "", o.Folders, log)
	mgr.SetNotes(o.Notes)
	return &Watcher{
		api:      o.API,
		st:       o.Store,
		importer: o.Importer,
		quiet:    o.Quiet,
		pollWait: o.PollTimeout,
		log:      log.With("component", "devsync"),
		notes:    o.Notes,
		folders:  o.Folders,
		mgr:      mgr,
		events:   make(chan syncthing.Event, 16),
		done:     make(chan struct{}),
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
			case "DeviceConnected", "DeviceDisconnected":
				w.recordPeerConnection(ctx, ev)
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
	return w.folders.SourceFor(data.Folder)
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
	w.notes.Add(NoteInfo, "Sync completed for "+src+" — incremental import dispatched")
	if err := w.st.RecordSyncImport(ctx, folderID, src); err != nil {
		w.log.Warn("could not record sync import", "folder", folderID, "error", err)
	}
	return true
}

// recordPeerConnection resolves a DeviceConnected/DeviceDisconnected event
// for the status surfaces (#158): a REGISTRY peer's connect touches its
// last_seen_at and both transitions land in the Logs event feed. Unpaired
// devices are ignored — connection chatter about devices the operator never
// accepted is not msgbrowse state.
func (w *Watcher) recordPeerConnection(ctx context.Context, ev syncthing.Event) {
	var data struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		w.log.Warn("undecodable device connection event", "type", ev.Type, "error", err)
		return
	}
	peer, err := w.st.GetSyncPeerByDeviceID(ctx, data.ID)
	if err != nil {
		return // not a paired peer; nothing to record
	}
	if ev.Type == "DeviceConnected" {
		if err := w.st.TouchSyncPeerSeen(ctx, peer.DeviceID, ev.Time); err != nil {
			w.log.Warn("could not record peer last-seen", "device_id", peer.DeviceID, "error", err)
		}
		w.notes.Add(NoteInfo, "Peer connected: "+peer.Name+" ("+peer.ShortID()+")")
		return
	}
	w.notes.Add(NoteInfo, "Peer disconnected: "+peer.Name+" ("+peer.ShortID()+")")
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
		if err := w.mgr.ensureDevice(ctx, *peer); err != nil {
			w.log.Warn("could not accept paired pending device", "device_id", peer.DeviceID, "error", err)
			continue
		}
		if err := w.mgr.ensureFolderShares(ctx, peer.DeviceID, w.managedIntersect(peer.Folders)); err != nil {
			w.log.Warn("could not re-share folders with paired device", "device_id", peer.DeviceID, "error", err)
			continue
		}
		w.log.Info("accepted pending device (explicitly paired)", "device_id", peer.DeviceID, "name", peer.Name)
	}
}

// acceptPendingFolders resolves a PendingFoldersChanged event under the
// SPEC-0014 acceptance scope:
//
// Scope decision (issue #157 review): pair-time folders[] is a SOFT
// introduction, not a hard cap. An offer is accepted when BOTH the offering
// device is in the paired registry AND the folder id maps onto the fixed
// source enum ("msgbrowse-<source>") — even if this node has never seen that
// folder before. The spec's both-ends-acceptance language places the trust
// decision on the DEVICE ("both peers MUST have accepted the other's device
// ID before any archive data flows"); folder ids are deterministic and
// public, and the replica role requires receiving archives for sources this
// node has no local roots for. What is NEVER accepted: an offer from an
// unpaired device, or a folder id outside the enum — both are logged and
// ignored, so a peer can never name a folder into existence.
//
// Accepting means: provision the managed root if absent (fresh-replica
// path), PERSIST the widened share to the peer's registry row FIRST (the
// durable decision — ApplyPeers regenerates the daemon config from the
// registry on every restart, so an unpersisted share would flip-flop away),
// then add the folder to the live daemon config and share it. /settings and
// `msgbrowse devices list` both render the registry, so the recorded share
// set is always the true one.
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
		src, ok := SourceForFolderID(add.FolderID)
		if !ok {
			w.log.Info("ignoring pending folder offer (folder id outside the managed source enum)",
				"folder", add.FolderID, "device_id", add.DeviceID)
			continue
		}
		peer, err := w.st.GetSyncPeerByDeviceID(ctx, add.DeviceID)
		if err != nil {
			w.log.Info("ignoring pending folder offer (device not explicitly paired)",
				"folder", add.FolderID, "device_id", add.DeviceID)
			continue
		}
		folder, created, err := w.folders.Provision(add.FolderID)
		if err != nil {
			w.log.Warn("could not provision offered managed folder",
				"folder", add.FolderID, "device_id", peer.DeviceID, "error", err)
			continue
		}
		// Persist before the daemon mutations: a restart regenerates config
		// from the registry (ApplyPeers), so the recorded set is what makes
		// the acceptance durable rather than flip-flopping away. A newly
		// provisioned root also records the offering peer as the source's
		// IMPORTER (SPEC-0014 REQ "Importer and Replica Roles"): the archive
		// originates there, this node is its replica, and the Providers card
		// + Enable guard read exactly this recorded role.
		changed := false
		if !containsString(peer.Folders, add.FolderID) {
			peer.Folders = append(peer.Folders, add.FolderID)
			changed = true
		}
		if created && peer.Roles[src] == "" {
			if peer.Roles == nil {
				peer.Roles = make(map[string]string)
			}
			peer.Roles[src] = devices.RoleImporter
			changed = true
		}
		if changed {
			if _, err := w.st.UpsertSyncPeer(ctx, *peer); err != nil {
				w.log.Warn("could not persist widened folder share",
					"folder", add.FolderID, "device_id", peer.DeviceID, "error", err)
				continue
			}
		}
		if err := w.mgr.ensureFolders(ctx, []syncthing.Folder{folder}); err != nil {
			w.log.Warn("could not configure offered folder",
				"folder", add.FolderID, "device_id", peer.DeviceID, "error", err)
			continue
		}
		if err := w.mgr.ensureFolderShares(ctx, peer.DeviceID, []string{add.FolderID}); err != nil {
			w.log.Warn("could not share offered folder with paired device",
				"folder", add.FolderID, "device_id", peer.DeviceID, "error", err)
			continue
		}
		w.log.Info("accepted pending folder offer from paired device",
			"folder", add.FolderID, "device_id", peer.DeviceID)
		w.notes.Add(NoteInfo, "Accepted folder offer "+add.FolderID+" from "+peer.Name+" ("+peer.ShortID()+")")
	}
}

// managedIntersect filters a peer's recorded folder set down to the folders
// this watcher manages.
func (w *Watcher) managedIntersect(folders []string) []string {
	var out []string
	for _, f := range folders {
		if w.folders.Contains(f) {
			out = append(out, f)
		}
	}
	return out
}
