package store

import (
	"context"
	"testing"

	"github.com/joestump/msgbrowse/internal/contacts"
	"github.com/joestump/msgbrowse/internal/source"
)

// contactIDOf returns the contact id linked to a conversation.
func contactIDOf(t *testing.T, st *Store, convID int64) int64 {
	t.Helper()
	var id int64
	if err := st.DB().QueryRow(`SELECT contact_id FROM conversations WHERE id = ?`, convID).Scan(&id); err != nil {
		t.Fatalf("contact id of conv %d: %v", convID, err)
	}
	return id
}

func countContacts(t *testing.T, st *Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM contacts`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func countLinks(t *testing.T, st *Store, kind string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM contact_links WHERE kind = ?`, kind).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func renameContact(t *testing.T, st *Store, id int64, name string) {
	t.Helper()
	if _, err := st.DB().Exec(`UPDATE contacts SET display_name = ? WHERE id = ?`, name, id); err != nil {
		t.Fatal(err)
	}
}

func TestMergeRulesDefaultsAndRoundTrip(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	got, err := st.GetMergeRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := MergeRules{AutoMerge: false, MatchPhone: true, MatchEmail: true, UseAddressBook: true}
	if got != want {
		t.Fatalf("default rules = %+v, want %+v", got, want)
	}

	set := MergeRules{AutoMerge: true, MatchPhone: true, MatchEmail: false, UseAddressBook: false}
	if err := st.SetMergeRules(ctx, set); err != nil {
		t.Fatal(err)
	}
	got, err = st.GetMergeRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != set {
		t.Fatalf("round-trip rules = %+v, want %+v", got, set)
	}
}

func TestMergeContactsUnionsPerson(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	sig, err := st.UpsertConversation(ctx, source.Signal, "MJ")
	if err != nil {
		t.Fatal(err)
	}
	im, err := st.UpsertConversation(ctx, source.IMessage, "+15551234567")
	if err != nil {
		t.Fatal(err)
	}
	cA, cB := contactIDOf(t, st, sig), contactIDOf(t, st, im)
	if cA == cB {
		t.Fatal("distinct conversations should start on distinct contacts")
	}

	// Identical fact on both contacts (dedup on merge) plus a distinct one.
	putTestFact(t, st, cA, "likes tea")
	putTestFact(t, st, cB, "likes tea")
	putTestFact(t, st, cB, "has a cat")

	winner, err := st.MergeContacts(ctx, cA, cB)
	if err != nil {
		t.Fatal(err)
	}
	// Both auto-created (name == identifier), so the lower id wins.
	wantWinner := cA
	if cB < cA {
		wantWinner = cB
	}
	if winner != wantWinner {
		t.Fatalf("winner = %d, want lower id %d", winner, wantWinner)
	}

	// Both conversations now render the same person.
	if got := contactIDOf(t, st, sig); got != winner {
		t.Errorf("signal conv contact = %d, want %d", got, winner)
	}
	if got := contactIDOf(t, st, im); got != winner {
		t.Errorf("imessage conv contact = %d, want %d", got, winner)
	}
	// Loser is gone.
	if n := countContacts(t, st); n != 1 {
		t.Errorf("contacts remaining = %d, want 1", n)
	}
	// Winner holds both identifiers.
	ids, err := contactPairs(ctx, st.db, winner)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Errorf("winner identifiers = %+v, want 2", ids)
	}
	// Facts deduplicated to two ("likes tea" once, "has a cat" once).
	if n, err := st.CountFacts(ctx); err != nil || n != 2 {
		t.Errorf("facts = %d (err=%v), want 2 after dedup", n, err)
	}
	// A bipartite merge link was recorded (1 identifier each => 1 pair).
	if n := countLinks(t, st, "merge"); n != 1 {
		t.Errorf("merge links = %d, want 1", n)
	}
}

