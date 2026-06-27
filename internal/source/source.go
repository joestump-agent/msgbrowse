// Package source defines the names of the message archive sources msgbrowse
// imports from. Every conversation, message, contact identifier, and ingest run
// is tagged with one of these strings so the unified store can hold data from
// every upstream exporter at once.
//
// New sources must add their constant here and to the migrations layer's
// allowed-source check; the value is persisted to SQLite and never renamed.
package source

// Known sources. Keep the literal values stable — they are written to the
// database `source` columns and to `contact_identifiers.source`.
const (
	// Signal identifies conversations and messages imported from a
	// signal-export Markdown archive.
	Signal = "signal"

	// IMessage identifies conversations and messages imported from an
	// imessage-exporter Markdown archive. Wired up in Slice 2.5.
	IMessage = "imessage"
)

// All is the canonical list of recognized sources. Validation paths should
// consult this rather than building their own switch.
var All = []string{Signal, IMessage}

// IsKnown reports whether s is a recognized source string.
func IsKnown(s string) bool {
	for _, k := range All {
		if k == s {
			return true
		}
	}
	return false
}

// Label returns the human-facing display name for a source string (e.g.
// "iMessage" for the stored "imessage"). Unknown values are returned unchanged
// so callers never render an empty label.
func Label(s string) string {
	switch s {
	case Signal:
		return "Signal"
	case IMessage:
		return "iMessage"
	default:
		return s
	}
}
