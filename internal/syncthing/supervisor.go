// The supervised Syncthing child-process lifecycle. The Supervisor verifies
// the binary, generates the daemon's entire config, starts Syncthing as a
// managed child bound to loopback for its REST API, confirms readiness,
// restarts it with backoff on unexpected exit, and — on context cancellation
// — shuts it down cleanly (SIGTERM, then kill after a grace period) so no
// Syncthing process ever outlives the app. It is started ONLY when
// device_sync.enabled is true; callers gate on the config key.
//
// The pattern mirrors the app's other supervised workers: internal/onboard's
// context-cancelled runner (one lifecycle owner, cancellable context, no
// orphaned subprocess) and internal/cli/serve.go's device-sync worker shape
// (start returns once live; a done channel reports the drain).
//
// Governing: ADR-0021 ("bundle + supervise"), SPEC-0014 REQ "Supervised
// Daemon Lifecycle" (loopback REST + generated API key, clean shutdown on
// quit/disable, restart with backoff), REQ "Concurrency Safety" (context
// propagation, explicit worker lifecycle, no orphan process, no leaked
// goroutine), REQ "Error Handling Standards" (typed errors; child
// stdout/stderr captured into structured logs, never discarded).
package syncthing

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// HomeDirName is the subdirectory of data_dir that holds Syncthing's home
// (its keys, database, and the msgbrowse-generated config.xml). It is a
// sibling of archives/, never inside a synced folder.
const HomeDirName = "syncthing"

// Defaults for the lifecycle knobs; overridable in Options mainly for tests.
const (
	defaultGrace        = 10 * time.Second
	defaultReadyTimeout = 30 * time.Second
	defaultBackoffMin   = time.Second
	defaultBackoffMax   = 30 * time.Second
	// stableUptime is how long a child must live for the restart backoff to
	// reset — a daemon that keeps crashing quickly backs off; one that ran
	// fine for a while gets a fresh fast restart.
	stableUptime = 60 * time.Second
)

// Options configures a Supervisor. BinPath and DataDir are required.
type Options struct {
	// BinPath is the resolved Syncthing binary. Resolution policy lives with
	// the caller: the desktop .app resolves it from Contents/Resources/tools
	// (never $PATH); serve resolves config key then $PATH (BYO).
	BinPath string
	// PinnedVersion, when non-empty, must appear in the binary's --version
	// output or startup fails (the bundled .app records it at build time).
	PinnedVersion string
	// DataDir is msgbrowse's data dir. The daemon home is <DataDir>/syncthing
	// and managed folders live under <DataDir>/archives/<source>.
	DataDir string
	// ListenAddr is the sync (P2P) listener host:port from
	// device_sync.listen_addr; it becomes a tcp:// listen address.
	ListenAddr string
	// GUIAddr optionally fixes the loopback REST bind (host:port). Empty
	// picks an ephemeral loopback port. Non-loopback values are refused.
	GUIAddr string
	// DeviceName is this node's friendly name shown to paired peers. Empty
	// derives the hostname at start.
	DeviceName string
	// Folders are the managed archive-root folders to configure. Every path
	// is validated against <DataDir>/archives/ before any config is written.
	Folders []Folder
	// Devices are the paired peers (none in the foundation story).
	Devices []Device
	// Grace bounds SIGTERM→kill on shutdown; 0 means the 10s default.
	Grace time.Duration
	// ReadyTimeout bounds the post-spawn readiness wait; 0 means 30s.
	ReadyTimeout time.Duration
	// Logger receives lifecycle and captured daemon output logs; nil uses
	// slog.Default().
	Logger *slog.Logger

	// extraEnv is appended to the child environment — the test seam that
	// routes the fake Syncthing helper. Unexported: production callers never
	// alter the child env.
	extraEnv []string
	// verifyRunner overrides the --version probe runner in tests.
	verifyRunner Runner
}

// Supervisor owns one Syncthing daemon lifecycle. Construct with New, start
// with Start, and read the terminal state with Wait. All exported methods
// are safe for concurrent use after Start returns.
type Supervisor struct {
	opts    Options
	log     *slog.Logger
	homeDir string

	mu      sync.Mutex
	apiAddr string
	apiKey  string
	started bool

	done chan error
}

