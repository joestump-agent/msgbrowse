// Package contacts defines the pluggable address-book seam behind
// cross-provider contact merging (issue #9, part of epic #8): a minimal
// Resolver interface a platform address-book provider implements (macOS
// Contacts, issue #10), the shared identifier model + normalization helpers
// the providers and the merge engine (issue #11) both canonicalize through,
// and the default Unavailable implementation every non-macOS platform (and
// every unwired shell) falls back to.
//
// The package is deliberately dependency-light — stdlib only, no store, no
// cgo — so it sits at the bottom of the layering the way internal/source and
// internal/devices do: importable by the merge engine, the web layer's
// injection seam (SetContactResolver, the SetPairingSource/SetEnabler
// pattern), and a future cgo-backed desktop provider alike.
//
// Contract highlights the merge engine relies on:
//   - Person.Key is STABLE across calls within one address book (e.g. the
//     CNContact identifier on macOS), so merge decisions can be recorded
//     against it and re-checked on a later run.
//   - Every Identifier a Resolver returns is already canonical (the Normalize*
//     helpers in this package), so the engine never re-normalizes provider
//     output. Equality is a byte comparison for KindEmail and KindHandle.
//     KindPhone is the ONE documented exception: because NormalizePhone never
//     guesses a country code, the same subscriber number can be stored national
//     on one provider and international ("+"-prefixed) on another, so the merge
//     engine (#11) matches KindPhone across those two shapes by comparing the
//     trailing subscriber digits — never re-normalizing, only widening the phone
//     compare. That is the single cross-shape rule; both sides still originate
//     from these helpers. (normalize.go documents the same rule.)
//   - "No address book" is NEVER an error: Availability reports Absent (or
//     NeedsPermission) and the query methods return empty results with a nil
//     error, so the merge path degrades to no-op instead of failing (the issue
//     #9 acceptance bar).
package contacts

import "context"

// Availability is the tri-state readiness of an address book, mirroring the
// permission model of internal/setup (PermissionOK / PermissionNeeded /
// PermissionNotApplicable). A boolean cannot distinguish "no provider on this
// platform" from "provider present but the OS grant is missing", yet the merge
// settings UI (#12) must render those two states differently — a Not-detected
// affordance versus a Grant-permission affordance — so the seam reports all
// three.
type Availability int

const (
	// Absent: there is no readable address book on this platform, or none was
	// wired (Linux, browser mode, the Unavailable default). Nothing to grant.
	Absent Availability = iota
	// NeedsPermission: a provider exists but the process cannot read it because
	// the OS consent grant is missing (e.g. macOS Contacts TCC not granted).
	// This is the state that drives the "connect your address book" affordance.
	NeedsPermission
	// Available: an address book is present and readable right now.
	Available
)

// String renders a stable token for logs and tests, matching the setup
// permission tokens ("needs-permission").
func (a Availability) String() string {
	switch a {
	case Available:
		return "available"
	case NeedsPermission:
		return "needs-permission"
	default:
		return "absent"
	}
}

// Person is one address-book entry projected to exactly what the merge
// engine needs: a stable key, a display name, and canonical identifiers. It
// deliberately carries nothing else (no avatars, no postal addresses) — the
// surface grows only when a consumer needs a field.
type Person struct {
	// Key is the provider's stable identifier for this person (e.g. the
	// CNContact identifier on macOS). It is opaque to callers, unique within
	// one address book, and stable across calls, so the merge engine can
	// record decisions against it. It is NOT stable across different
	// providers or machines.
	Key string
	// DisplayName is the person's human-readable name as the address book
	// composes it ("First Last", organization name, …). May be empty when
	// the entry has identifiers but no name.
	DisplayName string
	// Identifiers are the person's phone numbers, email addresses, and
	// service handles in canonical form (NormalizePhone / NormalizeEmail /
	// Normalize). Never nil-vs-empty significant; order is provider order.
	Identifiers []Identifier
}

// Resolver is the pluggable address-book provider seam (issue #9). The
// macOS desktop shell wires a Contacts.framework-backed implementation
// (issue #10); every other platform — and any shell that wires nothing —
// gets Unavailable. The merge engine (issue #11) consumes exactly this
// surface and must behave identically whether the answer is "no address
// book" or "address book with zero matches".
//
// Implementations must be safe for concurrent use: the web layer reads the
// wired Resolver without locking (the SetEnabler/SetPairingSource contract)
// and the merge engine may query from a background job.
type Resolver interface {
	// Availability reports whether an address book is present and readable on
	// this platform right now: Available when the grant is present, NeedsPermission
	// when a provider exists but the OS consent grant is missing, Absent when
	// there is no provider at all. Anything other than Available means
	// Resolve/People return empty results; it drives the UI's affordance
	// (Not-detected vs. Grant-permission), never an error.
	Availability(ctx context.Context) Availability
	// Resolve returns the address-book people carrying the given canonical
	// identifier, best match first. The identifier must already be canonical
	// (Normalize / NormalizePhone / NormalizeEmail); providers match it
	// against their own canonicalized values. No match is an empty result
	// (len 0) with a nil error — callers must test len(), not nil, since a nil
	// slice is equally valid. An error is reserved for a genuinely broken
	// provider (I/O failure), never for "not found" or "no address book".
	Resolve(ctx context.Context, id Identifier) ([]Person, error)
	// People enumerates every address-book person that carries at least one
	// identifier, for bulk matching passes. An empty address book (or an
	// unavailable one) is an empty result (len 0) with a nil error; as with
	// Resolve, callers must test len(), not nil.
	People(ctx context.Context) ([]Person, error)
}

// Unavailable is the default no-address-book Resolver: the Linux (and
// generally non-macOS) implementation issue #9 requires, and the fallback
// the web layer substitutes when no provider was wired. It answers every
// query with "nothing here" and never errors, so the merge path runs
// identically — just matchless — on platforms without a native address book.
type Unavailable struct{}

var _ Resolver = Unavailable{}

// Availability always reports Absent: there is no address book to read.
func (Unavailable) Availability(context.Context) Availability { return Absent }

// Resolve always returns no candidates and no error.
func (Unavailable) Resolve(context.Context, Identifier) ([]Person, error) { return nil, nil }

// People always returns no people and no error.
func (Unavailable) People(context.Context) ([]Person, error) { return nil, nil }
