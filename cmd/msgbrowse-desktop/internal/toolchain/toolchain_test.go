// Headless unit tests for the bundled-toolchain resolver and integrity check.
// They run with CGO_ENABLED=0 on Linux against a FAKED bundle layout built in a
// t.TempDir — the real macOS Contents/Resources/tools never exists on the build
// box, so the whole suite is table-driven with injected paths and a fake runner.
//
// Governing: ADR-0020 (bundled exporter toolchain), SPEC-0013 REQ "Bundled
// toolchain resolution", REQ "Bundled tool integrity and version check".
package toolchain

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeBundle materializes a Contents/Resources/tools tree under a temp dir with
// executable stub files at each tool's expected relative path, and returns the
// tools dir. Stubs are empty executable files: enough for the stat + exec-bit
// checks; the version probe is driven by an injected fake runner, not real exec.
func fakeBundle(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	toolsDir := filepath.Join(root, "Contents", "Resources", toolsSubdir)
	for _, tl := range AllTools {
		s := specs[tl]
		p := filepath.Join(toolsDir, s.relPath)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", s.name, err)
		}
		writeExec(t, p)
	}
	return toolsDir
}

func writeExec(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", path, err)
	}
}

// TestLocateDerivesToolsDirFromExecutable checks the .app-layout derivation:
// .../Contents/MacOS/msgbrowse -> .../Contents/Resources/tools, and that
// non-bundle layouts return ErrNotBundled so the CLI can fall back to PATH.
func TestLocateDerivesToolsDirFromExecutable(t *testing.T) {
	base := filepath.Join("/Applications", "msgbrowse.app")
	cases := []struct {
		name     string
		exec     string
		wantErr  error
		wantTail string // expected suffix of ToolsDir when no error
	}{
		{
			name:     "canonical .app layout",
			exec:     filepath.Join(base, "Contents", "MacOS", "msgbrowse"),
			wantTail: filepath.Join("Contents", "Resources", "tools"),
		},
		{
			name:    "plain CLI binary, not bundled",
			exec:    filepath.Join("/usr", "local", "bin", "msgbrowse"),
			wantErr: ErrNotBundled,
		},
		{
			name:    "linux desktop build under build/bin",
			exec:    filepath.Join("/home", "u", "src", "build", "bin", "msgbrowse"),
			wantErr: ErrNotBundled,
		},
		{
			name:    "empty exec path",
			exec:    "",
			wantErr: nil, // distinct non-ErrNotBundled error asserted below
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := Locate(c.exec)
			if c.exec == "" {
				if err == nil || errors.Is(err, ErrNotBundled) {
					t.Fatalf("empty exec: got err=%v, want a non-ErrNotBundled error", err)
				}
				return
			}
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got := r.ToolsDir(); filepath.Base(got) != "tools" || !hasSuffix(got, c.wantTail) {
				t.Errorf("ToolsDir = %q, want it to end with %q", got, c.wantTail)
			}
		})
	}
}

func hasSuffix(path, tail string) bool {
	return len(path) >= len(tail) && path[len(path)-len(tail):] == tail
}

// TestPathNeverConsultsPATH proves Path builds an absolute bundled path from the
// resolver's tools dir for each tool, matching the layout contract, and returns
// an error for an unknown tool rather than a bogus path.
func TestPathNeverConsultsPATH(t *testing.T) {
	toolsDir := filepath.Join("/x", "Contents", "Resources", "tools")
	r := NewResolver(toolsDir)
	want := map[Tool]string{
		Python:   filepath.Join(toolsDir, "python", "bin", "python3"),
		Signal:   filepath.Join(toolsDir, "venv", "bin", "sigexport"),
		WhatsApp: filepath.Join(toolsDir, "venv", "bin", "wtsexporter"),
		IMessage: filepath.Join(toolsDir, "imessage-exporter"),
	}
	for tl, exp := range want {
		got, err := r.Path(tl)
		if err != nil {
			t.Fatalf("Path(%v): %v", tl, err)
		}
		if got != exp {
			t.Errorf("Path(%v) = %q, want %q", tl, got, exp)
		}
	}
	if _, err := r.Path(Tool(999)); err == nil {
		t.Error("Path(unknown) = nil error, want an error")
	}
}

