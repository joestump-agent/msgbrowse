// Package macoscontacts is the macOS Contacts.framework-backed
// contacts.Resolver (issue #10, part of epic #8): the platform-specific
// address-book provider the merge engine (#11) reads people from, and the
// desktop shell wires via web.SetContactResolver. Every other platform — and
// the default build — keeps the contacts.Unavailable no-op.
//
// # Layering: pure-Go core, thin cgo edge
//
// Contacts.framework is a cgo/Objective-C dependency that (a) only exists on
// macOS, (b) requires the address-book entitlement + a usage string, and (c)
// cannot be compiled or run in this project's CGO_ENABLED=0 CI. So the package
// is split so that ALL the logic lives in pure Go and only a ~40-line binding
// touches the framework:
//
//   - provider.go (this file, ALWAYS compiled, no cgo): the Provider that
//     satisfies contacts.Resolver, the permission-state → Availability mapping
//     (availabilityFor / authStatusFromCN), the raw-record → contacts.Person
//     normalization (mapPeople, reusing the shared contacts.Normalize* helpers),
//     and the enumeration-dump parser (parseDump). Every one of these is
//     exercised by provider_test.go with a fake backend — no cgo, on Linux.
//   - backend_darwin.go (//go:build darwin && macoscontacts && cgo): the thin
//     Contacts.framework binding. It only fetches the raw authorization status
//     and dumps every contact's identifier/name/phones/emails into the serialized
//     form parseDump consumes. No mapping or normalization lives there.
//   - backend_stub.go (everything else): a no-op backend reporting "no provider".
//
// # Build tag choice: a dedicated `macoscontacts` tag (not `desktop && darwin`)
//
// The desktop module gates its plain macOS glue with `desktop && darwin`, which
// links unconditionally into every macOS desktop build. Contacts is different:
// like device sync (the `devicesync` tag precedent), it is an opt-in native
// integration that pulls in a framework link AND demands a TCC entitlement +
// usage-description in the signed .app, so it must NOT be force-linked into
// every build. A dedicated `macoscontacts` tag, layered on top of the desktop
// build exactly the way `devicesync` is, keeps the default binary free of any
// Contacts symbols (backend_stub.go is selected) while letting the real .app
// opt in. This mirrors the established repo precedent for not-yet-default
// native integrations rather than the always-on `desktop && darwin` glue.
//
// # Runtime guard
//
// Beyond the build tag, New applies a runtime GOOS==darwin guard: even if the
// real backend were somehow linked on a non-macOS target, New falls back to the
// no-provider backend, so the provider can only ever touch the framework on an
// actual Mac.
package macoscontacts

import (
	"context"
	"log/slog"
	"runtime"
	"strings"

	"github.com/joestump/msgbrowse/internal/contacts"
)

// authStatus is the pure-Go projection of the OS address-book authorization
// state, decoupled from the CNAuthorizationStatus integer so the mapping is
// unit-testable without cgo. authNoProvider is the extra "there is no Contacts
// framework at all" state the CN enum has no member for (the stub / non-macOS
// case), which maps to Absent rather than NeedsPermission.
type authStatus int

const (
	// authNoProvider: no address book on this platform (stub backend / not
	// built with the tag / not macOS). Maps to contacts.Absent.
	authNoProvider authStatus = iota
	// authNotDetermined: the user has never been asked. A provider exists but
	// the grant is missing → contacts.NeedsPermission (the detect-only model of
	// internal/setup: we never prompt, we guide).
	authNotDetermined
	// authRestricted: access is disallowed by policy (e.g. parental controls).
	// The grant cannot be obtained by us → contacts.NeedsPermission.
	authRestricted
	// authDenied: the user declined. A provider exists, no grant →
	// contacts.NeedsPermission.
	authDenied
	// authAuthorized: full access granted → contacts.Available.
	authAuthorized
	// authLimited: partial (limited-selection) access on newer macOS. We can
	// read the shared subset → contacts.Available.
	authLimited
)

// CNAuthorizationStatus raw values (Contacts/CNContactStore.h). Kept in pure
// Go so authStatusFromCN is testable without the framework; the cgo backend
// passes the framework's integer straight through to authStatusFromCN.
const (
	cnStatusNotDetermined = 0
	cnStatusRestricted    = 1
	cnStatusDenied        = 2
	cnStatusAuthorized    = 3
	cnStatusLimited       = 4
)

