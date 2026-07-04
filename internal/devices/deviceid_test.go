// Tests for Syncthing device-ID validation (issue #157 Security Checklist:
// "device IDs validated as Syncthing device-ID format").
//
// Governing: SPEC-0014 REQ "Pairing via Device ID and QR", §Trust Model.
package devices

import (
	"errors"
	"strings"
	"testing"
)

// docsDeviceID is the canonical example device ID from Syncthing's own
// documentation — a real, check-digit-valid ID, so it pins our Luhn mod-32
// implementation to upstream's.
const docsDeviceID = "P56IOI7-MZJNU2Y-IQGDREY-DM2MGTI-MGL3BXN-PQ6W5BM-TBBZ4TJ-XZWICQ2"

// generatedDeviceIDs are IDs produced by the same base32+Luhn construction
// Syncthing uses (SHA-256 → base32 → 4×13 groups + check chars).
var generatedDeviceIDs = []string{
	"XW4UY46-VHRCAEN-OTRLIUX-BIIMJVP-KPVFKQW-4H5TU2H-MYSYKFX-S53S7AL",
	"AL4V3SV-WOXMPPL-7OSHTP5-YBPGQTN-6CBXKHB-D5DWSIJ-563UQMW-5JXZFAO",
	"QRUVHQ4-LQMFCKZ-JPKWU3L-TJNB6NX-XZXB2AV-FLJ5RL4-DC2QFCT-EBHK5AG",
}

func TestCanonicalDeviceIDAcceptsRealIDs(t *testing.T) {
	for _, id := range append([]string{docsDeviceID}, generatedDeviceIDs...) {
		got, err := CanonicalDeviceID(id)
		if err != nil {
			t.Errorf("CanonicalDeviceID(%q) = %v, want ok", id, err)
			continue
		}
		if got != id {
			t.Errorf("CanonicalDeviceID(%q) = %q, want unchanged", id, got)
		}
	}
}

// TestCanonicalDeviceIDNormalizes: the transcription variants Syncthing
// itself tolerates — lowercase, missing dashes, whitespace, and the 0→O,
// 1→I, 8→B look-alikes — all canonicalize to the dashed uppercase form.
func TestCanonicalDeviceIDNormalizes(t *testing.T) {
	variants := []string{
		strings.ToLower(docsDeviceID),
		strings.ReplaceAll(docsDeviceID, "-", ""),
		strings.ReplaceAll(docsDeviceID, "-", " "),
		" " + docsDeviceID + "\n",
		// Look-alike typos: O→0, I→1, B→8 in the input must map back.
		strings.NewReplacer("O", "0", "I", "1", "B", "8").Replace(docsDeviceID),
	}
	for _, v := range variants {
		got, err := CanonicalDeviceID(v)
		if err != nil {
			t.Errorf("CanonicalDeviceID(%q) = %v, want ok", v, err)
			continue
		}
		if got != docsDeviceID {
			t.Errorf("CanonicalDeviceID(%q) = %q, want %q", v, got, docsDeviceID)
		}
	}
}

// TestCanonicalDeviceIDRejectsMalformed: wrong length, foreign characters,
// and corrupted check digits are all typed rejections — nothing malformed
// reaches the daemon config or the registry.
func TestCanonicalDeviceIDRejectsMalformed(t *testing.T) {
	bad := []string{
		"",
		"SHORT",
		strings.Repeat("A", 55) + "B",            // right length, wrong final check digit
		docsDeviceID[:len(docsDeviceID)-1] + "3", // one corrupted check char
		strings.ReplaceAll(docsDeviceID, "P", "!"),
		docsDeviceID + "AAAAAAA", // too long
	}
	for _, v := range bad {
		if _, err := CanonicalDeviceID(v); !errors.Is(err, ErrInvalidDeviceID) {
			t.Errorf("CanonicalDeviceID(%q) = %v, want ErrInvalidDeviceID", v, err)
		}
	}
}

// TestCanonicalDeviceIDRejectsCorruptedDataChar: flipping a DATA character
// (not just a check char) breaks the group's Luhn check.
func TestCanonicalDeviceIDRejectsCorruptedDataChar(t *testing.T) {
	// First char P → Q flips a data character in group 1.
	corrupted := "Q" + docsDeviceID[1:]
	if _, err := CanonicalDeviceID(corrupted); !errors.Is(err, ErrInvalidDeviceID) {
		t.Errorf("corrupted data char accepted: %v", err)
	}
}

func TestShortDeviceID(t *testing.T) {
	if got := ShortDeviceID(docsDeviceID); got != "P56IOI7" {
		t.Errorf("ShortDeviceID = %q, want P56IOI7", got)
	}
	if got := ShortDeviceID("nodash"); got != "nodash" {
		t.Errorf("ShortDeviceID(nodash) = %q, want passthrough", got)
	}
}