// TestVerifyToolReportsVersion drives the happy path: a present, executable
// stub whose fake runner prints a version yields a ToolInfo with the trimmed
// first line as Version and no error.
func TestVerifyToolReportsVersion(t *testing.T) {
	toolsDir := fakeBundle(t)
	r := NewResolver(toolsDir)

	// Keyed by the executable basename the fake runner sees (Python's bundled
	// binary is python3, per specs).
	versions := map[string]string{
		"python3":           "Python 3.12.7\n",
		"sigexport":         "sigexport, version 1.9.0\n",
		"wtsexporter":       "wtsexporter 0.12.0\n",
		"imessage-exporter": "imessage-exporter 3.0.0\nextra diagnostic line\n",
	}
	run := func(_ context.Context, name string, _ ...string) ([]byte, error) {
		return []byte(versions[filepath.Base(name)]), nil
	}

	wantVer := map[Tool]string{
		Python:   "Python 3.12.7",
		Signal:   "sigexport, version 1.9.0",
		WhatsApp: "wtsexporter 0.12.0",
		IMessage: "imessage-exporter 3.0.0", // first line only
	}
	for _, tl := range AllTools {
		info, err := r.VerifyTool(context.Background(), tl, run)
		if err != nil {
			t.Fatalf("VerifyTool(%s): %v", Name(tl), err)
		}
		if info.Version != wantVer[tl] {
			t.Errorf("VerifyTool(%s).Version = %q, want %q", Name(tl), info.Version, wantVer[tl])
		}
		if info.Name != specs[tl].name {
			t.Errorf("VerifyTool(%s).Name = %q, want %q", Name(tl), info.Name, specs[tl].name)
		}
	}
}

// TestVerifyToolMissingBinary asserts an absent bundled tool yields a *ToolError
// (never a panic, never a PATH fallback), carrying the tool identity and path.
func TestVerifyToolMissingBinary(t *testing.T) {
	toolsDir := fakeBundle(t)
	// Delete the iMessage binary to simulate a corrupt/incomplete bundle.
	imessage := filepath.Join(toolsDir, specs[IMessage].relPath)
	if err := os.Remove(imessage); err != nil {
		t.Fatalf("remove stub: %v", err)
	}
	r := NewResolver(toolsDir)

	ranProbe := false
	run := func(context.Context, string, ...string) ([]byte, error) {
		ranProbe = true
		return nil, nil
	}
	_, err := r.VerifyTool(context.Background(), IMessage, run)
	if err == nil {
		t.Fatal("VerifyTool for a missing binary = nil error, want a ToolError")
	}
	var te *ToolError
	if !errors.As(err, &te) {
		t.Fatalf("error type = %T, want *ToolError", err)
	}
	if te.Tool != IMessage || te.Name != "imessage-exporter" {
		t.Errorf("ToolError identity = {%v %q}, want {IMessage imessage-exporter}", te.Tool, te.Name)
	}
	if ranProbe {
		t.Error("version probe ran despite a missing binary; must fail before exec")
	}
}

// TestVerifyToolNonExecutable asserts a present-but-not-executable file is an
// integrity error, not a silent pass. The exec-bit is a real permission on the
// build box (Linux/macOS); skip on Windows where the bit is not modeled.
func TestVerifyToolNonExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix exec bit on windows")
	}
	toolsDir := fakeBundle(t)
	sig := filepath.Join(toolsDir, specs[Signal].relPath)
	if err := os.Chmod(sig, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	r := NewResolver(toolsDir)
	_, err := r.VerifyTool(context.Background(), Signal, func(context.Context, string, ...string) ([]byte, error) {
		t.Error("version probe ran despite a non-executable binary")
		return nil, nil
	})
	var te *ToolError
	if !errors.As(err, &te) {
		t.Fatalf("error = %v (%T), want *ToolError for a non-executable file", err, err)
	}
}