func TestMergeContactsWinnerByMeaningfulName(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	sig, _ := st.UpsertConversation(ctx, source.Signal, "+15551234567")
	im, _ := st.UpsertConversation(ctx, source.IMessage, "+15559999999")
	cA, cB := contactIDOf(t, st, sig), contactIDOf(t, st, im)
	// Give the HIGHER-id contact a user-meaningful name; it must still win.
	hi, lo := cA, cB
	if cB > cA {
		hi, lo = cB, cA
	}
	renameContact(t, st, hi, "Mary Jane")

	winner, err := st.MergeContacts(ctx, cA, cB)
	if err != nil {
		t.Fatal(err)
	}
	if winner != hi {
		t.Fatalf("winner = %d, want the meaningfully-named contact %d (lower id was %d)", winner, hi, lo)
	}
}

func TestSplitContactSeparatesAndRecords(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	sig, _ := st.UpsertConversation(ctx, source.Signal, "MJ")
	im, _ := st.UpsertConversation(ctx, source.IMessage, "+15551234567")
	cA, cB := contactIDOf(t, st, sig), contactIDOf(t, st, im)
	winner, err := st.MergeContacts(ctx, cA, cB)
	if err != nil {
		t.Fatal(err)
	}
	if countContacts(t, st) != 1 {
		t.Fatal("expected merged to one contact")
	}

	// Split the iMessage identifier back off.
	newID, err := st.SplitContact(ctx, winner, []ContactIdentifier{{Source: source.IMessage, Identifier: "+15551234567"}})
	if err != nil {
		t.Fatal(err)
	}
	if newID == winner {
		t.Fatal("split must create a new contact")
	}
	if countContacts(t, st) != 2 {
		t.Errorf("contacts = %d, want 2 after split", countContacts(t, st))
	}
	// The iMessage conversation followed its identifier to the new contact.
	if got := contactIDOf(t, st, im); got != newID {
		t.Errorf("imessage conv contact = %d, want new %d", got, newID)
	}
	if got := contactIDOf(t, st, sig); got != winner {
		t.Errorf("signal conv contact = %d, want original %d", got, winner)
	}
	// The split replaced the merge decision on that pair.
	if n := countLinks(t, st, "merge"); n != 0 {
		t.Errorf("merge links = %d, want 0 (replaced by split)", n)
	}
	if n := countLinks(t, st, "split"); n != 1 {
		t.Errorf("split links = %d, want 1", n)
	}
}

