// Candidate matching for cross-provider contact merging (issue #11, part of
// epic #8). This is the pure-Go core the merge engine (internal/store) drives:
// given the archive's stored identifiers and — optionally — an address book's
// people, it groups the ones that belong to the same real person and explains
// why. It performs NO storage, NO I/O, and NO merging: it only ever *suggests*
// (ADR-0022 / SPEC-0015 REQ-0015-004), so it is trivially unit-testable without
// a database or a framework.
//
// Both sides canonicalize through the Normalize* helpers in this package, so
// equality is a byte comparison — with the one documented exception for
// KindPhone (see phonesMatch), which reconciles a national number against its
// international ("+"-prefixed) form by comparing the trailing subscriber
// digits.
package contacts

import "sort"

// StoredIdentifier is one archive-side handle owned by a contact — the input
// to candidate matching. Raw is the identifier exactly as it sits in
// contact_identifiers (un-normalized); the matcher canonicalizes it through
// Normalize, so the caller never pre-normalizes.
type StoredIdentifier struct {
	ContactID int64
	Source    string
	Raw       string
}

// MatchRules controls which identifier kinds candidate detection trusts and
// whether address-book grouping contributes. It mirrors the persisted
// contact_merge_rules but carries no storage concerns, so the matcher stays
// pure. Auto-merge is deliberately NOT here: matching only suggests, and
// applying auto-merges is the store/reconcile layer's job.
type MatchRules struct {
	// MatchPhone includes cross-source phone-number equality as a candidate
	// reason.
	MatchPhone bool
	// MatchEmail includes cross-source email equality as a candidate reason.
	MatchEmail bool
	// UseAddressBook lets an address-book person that carries two of the
	// archive's identifiers contribute a candidate. Ignored when no people are
	// supplied.
	UseAddressBook bool
}

// ReasonKind is the machine-readable explanation for a candidate — which kind
// of evidence grouped the two contacts.
type ReasonKind string

const (
	// ReasonPhone: the two contacts share a normalized phone number.
	ReasonPhone ReasonKind = "phone"
	// ReasonEmail: the two contacts share a normalized email address.
	ReasonEmail ReasonKind = "email"
	// ReasonAddressBook: an address-book person carries an identifier of each
	// contact, so the book groups them.
	ReasonAddressBook ReasonKind = "address-book"
)

// priority orders reasons when one pair is grouped by several kinds; the
// strongest (most specific) evidence is reported.
func (r ReasonKind) priority() int {
	switch r {
	case ReasonPhone:
		return 0
	case ReasonEmail:
		return 1
	case ReasonAddressBook:
		return 2
	default:
		return 3
	}
}

// Candidate is one suggested merge: two distinct contacts that the matcher
// believes are the same person, with a machine-readable reason and the value
// that grouped them. ContactA < ContactB always, so a pair is canonical and
// never duplicated with sides swapped.
type Candidate struct {
	ContactA int64
	ContactB int64
	Reason   ReasonKind
	// Value is the matched normalized identifier (ReasonPhone / ReasonEmail)
	// or the address-book person's display name (ReasonAddressBook).
	Value string
}

