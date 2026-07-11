package contacts

import (
	"testing"
)

func TestPhonesMatch(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		// Identical shapes.
		{"+15551234567", "+15551234567", true},
		{"5551234567", "5551234567", true},
		// National vs international of the same number (the one cross-shape rule).
		{"+15551234567", "5551234567", true},
		{"5551234567", "+15551234567", true},
		// International with a 2-digit country code prefix over the same tail.
		{"+445551234567", "5551234567", true}, // intl digits 445551234567, national 5551234567 is the trailing suffix (diff=2)
		// Different numbers do not match.
		{"+15551234567", "+15559999999", false},
		{"5551234567", "5559999999", false},
		// Two national numbers sharing a suffix must NOT match (no country-code guess).
		{"5551234567", "1234567", false},
		// International vs national where the tail does not line up.
		{"+15551234567", "9995551234567", false}, // both would be intl-vs-national? second is national 13 digits, diff negative
	}
	for _, c := range cases {
		if got := phonesMatch(c.a, c.b); got != c.want {
			t.Errorf("phonesMatch(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestPhonesMatchCountryCode(t *testing.T) {
	// +44 20 7946 0958 (UK) national 2079460958 (10) vs +442079460958 (12): diff 2.
	if !phonesMatch("+442079460958", "2079460958") {
		t.Error("UK national should match its international form")
	}
	// A 4-digit prefix difference is beyond a plausible country code -> no match.
	if phonesMatch("+9999"+"5551234567", "5551234567") {
		t.Error("4-digit prefix difference must not match")
	}
}

func TestCandidatesPhoneCrossSource(t *testing.T) {
	// signal contact 1 holds a formatted number; imessage contact 2 holds the
	// E.164 form of the same number.
	stored := []StoredIdentifier{
		{ContactID: 1, Source: "signal", Raw: "+1 (555) 123-4567"},
		{ContactID: 2, Source: "imessage", Raw: "+15551234567"},
	}
	got := Candidates(stored, nil, MatchRules{MatchPhone: true, MatchEmail: true})
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	c := got[0]
	if c.ContactA != 1 || c.ContactB != 2 {
		t.Errorf("candidate contacts = (%d,%d), want (1,2)", c.ContactA, c.ContactB)
	}
	if c.Reason != ReasonPhone {
		t.Errorf("reason = %q, want phone", c.Reason)
	}
	if c.Value != "+15551234567" {
		t.Errorf("value = %q, want the E.164 form", c.Value)
	}
}

func TestCandidatesEmail(t *testing.T) {
	stored := []StoredIdentifier{
		{ContactID: 1, Source: "signal", Raw: "MJ@Example.com"},
		{ContactID: 2, Source: "imessage", Raw: "mj@example.com"},
		{ContactID: 3, Source: "whatsapp", Raw: "someone@else.com"},
	}
	got := Candidates(stored, nil, MatchRules{MatchPhone: true, MatchEmail: true})
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	if got[0].Reason != ReasonEmail || got[0].Value != "mj@example.com" {
		t.Errorf("candidate = %+v, want email mj@example.com", got[0])
	}
}

func TestCandidatesRespectsTrustedKinds(t *testing.T) {
	stored := []StoredIdentifier{
		{ContactID: 1, Source: "signal", Raw: "+15551234567"},
		{ContactID: 2, Source: "imessage", Raw: "+15551234567"},
		{ContactID: 3, Source: "signal", Raw: "a@b.com"},
		{ContactID: 4, Source: "imessage", Raw: "a@b.com"},
	}
	// Phone off: only the email pair survives.
	got := Candidates(stored, nil, MatchRules{MatchPhone: false, MatchEmail: true})
	if len(got) != 1 || got[0].Reason != ReasonEmail {
		t.Fatalf("phone-off = %+v, want only the email candidate", got)
	}
	// Email off: only the phone pair survives.
	got = Candidates(stored, nil, MatchRules{MatchPhone: true, MatchEmail: false})
	if len(got) != 1 || got[0].Reason != ReasonPhone {
		t.Fatalf("email-off = %+v, want only the phone candidate", got)
	}
	// Both off: nothing.
	if got := Candidates(stored, nil, MatchRules{}); len(got) != 0 {
		t.Fatalf("all-off = %+v, want none", got)
	}
}

func TestCandidatesSameContactNoSelfPair(t *testing.T) {
	// One contact holding two forms of its own number must not pair with itself.
	stored := []StoredIdentifier{
		{ContactID: 1, Source: "signal", Raw: "+15551234567"},
		{ContactID: 1, Source: "imessage", Raw: "5551234567"},
	}
	if got := Candidates(stored, nil, MatchRules{MatchPhone: true}); len(got) != 0 {
		t.Fatalf("self-pair produced %+v, want none", got)
	}
}

func TestCandidatesAddressBookGrouping(t *testing.T) {
	// Two contacts with unrelated-looking handles that the address book groups
	// onto one person via two different identifiers.
	stored := []StoredIdentifier{
		{ContactID: 1, Source: "signal", Raw: "+15551234567"},
		{ContactID: 2, Source: "imessage", Raw: "mj@example.com"},
	}
	people := []Person{{
		DisplayName: "Mary Jane",
		Identifiers: []Identifier{
			{Kind: KindPhone, Value: "+15551234567"},
			{Kind: KindEmail, Value: "mj@example.com"},
		},
	}}
	// Without the address book, no candidate (different kinds, no shared value).
	if got := Candidates(stored, people, MatchRules{MatchPhone: true, MatchEmail: true, UseAddressBook: false}); len(got) != 0 {
		t.Fatalf("address-book off = %+v, want none", got)
	}
	got := Candidates(stored, people, MatchRules{MatchPhone: true, MatchEmail: true, UseAddressBook: true})
	if len(got) != 1 || got[0].Reason != ReasonAddressBook || got[0].Value != "Mary Jane" {
		t.Fatalf("address-book on = %+v, want one address-book candidate for Mary Jane", got)
	}
}

func TestCandidatesReasonPriority(t *testing.T) {
	// A pair grouped by BOTH a shared phone and an address-book person is
	// reported as the stronger (phone) reason, once.
	stored := []StoredIdentifier{
		{ContactID: 1, Source: "signal", Raw: "+15551234567"},
		{ContactID: 2, Source: "imessage", Raw: "+15551234567"},
	}
	people := []Person{{
		DisplayName: "Mary Jane",
		Identifiers: []Identifier{{Kind: KindPhone, Value: "+15551234567"}},
	}}
	got := Candidates(stored, people, MatchRules{MatchPhone: true, UseAddressBook: true})
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1: %+v", len(got), got)
	}
	if got[0].Reason != ReasonPhone {
		t.Errorf("reason = %q, want phone (stronger than address-book)", got[0].Reason)
	}
}

func TestCandidatesDeterministicOrder(t *testing.T) {
	stored := []StoredIdentifier{
		{ContactID: 3, Source: "signal", Raw: "a@b.com"},
		{ContactID: 1, Source: "imessage", Raw: "a@b.com"},
		{ContactID: 2, Source: "whatsapp", Raw: "a@b.com"},
	}
	got := Candidates(stored, nil, MatchRules{MatchEmail: true})
	// 3 contacts sharing one value => 3 pairs, sorted (1,2),(1,3),(2,3).
	want := [][2]int64{{1, 2}, {1, 3}, {2, 3}}
	if len(got) != len(want) {
		t.Fatalf("got %d candidates, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i].ContactA != w[0] || got[i].ContactB != w[1] {
			t.Errorf("candidate %d = (%d,%d), want (%d,%d)", i, got[i].ContactA, got[i].ContactB, w[0], w[1])
		}
	}
}