// authStatusFromCN maps a raw CNAuthorizationStatus integer to the pure-Go
// authStatus. An unknown value is treated as authNotDetermined (conservative:
// surfaces as NeedsPermission rather than silently claiming access).
func authStatusFromCN(v int) authStatus {
	switch v {
	case cnStatusAuthorized:
		return authAuthorized
	case cnStatusLimited:
		return authLimited
	case cnStatusDenied:
		return authDenied
	case cnStatusRestricted:
		return authRestricted
	case cnStatusNotDetermined:
		return authNotDetermined
	default:
		return authNotDetermined
	}
}

// availabilityFor maps an authStatus to the contacts.Availability tri-state the
// merge settings UI (#12) renders: Available drives the connected state,
// NeedsPermission the "grant address-book access" affordance, Absent the
// not-detected state. This is the single place the permission model lines up
// with internal/setup's PermissionOK / PermissionNeeded / PermissionNotApplicable.
func availabilityFor(s authStatus) contacts.Availability {
	switch s {
	case authAuthorized, authLimited:
		return contacts.Available
	case authNotDetermined, authRestricted, authDenied:
		return contacts.NeedsPermission
	default: // authNoProvider
		return contacts.Absent
	}
}

// rawPerson is one address-book record as the backend dumps it, BEFORE
// normalization: the provider's stable key, a display name, and the phone /
// email strings exactly as the address book stores them. The pure-Go Provider
// canonicalizes these through the shared contacts.Normalize* helpers; the cgo
// backend never normalizes.
type rawPerson struct {
	Key         string
	DisplayName string
	Phones      []string
	Emails      []string
}

// backend is the thin address-book seam: the real implementation
// (backend_darwin.go) calls Contacts.framework via cgo; the fake in the tests
// drives every mapping path. It deliberately returns RAW records and a RAW
// authorization state — all classification, normalization, and matching stays
// in the pure-Go Provider so it is fully testable without cgo.
type backend interface {
	// authorization reports the current OS authorization state. It must not
	// prompt (detect-only, per ADR-0020 / the internal/setup model).
	authorization(ctx context.Context) authStatus
	// people enumerates every address-book record with its raw identifiers. A
	// genuine I/O failure returns an error; an empty book returns (nil, nil).
	people(ctx context.Context) ([]rawPerson, error)
}

// noProviderBackend is the address-book-less backend: the stub build, and the
// fallback New substitutes off macOS. It reports authNoProvider (→ Absent) and
// enumerates nothing, so the Provider behaves exactly like contacts.Unavailable.
type noProviderBackend struct{}

func (noProviderBackend) authorization(context.Context) authStatus    { return authNoProvider }
func (noProviderBackend) people(context.Context) ([]rawPerson, error) { return nil, nil }

// Provider is the macOS Contacts contacts.Resolver. It holds the thin backend
// and adds the pure-Go authorization mapping, normalization, and matching. It
// is safe for concurrent use: the backend is set once at construction and the
// framework calls are read-only (the SetContactResolver contract — handlers and
// the merge engine read it without locking).
type Provider struct {
	be  backend
	log *slog.Logger
}

var _ contacts.Resolver = (*Provider)(nil)

// New constructs the macOS Contacts provider. Off macOS — or in a build without
// the `macoscontacts` tag / without cgo — the backend is the no-provider stub,
// so the provider reports Absent and returns no people (identical to
// contacts.Unavailable). On a Mac built with the tag it is backed by the live
// Contacts.framework binding. The runtime GOOS==darwin guard is belt-and-braces
// on top of the build tag: the framework is only ever reached on an actual Mac.
func New(log *slog.Logger) *Provider {
	if log == nil {
		log = slog.Default()
	}
	if runtime.GOOS != "darwin" {
		return &Provider{be: noProviderBackend{}, log: log}
	}
	be, err := newBackend()
	if err != nil {
		log.Warn("macOS Contacts provider unavailable; falling back to no address book", "error", err)
		return &Provider{be: noProviderBackend{}, log: log}
	}
	return &Provider{be: be, log: log}
}

// Availability reports whether the address book is present and readable now
// (contacts.Resolver). It maps the backend's raw authorization state through
// the shared permission model and never errors.
func (p *Provider) Availability(ctx context.Context) contacts.Availability {
	return availabilityFor(p.be.authorization(ctx))
}

