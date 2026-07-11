// The device-sync seam shared by the tagged (syncthing.go) and untagged
// (syncthing_stub.go) builds. embedded.go holds the running stack only through
// this interface, so the concrete internal/devsync + internal/syncthing types
// exist ONLY in the `devicesync` build — the default desktop binary links
// without them (the feature is not release-ready; ADR-0021 / SPEC-0014).
package embedded

// deviceSyncHandle is the running device-sync stack the embedded server owns,
// reduced to what Close needs: a drain that stops the supervised engine and the
// folder-watch worker. The tagged build's *deviceSync implements it; the
// untagged build never produces one (wireDeviceSync returns nil).
type deviceSyncHandle interface {
	// Drain blocks until the device-sync child process and workers have exited.
	Drain()
}
