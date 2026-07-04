// Package syncthing bundles the device-sync transfer engine adopted by
// ADR-0021: a version-pinned Syncthing binary supervised as a managed child
// process and driven exclusively through its loopback REST API. msgbrowse owns
// the daemon's entire configuration (folders = the managed archive roots under
// <data_dir>/archives/<source>; devices = paired peers) — the user never edits
// Syncthing config or sees its GUI.
//
// The package has three parts:
//
//   - Supervisor (supervisor.go): starts the Syncthing binary as a managed
//     child with its home under <data_dir>/syncthing/, its REST/GUI API bound
//     to a loopback address with a msgbrowse-generated API key, and a
//     LAN-only network posture (global discovery OFF, relaying OFF, NAT
//     traversal OFF). Context cancellation drives a clean shutdown — SIGTERM,
//     then kill after a grace period — so no Syncthing process outlives the
//     app, and an unexpected exit restarts the daemon with backoff.
//
//   - Config generation (configgen.go): msgbrowse writes Syncthing's
//     config.xml before every daemon start. Folder entries come only from the
//     managed archive roots (never the data dir, never the SQLite DB), and
//     the LAN-only defaults are pinned in the generated XML.
//
//   - REST client (client.go): a typed loopback client (X-API-Key header)
//     covering system status/version, config get/put for devices and folders,
//     the events long-poll (the re-ingest trigger for the folder-watch story),
//     and folder completion.
//
// The package is pure Go with no cgo and no link against Syncthing — the
// engine is a separate supervised process, so the CGO_ENABLED=0 core build is
// unaffected (ADR-0013). Everything is exercised on headless Linux with a fake
// Syncthing binary and httptest stubs; only the macOS CI leg runs the real
// bundled binary.
//
// Binary resolution is the caller's job and mirrors the exporter split
// (ADR-0020): the desktop .app resolves the bundled binary from
// Contents/Resources/tools (never $PATH) via cmd/msgbrowse-desktop's toolchain
// resolver; `msgbrowse serve` falls back to the device_sync.syncthing_bin
// config key and then $PATH (bring-your-own for non-desktop installs).
//
// Governing: ADR-0021 (bundle + supervise Syncthing as the device-sync
// engine), SPEC-0014 REQ "Bundled Syncthing Runtime", REQ "Supervised Daemon
// Lifecycle", REQ "msgbrowse-Owned Config Generation", REQ "Error Handling
// Standards", REQ "Concurrency Safety", and the SPEC-0014 security
// requirement "Relay and Discovery Posture" (LAN + local discovery only by
// default).
package syncthing