// TestVerifyToolVersionProbeFails asserts a tool that exits non-zero on its
// version flag surfaces a *ToolError carrying the underlying exec error, never
// a panic.
func TestVerifyToolVersionProbeFails(t *testing.T) {
	toolsDir := fakeBundle(t)
	r := NewResolver(toolsDir)
	boom := errors.New("exit status 1")
	_, err := r.VerifyTool(context.Background(), Python, func(context.Context, string, ...string) ([]byte, error) {
		return []byte("Traceback..."), boom
	})
	var te *ToolError
	if !errors.As(err, &te) {
		t.Fatalf("error = %v (%T), want *ToolError", err, err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("ToolError does not unwrap to the exec error: %v", err)
	}
}

// TestVerifyCollectsInfosAndErrors drives the aggregate: with two tools removed,
// Verify returns two ToolInfo (the survivors) and two *ToolError, in AllTools
// order, and never a top-level error.
func TestVerifyCollectsInfosAndErrors(t *testing.T) {
	toolsDir := fakeBundle(t)
	// Break Signal and WhatsApp (the two venv scripts).
	for _, tl := range []Tool{Signal, WhatsApp} {
		if err := os.Remove(filepath.Join(toolsDir, specs[tl].relPath)); err != nil {
			t.Fatalf("remove %s: %v", Name(tl), err)
		}
	}
	r := NewResolver(toolsDir)
	run := func(_ context.Context, name string, _ ...string) ([]byte, error) {
		return []byte(filepath.Base(name) + " 1.0.0"), nil
	}
	infos, errs := r.Verify(context.Background(), run)
	if len(infos) != 3 {
		t.Fatalf("got %d infos, want 3 (python, imessage-exporter, syncthing)", len(infos))
	}
	if len(errs) != 2 {
		t.Fatalf("got %d errs, want 2 (sigexport, wtsexporter)", len(errs))
	}
	// Survivors are Python, IMessage, and Syncthing; failures are Signal and
	// WhatsApp.
	gotOK := map[Tool]bool{}
	for _, in := range infos {
		gotOK[in.Tool] = true
	}
	if !gotOK[Python] || !gotOK[IMessage] || !gotOK[Syncthing] {
		t.Errorf("survivors = %v, want python+imessage+syncthing", gotOK)
	}
	gotErr := map[Tool]bool{}
	for _, e := range errs {
		gotErr[e.Tool] = true
	}
	if !gotErr[Signal] || !gotErr[WhatsApp] {
		t.Errorf("failures = %v, want signal+whatsapp", gotErr)
	}
}

// TestVerifyAllPresent is the fully-healthy bundle: every tool verifies, no
// errors — the shape Setup treats as "all sources have a working toolchain".
func TestVerifyAllPresent(t *testing.T) {
	r := NewResolver(fakeBundle(t))
	run := func(_ context.Context, name string, _ ...string) ([]byte, error) {
		return []byte(filepath.Base(name) + " 9.9.9"), nil
	}
	infos, errs := r.Verify(context.Background(), run)
	if len(errs) != 0 {
		t.Fatalf("healthy bundle produced errors: %v", errs)
	}
	if len(infos) != len(AllTools) {
		t.Fatalf("got %d infos, want %d", len(infos), len(AllTools))
	}
}

// TestConvenienceWrappersMatchPath guards that SignalPath/IMessagePath/
// WhatsAppPath agree with Path — the desktop export path uses the wrappers.
func TestConvenienceWrappersMatchPath(t *testing.T) {
	r := NewResolver(filepath.Join("/b", "Contents", "Resources", "tools"))
	pairs := []struct {
		tool Tool
		fn   func() (string, error)
	}{
		{Signal, r.SignalPath},
		{IMessage, r.IMessagePath},
		{WhatsApp, r.WhatsAppPath},
	}
	for _, p := range pairs {
		want, _ := r.Path(p.tool)
		got, err := p.fn()
		if err != nil || got != want {
			t.Errorf("wrapper for %s = (%q,%v), want (%q,nil)", Name(p.tool), got, err, want)
		}
	}
}

// fakeBundleApp materializes a full .app layout (Contents/MacOS/msgbrowse plus
// Contents/Resources/tools with executable stubs) and returns the fake
// executable path — what os.Executable() would report inside the .app.
func fakeBundleApp(t *testing.T) (execPath string) {
	t.Helper()
	root := t.TempDir()
	app := filepath.Join(root, "msgbrowse.app")
	macos := filepath.Join(app, "Contents", "MacOS")
	if err := os.MkdirAll(macos, 0o755); err != nil {
		t.Fatalf("mkdir MacOS: %v", err)
	}
	exe := filepath.Join(macos, "msgbrowse")
	writeExec(t, exe)
	toolsDir := filepath.Join(app, "Contents", "Resources", toolsSubdir)
	for _, tl := range AllTools {
		p := filepath.Join(toolsDir, specs[tl].relPath)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", specs[tl].name, err)
		}
		writeExec(t, p)
	}
	return exe
}