// People enumerates every address-book person carrying at least one canonical
// identifier (contacts.Resolver). When the grant is missing (anything other
// than Available) it returns (nil, nil) — no address book is never an error.
// A genuine backend I/O failure propagates.
func (p *Provider) People(ctx context.Context) ([]contacts.Person, error) {
	if availabilityFor(p.be.authorization(ctx)) != contacts.Available {
		return nil, nil
	}
	raw, err := p.be.people(ctx)
	if err != nil {
		return nil, err
	}
	return mapPeople(raw), nil
}

// Resolve returns the people carrying the given canonical identifier, best
// match first (contacts.Resolver). Per the seam contract the identifier is
// already canonical and the provider matches it against its own canonicalized
// values by exact (Kind, Value) equality — the KindPhone national/international
// cross-shape widening is the merge engine's single documented job, not the
// provider's. A zero identifier, an unavailable book, or no match all return an
// empty result with a nil error.
func (p *Provider) Resolve(ctx context.Context, id contacts.Identifier) ([]contacts.Person, error) {
	if id.IsZero() {
		return nil, nil
	}
	people, err := p.People(ctx)
	if err != nil {
		return nil, err
	}
	var out []contacts.Person
	for _, person := range people {
		for _, pid := range person.Identifiers {
			if pid == id {
				out = append(out, person)
				break
			}
		}
	}
	return out, nil
}

// mapPeople normalizes raw address-book records into contacts.Person values:
// each phone through contacts.NormalizePhone (→ KindPhone), each email through
// contacts.NormalizeEmail (→ KindEmail), dropping values that do not
// canonicalize and de-duplicating identical identifiers within a person. A
// record left with zero identifiers is dropped entirely (People only surfaces
// people with at least one identifier). Provider order is preserved.
func mapPeople(raw []rawPerson) []contacts.Person {
	var out []contacts.Person
	for _, r := range raw {
		ids := make([]contacts.Identifier, 0, len(r.Phones)+len(r.Emails))
		seen := make(map[contacts.Identifier]struct{}, len(r.Phones)+len(r.Emails))
		add := func(id contacts.Identifier) {
			if id.IsZero() {
				return
			}
			if _, dup := seen[id]; dup {
				return
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
		for _, ph := range r.Phones {
			if v, ok := contacts.NormalizePhone(ph); ok {
				add(contacts.Identifier{Kind: contacts.KindPhone, Value: v})
			}
		}
		for _, em := range r.Emails {
			if v, ok := contacts.NormalizeEmail(em); ok {
				add(contacts.Identifier{Kind: contacts.KindEmail, Value: v})
			}
		}
		if len(ids) == 0 {
			continue
		}
		out = append(out, contacts.Person{
			Key:         r.Key,
			DisplayName: strings.TrimSpace(r.DisplayName),
			Identifiers: ids,
		})
	}
	return out
}

// Dump record/field separators shared by the cgo backend (which builds the
// serialized enumeration) and parseDump (which reads it). ASCII control chars
// that never occur in names, phone numbers, or email addresses, so no escaping
// is needed for a thin binding.
const (
	dumpRecordSep = "\x1e" // between contacts
	dumpFieldSep  = "\x1f" // between fields within a contact
)

// parseDump parses the backend's serialized contact enumeration into rawPersons.
// Layout per record: key, displayName, then zero or more identifier tokens each
// prefixed with a type byte — 'p' for a phone, 'e' for an email. Empty records
// and empty identifier tokens are skipped. This is the seam between the cgo dump
// and the pure-Go pipeline, so it is unit-tested directly (no framework needed).
func parseDump(s string) []rawPerson {
	if s == "" {
		return nil
	}
	var out []rawPerson
	for _, rec := range strings.Split(s, dumpRecordSep) {
		if rec == "" {
			continue
		}
		fields := strings.Split(rec, dumpFieldSep)
		if len(fields) < 2 {
			continue
		}
		p := rawPerson{Key: fields[0], DisplayName: fields[1]}
		for _, tok := range fields[2:] {
			if len(tok) < 2 {
				continue
			}
			switch tok[0] {
			case 'p':
				p.Phones = append(p.Phones, tok[1:])
			case 'e':
				p.Emails = append(p.Emails, tok[1:])
			}
		}
		out = append(out, p)
	}
	return out
}
