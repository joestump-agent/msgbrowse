//go:build macoscontacts

// Contacts-provider wiring for the desktop shell, compiled ONLY under the
// `macoscontacts` build tag (issue #10). The real Contacts.framework backend
// lives behind `darwin && macoscontacts && cgo` in internal/macoscontacts;
// this file wires the provider into the web server via SetContactResolver.
// contacts_stub.go supplies the no-op for builds without the tag, where the web
// layer keeps its contacts.Unavailable default. Layered on top of the desktop
// build the way `devicesync` is — an opt-in native integration, not force-linked
// into every macOS build (Contacts needs a TCC entitlement + usage string).
package embedded

import (
	"log/slog"

	"github.com/joestump/msgbrowse/internal/macoscontacts"
	"github.com/joestump/msgbrowse/internal/web"
)

// contactsCompiledIn reports that this desktop binary was built with the macOS
// Contacts provider.
const contactsCompiledIn = true

// wireContacts injects the macOS Contacts provider into the web server so the
// merge engine (#11) and merge settings UI (#12) resolve archive identifiers
// against the address book. macoscontacts.New self-degrades to a no-address-book
// provider off macOS (runtime GOOS guard), so wiring it is always safe.
func wireContacts(srv *web.Server, log *slog.Logger) {
	srv.SetContactResolver(macoscontacts.New(log))
}