// New validates options and builds a Supervisor. It performs no I/O.
func New(opts Options) (*Supervisor, error) {
	if opts.BinPath == "" {
		return nil, fmt.Errorf("syncthing supervisor: %w: no binary path", ErrBinaryNotFound)
	}
	if opts.DataDir == "" {
		return nil, errors.New("syncthing supervisor: data dir is required")
	}
	if opts.ListenAddr == "" {
		return nil, errors.New("syncthing supervisor: sync listen address is required")
	}
	if opts.Grace <= 0 {
		opts.Grace = defaultGrace
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = defaultReadyTimeout
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Supervisor{
		opts:    opts,
		log:     log.With("component", "syncthing"),
		homeDir: filepath.Join(opts.DataDir, HomeDirName),
		done:    make(chan error, 1),
	}, nil
}

// APIAddr returns the loopback host:port of the daemon's REST API (valid
// after Start succeeds).
func (s *Supervisor) APIAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apiAddr
}

// APIKey returns the msgbrowse-generated REST API key (valid after Start
// succeeds).
func (s *Supervisor) APIKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.apiKey
}

// Client returns a REST client bound to the supervised daemon.
func (s *Supervisor) Client() *Client {
	return NewClient(s.APIAddr(), s.APIKey())
}

// Start verifies the binary, generates config, launches the daemon, and
// returns once its REST API answers (or with a typed error; on any failure
// no child is left running). ctx governs the daemon's whole lifetime:
// cancelling it triggers the clean shutdown sequence, after which Wait
// returns. Start must be called at most once.
func (s *Supervisor) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return errors.New("syncthing supervisor: already started")
	}
	s.started = true
	s.mu.Unlock()

	// 1. Integrity/version probe BEFORE anything is launched (SPEC-0014
	//    "Tampered bundled binary refuses to launch").
	version, err := VerifyBinary(ctx, s.opts.BinPath, s.opts.PinnedVersion, s.probeRunner())
	if err != nil {
		return fmt.Errorf("device sync start failed: %w", err)
	}
	s.log.Info("syncthing binary verified", "path", s.opts.BinPath, "version", version)

	// 2. Provision the daemon home and API key.
	if err := os.MkdirAll(s.homeDir, 0o700); err != nil {
		return fmt.Errorf("device sync start failed: create syncthing home %s: %w", s.homeDir, err)
	}
	apiKey, err := loadOrCreateAPIKey(s.homeDir)
	if err != nil {
		return fmt.Errorf("device sync start failed: %w", err)
	}

	// 3. Resolve the loopback REST bind.
	apiAddr := s.opts.GUIAddr
	if apiAddr == "" {
		apiAddr, err = pickLoopbackAddr()
		if err != nil {
			return fmt.Errorf("device sync start failed: pick loopback REST port: %w", err)
		}
	}
	if err := requireLoopback(apiAddr); err != nil {
		return fmt.Errorf("device sync start failed: %w", err)
	}
	s.mu.Lock()
	s.apiAddr = apiAddr
	s.apiKey = apiKey
	s.mu.Unlock()

	// 4. Validate + prepare folders, then write the daemon's entire config.
	//    Validation is the no-DB-in-a-synced-folder guard: any path outside
	//    <data_dir>/archives/ is refused outright.
	for _, f := range s.opts.Folders {
		if err := ValidateManagedFolderPath(s.opts.DataDir, f.Path); err != nil {
			return fmt.Errorf("device sync start failed: folder %s: %w", f.ID, err)
		}
		if err := prepareFolder(f); err != nil {
			return fmt.Errorf("device sync start failed: %w", err)
		}
	}
	cfgXML, err := GenerateConfigXML(ConfigSpec{
		GUIAddress:    apiAddr,
		APIKey:        apiKey,
		ListenAddress: "tcp://" + s.opts.ListenAddr,
		Folders:       s.opts.Folders,
		Devices:       s.opts.Devices,
	})
	if err != nil {
		return fmt.Errorf("device sync start failed: %w", err)
	}
	if err := os.WriteFile(filepath.Join(s.homeDir, "config.xml"), cfgXML, 0o600); err != nil {
		return fmt.Errorf("device sync start failed: write syncthing config: %w", err)
	}

	// 5. Launch and confirm readiness. A child that dies or never answers
	//    within ReadyTimeout is a startup failure, and the child is torn
	//    down before returning — Start never leaks a half-started daemon.
	cmd, exited, err := s.spawn()
	if err != nil {
		return fmt.Errorf("device sync start failed: %w", err)
	}
	if err := s.waitReady(ctx, exited); err != nil {
		s.terminate(cmd, exited)
		return fmt.Errorf("device sync start failed: %w", err)
	}
	s.log.Info("syncthing daemon ready", "api_addr", apiAddr, "home", s.homeDir,
		"folders", len(s.opts.Folders), "devices", len(s.opts.Devices))

	// 6. Best-effort post-start ownership touches: name this node for peers.
	//    Failure is logged with context, never fatal and never silent
	//    (SPEC-0014 REQ "Error Handling Standards": explicit handling with a
	//    documented reason — a cosmetic rename must not kill sync).
	s.setOwnDeviceName(ctx)

	go s.supervise(ctx, cmd, exited)
	return nil
}

