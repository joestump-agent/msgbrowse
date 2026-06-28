package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/joestump/msgbrowse/internal/source"
)

// TestOpenReadOnly verifies the inspection path used by `doctor`: an existing DB
// opens read-only, reports the real schema version + counts, and does NOT accept
// writes (mode=ro) or migrate.
func TestOpenReadOnly(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ro.sqlite")

	// Create + migrate a normal DB, put a row in it, close it.
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := st.UpsertConversation(ctx, source.Signal, "Harper"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()

	// Reads work and report the true on-disk schema version.
	if v, err := ro.UserVersion(ctx); err != nil || v != SchemaVersion() {
		t.Fatalf("UserVersion = %d (err %v), want %d", v, err, SchemaVersion())
	}
	convs, err := ro.ListConversations(ctx)
	if err != nil || len(convs) != 1 {
		t.Fatalf("ListConversations = %d (err %v), want 1", len(convs), err)
	}

	// Writes must be rejected (read-only connection).
	if _, err := ro.UpsertConversation(ctx, source.Signal, "Nope"); err == nil {
		t.Error("expected a write to fail on a read-only handle, got nil error")
	}
}

// TestOpenReadOnlyMissingFile confirms OpenReadOnly does not create a database
// (mode=ro on a nonexistent file fails rather than creating it).
func TestOpenReadOnlyMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.sqlite")
	ro, err := OpenReadOnly(path)
	if err != nil {
		// Expected: open succeeds lazily but the first query fails — accept either,
		// as long as no file was created.
		_ = err
	} else {
		_ = ro.Close()
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Errorf("OpenReadOnly created %q; it must not create databases", path)
	}
}
