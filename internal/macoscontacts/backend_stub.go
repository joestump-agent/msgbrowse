//go:build !darwin || !macoscontacts || !cgo

// Default backend: NO Contacts.framework. Selected whenever the target is not
// macOS, the `macoscontacts` build tag is absent, or cgo is disabled (e.g. the
// CGO_ENABLED=0 CI build and every release binary). It links zero Contacts
// symbols, so `go build ./...` with no tags — and the default desktop binary —
// stay framework-free. newBackend hands back the no-provider backend, so New's
// Provider behaves exactly like contacts.Unavailable. backend_darwin.go is the
// real binding, compiled only under `darwin && macoscontacts && cgo`.
package macoscontacts

// backendCompiledIn reports that this build did NOT link the real
// Contacts.framework backend. The default-build test asserts this is false, the
// build-constraint proof that a no-tag build carries no Contacts symbols.
const backendCompiledIn = false

// newBackend returns the no-provider backend: no framework, no cgo. It never
// errors — "no address book" is a state, not a failure.
func newBackend() (backend, error) { return noProviderBackend{}, nil }