// Wait blocks until the supervision loop has fully drained after context
// cancellation (clean shutdown → nil) or an unrecoverable supervision
// failure (the error). It mirrors serve.go's worker Wait contract.
func (s *Supervisor) Wait() error { return <-s.done }

// spawn launches one Syncthing child with the msgbrowse-owned home dir and
// loopback REST bind, wiring its stdout/stderr into structured logs. It
// returns the running cmd and a channel that receives its exit error.
func (s *Supervisor) spawn() (*exec.Cmd, <-chan error, error) {
	// Flags verified against the pinned upstream's v2 `serve` CLI (the v1-era
	// --no-default-folder was removed in v2 and would abort startup; the
	// pre-written config.xml already means no default folder is created).
	args := []string{
		"serve",
		"--home=" + s.homeDir,
		"--no-browser",
		"--no-restart",
		"--gui-address=" + s.APIAddr(),
		"--gui-apikey=" + s.APIKey(),
	}
	cmd := exec.Command(s.opts.BinPath, args...)
	// STNOUPGRADE hard-disables self-upgrade (belt to the config's
	// autoUpgradeIntervalH=0 suspenders): the pinned binary only changes via
	// a msgbrowse release.
	cmd.Env = append(os.Environ(), "STNOUPGRADE=1")
	cmd.Env = append(cmd.Env, s.opts.extraEnv...)
	// Line-split the daemon's output into structured logs rather than
	// discarding it (SPEC-0014: "Syncthing's own stderr/stdout MUST be
	// captured into msgbrowse's structured logs"). exec.Cmd copies each
	// stream in its own goroutine and Wait joins them, so no pipe-ordering
	// hazard exists.
	cmd.Stdout = &lineLogger{log: s.log, stream: "stdout"}
	cmd.Stderr = &lineLogger{log: s.log, stream: "stderr"}

	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start syncthing child: %w", err)
	}
	s.log.Info("syncthing child started", "pid", cmd.Process.Pid, "bin", s.opts.BinPath)
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	return cmd, exited, nil
}

// waitReady polls the REST ping until the daemon answers, the child exits,
// or the ready timeout / context expires.
func (s *Supervisor) waitReady(ctx context.Context, exited <-chan error) error {
	client := s.Client()
	deadline := time.NewTimer(s.opts.ReadyTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: cancelled while waiting for readiness: %v", ErrNotRunning, ctx.Err())
		case err := <-exited:
			return fmt.Errorf("%w: syncthing exited before becoming ready: %v", ErrNotRunning, err)
		case <-deadline.C:
			return fmt.Errorf("%w: syncthing REST API did not answer within %s", ErrNotRunning, s.opts.ReadyTimeout)
		case <-tick.C:
			pingCtx, cancel := context.WithTimeout(ctx, time.Second)
			err := client.Ping(pingCtx)
			cancel()
			if err == nil {
				return nil
			}
		}
	}
}

// supervise owns the child from readiness to teardown: on context
// cancellation it runs the SIGTERM→grace→kill sequence and reports a clean
// nil through done; on unexpected exit it restarts the daemon with capped
// exponential backoff (SPEC-0014 "restart it with backoff if it exits
// unexpectedly while sync remains enabled"). A failure to respawn is
// unrecoverable and surfaces through Wait — never swallowed.
func (s *Supervisor) supervise(ctx context.Context, cmd *exec.Cmd, exited <-chan error) {
	backoff := defaultBackoffMin
	for {
		started := time.Now()
		select {
		case <-ctx.Done():
			s.terminate(cmd, exited)
			s.done <- nil
			return
		case err := <-exited:
			if ctx.Err() != nil {
				// Cancellation raced the exit; either way the child is gone.
				s.done <- nil
				return
			}
			s.log.Error("syncthing exited unexpectedly; restarting",
				"error", err, "uptime", time.Since(started).Round(time.Millisecond), "backoff", backoff)
			if time.Since(started) >= stableUptime {
				backoff = defaultBackoffMin
			}
			select {
			case <-ctx.Done():
				s.done <- nil
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, defaultBackoffMax)
			var spawnErr error
			cmd, exited, spawnErr = s.spawn()
			if spawnErr != nil {
				s.done <- fmt.Errorf("device sync supervision failed: respawn syncthing: %w", spawnErr)
				return
			}
		}
	}
}

