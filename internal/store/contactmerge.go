// Cross-provider contact merge / de-dup engine (issue #11, ADR-0022 /
// SPEC-0015). The pure matching core lives in internal/contacts; this file is
// the persistence + transaction half: the merge-rules settings, candidate
// detection over the stored identifiers, the manual merge/split transactions,
// and the idempotent reconcile pass that re-applies durable decisions after
// every import.
//
// The durability model is a decision journal (contact_links, schema v12), not
// a pointer graph. Decisions are keyed by canonical-ordered stable
// (source, identifier) pairs — never contact rowids, which churn under
// DeleteSourceData + re-enable — so a manual merge or split survives re-ingest:
// UpsertConversation may resurrect an unmerged contact mid-import, and the
// reconcile pass folds it straight back. Precedence is manual split > manual
// merge > auto rules, enforced by one-current-decision-per-pair plus a
// split-block check in the auto path.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/joestump/msgbrowse/internal/contacts"
)

// MergeRules is the persisted single-row settings that drive candidate
// detection and auto-merge. Zero value is NOT the default — read the stored
// row via GetMergeRules (which is seeded with the conservative defaults in
// migration v12: auto-merge off, phone+email trusted, address book on).
type MergeRules struct {
	// AutoMerge enables applying exact-normalized-identifier merges during
	// reconcile. Off by default: msgbrowse only ever suggests unless the user
	// opts in (ADR-0003 / ADR-0022).
	AutoMerge bool
	// MatchPhone trusts phone-number equality for candidates and (when
	// AutoMerge is on) auto-merge.
	MatchPhone bool
	// MatchEmail trusts email equality for candidates and auto-merge.
	MatchEmail bool
	// UseAddressBook lets an available resolver's people contribute candidate
	// suggestions. Address-book hints NEVER auto-merge regardless of AutoMerge.
	UseAddressBook bool
}

func (r MergeRules) match() contacts.MatchRules {
	return contacts.MatchRules{
		MatchPhone:     r.MatchPhone,
		MatchEmail:     r.MatchEmail,
		UseAddressBook: r.UseAddressBook,
	}
}

// querier is the subset of *sql.DB / *sql.Tx this engine needs, so the same
// helpers run both inside a merge transaction and against the pool for
// read-only candidate detection.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// GetMergeRules returns the persisted merge rules. The row is seeded by
// migration v12, but GetMergeRules falls back to the same conservative
// defaults if it is somehow absent, so callers never see a zero-value struct.
func (s *Store) GetMergeRules(ctx context.Context) (MergeRules, error) {
	return getMergeRules(ctx, s.db)
}

func getMergeRules(ctx context.Context, q querier) (MergeRules, error) {
	var r MergeRules
	var auto, phone, email, book int
	err := q.QueryRowContext(ctx,
		`SELECT auto_merge, match_phone, match_email, use_address_book
		   FROM contact_merge_rules WHERE id = 1`).Scan(&auto, &phone, &email, &book)
	if err == sql.ErrNoRows {
		return MergeRules{MatchPhone: true, MatchEmail: true, UseAddressBook: true}, nil
	}
	if err != nil {
		return r, fmt.Errorf("get merge rules: %w", err)
	}
	return MergeRules{
		AutoMerge:      auto != 0,
		MatchPhone:     phone != 0,
		MatchEmail:     email != 0,
		UseAddressBook: book != 0,
	}, nil
}

