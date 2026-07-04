// Integrity/version probe for the resolved Syncthing binary, mirroring the
// bundled-exporter probe (cmd/msgbrowse-desktop/internal/toolchain.VerifyTool,
// ADR-0020): confirm the binary exists, is executable, answers --version with
// exit 0, and — when the bundle pins a version — reports exactly that pinned
// version. A failure is a typed error and device-sync startup aborts; an
// unverified binary is never launched (SPEC-0014 "Tampered bundled binary
// refuses to launch").
//
// Governing: ADR-0021, SPEC-0014 REQ "Bundled Syncthing Runtime".
package syncthing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Runner runs the binary with args and returns combined stdout+stderr. It is
// the probe's testing seam: tests inject a fake to script version output;
// production passes nil to exec the real binary. The invocation is trusted
// and argument-fixed (an absolute resolved path plus the constant --version).
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// versionProbeTimeout bounds the --version probe so a wedged binary cannot
// hang device-sync startup.
const versionProbeTimeout = 15 * time.Second

// execRunner is the production Runner: run the binary, capture combined
// output, inherit the process environment (Syncthing is a native binary; it
// needs no special env for a version probe).
func execRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	var buf bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	return buf.Bytes(), err
}

// VerifyBinary integrity-checks the Syncthing binary at binPath and returns
// the version line it reported. pinnedVersion, when non-empty (the bundled
// .app records it at build time as tools/syncthing.version), must appear in
// the probe output or the check fails — a swapped or corrupted binary is
// refused before any process supervision starts. run nil uses the real
// process runner.
//
// Failure modes are sentinel-matchable: a missing file is ErrBinaryNotFound;
// a non-executable file, a failing probe, or a version mismatch is
// ErrIntegrity. Errors carry the path and cause (SPEC-0014 REQ "Error
// Handling Standards").
func VerifyBinary(ctx context.Context, binPath, pinnedVersion string, run Runner) (string, error) {
	if binPath == "" {
		return "", fmt.Errorf("verify syncthing: empty binary path: %w", ErrBinaryNotFound)
	}
	fi, err := os.Stat(binPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("verify syncthing at %s: %w: %v", binPath, ErrBinaryNotFound, err)
		}
		return "", fmt.Errorf("verify syncthing at %s: %w: %v", binPath, ErrIntegrity, err)
	}
	if fi.IsDir() {
		return "", fmt.Errorf("verify syncthing at %s: %w: expected an executable file, found a directory", binPath, ErrIntegrity)
	}
	if fi.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("verify syncthing at %s: %w: file is not executable", binPath, ErrIntegrity)
	}

	if run == nil {
		run = execRunner
	}
	probeCtx, cancel := context.WithTimeout(ctx, versionProbeTimeout)
	defer cancel()
	out, err := run(probeCtx, binPath, "--version")
	if err != nil {
		return "", fmt.Errorf("verify syncthing at %s: %w: version probe failed: %v (output: %s)",
			binPath, ErrIntegrity, err, strings.TrimSpace(string(out)))
	}
	version := firstLine(out)
	if pinnedVersion != "" && !strings.Contains(version, pinnedVersion) {
		return "", fmt.Errorf("verify syncthing at %s: %w: version mismatch: probe reported %q, bundle pins %q",
			binPath, ErrIntegrity, version, pinnedVersion)
	}
	return version, nil
}

// firstLine returns the trimmed first line of probe output — Syncthing prints
// a single "syncthing vX.Y.Z ..." line; taking the first guards against
// trailing diagnostics.
func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