// TestResolveExportersBundled proves the .app path: a healthy bundle yields
// absolute bundled paths for all three exporters, Bundled=true, and never
// touches $PATH.
func TestResolveExportersBundled(t *testing.T) {
	exe := fakeBundleApp(t)
	run := func(_ context.Context, name string, _ ...string) ([]byte, error) {
		return []byte(filepath.Base(name) + " 1.0.0"), nil
	}
	got, err := ResolveExporters(context.Background(), exe, run)
	if err != nil {
		t.Fatalf("ResolveExporters: %v", err)
	}
	if !got.Bundled {
		t.Fatal("Bundled = false in a .app, want true")
	}
	// Every exporter path must be absolute and sit under the bundle's tools dir.
	toolsDir := filepath.Join(filepath.Dir(filepath.Dir(exe)), "Resources", "tools")
	for _, p := range []string{got.Signal, got.IMessage, got.WhatsApp} {
		if !filepath.IsAbs(p) {
			t.Errorf("exporter path %q is not absolute", p)
		}
		rel, rerr := filepath.Rel(toolsDir, p)
		if rerr != nil || rel == ".." || hasPrefix(rel, ".."+string(filepath.Separator)) {
			t.Errorf("exporter path %q is not under the bundled tools dir %q", p, toolsDir)
		}
	}
}

func hasPrefix(s, pre string) bool { return len(s) >= len(pre) && s[:len(pre)] == pre }

// TestResolveExportersNotBundled proves the fallback: outside a .app,
// ResolveExporters returns Bundled=false with empty fields so the CLI export
// path falls back to $PATH.
func TestResolveExportersNotBundled(t *testing.T) {
	got, err := ResolveExporters(context.Background(), filepath.Join("/usr", "local", "bin", "msgbrowse"), nil)
	if err != nil {
		t.Fatalf("ResolveExporters (non-bundled): %v", err)
	}
	if got.Bundled {
		t.Error("Bundled = true outside a .app, want false")
	}
	if got.Signal != "" || got.IMessage != "" || got.WhatsApp != "" {
		t.Errorf("non-bundled override paths must be empty, got %+v", got)
	}
}

// TestResolveExportersBrokenBundleIsTypedError proves a corrupt .app (a missing
// bundled tool) is a hard *ToolError — never a silent $PATH fallback.
func TestResolveExportersBrokenBundleIsTypedError(t *testing.T) {
	exe := fakeBundleApp(t)
	toolsDir := filepath.Join(filepath.Dir(filepath.Dir(exe)), "Resources", "tools")
	if err := os.Remove(filepath.Join(toolsDir, specs[Signal].relPath)); err != nil {
		t.Fatalf("remove sigexport stub: %v", err)
	}
	run := func(_ context.Context, name string, _ ...string) ([]byte, error) {
		return []byte(filepath.Base(name) + " 1.0.0"), nil
	}
	_, err := ResolveExporters(context.Background(), exe, run)
	if err == nil {
		t.Fatal("broken bundle = nil error, want a ToolError")
	}
	var te *ToolError
	if !errors.As(err, &te) {
		t.Fatalf("error type = %T, want *ToolError", err)
	}
	if te.Tool != Signal {
		t.Errorf("ToolError.Tool = %v, want Signal", te.Tool)
	}
}

// TestNameAndIsPythonScript covers the small classifiers used by errors, the
// About view, and the export-path documentation.
func TestNameAndIsPythonScript(t *testing.T) {
	if Name(Signal) != "sigexport" || Name(IMessage) != "imessage-exporter" {
		t.Errorf("Name mismatch: %q %q", Name(Signal), Name(IMessage))
	}
	if Name(Tool(1234)) == "" {
		t.Error("Name(unknown) must not be empty")
	}
	if !IsPythonScript(Signal) || !IsPythonScript(WhatsApp) {
		t.Error("Signal and WhatsApp must be python scripts")
	}
	if IsPythonScript(IMessage) || IsPythonScript(Python) {
		t.Error("IMessage and Python are native, not python scripts")
	}
}

