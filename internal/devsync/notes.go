// The device-sync event feed for the Logs page: a bounded, concurrency-safe
// ring of the significant things the pairing manager and folder-watch worker
// did — pairs, unpairs, accepted offers, dispatched imports, peer
// connect/disconnect, event-stream failures — so sync activity is observable
// in-app instead of buried in stderr (SPEC-0014 REQ "Status and Doctor
// Surfacing": Syncthing state surfaced into Settings/LOGS/Status; the user
// never opens Syncthing's GUI). It mirrors the desktop shell's ShellNote ring
// (issue #167): server-owned strings only, never message content, never
// persisted to disk.
//
// Governing: ADR-0021, SPEC-0014 REQ "Status and Doctor Surfacing", REQ
// "Error Handling Standards" (failures surfaced, not swallowed).
package devsync

import (
	"sync"
	"time"
)

// Note levels. Plain strings (matching web.ShellNote's shape) so the web
// template renders them without a type dance.
const (
	NoteInfo  = "info"
	NoteError = "error"
)

// Note is one recorded device-sync event line. Every field is composed by
// msgbrowse from validated identifiers and fixed prose — never request- or
// message-derived free text — so html/template escaping is the only encoding
// the Logs page needs.
type Note struct {
	// Time is when the event was recorded.
	Time time.Time
	// Level is NoteInfo or NoteError.
	Level string
	// Message is the human-readable event line.
	Message string
}

// IsError reports whether the note is an error, for the template's badge.
func (n Note) IsError() bool { return n.Level == NoteError }

// Clock renders the note's time-of-day for the Logs page; sync activity is
// read within one app session, so the date would be noise (the ShellNote
// convention).
func (n Note) Clock() string { return n.Time.Format("15:04:05") }

// defaultNotesCap bounds the ring when NewNotes is given a non-positive
// capacity.
const defaultNotesCap = 64

// Notes is the bounded ring. One instance is shared by the Manager and the
// Watcher (wired by serve / the desktop shell) so the Logs page reads a
// single chronological feed. All methods are safe for concurrent use and
// nil-receiver safe, so recording sites never nil-check.
type Notes struct {
	mu    sync.Mutex
	cap   int
	notes []Note
}

// NewNotes builds a ring holding at most capacity notes (<=0 means the
// default).
func NewNotes(capacity int) *Notes {
	if capacity <= 0 {
		capacity = defaultNotesCap
	}
	return &Notes{cap: capacity}
}

// Add records one event line, evicting the oldest when full. No-op on a nil
// ring (a Manager or Watcher wired without notes).
func (n *Notes) Add(level, message string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.notes = append(n.notes, Note{Time: time.Now(), Level: level, Message: message})
	if len(n.notes) > n.cap {
		n.notes = n.notes[len(n.notes)-n.cap:]
	}
}

// Snapshot returns the recorded notes oldest-first (a copy; safe to render
// while recording continues). Nil-safe: a nil ring snapshots empty.
func (n *Notes) Snapshot() []Note {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]Note, len(n.notes))
	copy(out, n.notes)
	return out
}
