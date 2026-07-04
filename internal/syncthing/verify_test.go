// Integrity/version probe tests with an injected runner (no real Syncthing
// on the build box), mirroring the toolchain probe's test approach.
//
// Governing: ADR-0021, SPEC-0014 REQ "Bundled Syncthing Runtime".
package syncthing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeStubBinary creates an executable stub file (the probe's stat and
// exec-bit checks; the runner fake supplies the version output).
func writeStubBinary(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "syncthing")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return p
}

func TestVerifyBinary(t *testing.T) {
	ctx := context.Background()
	okRunner := func(context.Context, string, ...string) ([]byte, error) {
		return []byte("syncthing v2.1.1 \"Gold Grasshopper\" (go1.24 darwin-universal)\nExtra: line\n"), nil
	}

	t.Run("ok without pin", func(t *testing.T) {
		bin := writeStubBinary(t, t.TempDir())
		v, err := VerifyBinary(ctx, bin, "", okRunner)
		if err != nil {
			t.Fatalf("VerifyBinary: %v", err)
		}
		if v != `syncthing v2.1.1 "Gold Grasshopper" (go1.24 darwin-universal)` {
			t.Errorf("version = %q", v)
		}
	})
	t.Run("ok with matching pin", func(t *testing.T) {
		bin := writeStubBinary(t, t.TempDir())
		if _, err := VerifyBinary(ctx, bin, "v2.1.1", okRunner); err != nil {
			t.Fatalf("VerifyBinary with pin: %v", err)
		}
	})
	t.Run("pin mismatch is ErrIntegrity", func(t *testing.T) {
		bin := writeStubBinary(t, t.TempDir())
		_, err := VerifyBinary(ctx, bin, "v9.9.9", okRunner)
		if !errors.Is(err, ErrIntegrity) {
			t.Fatalf("err = %v, want ErrIntegrity", err)
		}
	})
	t.Run("missing binary is ErrBinaryNotFound", func(t *testing.T) {
		_, err := VerifyBinary(ctx, filepath.Join(t.TempDir(), "nope"), "", okRunner)
		if !errors.Is(err, ErrBinaryNotFound) {
			t.Fatalf("err = %v, want ErrBinaryNotFound", err)
		}
	})
	t.Run("empty path is ErrBinaryNotFound", func(t *testing.T) {
		_, err := VerifyBinary(ctx, "", "", okRunner)
		if !errors.Is(err, ErrBinaryNotFound) {
			t.Fatalf("err = %v, want ErrBinaryNotFound", err)
		}
	})
	t.Run("non-executable is ErrIntegrity", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "syncthing")
		if err := os.WriteFile(p, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := VerifyBinary(ctx, p, "", okRunner)
		if !errors.Is(err, ErrIntegrity) {
			t.Fatalf("err = %v, want ErrIntegrity", err)
		}
	})
	t.Run("directory is ErrIntegrity", func(t *testing.T) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "syncthing")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := VerifyBinary(ctx, sub, "", okRunner)
		if !errors.Is(err, ErrIntegrity) {
			t.Fatalf("err = %v, want ErrIntegrity", err)
		}
	})
	t.Run("failing probe is ErrIntegrity", func(t *testing.T) {
		bin := writeStubBinary(t, t.TempDir())
		failing := func(context.Context, string, ...string) ([]byte, error) {
			return []byte("boom"), errors.New("exit status 1")
		}
		_, err := VerifyBinary(ctx, bin, "", failing)
		if !errors.Is(err, ErrIntegrity) {
			t.Fatalf("err = %v, want ErrIntegrity", err)
		}
	})
}