// --- issue #147: relocation env + per-source decoupling ---------------------

// fakeBundleAppWithVenv extends fakeBundleApp with a venv site-packages dir so
// the PYTHONPATH glob (venv/lib/python3.*/site-packages) resolves, mirroring
// what desktop.yml's `python -m venv` creates in the .app.
func fakeBundleAppWithVenv(t *testing.T) (execPath, toolsDir string) {
	t.Helper()
	exe := fakeBundleApp(t)
	toolsDir = filepath.Join(filepath.Dir(filepath.Dir(exe)), "Resources", "tools")
	sp := filepath.Join(toolsDir, "venv", "lib", "python3.12", "site-packages")
	if err := os.MkdirAll(sp, 0o755); err != nil {
		t.Fatalf("mkdir site-packages: %v", err)
	}
	return exe, toolsDir
}

// envValue returns the value of KEY in a KEY=VALUE env slice, and ok=false if
// the key is absent — the assertion helper for the PYTHONHOME/PYTHONPATH tests.
func envValue(env []string, key string) (string, bool) {
	pre := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, pre) {
			return kv[len(pre):], true
		}
	}
	return "", false
}

// TestEnvForToolSetsRelocatedPythonHomeAndPath is the core relocation-fix unit
// test (issue #147): a Python tool's env carries PYTHONHOME = the bundled python
// home and PYTHONPATH = the venv site-packages, BOTH computed relative to the
// runtime bundle location (toolsDir derived from the fake executable path) — no
// build-time path baked in. It asserts against a fake .app in a temp dir, so it
// runs headless on Linux.
func TestEnvForToolSetsRelocatedPythonHomeAndPath(t *testing.T) {
	_, toolsDir := fakeBundleAppWithVenv(t)
	r := NewResolver(toolsDir)

	wantHome := filepath.Join(toolsDir, "python")
	wantPath := filepath.Join(toolsDir, "venv", "lib", "python3.12", "site-packages")

	for _, tl := range []Tool{Signal, WhatsApp, Python} {
		env := r.EnvForTool(tl)
		if env == nil {
			t.Fatalf("EnvForTool(%s) = nil; python tools must get a corrected env", Name(tl))
		}
		if got, ok := envValue(env, "PYTHONHOME"); !ok || got != wantHome {
			t.Errorf("EnvForTool(%s) PYTHONHOME = %q (ok=%v); want %q", Name(tl), got, ok, wantHome)
		}
		if got, ok := envValue(env, "PYTHONPATH"); !ok || got != wantPath {
			t.Errorf("EnvForTool(%s) PYTHONPATH = %q (ok=%v); want %q", Name(tl), got, ok, wantPath)
		}
	}
}

// TestEnvForToolIMessageGetsNoPythonEnv proves the Rust binary is decoupled from
// the Python runtime (issue #147): imessage-exporter's env is nil (inherit),
// with no PYTHONHOME/PYTHONPATH injected — setting those for a Rust binary would
// be meaningless and is explicitly not done.
func TestEnvForToolIMessageGetsNoPythonEnv(t *testing.T) {
	_, toolsDir := fakeBundleAppWithVenv(t)
	r := NewResolver(toolsDir)
	if env := r.EnvForTool(IMessage); env != nil {
		t.Errorf("EnvForTool(IMessage) = %v; want nil (native Rust binary inherits env, no Python vars)", env)
	}
}

// TestPythonEnvOverridesInheritedPythonHome guards the merge semantics: an
// inherited PYTHONHOME from the parent environment MUST be replaced (not
// duplicated) by the bundled home, or the relocated interpreter would still pick
// up a stale prefix. It sets a bogus PYTHONHOME in the process env and asserts
// the returned env has exactly one PYTHONHOME, equal to the bundled home.
func TestPythonEnvOverridesInheritedPythonHome(t *testing.T) {
	_, toolsDir := fakeBundleAppWithVenv(t)
	t.Setenv("PYTHONHOME", "/some/stale/prefix")
	r := NewResolver(toolsDir)
	env := r.PythonEnv()

	wantHome := filepath.Join(toolsDir, "python")
	count := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "PYTHONHOME=") {
			count++
			if got := kv[len("PYTHONHOME="):]; got != wantHome {
				t.Errorf("PYTHONHOME = %q; want the bundled home %q", got, wantHome)
			}
		}
	}
	if count != 1 {
		t.Errorf("PYTHONHOME appears %d times; want exactly 1 (inherited value must be replaced, not duplicated)", count)
	}
}