// terminate runs the clean-shutdown sequence on a live child: SIGTERM (the
// daemon persists state and exits), then Kill after the grace period. It
// always drains the exit channel, so the child is fully reaped — no orphan,
// no zombie (SPEC-0014 "App quit stops the daemon").
func (s *Supervisor) terminate(cmd *exec.Cmd, exited <-chan error) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		// Already exited and reaped (the exit channel may even be drained —
		// waitReady consumes it when the child dies before readiness), so
		// there is nothing to wait for; the buffered channel never blocks
		// its sender.
		return
	}
	select {
	case err := <-exited:
		s.log.Info("syncthing stopped", "error", errText(err))
	case <-time.After(s.opts.Grace):
		s.log.Warn("syncthing did not exit within grace period; killing", "grace", s.opts.Grace)
		_ = cmd.Process.Kill()
		<-exited
	}
}

// setOwnDeviceName renames this node's own device entry so paired peers see
// a friendly name instead of a truncated device ID. Best-effort by design:
// a failure is logged with full context and sync proceeds — the rename is
// cosmetic and must not take the engine down (documented suppression per
// SPEC-0014 REQ "Error Handling Standards").
func (s *Supervisor) setOwnDeviceName(ctx context.Context) {
	name := s.opts.DeviceName
	if name == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			s.log.Warn("skip device rename: no device name and hostname unavailable", "error", errText(err))
			return
		}
		name = host
	}
	opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	client := s.Client()
	status, err := client.SystemStatus(opCtx)
	if err != nil {
		s.log.Warn("skip device rename: read own device id", "error", err)
		return
	}
	if err := client.PatchDevice(opCtx, status.MyID, map[string]any{"name": name}); err != nil {
		s.log.Warn("skip device rename: patch own device", "device_id", status.MyID, "name", name, "error", err)
		return
	}
	s.log.Info("device name set", "device_id", status.MyID, "name", name)
}

// probeRunner returns the Runner for the integrity probe: the injected test
// runner when set; otherwise, when extraEnv is present (tests routing the
// fake daemon), a runner that carries that env so the probe execs the same
// fake the spawn will; otherwise nil (VerifyBinary's real process runner).
func (s *Supervisor) probeRunner() Runner {
	if s.opts.verifyRunner != nil {
		return s.opts.verifyRunner
	}
	if len(s.opts.extraEnv) == 0 {
		return nil
	}
	env := append(os.Environ(), s.opts.extraEnv...)
	return func(ctx context.Context, name string, args ...string) ([]byte, error) {
		var buf bytes.Buffer
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = env
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		err := cmd.Run()
		return buf.Bytes(), err
	}
}

// pickLoopbackAddr reserves an ephemeral loopback port for the REST bind by
// binding and releasing it. The tiny bind race is acceptable: the daemon
// binds moments later, and a collision surfaces as a readiness failure with
// a clear error rather than anything silent.
func pickLoopbackAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		return "", err
	}
	return addr, nil
}

// lineLogger adapts a child stream to structured logging, splitting on
// newlines. Syncthing terminates every log line, so buffered partials only
// exist between writes; a final unterminated fragment at process death is
// retained but not emitted (acceptable: it cannot be a complete message).
// One instance per stream; exec.Cmd serializes writes per stream.
type lineLogger struct {
	log    *slog.Logger
	stream string
	buf    bytes.Buffer
}

func (l *lineLogger) Write(p []byte) (int, error) {
	l.buf.Write(p)
	for {
		line, err := l.buf.ReadString('\n')
		if err != nil {
			// No full line yet; keep the partial for the next write.
			l.buf.WriteString(line)
			break
		}
		if line = trimEOL(line); line != "" {
			l.log.Info("syncthing output", "stream", l.stream, "line", line)
		}
	}
	return len(p), nil
}

func trimEOL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
