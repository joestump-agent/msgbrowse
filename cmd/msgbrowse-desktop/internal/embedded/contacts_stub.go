//go:build !macoscontacts

// Default desktop build: the macOS Contacts provider is NOT compiled in. This
// stub replaces contacts.go so the embedded server links WITHOUT
// internal/macoscontacts — the web layer keeps its contacts.Unavailable no-op
// (merging still runs and finds no address-book matches). Build with
// `-tags macoscontacts` on macOS to wire the real provider (issue #10).
package embedded

import (
	"log/slog"

	"github.com/joestump/msgbrowse/internal/web"
)

// contactsCompiledIn reports that this desktop binary was built WITHOUT the
// macOS Contacts provider.
const contactsCompiledIn = false

// wireContacts is the no-op seam for builds without the `macoscontacts` tag: it
// wires nothing, leaving the web layer's contacts.Unavailable default in place.
func wireContacts(_ *web.Server, _ *slog.Logger) {}