// SetMergeRules persists the merge rules (single row, id = 1). It upserts so a
// database missing the seed row is still repaired.
func (s *Store) SetMergeRules(ctx context.Context, r MergeRules) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
INSERT INTO contact_merge_rules (id, auto_merge, match_phone, match_email, use_address_book, updated_at)
VALUES (1, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    auto_merge=excluded.auto_merge,
    match_phone=excluded.match_phone,
    match_email=excluded.match_email,
    use_address_book=excluded.use_address_book,
    updated_at=excluded.updated_at`,
		boolToInt(r.AutoMerge), boolToInt(r.MatchPhone), boolToInt(r.MatchEmail),
		boolToInt(r.UseAddressBook), now)
	if err != nil {
		return fmt.Errorf("set merge rules: %w", err)
	}
	return nil
}

// idPair is a stable (source, identifier) handle — the key contact_links is
// journaled against.
type idPair struct {
	Source     string
	Identifier string
}

func lessPair(a, b idPair) bool {
	if a.Source != b.Source {
		return a.Source < b.Source
	}
	return a.Identifier < b.Identifier
}

// orderPair returns the two pairs in canonical order so a symmetric decision
// always writes the same row and collides on the UNIQUE constraint.
func orderPair(a, b idPair) (idPair, idPair) {
	if lessPair(a, b) {
		return a, b
	}
	return b, a
}

func pairKey(a, b idPair) string {
	lo, hi := orderPair(a, b)
	return lo.Source + "\x00" + lo.Identifier + "\x00" + hi.Source + "\x00" + hi.Identifier
}

// MergeCandidate is one suggested cross-source merge for the settings UI,
// enriched with display names on top of the pure contacts.Candidate.
type MergeCandidate struct {
	ContactA int64
	ContactB int64
	NameA    string
	NameB    string
	Reason   string
	Value    string
}

// MergeCandidates computes the suggested merges for the current archive under
// the persisted rules, consulting the resolver's address book only when it is
// available and enabled. Pairs deliberately kept apart by a manual split are
// excluded so the UI never re-suggests an undone merge. A nil resolver is
// treated as no address book (contacts.Unavailable).
func (s *Store) MergeCandidates(ctx context.Context, resolver contacts.Resolver) ([]MergeCandidate, error) {
	rules, err := s.GetMergeRules(ctx)
	if err != nil {
		return nil, err
	}
	stored, err := loadStoredIdentifiers(ctx, s.db)
	if err != nil {
		return nil, err
	}
	people := addressBookPeople(ctx, resolver, rules)
	cands := contacts.Candidates(stored, people, rules.match())
	if len(cands) == 0 {
		return nil, nil
	}
	splitSet, err := loadSplitSet(ctx, s.db)
	if err != nil {
		return nil, err
	}
	names, err := loadContactNames(ctx, s.db)
	if err != nil {
		return nil, err
	}
	out := make([]MergeCandidate, 0, len(cands))
	for _, c := range cands {
		blocked, err := splitBlocks(ctx, s.db, splitSet, c.ContactA, c.ContactB)
		if err != nil {
			return nil, err
		}
		if blocked {
			continue
		}
		out = append(out, MergeCandidate{
			ContactA: c.ContactA,
			ContactB: c.ContactB,
			NameA:    names[c.ContactA],
			NameB:    names[c.ContactB],
			Reason:   string(c.Reason),
			Value:    c.Value,
		})
	}
	return out, nil
}

// MergeContacts unions two contacts into one person (issue #11 / REQ-0015-005):
// it records the full bipartite pairing of their identifiers as manual merge
// decisions (so the merge survives re-ingest), repoints the loser's
// identifiers, conversations, and facts onto the deterministically-chosen
// winner (facts deduplicating via UNIQUE(contact_id, fact_hash)), and deletes
// the loser's contacts row. It returns the surviving (winner) contact id.
//
// The winner is chosen by the ordered rule (ADR-0022 / REQ-0015-007): the
// contact with a user-meaningful display_name (differs from all its
// identifiers) wins; otherwise the lower id wins. Merging a pair that was
// previously split replaces the split records — the latest manual action wins.
func (s *Store) MergeContacts(ctx context.Context, a, b int64) (int64, error) {
	if a == b {
		return 0, fmt.Errorf("merge contacts: cannot merge a contact with itself (%d)", a)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()

	aIDs, err := contactPairs(ctx, tx, a)
	if err != nil {
		return 0, err
	}
	bIDs, err := contactPairs(ctx, tx, b)
	if err != nil {
		return 0, err
	}
	if len(aIDs) == 0 && len(bIDs) == 0 {
		return 0, fmt.Errorf("merge contacts: neither %d nor %d exists", a, b)
	}

	// Record the manual decision for every cross pair, replacing any prior
	// merge/split row on the same pair (latest manual action wins).
	for _, pa := range aIDs {
		for _, pb := range bIDs {
			if err := recordLink(ctx, tx, "merge", "manual", pa, pb, true); err != nil {
				return 0, err
			}
		}
	}

	winner, err := unionByRule(ctx, tx, a, b)
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	rollback = false
	return winner, nil
}

// SplitContact separates the chosen identifiers off contactID onto a fresh
// contact (issue #11 / REQ-0015-006). The moved identifiers' conversations
// follow them to the new contact, and every moved↔remaining identifier pair is
// recorded as a manual split decision so auto-match and any stored merge never
// re-unite them (precedence: manual split wins). It returns the new contact id.
//
// moved must be a non-empty proper subset of contactID's identifiers — at least
// one identifier must remain, otherwise there is nothing to split apart.
func (s *Store) SplitContact(ctx context.Context, contactID int64, moved []ContactIdentifier) (int64, error) {
	if len(moved) == 0 {
		return 0, fmt.Errorf("split contact %d: no identifiers to move", contactID)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()

	all, err := contactPairs(ctx, tx, contactID)
	if err != nil {
		return 0, err
	}
	if len(all) == 0 {
		return 0, fmt.Errorf("split contact %d: contact does not exist or has no identifiers", contactID)
	}

	// Validate that every moved identifier belongs to this contact, and compute
	// the remaining set.
	owned := make(map[idPair]bool, len(all))
	for _, p := range all {
		owned[p] = true
	}
	movedPairs := make([]idPair, 0, len(moved))
	movedSet := make(map[idPair]bool, len(moved))
	for _, m := range moved {
		p := idPair{Source: m.Source, Identifier: m.Identifier}
		if !owned[p] {
			return 0, fmt.Errorf("split contact %d: identifier %s/%s is not on this contact", contactID, m.Source, m.Identifier)
		}
		if !movedSet[p] {
			movedSet[p] = true
			movedPairs = append(movedPairs, p)
		}
	}
	var remaining []idPair
	for _, p := range all {
		if !movedSet[p] {
			remaining = append(remaining, p)
		}
	}
	if len(remaining) == 0 {
		return 0, fmt.Errorf("split contact %d: cannot move every identifier (nothing left to split from)", contactID)
	}

	// New contact, named after the first moved identifier (the auto-create
	// convention); the user can rename it later.
	res, err := tx.ExecContext(ctx, `INSERT INTO contacts(display_name) VALUES(?)`, movedPairs[0].Identifier)
	if err != nil {
		return 0, fmt.Errorf("split contact %d: create new contact: %w", contactID, err)
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	for _, p := range movedPairs {
		if _, err := tx.ExecContext(ctx,
			`UPDATE contact_identifiers SET contact_id = ? WHERE source = ? AND identifier = ? AND contact_id = ?`,
			newID, p.Source, p.Identifier, contactID); err != nil {
			return 0, fmt.Errorf("split contact %d: move identifier: %w", contactID, err)
		}
		// A 1:1 conversation's identity is (source, name) == the identifier, so it
		// follows its identifier to the new contact.
		if _, err := tx.ExecContext(ctx,
			`UPDATE conversations SET contact_id = ? WHERE source = ? AND name = ? AND contact_id = ?`,
			newID, p.Source, p.Identifier, contactID); err != nil {
			return 0, fmt.Errorf("split contact %d: move conversation: %w", contactID, err)
		}
	}

	// Record that every moved↔remaining pair must stay apart, replacing any
	// prior merge row on the same pair.
	for _, m := range movedPairs {
		for _, r := range remaining {
			if err := recordLink(ctx, tx, "split", "manual", m, r, true); err != nil {
				return 0, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	rollback = false
	return newID, nil
}

// ReconcileContacts is the idempotent decision-replay pass that runs after
// every import and on demand (issue #11 / REQ-0015-007). It (1) re-applies
// every stored merge decision whose two identifiers currently sit on different
// contacts, folding a re-ingested identity back onto its person, then (2), if
// auto-merge is enabled, applies exact-normalized-identifier merges on the
// trusted kinds — skipping any pair a manual split keeps apart and recording
// each applied auto-merge as a durable 'auto' decision so it, too, survives the
// next churn. Running it twice is a no-op by construction. A nil resolver means
// no address book (contacts.Unavailable); the pass performs no network I/O.
func (s *Store) ReconcileContacts(ctx context.Context, resolver contacts.Resolver) error {
	rules, err := s.GetMergeRules(ctx)
	if err != nil {
		return err
	}
	// The resolver is accepted for API symmetry with MergeCandidates and the
	// on-demand settings path, but reconcile deliberately does not consult the
	// address book: hints only ever *suggest* (ADR-0022), they never
	// auto-merge, so no address-book snapshot can change reconcile's outcome.
	_ = resolver

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	rollback := true
	defer func() {
		if rollback {
			_ = tx.Rollback()
		}
	}()

	// Step 1: re-apply stored merge decisions.
	mergeLinks, err := loadMergeLinks(ctx, tx)
	if err != nil {
		return err
	}
	for _, ml := range mergeLinks {
		ca, err := ownerOf(ctx, tx, ml[0])
		if err != nil {
			return err
		}
		cb, err := ownerOf(ctx, tx, ml[1])
		if err != nil {
			return err
		}
		if ca == 0 || cb == 0 || ca == cb {
			continue // an identifier is absent (inert link) or already merged
		}
		if _, err := unionByRule(ctx, tx, ca, cb); err != nil {
			return err
		}
	}

	// Step 2: opt-in auto-merge on exact normalized equality.
	if rules.AutoMerge {
		stored, err := loadStoredIdentifiers(ctx, tx)
		if err != nil {
			return err
		}
		splitSet, err := loadSplitSet(ctx, tx)
		if err != nil {
			return err
		}
		// Address-book reasons are filtered below, so no people are needed.
		cands := contacts.Candidates(stored, nil, rules.match())
		remap := map[int64]int64{}
		resolve := func(id int64) int64 {
			for {
				next, ok := remap[id]
				if !ok {
					return id
				}
				id = next
			}
		}
		for _, c := range cands {
			if c.Reason == contacts.ReasonAddressBook {
				continue // hints never auto-merge
			}
			a, b := resolve(c.ContactA), resolve(c.ContactB)
			if a == b {
				continue
			}
			blocked, err := splitBlocks(ctx, tx, splitSet, a, b)
			if err != nil {
				return err
			}
			if blocked {
				continue // manual split takes precedence over auto rules
			}
			// Record the applied auto-merge (bipartite, non-replacing so a manual
			// decision is never clobbered) so it survives re-ingest.
			aIDs, err := contactPairs(ctx, tx, a)
			if err != nil {
				return err
			}
			bIDs, err := contactPairs(ctx, tx, b)
			if err != nil {
				return err
			}
			for _, pa := range aIDs {
				for _, pb := range bIDs {
					if err := recordLink(ctx, tx, "merge", "auto", pa, pb, false); err != nil {
						return err
					}
				}
			}
			winner, err := unionByRule(ctx, tx, a, b)
			if err != nil {
				return err
			}
			loser := a
			if winner == a {
				loser = b
			}
			remap[loser] = winner
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	rollback = false
	return nil
}

// --- helpers -------------------------------------------------------------

// addressBookPeople returns the resolver's people when it is available and the
// rules enable it, and nil otherwise. A resolver error is swallowed (an absent
// or permission-denied book is never an error per the seam contract).
func addressBookPeople(ctx context.Context, resolver contacts.Resolver, rules MergeRules) []contacts.Person {
	if resolver == nil || !rules.UseAddressBook {
		return nil
	}
	if resolver.Availability(ctx) != contacts.Available {
		return nil
	}
	people, err := resolver.People(ctx)
	if err != nil {
		return nil
	}
	return people
}

func loadStoredIdentifiers(ctx context.Context, q querier) ([]contacts.StoredIdentifier, error) {
	rows, err := q.QueryContext(ctx, `SELECT contact_id, source, identifier FROM contact_identifiers`)
	if err != nil {
		return nil, fmt.Errorf("load stored identifiers: %w", err)
	}
	defer rows.Close()
	var out []contacts.StoredIdentifier
	for rows.Next() {
		var si contacts.StoredIdentifier
		if err := rows.Scan(&si.ContactID, &si.Source, &si.Raw); err != nil {
			return nil, err
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

func loadContactNames(ctx context.Context, q querier) (map[int64]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, display_name FROM contacts`)
	if err != nil {
		return nil, fmt.Errorf("load contact names: %w", err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

// loadMergeLinks returns every stored merge decision as an ordered pair of
// idPairs.
func loadMergeLinks(ctx context.Context, q querier) ([][2]idPair, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT source_a, identifier_a, source_b, identifier_b FROM contact_links WHERE kind = 'merge'`)
	if err != nil {
		return nil, fmt.Errorf("load merge links: %w", err)
	}
	defer rows.Close()
	var out [][2]idPair
	for rows.Next() {
		var a, b idPair
		if err := rows.Scan(&a.Source, &a.Identifier, &b.Source, &b.Identifier); err != nil {
			return nil, err
		}
		out = append(out, [2]idPair{a, b})
	}
	return out, rows.Err()
}

// loadSplitSet returns the set of canonical pair keys carrying a split
// decision, for O(1) precedence checks.
func loadSplitSet(ctx context.Context, q querier) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT source_a, identifier_a, source_b, identifier_b FROM contact_links WHERE kind = 'split'`)
	if err != nil {
		return nil, fmt.Errorf("load split set: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var a, b idPair
		if err := rows.Scan(&a.Source, &a.Identifier, &b.Source, &b.Identifier); err != nil {
			return nil, err
		}
		out[pairKey(a, b)] = true
	}
	return out, rows.Err()
}

// splitBlocks reports whether any identifier of contact a and any identifier of
// contact b are kept apart by a manual split decision.
func splitBlocks(ctx context.Context, q querier, splitSet map[string]bool, a, b int64) (bool, error) {
	if len(splitSet) == 0 {
		return false, nil
	}
	aIDs, err := contactPairs(ctx, q, a)
	if err != nil {
		return false, err
	}
	bIDs, err := contactPairs(ctx, q, b)
	if err != nil {
		return false, err
	}
	for _, pa := range aIDs {
		for _, pb := range bIDs {
			if splitSet[pairKey(pa, pb)] {
				return true, nil
			}
		}
	}
	return false, nil
}

// contactPairs returns a contact's (source, identifier) handles.
func contactPairs(ctx context.Context, q querier, contactID int64) ([]idPair, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT source, identifier FROM contact_identifiers WHERE contact_id = ? ORDER BY source, identifier`,
		contactID)
	if err != nil {
		return nil, fmt.Errorf("contact identifiers: %w", err)
	}
	defer rows.Close()
	var out []idPair
	for rows.Next() {
		var p idPair
		if err := rows.Scan(&p.Source, &p.Identifier); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ownerOf returns the contact id currently holding a (source, identifier), or 0
// when the identifier is absent (an inert link).
func ownerOf(ctx context.Context, q querier, p idPair) (int64, error) {
	var id int64
	err := q.QueryRowContext(ctx,
		`SELECT contact_id FROM contact_identifiers WHERE source = ? AND identifier = ?`,
		p.Source, p.Identifier).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("owner of %s/%s: %w", p.Source, p.Identifier, err)
	}
	return id, nil
}

// recordLink writes (or replaces) the one current decision for a pair. When
// replace is true (manual actions) the latest decision overwrites any prior
// merge/split row on the same pair; when false (auto rules) an existing
// decision is preserved, so a manual decision is never clobbered by auto.
func recordLink(ctx context.Context, tx *sql.Tx, kind, origin string, a, b idPair, replace bool) error {
	lo, hi := orderPair(a, b)
	now := time.Now().UTC().Format(time.RFC3339)
	onConflict := `DO NOTHING`
	if replace {
		onConflict = `DO UPDATE SET kind=excluded.kind, origin=excluded.origin, created_at=excluded.created_at`
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO contact_links (kind, origin, source_a, identifier_a, source_b, identifier_b, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(source_a, identifier_a, source_b, identifier_b) `+onConflict,
		kind, origin, lo.Source, lo.Identifier, hi.Source, hi.Identifier, now)
	if err != nil {
		return fmt.Errorf("record %s link: %w", kind, err)
	}
	return nil
}

// unionByRule picks the deterministic winner between two contacts and unions
// the loser into it, returning the winner id.
func unionByRule(ctx context.Context, tx *sql.Tx, a, b int64) (int64, error) {
	aName, aIDs, err := loadContactRow(ctx, tx, a)
	if err != nil {
		return 0, err
	}
	bName, bIDs, err := loadContactRow(ctx, tx, b)
	if err != nil {
		return 0, err
	}
	winner, loser := pickWinner(a, aName, aIDs, b, bName, bIDs)
	if err := unionContacts(ctx, tx, winner, loser); err != nil {
		return 0, err
	}
	return winner, nil
}

// unionContacts repoints the loser's identifiers, conversations, and facts onto
// the winner and deletes the loser. Facts move with UPDATE OR IGNORE so a
// duplicate (contact_id, fact_hash) is left on the loser and cascades away when
// its contacts row is deleted (ON DELETE CASCADE, schema v4).
func unionContacts(ctx context.Context, tx *sql.Tx, winner, loser int64) error {
	if winner == loser {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE contact_identifiers SET contact_id = ? WHERE contact_id = ?`, winner, loser); err != nil {
		return fmt.Errorf("union: repoint identifiers: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE conversations SET contact_id = ? WHERE contact_id = ?`, winner, loser); err != nil {
		return fmt.Errorf("union: repoint conversations: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE OR IGNORE contact_facts SET contact_id = ? WHERE contact_id = ?`, winner, loser); err != nil {
		return fmt.Errorf("union: repoint facts: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM contacts WHERE id = ?`, loser); err != nil {
		return fmt.Errorf("union: delete loser: %w", err)
	}
	return nil
}

func loadContactRow(ctx context.Context, tx *sql.Tx, id int64) (string, []idPair, error) {
	var name string
	err := tx.QueryRowContext(ctx, `SELECT display_name FROM contacts WHERE id = ?`, id).Scan(&name)
	if err == sql.ErrNoRows {
		return "", nil, fmt.Errorf("contact %d not found", id)
	}
	if err != nil {
		return "", nil, fmt.Errorf("load contact %d: %w", id, err)
	}
	ids, err := contactPairs(ctx, tx, id)
	if err != nil {
		return "", nil, err
	}
	return name, ids, nil
}

// pickWinner implements the deterministic winner rule (ADR-0022 /
// REQ-0015-007): if exactly one contact has a user-meaningful display_name
// (differs from all its identifiers), it wins; otherwise the lower id wins.
func pickWinner(aID int64, aName string, aIDs []idPair, bID int64, bName string, bIDs []idPair) (winner, loser int64) {
	am := isMeaningfulName(aName, aIDs)
	bm := isMeaningfulName(bName, bIDs)
	switch {
	case am && !bm:
		return aID, bID
	case bm && !am:
		return bID, aID
	default:
		if aID < bID {
			return aID, bID
		}
		return bID, aID
	}
}

// isMeaningfulName reports whether a display name looks user-curated rather
// than auto-created: non-empty and not equal to any of the contact's
// identifiers (auto-creation sets display_name to the identifier).
func isMeaningfulName(name string, ids []idPair) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, p := range ids {
		if p.Identifier == name {
			return false
		}
	}
	return true
}