func TestReconcileReappliesManualMergeAcrossReingest(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	sig, _ := st.UpsertConversation(ctx, source.Signal, "MJ")
	im, _ := st.UpsertConversation(ctx, source.IMessage, "+15551234567")
	cA, cB := contactIDOf(t, st, sig), contactIDOf(t, st, im)
	winner, err := st.MergeContacts(ctx, cA, cB)
	if err != nil {
		t.Fatal(err)
	}
	if countContacts(t, st) != 1 {
		t.Fatal("expected one contact after merge")
	}

	// Simulate disabling + re-importing iMessage: its identifiers and orphaned
	// contacts are pruned, then the same identity is re-created fresh.
	if _, err := st.DeleteSourceData(ctx, source.IMessage); err != nil {
		t.Fatal(err)
	}
	// The merge decision must survive the source delete.
	if n := countLinks(t, st, "merge"); n != 1 {
		t.Fatalf("merge links after source delete = %d, want 1 (overrides must survive)", n)
	}
	// Re-import recreates a fresh, unmerged iMessage contact.
	im2, err := st.UpsertConversation(ctx, source.IMessage, "+15551234567")
	if err != nil {
		t.Fatal(err)
	}
	if contactIDOf(t, st, im2) == winner {
		t.Fatal("re-import should transiently create a separate contact before reconcile")
	}
	if countContacts(t, st) != 2 {
		t.Fatalf("contacts before reconcile = %d, want 2", countContacts(t, st))
	}

	// Reconcile folds it back.
	if err := st.ReconcileContacts(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if countContacts(t, st) != 1 {
		t.Fatalf("contacts after reconcile = %d, want 1 (folded back)", countContacts(t, st))
	}
	if got := contactIDOf(t, st, im2); got != winner {
		t.Errorf("re-imported conv contact = %d, want folded onto %d", got, winner)
	}

	// Reconcile is idempotent: a second pass changes nothing.
	if err := st.ReconcileContacts(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if countContacts(t, st) != 1 {
		t.Fatalf("contacts after second reconcile = %d, want 1 (idempotent)", countContacts(t, st))
	}
}

func TestReconcileAutoMergeAndSplitPrecedence(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// Two contacts sharing an exact phone value.
	sig, _ := st.UpsertConversation(ctx, source.Signal, "+15551234567")
	im, _ := st.UpsertConversation(ctx, source.IMessage, "+15551234567")
	_ = contactIDOf(t, st, sig)
	_ = contactIDOf(t, st, im)

	// Default rules: auto-merge OFF => reconcile leaves them apart.
	if err := st.ReconcileContacts(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if countContacts(t, st) != 2 {
		t.Fatalf("auto-merge off: contacts = %d, want 2 (suggestion only)", countContacts(t, st))
	}

	// Enable auto-merge on phone => reconcile merges them and records an auto link.
	if err := st.SetMergeRules(ctx, MergeRules{AutoMerge: true, MatchPhone: true, MatchEmail: true, UseAddressBook: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.ReconcileContacts(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if countContacts(t, st) != 1 {
		t.Fatalf("auto-merge on: contacts = %d, want 1", countContacts(t, st))
	}
	if n := countLinks(t, st, "merge"); n != 1 {
		t.Errorf("auto merge links = %d, want 1", n)
	}

	// A second reconcile is a no-op.
	if err := st.ReconcileContacts(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if countContacts(t, st) != 1 {
		t.Fatalf("second reconcile changed contacts to %d, want 1 (idempotent)", countContacts(t, st))
	}

	// Split them apart. The split must survive auto-merge on the next reconcile.
	merged := contactIDOf(t, st, sig)
	if _, err := st.SplitContact(ctx, merged, []ContactIdentifier{{Source: source.IMessage, Identifier: "+15551234567"}}); err != nil {
		t.Fatal(err)
	}
	if countContacts(t, st) != 2 {
		t.Fatalf("after split: contacts = %d, want 2", countContacts(t, st))
	}
	if err := st.ReconcileContacts(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if countContacts(t, st) != 2 {
		t.Fatalf("split precedence: reconcile re-merged (contacts=%d), want 2 kept apart", countContacts(t, st))
	}
}

func TestMergeCandidatesExcludesSplitPairs(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	sig, _ := st.UpsertConversation(ctx, source.Signal, "+15551234567")
	im, _ := st.UpsertConversation(ctx, source.IMessage, "+15551234567")
	_ = sig
	_ = im

	// The shared phone yields one candidate.
	cands, err := st.MergeCandidates(ctx, contacts.Unavailable{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].Reason != string(contacts.ReasonPhone) {
		t.Fatalf("candidates = %+v, want one phone candidate", cands)
	}

	// Merge then split => a manual split record; the pair must no longer be
	// suggested.
	winner, err := st.MergeContacts(ctx, cands[0].ContactA, cands[0].ContactB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SplitContact(ctx, winner, []ContactIdentifier{{Source: source.IMessage, Identifier: "+15551234567"}}); err != nil {
		t.Fatal(err)
	}
	cands, err = st.MergeCandidates(ctx, contacts.Unavailable{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 0 {
		t.Fatalf("candidates after split = %+v, want none (split excluded)", cands)
	}
}

// putTestFact inserts a minimal fact for a contact.
func putTestFact(t *testing.T, st *Store, contactID int64, fact string) {
	t.Helper()
	if _, err := st.PutFact(context.Background(), FactInput{
		ContactID:         contactID,
		Fact:              fact,
		Category:          "personal",
		Source:            source.Signal,
		SourceMessageHash: "hash-" + fact,
		SourceTS:          "2026-01-01T00:00:00Z",
		SourceTSUnix:      1767225600,
		Model:             "test",
	}); err != nil {
		t.Fatalf("put fact %q: %v", fact, err)
	}
}