// Candidates groups the stored identifiers (optionally augmented by
// address-book people) into suggested merges. Each returned Candidate carries
// exactly one reason — the strongest, when a pair is grouped several ways — and
// the output is deterministic (sorted by contact ids). It never mutates its
// inputs and never merges anything.
//
// Address-book grouping is applied only when rules.UseAddressBook is set and
// people were supplied; on the no-op resolver (or Linux) people is empty and
// matching runs on stored identifiers alone, exactly as SPEC-0015 REQ-0015-001
// requires.
func Candidates(stored []StoredIdentifier, people []Person, rules MatchRules) []Candidate {
	// Canonicalize every stored identifier once; drop anything that is not an
	// identifier (blank / junk).
	type normStored struct {
		contactID int64
		id        Identifier
	}
	norms := make([]normStored, 0, len(stored))
	for _, s := range stored {
		id := Normalize(s.Raw)
		if id.IsZero() {
			continue
		}
		norms = append(norms, normStored{s.ContactID, id})
	}

	// best[pair] keeps the strongest reason seen for a canonical contact pair,
	// so a pair matched by both phone and address book is reported as a phone
	// candidate (the more specific evidence).
	type pair struct{ a, b int64 }
	best := map[pair]Candidate{}
	record := func(a, b int64, reason ReasonKind, value string) {
		if a == b {
			return
		}
		if a > b {
			a, b = b, a
		}
		p := pair{a, b}
		cand := Candidate{ContactA: a, ContactB: b, Reason: reason, Value: value}
		if prev, ok := best[p]; !ok || reason.priority() < prev.Reason.priority() {
			best[p] = cand
		}
	}

	// Email: exact equality after normalization groups all contacts sharing a
	// value.
	if rules.MatchEmail {
		byEmail := map[string][]int64{}
		for _, n := range norms {
			if n.id.Kind == KindEmail {
				byEmail[n.id.Value] = appendDistinct(byEmail[n.id.Value], n.contactID)
			}
		}
		for value, ids := range byEmail {
			for i := 0; i < len(ids); i++ {
				for j := i + 1; j < len(ids); j++ {
					record(ids[i], ids[j], ReasonEmail, value)
				}
			}
		}
	}

	// Phone: pairwise, because national and international shapes of one number
	// are byte-unequal but must still match (phonesMatch). Phone identifiers
	// number in the hundreds, so O(n^2) is trivial.
	if rules.MatchPhone {
		var phones []normStored
		for _, n := range norms {
			if n.id.Kind == KindPhone {
				phones = append(phones, n)
			}
		}
		for i := 0; i < len(phones); i++ {
			for j := i + 1; j < len(phones); j++ {
				if phones[i].contactID == phones[j].contactID {
					continue
				}
				if phonesMatch(phones[i].id.Value, phones[j].id.Value) {
					record(phones[i].contactID, phones[j].contactID, ReasonPhone, longerPhone(phones[i].id.Value, phones[j].id.Value))
				}
			}
		}
	}

	// Address book: a person carrying an identifier of two different contacts
	// groups them. Never auto-merges (that is enforced by the store), only
	// suggests.
	if rules.UseAddressBook && len(people) > 0 {
		for _, p := range people {
			var ids []int64
			for _, pid := range p.Identifiers {
				for _, n := range norms {
					if identifiersMatch(pid, n.id) {
						ids = appendDistinct(ids, n.contactID)
					}
				}
			}
			for i := 0; i < len(ids); i++ {
				for j := i + 1; j < len(ids); j++ {
					record(ids[i], ids[j], ReasonAddressBook, p.DisplayName)
				}
			}
		}
	}

	out := make([]Candidate, 0, len(best))
	for _, c := range best {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ContactA != out[j].ContactA {
			return out[i].ContactA < out[j].ContactA
		}
		if out[i].ContactB != out[j].ContactB {
			return out[i].ContactB < out[j].ContactB
		}
		return out[i].Reason < out[j].Reason
	})
	return out
}

// identifiersMatch reports whether two canonical identifiers denote the same
// handle: same Kind and equal value, with the one KindPhone cross-shape
// exception.
func identifiersMatch(a, b Identifier) bool {
	if a.Kind != b.Kind {
		return false
	}
	if a.Kind == KindPhone {
		return phonesMatch(a.Value, b.Value)
	}
	return a.Value == b.Value
}

// phonesMatch is the single documented cross-shape rule for KindPhone
// (resolver.go / normalize.go). Two canonical phone values match when their
// digits are identical, OR when one is national and the other international and
// the national digits are the trailing subscriber digits of the international
// form (i.e. the international value is the national value prefixed by a 1–3
// digit country code). It never re-normalizes; it only widens the compare, and
// it deliberately does NOT match two national numbers that merely share a
// suffix (that would silently merge different people).
func phonesMatch(a, b string) bool {
	intlA := len(a) > 0 && a[0] == '+'
	intlB := len(b) > 0 && b[0] == '+'
	da, db := a, b
	if intlA {
		da = a[1:]
	}
	if intlB {
		db = b[1:]
	}
	if da == db {
		return true
	}
	// Only a national/international pair may match cross-shape.
	if intlA == intlB {
		return false
	}
	natl, intl := da, db
	if intlA {
		natl, intl = db, da
	}
	diff := len(intl) - len(natl)
	if diff < 1 || diff > 3 {
		return false
	}
	return hasSuffix(intl, natl)
}

// longerPhone returns the international/longer of two matching phone values as
// the candidate's reported value, so the reason shows the fully-qualified form
// when one side has it.
func longerPhone(a, b string) string {
	if len(a) >= len(b) {
		return a
	}
	return b
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

func appendDistinct(ids []int64, v int64) []int64 {
	for _, x := range ids {
		if x == v {
			return ids
		}
	}
	return append(ids, v)
}
