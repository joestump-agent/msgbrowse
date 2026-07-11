// Identifier model + normalization helpers shared by every address-book
// provider and the merge engine (issues #9/#10/#11). Both sides of a match —
// the provider's address-book values and the archive's contact_identifiers —
// canonicalize through THESE functions, so equality is a byte comparison and
// the rules live in exactly one place.
//
// The phone canonical form is deliberately "E.164-ish", not strict E.164:
// without a configured default region a national number ("555-867-5309")
// cannot be prefixed with a country code without guessing, and a wrong guess
// silently merges different people. So the canonical form preserves exactly
// what the input asserted — "+" + digits when an international prefix was
// present, bare digits otherwise — and the merge engine decides how boldly
// to match across the two shapes. (A decision the contact-merge ADR/spec
// should ratify.)
package contacts

import "strings"

// Kind classifies an identifier for matching: providers and the merge engine
// only compare values of the same Kind.
type Kind string

const (
	// KindPhone is a telephone number in the canonical NormalizePhone form.
	KindPhone Kind = "phone"
	// KindEmail is an email address in the canonical NormalizeEmail form.
	KindEmail Kind = "email"
	// KindHandle is an opaque service-specific handle (a Signal username, a
	// group id, …) that is neither phone- nor email-shaped. Canonical form is
	// the trimmed original — handles may be case-sensitive on their service,
	// so no case folding is applied.
	KindHandle Kind = "handle"
)

// Identifier is one classified, canonical identifier. The zero value (empty
// Kind and Value) means "not an identifier" — Normalize returns it for
// blank input, and Resolvers treat it as matching nothing.
type Identifier struct {
	Kind  Kind
	Value string
}

// IsZero reports whether the Identifier is the "not an identifier" zero
// value.
func (id Identifier) IsZero() bool { return id.Kind == "" && id.Value == "" }

// e164MaxDigits is the E.164 maximum significant digits; anything longer is
// not a phone number.
const e164MaxDigits = 15

// phoneMinDigits is the floor below which a digit string is not treated as a
// phone number (guards against zip codes, short numeric handles). Real
// subscriber numbers are ≥7 digits everywhere; SMS short codes are excluded
// on purpose — an address book never holds one.
const phoneMinDigits = 7

// NormalizePhone canonicalizes a phone number to its E.164-ish form:
// visual separators (spaces, dashes, dots, parentheses, slashes) are
// stripped, an international "00" prefix becomes "+", and the result is
// "+"+digits (international) or bare digits (national — no country code is
// ever guessed). ok is false when the input is not phone-shaped: any
// non-separator non-digit, or fewer than 7 / more than 15 digits.
func NormalizePhone(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}
	international := false
	switch {
	case strings.HasPrefix(s, "+"):
		international = true
		s = s[1:]
	case strings.HasPrefix(s, "00"):
		international = true
		s = s[2:]
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '.' || r == '(' || r == ')' || r == '/' || r == '\u00a0':
			// visual separator: dropped
		default:
			return "", false
		}
	}
	digits := b.String()
	if len(digits) < phoneMinDigits || len(digits) > e164MaxDigits {
		return "", false
	}
	if international {
		return "+" + digits, true
	}
	return digits, true
}

// NormalizeEmail canonicalizes an email address: trimmed and lowercased in
// full (the domain is case-insensitive by RFC; the local part is treated
// case-insensitively too, because every real-world provider folds it and a
// case-sensitive miss would silently split one person in two — the worse
// failure for merging). ok is false when the input is not email-shaped:
// not exactly one "@", an empty local part or domain, embedded whitespace,
// or a dotless domain.
func NormalizeEmail(raw string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" || strings.ContainsAny(s, " \t\n\r") {
		return "", false
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 || at != strings.LastIndexByte(s, '@') {
		return "", false
	}
	local, domain := s[:at], s[at+1:]
	if local == "" || domain == "" || !strings.Contains(domain, ".") ||
		strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") {
		return "", false
	}
	return s, true
}

// Normalize classifies a raw identifier string and returns its canonical
// Identifier: email-shaped input becomes KindEmail, phone-shaped input
// KindPhone, anything else a trimmed KindHandle. Blank input returns the
// zero Identifier. This is the single entry point providers and the merge
// engine use to canonicalize archive handles of unknown kind (iMessage
// mixes phones and emails; Signal mixes phones and usernames).
func Normalize(raw string) Identifier {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Identifier{}
	}
	if strings.ContainsRune(s, '@') {
		if email, ok := NormalizeEmail(s); ok {
			return Identifier{Kind: KindEmail, Value: email}
		}
		return Identifier{Kind: KindHandle, Value: s}
	}
	if phone, ok := NormalizePhone(s); ok {
		return Identifier{Kind: KindPhone, Value: phone}
	}
	return Identifier{Kind: KindHandle, Value: s}
}
