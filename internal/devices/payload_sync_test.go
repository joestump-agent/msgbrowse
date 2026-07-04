// Tests for the version-2 (device-ID) pairing payload: encode/decode
// round-trips across all three presentations, the version gate, unknown-field
// rejection, and field hygiene — the #104 payload test shape repurposed for
// SPEC-0014.
//
// Governing: ADR-0021, SPEC-0014 REQ "Pairing via Device ID and QR" ("a QR
// code and … a copyable manual code carrying the same fields", "the payload
// MUST NOT contain a secret token").
package devices

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const (
	syncSelfID = "QRUVHQ4-LQMFCKZ-JPKWU3L-TJNB6NX-XZXB2AV-FLJ5RL4-DC2QFCT-EBHK5AG"
	syncPeerID = "XW4UY46-VHRCAEN-OTRLIUX-BIIMJVP-KPVFKQW-4H5TU2H-MYSYKFX-S53S7AL"
)

func newTestSyncPayload(t *testing.T) *SyncPayload {
	t.Helper()
	p, err := NewSyncPayload(syncSelfID, []string{"msgbrowse-signal", "msgbrowse-imessage"}, "studio-mac")
	if err != nil {
		t.Fatalf("NewSyncPayload: %v", err)
	}
	return p
}

// TestSyncPayloadQRRoundTrip: the QR bytes are compact JSON carrying exactly
// the four public fields, and decode back to an identical payload.
func TestSyncPayloadQRRoundTrip(t *testing.T) {
	p := newTestSyncPayload(t)
	qr, err := p.EncodeQR()
	if err != nil {
		t.Fatalf("EncodeQR: %v", err)
	}

	// Wire shape: version 2, deviceID, folders, name — and nothing else
	// (there is no token or fingerprint field to leak).
	var wire map[string]any
	if err := json.Unmarshal(qr, &wire); err != nil {
		t.Fatalf("QR bytes are not JSON: %v", err)
	}
	for _, forbidden := range []string{"token", "fp", "endpoint"} {
		if _, ok := wire[forbidden]; ok {
			t.Errorf("v2 payload carries retired v1 field %q", forbidden)
		}
	}
	if wire["v"] != float64(2) || wire["deviceID"] != syncSelfID {
		t.Errorf("wire = %v; want v=2 and the canonical device ID", wire)
	}

	got, err := DecodeSyncPayload(qr)
	if err != nil {
		t.Fatalf("DecodeSyncPayload(QR): %v", err)
	}
	if got.DeviceID != p.DeviceID || got.Name != p.Name || len(got.Folders) != 2 {
		t.Errorf("round-trip = %+v, want %+v", got, p)
	}
}

// TestSyncPayloadManualCodeRoundTrip: the manual code is the self-identifying
// MSGB2. presentation of the same fields, whitespace-tolerant on decode.
func TestSyncPayloadManualCodeRoundTrip(t *testing.T) {
	p := newTestSyncPayload(t)
	code, err := p.EncodeManualCode()
	if err != nil {
		t.Fatalf("EncodeManualCode: %v", err)
	}
	if !strings.HasPrefix(code, SyncManualCodePrefix) {
		t.Fatalf("manual code %q missing the %q prefix", code, SyncManualCodePrefix)
	}
	got, err := DecodeSyncPayload([]byte("  " + code + "\n"))
	if err != nil {
		t.Fatalf("DecodeSyncPayload(manual): %v", err)
	}
	if got.DeviceID != p.DeviceID || got.Name != p.Name {
		t.Errorf("manual round-trip = %+v, want %+v", got, p)
	}
}

// TestDecodeSyncPayloadBareDeviceID: pasting a bare device ID (what
// Syncthing's own UI shows) synthesizes a payload with no folder
// introduction — the SPEC-0014 manual device-ID entry path.
func TestDecodeSyncPayloadBareDeviceID(t *testing.T) {
	got, err := DecodeSyncPayload([]byte(strings.ToLower(syncPeerID)))
	if err != nil {
		t.Fatalf("DecodeSyncPayload(bare id): %v", err)
	}
	if got.DeviceID != syncPeerID {
		t.Errorf("DeviceID = %q, want canonicalized %q", got.DeviceID, syncPeerID)
	}
	if got.Version != SyncPayloadVersion || len(got.Folders) != 0 || got.Name != "" {
		t.Errorf("bare-ID payload = %+v, want empty introduction", got)
	}
}

// TestDecodeSyncPayloadRejects: version gate, unknown fields, malformed
// device IDs, oversized names, and hostile folder ids are all typed
// ErrInvalidSyncPayload rejections.
func TestDecodeSyncPayloadRejects(t *testing.T) {
	cases := map[string]string{
		"v1 payload":        `{"v":1,"endpoint":"10.0.0.1:8788","token":"x","fp":"aa"}`,
		"wrong version":     `{"v":3,"deviceID":"` + syncSelfID + `"}`,
		"unknown field":     `{"v":2,"deviceID":"` + syncSelfID + `","token":"sneaky"}`,
		"bad device id":     `{"v":2,"deviceID":"NOT-A-DEVICE-ID"}`,
		"empty":             "",
		"garbage":           "not json and not an id",
		"hostile folder id": `{"v":2,"deviceID":"` + syncSelfID + `","folders":["../etc"]}`,
		"folder id case":    `{"v":2,"deviceID":"` + syncSelfID + `","folders":["MsgBrowse-Signal"]}`,
		"oversized name":    `{"v":2,"deviceID":"` + syncSelfID + `","name":"` + strings.Repeat("n", 200) + `"}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeSyncPayload([]byte(in)); !errors.Is(err, ErrInvalidSyncPayload) {
				t.Errorf("DecodeSyncPayload(%s) = %v, want ErrInvalidSyncPayload", name, err)
			}
		})
	}
}

// TestNewSyncPayloadCanonicalizes: a transcribed device ID is canonicalized
// at construction, so the QR always carries the dashed uppercase form.
func TestNewSyncPayloadCanonicalizes(t *testing.T) {
	p, err := NewSyncPayload(strings.ToLower(strings.ReplaceAll(syncSelfID, "-", "")), nil, "")
	if err != nil {
		t.Fatalf("NewSyncPayload: %v", err)
	}
	if p.DeviceID != syncSelfID {
		t.Errorf("DeviceID = %q, want %q", p.DeviceID, syncSelfID)
	}
}
