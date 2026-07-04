// FolderSet is this node's live managed-folder registry, shared by the
// pairing Manager and the folder-watch Watcher so the two can never disagree
// about what is managed. It exists because the managed set is no longer fixed
// at startup: a FRESH REPLICA has no <data_dir>/archives/<source> roots at
// all, and SPEC-0014's replica role requires that pairing with an importer —
// or accepting a paired importer's later folder offer — PROVISIONS the
// corresponding managed root on the spot. A folder provisioned mid-run is
// immediately visible to the Watcher (its completion events dispatch imports)
// and to the Manager (future pairing payloads introduce it).
//
// The source enum is the hard boundary: Provision maps a folder id onto the
// fixed source set ("msgbrowse-<source>", source.IsKnown) and refuses
// anything else, so a peer can never name a path into existence — the id
// selects from the deterministic managed layout, it never carries one
// (SPEC-0014 "msgbrowse-Owned Config Generation"; the archive-not-DB guard is
// re-asserted inside syncthing.ProvisionManagedFolder).
//
// Governing: ADR-0021, SPEC-0014 REQ "Importer and Replica Roles", REQ
// "Pairing via Device ID and QR", "The database is never in a synced folder".
package devsync

import (
	"fmt"
	"strings"
	"sync"

	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/syncthing"
)

// FolderSet is a concurrency-safe registry of the managed Syncthing folders
// this node currently has on disk, with the ability to provision new ones for
// known sources at runtime. Construct with NewFolderSet; all methods are safe
// for concurrent use (the Manager and Watcher run on different goroutines).
type FolderSet struct {
	dataDir string

	mu      sync.RWMutex
	folders []syncthing.Folder
	sources map[string]string // folder id → source id
}

// NewFolderSet validates the initial folders (each id must map onto a known
// source — the check NewWatcher used to own) and builds the set. dataDir is
// the app-owned data directory provisioning anchors to; it must be non-empty,
// because a mis-anchored provision could otherwise never be caught.
func NewFolderSet(dataDir string, initial []syncthing.Folder) (*FolderSet, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("devsync folder set: data dir is empty")
	}
	s := &FolderSet{
		dataDir: dataDir,
		folders: append([]syncthing.Folder(nil), initial...),
		sources: make(map[string]string, len(initial)),
	}
	for _, f := range initial {
		src, ok := SourceForFolderID(f.ID)
		if !ok {
			return nil, fmt.Errorf("devsync folder set: folder %q does not map to a known source", f.ID)
		}
		s.sources[f.ID] = src
	}
	return s, nil
}

// SourceForFolderID maps a managed folder id ("msgbrowse-<source>") onto the
// fixed source enum, ok=false for any other id. This is the scope check every
// externally-supplied folder id passes through: ids outside the enum are
// never provisioned, configured, or shared, no matter who offers them.
func SourceForFolderID(folderID string) (string, bool) {
	src, found := strings.CutPrefix(folderID, syncthing.FolderIDPrefix)
	if !found || !source.IsKnown(src) {
		return "", false
	}
	return src, true
}

// List returns a copy of the current managed folders.
func (s *FolderSet) List() []syncthing.Folder {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]syncthing.Folder(nil), s.folders...)
}

// IDs returns the current managed folder ids in registration order.
func (s *FolderSet) IDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.folders))
	for _, f := range s.folders {
		ids = append(ids, f.ID)
	}
	return ids
}

// SourceFor returns the source a CURRENTLY MANAGED folder id maps to,
// ok=false for ids not in the set (the Watcher's event filter: events for
// folders msgbrowse does not manage are ignored outright).
func (s *FolderSet) SourceFor(folderID string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src, ok := s.sources[folderID]
	return src, ok
}

// Contains reports whether folderID is in the managed set.
func (s *FolderSet) Contains(folderID string) bool {
	_, ok := s.SourceFor(folderID)
	return ok
}

// Provision ensures the managed folder for folderID exists — on disk and in
// this set — and returns it, with created reporting whether this call
// materialized a root the node did not previously manage. Idempotent: an
// already-managed folder is returned as-is with created=false. The created
// flag is the ROLE-DETECTION signal (SPEC-0014 REQ "Importer and Replica
// Roles"): a share whose root had to be provisioned here originates on the
// peer — the peer is that source's importer and this node its replica — while
// a share of a root this node already held leaves this node the importer.
// A folder id that does not map onto the fixed source enum is refused with an
// error; only <data_dir>/archives/<source> roots can ever be materialized
// (syncthing.ProvisionManagedFolder owns the mkdir, the archive-not-DB
// validation, and the .stignore/.stfolder preparation).
func (s *FolderSet) Provision(folderID string) (f syncthing.Folder, created bool, err error) {
	src, ok := SourceForFolderID(folderID)
	if !ok {
		return syncthing.Folder{}, false, fmt.Errorf("provision folder %q: id does not map to a known source", folderID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.folders {
		if existing.ID == folderID {
			return existing, false, nil
		}
	}
	f, err = syncthing.ProvisionManagedFolder(s.dataDir, src)
	if err != nil {
		return syncthing.Folder{}, false, err
	}
	s.folders = append(s.folders, f)
	s.sources[f.ID] = src
	return f, true, nil
}
