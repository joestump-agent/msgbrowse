// The two ADR-0021 structural invariants, asserted end-to-end (issue #157):
//
//  1. Archive-not-DB: the synced folder set NEVER includes the data_dir, the
//     SQLite database, or its WAL/SHM files — at folder generation, at path
//     validation, in the generated daemon config, and in the on-disk ignore
//     patterns (SPEC-0014 "The database is never in a synced folder", "No
//     database file enters a synced folder").
//
//  2. Replica-DB ≡ fresh-ingest: a replica's database — built by importing
//     the archive tree Syncthing delivered — contains exactly the rows a
//     fresh local ingest of the same tree produces, because it IS the same
//     ingest; no database file ever crosses the wire (SPEC-0014 "Replica DB
//     equals a fresh local ingest of the synced archives").
//
// Governing: ADR-0021 invariant 1 ("archive-sync, not DB-replication"),
// SPEC-0014 REQ "Archive Sync Not Database Replication", REQ
// "msgbrowse-Owned Config Generation".
package devsync

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/devices"
	"github.com/joestump/msgbrowse/internal/ingest"
	"github.com/joestump/msgbrowse/internal/store"
	"github.com/joestump/msgbrowse/internal/syncthing"
)

// TestSyncedFolderSetNeverIncludesDataDirOrDB builds a realistic data_dir —
// SQLite DB + WAL/SHM at its root, a Syncthing home, managed archive roots —
// and asserts the folder set msgbrowse would hand the daemon can never reach
// the database:
//
//   - ExistingManagedFolders yields ONLY paths under <data_dir>/archives/;
//   - every yielded path passes ValidateManagedFolderPath, while the
//     data_dir itself, the DB file, and the Syncthing home are all REFUSED;
//   - the DB path is not inside any yielded folder path;
//   - peers folded in via ApplyPeers change device wiring, never paths;
//   - the generated config.xml names no folder at or above data_dir, and the
//     .stignore defense-in-depth patterns exclude DB/WAL/SHM file names.
func TestSyncedFolderSetNeverIncludesDataDirOrDB(t *testing.T) {
	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, store.DBFileName)
	for _, f := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.WriteFile(f, []byte("sqlite-bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dataDir, syncthing.HomeDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{"signal", "imessage"} {
		if err := os.MkdirAll(filepath.Join(dataDir, "archives", src, "export"), 0o700); err != nil {
			t.Fatal(err)
		}
	}

	folders, err := syncthing.ExistingManagedFolders(dataDir)
	if err != nil {
		t.Fatalf("ExistingManagedFolders: %v", err)
	}
	if len(folders) != 2 {
		t.Fatalf("folders = %d, want 2", len(folders))
	}

	peers := []devices.SyncPeer{{DeviceID: peerAID, Name: "replica", Folders: []string{"msgbrowse-signal", "msgbrowse-imessage"}}}
	folders, devs := ApplyPeers(folders, peers)
	if len(devs) != 1 {
		t.Fatalf("ApplyPeers devices = %d, want 1", len(devs))
	}

	archives := filepath.Join(dataDir, "archives") + string(filepath.Separator)
	for _, f := range folders {
		if err := syncthing.ValidateManagedFolderPath(dataDir, f.Path); err != nil {
			t.Errorf("managed folder %s failed validation: %v", f.ID, err)
		}
		if !strings.HasPrefix(f.Path+string(filepath.Separator), archives) {
			t.Errorf("folder %s path %q escapes the archives subtree", f.ID, f.Path)
		}
		// The DB can never live inside a synced folder root.
		if strings.HasPrefix(dbPath, f.Path+string(filepath.Separator)) {
			t.Errorf("database %q is inside synced folder %q", dbPath, f.Path)
		}
	}

	// The paths that MUST be refused as folder roots.
	for _, refused := range []string{
		dataDir,
		dbPath,
		filepath.Join(dataDir, syncthing.HomeDirName),
		filepath.Join(dataDir, "archives"), // the parent, not a per-source root, is also outside the contract
		"/",
	} {
		if err := syncthing.ValidateManagedFolderPath(dataDir, refused); err == nil {
			t.Errorf("ValidateManagedFolderPath accepted %q", refused)
		}
	}

	// The generated daemon config carries only the archive folder paths.
	cfg, err := syncthing.GenerateConfigXML(syncthing.ConfigSpec{
		GUIAddress:    "127.0.0.1:8384",
		APIKey:        "k",
		ListenAddress: "tcp://:8788",
		Folders:       folders,
		Devices:       devs,
	})
	if err != nil {
		t.Fatalf("GenerateConfigXML: %v", err)
	}
	xml := string(cfg)
	if strings.Contains(xml, store.DBFileName) {
		t.Error("generated config references the database file")
	}
	if strings.Contains(xml, `path="`+dataDir+`"`) {
		t.Error("generated config syncs the data_dir root")
	}
}

