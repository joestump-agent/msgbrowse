// Syncthing device-ID validation for the SPEC-0014 pairing payload. A device
// ID is the SHA-256 of a device's TLS certificate, rendered as 52 base32
// characters with four Luhn mod-32 check characters interleaved (one per
// 13-character group) and displayed as eight dash-separated groups of seven.
// The ID is a PUBLIC identifier — validating it here is input hygiene
// (issue #157 Security Checklist: "device IDs validated as Syncthing
// device-ID format"), not secrecy: possession of a device ID grants nothing
// until the peer is accepted on both ends.
//
// The normalization mirrors Syncthing's own tolerance for human transcription
// (lowercase, spaces, missing dashes, and the 0→O / 1→I / 8→B look-alike
// substitutions), so a manually typed ID canonicalizes to the same string the
// daemon reports.
//
// Governing: ADR-0021 ("pairing is a device-ID QR"), SPEC-0014 REQ "Pairing
// via Device ID and QR", SPEC-0014 §Trust Model.
package devices

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidDeviceID reports a string that is not a valid Syncthing device ID
// (wrong length, character set, or Luhn check failure).
var ErrInvalidDeviceID = errors.New("devices: not a valid syncthing device ID")

// deviceIDAlphabet is the base32 alphabet (RFC 4648) Syncthing encodes device
// IDs and their Luhn mod-32 check characters with.
const deviceIDAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"

// deviceIDLen is the character count of a device ID once dashes are stripped:
// 52 data characters (base32 of a SHA-256) plus 4 check characters.
const deviceIDLen = 56

// CanonicalDeviceID validates s as a Syncthing device ID and returns it in
// canonical presentation: uppercase, eight dash-separated groups of seven
// characters (e.g. "P56IOI7-MZJNU2Y-…"). It tolerates the transcription
// variants Syncthing itself accepts — lowercase, embedded spaces, missing or
// misplaced dashes, and the 0→O, 1→I, 8→B look-alike typos — and rejects
// anything whose length, alphabet, or Luhn mod-32 check characters do not
// hold, so no malformed identifier reaches the daemon config or the database.
func CanonicalDeviceID(s string) (string, error) {
	clean := normalizeDeviceID(s)
	if len(clean) != deviceIDLen {
		return "", fmt.Errorf("%w: %d significant characters, want %d", ErrInvalidDeviceID, len(clean), deviceIDLen)
	}
	// Four groups of 13 data characters, each followed by its Luhn mod-32
	// check character (Syncthing lib/protocol's check layout).
	for g := 0; g < 4; g++ {
		grp := clean[g*14 : (g+1)*14]
		check, err := luhn32(grp[:13])
		if err != nil {
			return "", err
		}
		if check != grp[13] {
			return "", fmt.Errorf("%w: check character %d/4 mismatch", ErrInvalidDeviceID, g+1)
		}
	}
	var b strings.Builder
	b.Grow(deviceIDLen + 7)
	for i := 0; i < 8; i++ {
		if i > 0 {
			b.WriteByte('-')
		}
		b.WriteString(clean[i*7 : (i+1)*7])
	}
	return b.String(), nil
}

// ShortDeviceID returns the conventional short form of a canonical device ID —
// its first dash-separated group — for compact display beside a device name.
// It never truncates mid-group; a malformed input is returned unchanged.
func ShortDeviceID(id string) string {
	if i := strings.IndexByte(id, '-'); i > 0 {
		return id[:i]
	}
	return id
}

// normalizeDeviceID uppercases s, strips dashes and whitespace, and applies
// Syncthing's look-alike substitutions (0→O, 1→I, 8→B) so hand-typed IDs
// survive transcription. It does not validate — CanonicalDeviceID does.
func normalizeDeviceID(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToUpper(s) {
		switch r {
		case '-', ' ', '\t', '\n', '\r':
			continue
		case '0':
			r = 'O'
		case '1':
			r = 'I'
		case '8':
			r = 'B'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// luhn32 computes the Luhn mod-32 check character over s using the base32
// alphabet, exactly as Syncthing's lib/luhn does for device-ID check digits.
func luhn32(s string) (byte, error) {
	factor := 1
	sum := 0
	const n = 32
	for i := 0; i < len(s); i++ {
		codepoint := strings.IndexByte(deviceIDAlphabet, s[i])
		if codepoint < 0 {
			return 0, fmt.Errorf("%w: character %q outside the base32 alphabet", ErrInvalidDeviceID, s[i])
		}
		addend := factor * codepoint
		if factor == 2 {
			factor = 1
		} else {
			factor = 2
		}
		addend = (addend / n) + (addend % n)
		sum += addend
	}
	remainder := sum % n
	return deviceIDAlphabet[(n-remainder)%n], nil
}
