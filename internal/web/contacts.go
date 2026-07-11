// The address-book injection seam behind cross-provider contact merging
// (issue #9, part of epic #8). The native-Contacts integration is Mac-only
// and (like the exporter toolchain behind Enabler) can never be imported by
// this package: the web layer must stay platform-agnostic and cgo-free. So
// the provider is injected through the established Set… pattern —
// SetContactResolver mirrors SetPairingSource / SetEnabler exactly — and
// everything else programs against internal/contacts.Resolver, a pure-Go,
// dependency-light interface.
//
// Unlike the Enabler seam, "nothing wired" is not a disabled feature here:
// merging must still run (and cleanly find no address-book matches) on
// Linux and in browser mode. contactResolver() therefore substitutes
// contacts.Unavailable{} for a nil field, so consumers — the merge engine
// (#11) and the merge settings UI (#12) when they land — never nil-check
// and never see an error for "no address book available".
package web

import "github.com/joestump/msgbrowse/internal/contacts"

// SetContactResolver wires a platform address-book provider into the server
// for contact merging (issue #9): the macOS desktop shell injects its
// Contacts.framework-backed contacts.Resolver; `msgbrowse serve` on Linux
// wires nothing. Call it after NewServer and before serving begins —
// handlers read the field without locking, so late wiring would race (the
// SetPairingSource / SetEnabler contract).
func (s *Server) SetContactResolver(r contacts.Resolver) { s.addressBook = r }

// contactResolver returns the wired address-book provider, or
// contacts.Unavailable{} when none was wired, so callers get the guaranteed
// "no address book, no error" behavior without a nil check. This is the
// accessor the merge engine (#11) and merge settings UI (#12) handlers read.
func (s *Server) contactResolver() contacts.Resolver {
	if s.addressBook == nil {
		return contacts.Unavailable{}
	}
	return s.addressBook
}
