// Sentinel errors and the typed REST error for the device-sync engine.
// Callers distinguish these failure modes programmatically (SPEC-0014 REQ
// "Error Handling Standards": sentinel errors for the modes callers branch
// on; every error wrapped with operation context; nothing swallowed).
//
// Governing: ADR-0021, SPEC-0014 REQ "Error Handling Standards".
package syncthing

import (
	"errors"
	"fmt"
)

// ErrBinaryNotFound reports that no Syncthing binary could be resolved: the
// bundled binary is missing from the .app, or (in the bring-your-own CLI
// path) neither the device_sync.syncthing_bin config key nor $PATH yielded
// one. Device sync cannot start without the engine.
var ErrBinaryNotFound = errors.New("syncthing binary not found")

// ErrIntegrity reports that the resolved Syncthing binary failed its
// integrity/version probe: it is not an executable file, its --version probe
// exited non-zero, or the reported version does not match the version pinned
// into the bundle at build time. Startup MUST fail rather than launch an
// unverified binary (SPEC-0014 "Tampered bundled binary refuses to launch").
var ErrIntegrity = errors.New("syncthing binary failed integrity check")

// ErrNotRunning reports that the supervised Syncthing daemon is not running —
// it exited before becoming ready, or never confirmed readiness on its REST
// API within the startup timeout.
var ErrNotRunning = errors.New("syncthing daemon is not running")

// ErrAPIAuth reports a 401/403 from Syncthing's REST API: the msgbrowse-
// generated API key was rejected. This indicates config drift between the
// generated config.xml and the client, and is never retried silently.
var ErrAPIAuth = errors.New("syncthing REST API rejected the API key")

// ErrUnexpectedRedirect reports that Syncthing's loopback REST API answered
// with a redirect, which the client treats as a protocol error (SPEC-0014
// security requirement "CSRF and Redirect").
var ErrUnexpectedRedirect = errors.New("syncthing REST API returned an unexpected redirect")

// ErrUnmanagedFolder reports an attempt to configure a Syncthing folder whose
// path is outside the managed archive roots (<data_dir>/archives/<source>).
// Config generation refuses it outright so the database and other data_dir
// state can never enter a synced folder (SPEC-0014 "The database is never in
// a synced folder").
var ErrUnmanagedFolder = errors.New("folder path is outside the managed archive roots")

// APIError is the typed error for a non-2xx response from Syncthing's REST
// API. It carries the operation, the HTTP status, and a bounded body excerpt
// so failures are attributable (SPEC-0014 "REST failure is attributable and
// surfaced"). Auth failures additionally match ErrAPIAuth via Unwrap.
type APIError struct {
	// Op is the logical operation, e.g. "get config" or "put folders".
	Op string
	// StatusCode is the HTTP status Syncthing returned.
	StatusCode int
	// Body is a bounded excerpt of the response body (often Syncthing's
	// plain-text error message).
	Body string
	// Err is the matching sentinel (ErrAPIAuth for 401/403), or nil.
	Err error
}

func (e *APIError) Error() string {
	if e.Body != "" {
		return fmt.Sprintf("syncthing REST %s: status %d: %s", e.Op, e.StatusCode, e.Body)
	}
	return fmt.Sprintf("syncthing REST %s: status %d", e.Op, e.StatusCode)
}

func (e *APIError) Unwrap() error { return e.Err }