// TestEnvForToolPathMapsResolvedPath proves the by-path seam onboard uses: given
// the resolved absolute path of the Signal tool it returns the Python env, and
// given the imessage-exporter path it returns nil; an unrelated path returns nil.
func TestEnvForToolPathMapsResolvedPath(t *testing.T) {
	_, toolsDir := fakeBundleAppWithVenv(t)
	r := NewResolver(toolsDir)

	sig, _ := r.SignalPath()
	if env := r.EnvForToolPath(sig); env == nil {
		t.Error("EnvForToolPath(sigexport) = nil; want the corrected Python env")
	}
	im, _ := r.IMessagePath()
	if env := r.EnvForToolPath(im); env != nil {
		t.Error("EnvForToolPath(imessage-exporter) != nil; want nil (Rust binary inherits env)")
	}
	if env := r.EnvForToolPath("/not/a/bundled/tool"); env != nil {
		t.Error("EnvForToolPath(unrelated) != nil; a non-bundle path must inherit env")
	}
}

// TestResolveExporterPerSourceOnly proves the decoupling contract (issue #147):
// ResolveExporter for iMessage integrity-checks ONLY imessage-exporter and
// succeeds even when the Python sigexport tool is BROKEN (removed) — a Python
// failure must never block iMessage. Conversely resolving Signal against the
// same broken bundle surfaces the sigexport ToolError.
func TestResolveExporterPerSourceOnly(t *testing.T) {
	exe, toolsDir := fakeBundleAppWithVenv(t)
	// Break the Python (sigexport) tool: remove its stub so any probe of it fails.
	if err := os.Remove(filepath.Join(toolsDir, specs[Signal].relPath)); err != nil {
		t.Fatalf("remove sigexport stub: %v", err)
	}
	run := func(_ context.Context, name string, _ ...string) ([]byte, error) {
		return []byte(filepath.Base(name) + " 1.0.0"), nil
	}

	// iMessage resolves fine — it depends only on imessage-exporter (Rust).
	res, err := ResolveExporter(context.Background(), exe, "imessage", run)
	if err != nil {
		t.Fatalf("ResolveExporter(imessage) with a broken sigexport: %v — iMessage must NOT depend on Python", err)
	}
	if !res.Bundled || res.Path == "" {
		t.Fatalf("ResolveExporter(imessage) = %+v; want a bundled path", res)
	}
	if res.Env != nil {
		t.Errorf("ResolveExporter(imessage).Env = %v; want nil (native binary)", res.Env)
	}

	// Signal resolution surfaces the sigexport ToolError (its own tool is broken).
	if _, err := ResolveExporter(context.Background(), exe, "signal", run); err == nil {
		t.Fatal("ResolveExporter(signal) with a broken sigexport = nil error; want a ToolError")
	} else {
		var te *ToolError
		if !errors.As(err, &te) || te.Tool != Signal {
			t.Fatalf("ResolveExporter(signal) error = %v; want a Signal *ToolError", err)
		}
	}
}

// TestResolveExporterBundledSetsEnv proves the happy path: a healthy bundle
// resolves Signal to its bundled path WITH the corrected Python env, and
// iMessage to its bundled path with a nil (inherit) env.
func TestResolveExporterBundledSetsEnv(t *testing.T) {
	exe, toolsDir := fakeBundleAppWithVenv(t)
	run := func(_ context.Context, name string, _ ...string) ([]byte, error) {
		return []byte(filepath.Base(name) + " 1.0.0"), nil
	}

	sig, err := ResolveExporter(context.Background(), exe, "signal", run)
	if err != nil {
		t.Fatalf("ResolveExporter(signal): %v", err)
	}
	if !sig.Bundled || sig.Path != filepath.Join(toolsDir, specs[Signal].relPath) {
		t.Fatalf("ResolveExporter(signal) = %+v; want the bundled sigexport path", sig)
	}
	if _, ok := envValue(sig.Env, "PYTHONHOME"); !ok {
		t.Error("ResolveExporter(signal).Env missing PYTHONHOME; the export spawn would break after relocation")
	}

	im, err := ResolveExporter(context.Background(), exe, "imessage", run)
	if err != nil {
		t.Fatalf("ResolveExporter(imessage): %v", err)
	}
	if im.Env != nil {
		t.Errorf("ResolveExporter(imessage).Env = %v; want nil", im.Env)
	}
}

