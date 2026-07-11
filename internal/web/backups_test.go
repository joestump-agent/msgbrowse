package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBackupsPage (issue #2): the Backups tab renders the snapshot inventory
// that used to live on /status — total footprint, count, and the per-snapshot
// table (name, taken-at, size, retention tier). The fixture archive carries a
// .snapshots dir with three tarballs (daily/monthly/yearly tiers), so the card
// and its rows must render.
func TestBackupsPage(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := get(t, srv, "/backups")
	if rec.Code != http.StatusOK {
		t.Fatalf("backups page = %d", rec.Code)
	}
	body := rec.Body.String()

	// The snapshot card, its footprint copy, and the per-tier rows.
	for _, want := range []string{
		"Encrypted DB snapshots",
		"Total footprint",
		"never opens or decrypts",
		"status-table",
		"tier-pill",
		"daily", "monthly", "yearly",
	} {
		if !contains(body, want) {
			t.Errorf("backups page missing %q", want)
		}
	}
	// It renders inside the Settings shell with the Backups tab active.
	if !contains(body, `<h1 class="screen-h1">Settings</h1>`) {
		t.Error("backups page missing the shared Settings shell h1")
	}
	if !contains(body, `href="/backups" class="settings-tab settings-tab-active"`) {
		t.Error("backups page missing its active sub-nav tab")
	}
	// The inventory-only scope: no ingest grid, no archive-freshness strip.
	for _, absent := range []string{"Last ingest", "Archive freshness"} {
		if contains(body, absent) {
			t.Errorf("backups page leaked the Status-only surface %q", absent)
		}
	}
}

// TestBackupsPageNoPipeline is the issue-#164 behavior, preserved on the new
// tab: with no snapshots recorded and no .snapshots dir in the signal archive
// (the desktop-onboarded shape — newManagedRootServer's temp managed root),
// the Encrypted-DB-snapshots card is replaced by one neutral line; growing a
// .snapshots dir brings the card back even before any rows are ingested.
func TestBackupsPageNoPipeline(t *testing.T) {
	srv, _, managed := newManagedRootServer(t)

	body := get(t, srv, "/backups").Body.String()
	if contains(body, "Encrypted DB snapshots") {
		t.Error("/backups rendered the snapshots card with no snapshot pipeline")
	}
	if !contains(body, "No snapshot pipeline on this machine.") {
		t.Error("/backups missing the neutral no-pipeline line")
	}

	if err := os.MkdirAll(filepath.Join(managed, ".snapshots"), 0o755); err != nil {
		t.Fatal(err)
	}
	body = get(t, srv, "/backups").Body.String()
	if !contains(body, "Encrypted DB snapshots") {
		t.Error("/backups hid the snapshots card despite a .snapshots dir in the archive")
	}
	if contains(body, "No snapshot pipeline on this machine.") {
		t.Error("/backups kept the no-pipeline line beside the snapshots card")
	}
}

// TestBackupsBoostedPartial: the boosted (#main-content) swap of /backups
// carries the snapshot inventory and its owning <title>, but none of the
// document shell — the SPEC-0008 REQ-0008-006 *_content contract.
func TestBackupsBoostedPartial(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := getPartial(t, srv, "/backups")
	if rec.Code != http.StatusOK {
		t.Fatalf("partial status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, "<title>Backups · msgbrowse</title>") {
		t.Errorf("boosted partial missing its owning title; body starts %q", body[:min(120, len(body))])
	}
	if !contains(body, `id="main-content"`) || !contains(body, "Encrypted DB snapshots") {
		t.Error("boosted partial missing the #main-content snapshot inventory")
	}
	// No shell in the boosted swap.
	for _, forbidden := range []string{"<!doctype", "app-sidebar", "app-toolbar"} {
		if contains(strings.ToLower(body), forbidden) {
			t.Errorf("boosted partial leaked shell marker %q", forbidden)
		}
	}
}
