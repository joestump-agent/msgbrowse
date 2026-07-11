package contacts

import "testing"

// TestNormalizePhone pins the E.164-ish canonical form: separators stripped,
// "00" → "+", no country-code guessing, and non-phone-shaped input rejected.
// Both the macOS provider (#10) and the merge engine (#11) canonicalize
// through this function, so these cases are the cross-team contract.
func TestNormalizePhone(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"already canonical international", "+15558675309", "+15558675309", true},
		{"us formatted", "+1 (555) 867-5309", "+15558675309", true},
		{"dots as separators", "555.867.5309", "5558675309", true},
		{"dashes national", "555-867-5309", "5558675309", true},
		{"double-zero international prefix", "0044 20 7946 0958", "+442079460958", true},
		{"surrounding whitespace", "  +1 555 867 5309  ", "+15558675309", true},
		{"slash separator", "089/12345678", "08912345678", true},
		{"non-breaking spaces", "+49\u00a089\u00a012345678", "+498912345678", true},
		{"bare national digits", "5558675309", "5558675309", true},
		{"fifteen digits max ok", "+123456789012345", "+123456789012345", true},

		{"empty", "", "", false},
		{"whitespace only", "   ", "", false},
		{"letters", "call me maybe", "", false},
		{"alphanumeric vanity", "1-800-FLOWERS", "", false},
		{"email shaped", "a@b.com", "", false},
		{"too short", "911", "", false},
		{"six digits still short", "123456", "", false},
		{"sixteen digits too long", "+1234567890123456", "", false},
		{"plus only", "+", "", false},
		{"internal plus", "555+8675309", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NormalizePhone(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("NormalizePhone(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestNormalizeEmail pins the email canonical form: trimmed, fully
// lowercased, exactly one @, non-empty local part, dotted domain.
func TestNormalizeEmail(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"simple", "alice@example.com", "alice@example.com", true},
		{"uppercase folded", "Alice@Example.COM", "alice@example.com", true},
		{"surrounding whitespace", "  bob@example.org ", "bob@example.org", true},
		{"plus tag preserved", "Bob+Tag@Example.com", "bob+tag@example.com", true},
		{"subdomain", "c@mail.example.co.uk", "c@mail.example.co.uk", true},

		{"empty", "", "", false},
		{"no at", "example.com", "", false},
		{"two ats", "a@b@example.com", "", false},
		{"empty local", "@example.com", "", false},
		{"empty domain", "alice@", "", false},
		{"dotless domain", "alice@localhost", "", false},
		{"leading dot domain", "alice@.example.com", "", false},
		{"trailing dot domain", "alice@example.com.", "", false},
		{"embedded space", "a lice@example.com", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := NormalizeEmail(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("NormalizeEmail(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestNormalize pins the classification entry point: email beats handle,
// phone beats handle, everything else is a trimmed handle with its case
// preserved, and blank input is the zero Identifier.
func TestNormalize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want Identifier
	}{
		{"phone", "+1 (555) 867-5309", Identifier{Kind: KindPhone, Value: "+15558675309"}},
		{"email", " Alice@Example.com ", Identifier{Kind: KindEmail, Value: "alice@example.com"}},
		{"signal username handle", "alice.42", Identifier{Kind: KindHandle, Value: "alice.42"}},
		{"handle case preserved", " CaseSensitive ", Identifier{Kind: KindHandle, Value: "CaseSensitive"}},
		{"malformed email stays handle", "a@b@c.com", Identifier{Kind: KindHandle, Value: "a@b@c.com"}},
		{"short digits stay handle", "911", Identifier{Kind: KindHandle, Value: "911"}},
		{"blank is zero", "   ", Identifier{}},
		{"empty is zero", "", Identifier{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Normalize(tc.in)
			if got != tc.want {
				t.Fatalf("Normalize(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}

	if !Normalize("").IsZero() {
		t.Fatal("Normalize(\"\") must be IsZero")
	}
	if Normalize("alice@example.com").IsZero() {
		t.Fatal("a real identifier must not be IsZero")
	}
}