// TestResolveExporterNotBundled proves the fallback: outside a .app,
// ResolveExporter returns Bundled=false so the caller falls back to $PATH.
func TestResolveExporterNotBundled(t *testing.T) {
	res, err := ResolveExporter(context.Background(), filepath.Join("/usr", "local", "bin", "msgbrowse"), "signal", nil)
	if err != nil {
		t.Fatalf("ResolveExporter (non-bundled): %v", err)
	}
	if res.Bundled || res.Path != "" || res.Env != nil {
		t.Errorf("non-bundled ResolveExporter = %+v; want Bundled=false, empty path, nil env", res)
	}
}

// --- Bundled Syncthing (ADR-0021 / SPEC-0014) --------------------------------

// TestSyncthingPathAndVersionPin: the bundled sync engine resolves like every
// other tool (tools/syncthing), and its build-time version pin
// (tools/syncthing.version, written by desktop.yml) reads back trimmed. A
// missing or empty pin is a typed *ToolError — the runtime supervisor must
// refuse to launch without the pin, never skip the version check (SPEC-0014
// "Tampered bundled binary refuses to launch").
func TestSyncthingPathAndVersionPin(t *testing.T) {
	toolsDir := fakeBundle(t)
	r := NewResolver(toolsDir)

	got, err := r.SyncthingPath()
	want := filepath.Join(toolsDir, "syncthing")
	if err != nil || got != want {
		t.Fatalf("SyncthingPath = (%q, %v), want (%q, nil)", got, err, want)
	}

	// Missing pin file: typed error.
	if _, err := r.SyncthingVersionPin(); err == nil {
		t.Fatal("SyncthingVersionPin with no pin file: want a *ToolError, got nil")
	} else {
		var te *ToolError
		if !errors.As(err, &te) || te.Tool != Syncthing {
			t.Fatalf("SyncthingVersionPin err = %v (%T), want *ToolError for syncthing", err, err)
		}
	}

	// Empty pin file: typed error too.
	pinPath := filepath.Join(toolsDir, "syncthing.version")
	if err := os.WriteFile(pinPath, []byte("  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SyncthingVersionPin(); err == nil {
		t.Fatal("SyncthingVersionPin with empty pin file: want error, got nil")
	}

	// Real pin reads back trimmed.
	if err := os.WriteFile(pinPath, []byte("v2.1.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pin, err := r.SyncthingVersionPin()
	if err != nil || pin != "v2.1.1" {
		t.Fatalf("SyncthingVersionPin = (%q, %v), want (v2.1.1, nil)", pin, err)
	}
}

// TestResolveExportersIgnoresBrokenSyncthing: a corrupt bundled sync engine
// must never block exporter resolution — the decoupling issue #147
// established between sources extends to the sync engine (its integrity is
// checked by the supervisor at device-sync start, not at export time).
func TestResolveExportersIgnoresBrokenSyncthing(t *testing.T) {
	exe := fakeBundleApp(t)
	toolsDir := filepath.Join(filepath.Dir(filepath.Dir(exe)), "Resources", "tools")
	if err := os.Remove(filepath.Join(toolsDir, specs[Syncthing].relPath)); err != nil {
		t.Fatalf("remove syncthing stub: %v", err)
	}
	run := func(_ context.Context, name string, _ ...string) ([]byte, error) {
		return []byte(filepath.Base(name) + " 1.0.0"), nil
	}
	got, err := ResolveExporters(context.Background(), exe, run)
	if err != nil {
		t.Fatalf("ResolveExporters with broken syncthing: %v (exporters must not couple to the sync engine)", err)
	}
	if !got.Bundled {
		t.Error("Bundled = false, want true")
	}
}