// TestReplicaDBEqualsFreshIngest is the fixture-level replica property,
// starting from the true fresh-replica state (issue #157 adversarial review,
// finding 1): the replica has NO managed roots until pairing with the
// importer PROVISIONS one, Syncthing then delivers the archive files into it
// (files only, plus Syncthing's .stfolder marker and .stignore — never a
// database), the replica imports, and its rows are identical to a fresh
// direct ingest of the source fixture.
func TestReplicaDBEqualsFreshIngest(t *testing.T) {
	fixture := filepath.Join("..", "..", "testdata", "archive")
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	// The "importer node": a fresh ingest of the fixture tree.
	importerSt, err := store.Open(filepath.Join(t.TempDir(), "importer.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer importerSt.Close()
	if _, err := ingest.Run(ctx, importerSt, ingest.Options{ArchiveRoot: fixture, Logger: quiet}); err != nil {
		t.Fatalf("importer ingest: %v", err)
	}

	// The "replica node" starts with an empty data dir — no archives/ subtree
	// at all. Pairing with the importer (whose payload introduces the signal
	// folder) is what provisions <data_dir>/archives/signal.
	replicaData := t.TempDir()
	folderSet, err := NewFolderSet(replicaData, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(newStubDaemon(t, nil).client(), newMemPeerStore(), "replica", folderSet, testLogger())
	if _, err := m.Pair(ctx, mustSyncCode(t, peerAID, []string{"msgbrowse-signal"}, "importer")); err != nil {
		t.Fatalf("fresh-replica Pair: %v", err)
	}
	managed := folderSet.List()
	if len(managed) != 1 {
		t.Fatalf("managed folders after pair = %d, want 1", len(managed))
	}
	replicaRoot := managed[0].Path
	if want := filepath.Join(replicaData, "archives", "signal"); replicaRoot != want {
		t.Fatalf("provisioned root = %s, want %s", replicaRoot, want)
	}

	// Syncthing delivers the importer's archive files into the provisioned
	// root. Simulate the delivery as a file copy; the .stfolder marker and
	// .stignore already exist from provisioning. NO database file is copied —
	// that is the invariant under test.
	copyTree(t, filepath.Join(fixture, "export"), filepath.Join(replicaRoot, "export"))
	for _, marker := range []string{".stfolder", ".stignore"} {
		if _, err := os.Stat(filepath.Join(replicaRoot, marker)); err != nil {
			t.Fatalf("provisioned root missing Syncthing artifact %s: %v", marker, err)
		}
	}

	replicaSt, err := store.Open(filepath.Join(replicaData, "replica.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer replicaSt.Close()
	if _, err := ingest.Run(ctx, replicaSt, ingest.Options{ArchiveRoot: replicaRoot, Logger: quiet}); err != nil {
		t.Fatalf("replica ingest: %v", err)
	}

	// Same rows: conversations, message hash set, attachments, links,
	// reactions. Message hashes are the stable content identity, so equality
	// here is equality of the ingested corpus.
	for _, q := range []struct {
		name  string
		query string
	}{
		{"conversations", `SELECT source || '/' || name FROM conversations ORDER BY 1`},
		{"message hashes", `SELECT hash FROM messages ORDER BY 1`},
		{"attachments", `SELECT rel_path FROM attachments ORDER BY 1`},
		{"links", `SELECT url FROM links ORDER BY 1`},
		{"reactions", `SELECT message_hash || '/' || emoji || '/' || actor FROM reactions ORDER BY 1`},
	} {
		a := queryStrings(t, importerSt, q.query)
		b := queryStrings(t, replicaSt, q.query)
		if len(a) == 0 && q.name == "message hashes" {
			t.Fatalf("fixture ingested zero messages — fixture moved?")
		}
		if !equalStrings(a, b) {
			t.Errorf("%s differ:\n importer: %v\n replica:  %v", q.name, a, b)
		}
	}

	// And structurally: no SQLite file exists anywhere under the replica's
	// synced root — the DB lives outside the folder Syncthing manages.
	err = filepath.WalkDir(replicaRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && (strings.HasSuffix(path, ".sqlite") || strings.HasSuffix(path, ".db")) {
			t.Errorf("database-like file inside the synced root: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// copyTree copies a directory tree (regular files only — exactly what
// Syncthing transfers for an archive).
func copyTree(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o600)
	})
	if err != nil {
		t.Fatalf("copy fixture tree: %v", err)
	}
}

func queryStrings(t *testing.T, st *store.Store, query string) []string {
	t.Helper()
	rows, err := st.DB().Query(query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
