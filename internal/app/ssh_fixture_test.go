//go:build !windows

package app

// Controlled loopback sshd fixture.
//
// The fixture starts an unprivileged `sshd` on a free loopback port and
// prepares an isolated SSH home that points to that sshd. It generates
// host and client keys, an `authorized_keys`, and an SSH config alias,
// then execs the system `ssh` and `ssh-keygen` binaries using only the
// configured alias. The fixture never touches a real user home, real
// keyring, or real Vci state.
//
// All Vci SSH integration tests obtain their fixture through Fixture and
// the T.Cleanup chain ensures every sshd process, port, temp dir, and
// home is removed when the test exits, regardless of pass or fail.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// SSHFixture is a transient, isolated, loopback-only SSH server that a
// real-process integration test can use to execute the compiled Vci
// binary through the system `ssh` executable over an ordinary SSH
// config alias. The fixture owns its host key, client key, authorized
// keys file, ssh_config alias, listening port, sshd process, and an
// isolated user home; it never inspects or references a real user
// account.
type SSHFixture struct {
	t       *testing.T
	homeDir string
	binDir  string
	port    int
	alias   string
	config  string
	// coordinatorRoot is the VCI root the remote sshd session sees.
	// It lives directly under f.homeDir (so the remote `vci`
	// resolves it via SetEnv HOME) because macOS's OpenSSH silently
	// ignores SetEnv keys with underscores (observed in OpenSSH
	// 10.x); keeping the coordinator at HOME/.vci avoids the need
	// to push a separate VCI_ROOT across the SSH boundary.
	coordinatorRoot string
	buildDir        string
	binary          string
	sshd            *exec.Cmd
	sshdCancel      context.CancelFunc
	// sshdExited is closed once the foreground sshd listener process
	// (started with -D) has been waited on. shutdown selects on it so
	// cleanup returns only after the listener and its process group
	// are truly gone and their ports released.
	sshdExited chan struct{}
}

// NewSSHFixture prepares and starts an isolated loopback sshd. The
// fixture skips (not fails) the calling test when ssh, sshd, or
// ssh-keygen is unavailable. Cleanup is registered automatically on the
// returned fixture.
func NewSSHFixture(t *testing.T) *SSHFixture {
	t.Helper()
	fixture := &SSHFixture{t: t}
	fixture.setupSkip()
	fixture.layout()
	fixture.binaries()
	fixture.port = pickLoopbackPort(t)
	fixture.writeConfig()
	fixture.writeKeys()
	fixture.bootstrapCoordinator()
	fixture.writeSSHHelper()
	fixture.buildBinary()
	fixture.startSSHD()
	t.Cleanup(fixture.shutdown)
	// Safety net: if the test process dies before t.Cleanup runs
	// (signal, panic, OOM, or ProcessGroup-only kill), the sshd
	// listener and its unprivileged worker children can leak. The
	// per-uid process limit on macOS is low enough that hundreds of
	// leaked listeners make the development machine unable to fork
	// new processes. Schedule a deferred go-routine that watches
	// the parent test pid and kills the sshd process group if the
	// test has not reached cleanup.
	watcherStop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-watcherStop:
				return
			case <-ticker.C:
				if !testProcessAlive(fixture.t) {
					fixture.shutdown()
					return
				}
			}
		}
	}()
	t.Cleanup(func() { close(watcherStop) })
	prependTestPath(t, fixture.binDir)
	return fixture
}

// prependTestPath inserts dir at the front of the parent process PATH
// for the duration of one test; restoration is registered with t.Cleanup
// so a t.Fatalf path also restores.
func prependTestPath(t *testing.T, dir string) {
	t.Helper()
	fixtureMutex.Lock()
	previous := os.Getenv("PATH")
	os.Setenv("PATH", dir+string(os.PathListSeparator)+previous)
	t.Cleanup(func() {
		os.Setenv("PATH", previous)
		fixtureMutex.Unlock()
	})
}

var fixtureMutex sync.Mutex

// SSHAlias returns the SSH config alias the fixture inserted into its
// isolated home. Callers invoke `ssh <fixture.SSHAlias()>` from `Exec`
// or by hand; the fixture's UserHome + configFile makes that alias
// resolve to a loopback port with the fixture's identity.
func (f *SSHFixture) SSHAlias() string { return f.alias }

// CoordinatorRoot returns the VCI root that the running coordinator
// binary reads under `VCI_ROOT`. Tests that write coordinator config or
// secrets point at this path on the remote, where they have no effect
// on the host's real ~/.vci.
func (f *SSHFixture) CoordinatorRoot() string { return f.coordinatorRoot }

// BinaryPath returns the path to the compiled Vci binary the fixture
// placed in PATH for the sshd session.
func (f *SSHFixture) BinaryPath() string { return f.binary }

// ExecSSHCommand runs `ssh <alias> <command...>` through the system
// ssh with the fixture's HOME and config. ctx is honored for
// cancellation. stdout and stderr capture the same output an ordinary
// ssh invocation would.
func (f *SSHFixture) ExecSSHCommand(ctx context.Context, command string, args ...string) ([]byte, []byte, error) {
	fullArgs := []string{
		"-F", f.config,
		"-i", filepath.Join(f.homeDir, ".ssh", "id_ed25519"),
		"-o", "BatchMode=yes",
		"-o", "LogLevel=ERROR",
		f.alias, command,
	}
	fullArgs = append(fullArgs, args...)
	cmd := exec.CommandContext(ctx, "ssh", fullArgs...)
	cmd.Env = append(os.Environ(),
		"HOME="+f.homeDir,
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return []byte(stdout.String()), []byte(stderr.String()), err
}

// ExecSSHVerbose runs an explicit ssh command with -v logging to a
// capture buffer. Used only by debug tests; production tests use
// ExecSSHCommand.
func (f *SSHFixture) ExecSSHVerbose(ctx context.Context, command string, args ...string) ([]byte, []byte, error) {
	fullArgs := []string{
		"-F", f.config,
		"-i", filepath.Join(f.homeDir, ".ssh", "id_ed25519"),
		f.alias, command,
	}
	fullArgs = append(fullArgs, args...)
	cmd := exec.CommandContext(ctx, "ssh", fullArgs...)
	cmd.Env = append(os.Environ(),
		"HOME="+f.homeDir,
	)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return []byte(stdout.String()), []byte(stderr.String()), err
}

// setupSkip skips the calling test when one of the three required
// tools is missing. The skip message names the precise missing tool.
func (f *SSHFixture) setupSkip() {
	for _, name := range []string{"ssh", "sshd", "ssh-keygen"} {
		if _, err := exec.LookPath(name); err != nil {
			f.t.Skipf("controlled ssh fixture: %s is not available: %v", name, err)
		}
	}
}

// layout allocates an isolated temp directory that owns the entire
// fixture: a fake $HOME for the ssh session, a fake bin dir that the
// remote PATH prepends with the compiled vci binary, a coordinator
// root directly under that fake $HOME so the remote `vci` finds it
// via the standard HOME-based default layout, and a build directory
// for the binary.
func (f *SSHFixture) layout() {
	base := f.t.TempDir()
	f.homeDir = filepath.Join(base, "home")
	f.binDir = filepath.Join(base, "bin")
	// coordinatorRoot sits under f.homeDir so the remote vci finds
	// it at $HOME/.vci without needing a separate VCI_ROOT env. The
	// SSH boundary pushes HOME across, but not VCI_ROOT (macOS
	// OpenSSH 10.x silently drops SetEnv entries with
	// underscores).
	f.coordinatorRoot = filepath.Join(f.homeDir, ".vci")
	f.buildDir = filepath.Join(base, "build")
	for _, dir := range []string{f.homeDir, filepath.Join(f.homeDir, ".ssh"), f.binDir, f.buildDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			f.t.Fatalf("fixture mkdir %s: %v", dir, err)
		}
	}
}

// binaries checks for the tools this fixture requires. setupSkip has
// already short-circuited on missing tools.
func (f *SSHFixture) binaries() {
	for _, name := range []string{"ssh", "sshd", "ssh-keygen"} {
		if _, err := exec.LookPath(name); err != nil {
			f.t.Fatalf("fixture: required tool %s missing after skip: %v", name, err)
		}
	}
}

// testProcessAlive reports whether the parent test process is still
// running. The safety-net goroutine uses this to detect a test that
// died without running t.Cleanup and triggers shutdown so sshd
// listeners are not leaked.
func testProcessAlive(t *testing.T) bool {
	t.Helper()
	pid := os.Getpid()
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 is the standard "is this pid alive?" probe.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return true
}

// pickLoopbackPort opens a listener on an unused loopback port, drops
// it, and returns the port number. The race window between closing and
// sshd binding is acceptable: if another process grabs the port, the
// sshd start will fail and the test will surface a precise error.
func pickLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fixture: pick loopback port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// writeConfig writes a minimal ssh_config that aliases the chosen
// port and pins the user key to the fixture's private key. The
// fixture sets HOME so the config is auto-loaded. The login user is
// the current test uid; sshd on macOS rejects users it cannot resolve
// to a real account even on loopback. SendEnv forwards PATH and
// VCI_ROOT into the remote session so the fixture's bin dir is
// honored when the client invokes the public vci command.
func (f *SSHFixture) writeConfig() {
	f.alias = "vci-fixture"
	cfg := filepath.Join(f.homeDir, ".ssh", "config")
	key := filepath.Join(f.homeDir, ".ssh", "id_ed25519")
	user := currentUser(f.t)
	body := fmt.Sprintf(`Host %s
  HostName 127.0.0.1
  Port %d
  User %s
  StrictHostKeyChecking no
  UserKnownHostsFile %s
  IdentityFile %s
  IdentitiesOnly yes
  LogLevel ERROR
  PreferredAuthentications publickey
`, f.alias, f.port, user, filepath.Join(f.homeDir, ".ssh", "known_hosts"), key)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		f.t.Fatalf("fixture: write ssh_config: %v", err)
	}
	f.config = cfg
	if data, err := os.ReadFile(cfg); err == nil {
		f.t.Logf("fixture ssh_config:\n%s", data)
	}
}

// currentUser returns the unix user that owns the running test. The
// fixture's SSH alias uses it as the login name because sshd rejects
// loopback logins for unknown users.
func currentUser(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("whoami")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("fixture: whoami: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// writeKeys generates an ed25519 host key and an ed25519 client key,
// pins the host key into the fixture's known_hosts, and adds the
// client's public key to its own authorized_keys. ssh-keygen is
// forced into an empty passphrase and a non-default filename so it
// never collides with the developer's real keyring.
func (f *SSHFixture) writeKeys() {
	sshDir := filepath.Join(f.homeDir, ".ssh")
	hostKey := filepath.Join(sshDir, "host_ed25519")
	clientKey := filepath.Join(sshDir, "id_ed25519")
	sshKeygen := func(path string) {
		cmd := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", path, "-C", "vci-fixture")
		out, err := cmd.CombinedOutput()
		if err != nil {
			f.t.Fatalf("fixture: ssh-keygen %s failed: %v\n%s", path, err, out)
		}
	}
	sshKeygen(hostKey)
	sshKeygen(clientKey)

	authKeys := filepath.Join(sshDir, "authorized_keys")
	clientPub, err := os.ReadFile(clientKey + ".pub")
	if err != nil {
		f.t.Fatalf("fixture: read client pubkey: %v", err)
	}
	if err := os.WriteFile(authKeys, clientPub, 0o600); err != nil {
		f.t.Fatalf("fixture: write authorized_keys: %v", err)
	}

	// Pre-seed known_hosts with the host's public key so the alias
	// resolves without SSH asking the user to trust the host.
	hostPub, err := os.ReadFile(hostKey + ".pub")
	if err != nil {
		f.t.Fatalf("fixture: read host pubkey: %v", err)
	}
	known := fmt.Sprintf("[127.0.0.1]:%d %s\n", f.port, strings.TrimSpace(string(hostPub)))
	if err := os.WriteFile(filepath.Join(sshDir, "known_hosts"), []byte(known), 0o600); err != nil {
		f.t.Fatalf("fixture: write known_hosts: %v", err)
	}
}

// bootstrapCoordinator initializes a coordinator Vci root inside the
// fixture with orchestrator = "self" so a remote SSH command run by
// the fixture can present credentials and see a fully-initialized
// home.
func (f *SSHFixture) bootstrapCoordinator() {
	cfgPath := filepath.Join(f.coordinatorRoot, "config.toml")
	body := "schema_version = 1\norchestrator = \"self\"\n\n[log_limits]\nstdout_bytes = 4194304\nstderr_bytes = 4194304\n\n[retention]\nmax_bytes = 536870912\n\n[machines.mac-local]\n"
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		f.t.Fatalf("fixture: mkdir coordinator root: %v", err)
	}
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		f.t.Fatalf("fixture: write coordinator config: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(f.coordinatorRoot, "state", "tmp"), 0o700); err != nil {
		f.t.Fatalf("fixture: mkdir state/tmp: %v", err)
	}
	for _, sub := range []string{"runs", "sources/blobs", "sources/manifests", "work", "locks"} {
		if err := os.MkdirAll(filepath.Join(f.coordinatorRoot, "state", sub), 0o700); err != nil {
			f.t.Fatalf("fixture: mkdir state/%s: %v", sub, err)
		}
	}
}

// writeSSHHelper drops a minimal `ssh` shell helper into the fixture's bin dir.
// macOS OpenSSH (/usr/bin/ssh) resolves ~/.ssh/config from getpwuid(3)
// rather than $HOME, ignoring process HOME overrides. The helper passes
// -F <config> and -i <key> so tests can exercise system SSH against loopback sshd.
func (f *SSHFixture) writeSSHHelper() {
	helperPath := filepath.Join(f.binDir, "ssh")
	script := fmt.Sprintf("#!/bin/sh\nexec /usr/bin/ssh -F %q -i %q \"$@\"\n",
		f.config,
		filepath.Join(f.homeDir, ".ssh", "id_ed25519"),
	)
	if err := os.WriteFile(helperPath, []byte(script), 0o755); err != nil {
		f.t.Fatalf("fixture: write ssh helper: %v", err)
	}
}

// buildBinary compiles the vci binary once for the fixture and
// installs it into the fixture's `$HOME/bin/vci` (remote) and
// `$HOME/client-bin/vci` (local client).
func (f *SSHFixture) buildBinary() {
	homeBin := filepath.Join(f.homeDir, "bin")
	clientBin := filepath.Join(f.homeDir, "client-bin")
	if err := os.MkdirAll(homeBin, 0o700); err != nil {
		f.t.Fatalf("fixture: mkdir $HOME/bin: %v", err)
	}
	if err := os.MkdirAll(clientBin, 0o700); err != nil {
		f.t.Fatalf("fixture: mkdir $HOME/client-bin: %v", err)
	}
	remoteVci := filepath.Join(homeBin, "vci")
	localVci := filepath.Join(clientBin, "vci")
	cmd := exec.Command("go", "build", "-o", remoteVci, "./cmd/vci")
	cmd.Dir = repoRoot(f.t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("fixture: build vci: %v\n%s", err, out)
	}
	if err := os.Chmod(remoteVci, 0o700); err != nil {
		f.t.Fatalf("fixture: chmod vci: %v", err)
	}
	if err := copyFile(remoteVci, localVci, 0o755); err != nil {
		f.t.Fatalf("fixture: copy client vci: %v", err)
	}
	f.binary = localVci
}

func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, mode); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

// startSSHD writes an sshd_config that pins ChrootDirectory=off,
// forces a private key-only auth, points the remote session at the
// fixture's home, and starts sshd as the current uid. Only HOME is
// set; the remote vci finds its config under $HOME/.vci because
// macOS OpenSSH silently drops sshd's SetEnv entries whose names
// contain underscores (observed in OpenSSH 10.x). The fixture does
// not push VCI_ROOT across SSH at all.
func (f *SSHFixture) startSSHD() {
	sshDir := filepath.Join(f.homeDir, ".ssh")
	hostKey := filepath.Join(sshDir, "host_ed25519")
	cfg := filepath.Join(sshDir, "sshd_config")
	portStr := fmt.Sprintf("%d", f.port)
	// Listen on loopback only so the fixture is never reachable
	// from another machine. UseLoginPrivilegePath is omitted so the
	// user's normal login path handles the vci-detached worker. The
	// test doesn't authenticate as another user so PAM can stay off.
	body := fmt.Sprintf(`Port %s
ListenAddress 127.0.0.1
HostKey %s
PidFile %s
AuthorizedKeysFile %s
PasswordAuthentication no
ChallengeResponseAuthentication no
UsePAM no
PermitRootLogin no
StrictModes no
AcceptEnv LANG LC_* PATH
SetEnv HOME=%s
Subsystem sftp /usr/libexec/sftp-server
`, portStr, hostKey, filepath.Join(sshDir, "sshd.pid"), filepath.Join(sshDir, "authorized_keys"), f.homeDir)
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		f.t.Fatalf("fixture: write sshd_config: %v", err)
	}
	// Write `.zshenv` so non-interactive remote shells prepend the
	// fixture's bin dir to PATH without an interactive PATH reset.
	zshEnv := filepath.Join(f.homeDir, ".zshenv")
	zshBody := "export PATH=\"$HOME/bin:$PATH\"\n"
	if err := os.WriteFile(zshEnv, []byte(zshBody), 0o644); err != nil {
		f.t.Fatalf("fixture: write .zshenv: %v", err)
	}
	// Same trick for bash in case the test host's sshd flips the
	// login shell to bash for any reason.
	bashProfile := filepath.Join(f.homeDir, ".bashrc")
	bashBody := "export PATH=\"$HOME/bin:$PATH\"\n"
	if err := os.WriteFile(bashProfile, []byte(bashBody), 0o644); err != nil {
		f.t.Fatalf("fixture: write .bashrc: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	f.sshdCancel = cancel
	sshdPath, err := exec.LookPath("sshd")
	if err != nil {
		f.t.Fatalf("fixture: locate sshd: %v", err)
	}
	// -D keeps sshd in the foreground. Without it, macOS OpenSSH
	// daemonizes: the exec'd process forks a detached listener and
	// exits immediately, so by the time shutdown runs the tracked
	// process is gone, Getpgid fails with ESRCH, and the real
	// listener leaks forever (holding its port and keeping the
	// session's build workers alive to race t.TempDir cleanup). With
	// -D the exec'd process IS the listener, so shutdown's
	// process-group kill and Wait actually terminate it before the
	// next test binds.
	cmd := exec.CommandContext(ctx, sshdPath, "-D", "-f", cfg, "-E", filepath.Join(sshDir, "sshd.log"))
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	// Run sshd in its own process group so shutdown can kill any
	// forked worker processes too. Without this, an abrupt test
	// termination leaks every sshd listener as an orphan, and
	// subsequent tests cannot fork because the per-uid process
	// limit (kern.maxprocperuid) is exhausted.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// sshd now lives for the whole test, so Start (not
	// CombinedOutput) is required; startup errors land in stderrBuf
	// and are surfaced by the readiness poll below.
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	if err := cmd.Start(); err != nil {
		f.t.Fatalf("fixture: sshd start: %v", err)
	}
	f.sshd = cmd
	f.sshdExited = make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(f.sshdExited)
	}()

	// Wait until the listener accepts a connection so the alias has
	// something real to talk to.
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case <-f.sshdExited:
			log, _ := os.ReadFile(filepath.Join(sshDir, "sshd.log"))
			f.t.Fatalf("fixture: sshd exited during startup: %s\nsshd log:\n%s", strings.TrimSpace(stderrBuf.String()), log)
		default:
		}
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", f.port), 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		if time.Now().After(deadline) {
			log, _ := os.ReadFile(filepath.Join(sshDir, "sshd.log"))
			f.t.Fatalf("fixture: sshd never accepted loopback connection; log:\n%s", log)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// shutdown stops sshd, removes pid and known hosts, and any other
// files written by the fixture. t.Cleanup calls it always.
func (f *SSHFixture) shutdown() {
	if f.sshdCancel != nil {
		f.sshdCancel()
	}
	if f.sshd != nil && f.sshd.Process != nil {
		// Kill the entire process group: sshd forks a privileged
		// listener plus unprivileged child workers for each
		// connection, and the children are not in the parent's
		// process group unless sshd was started with Setpgid. Kill
		// the group by sending SIGTERM to -pgid. A second SIGKILL
		// is the safety net for any workers that ignore SIGTERM.
		pgid, err := syscall.Getpgid(f.sshd.Process.Pid)
		if err == nil {
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
		}
		done := f.sshdExited
		if done == nil {
			done = make(chan struct{})
			go func() {
				_ = f.sshd.Wait()
				close(done)
			}()
		}
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			if err == nil {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			} else {
				_ = f.sshd.Process.Kill()
			}
			<-done
		}
	}
	// The detached build worker (`vci internal-run <run-id>`) is
	// spawned with setsid (internal/cli/spawn.go) and lives in its
	// own session, so it survives the sshd process-group kill above
	// and keeps writing run records, leases, and the scheduler lock
	// into the fixture home while t.TempDir cleanup races it. Kill
	// any remaining fixture-owned process and wait for it to exit so
	// the fixture home is quiescent before this cleanup returns.
	f.killRemainingFixtureProcesses()
}

// killRemainingFixtureProcesses terminates every process that still
// references the fixture home after the sshd process group has been
// killed. The remote build worker escapes the sshd group via setsid
// (internal/cli/spawn.go spawnRun), and its argv[0] is the
// fixture-built `vci` binary under the fixture home, so the process
// table is the reliable place to find it. SIGTERM first, escalate to
// SIGKILL after a bounded grace, and return only once no fixture
// process remains so the fixture home is quiescent before the test's
// t.TempDir removal runs.
func (f *SSHFixture) killRemainingFixtureProcesses() {
	termDeadline := time.Now().Add(2 * time.Second)
	hardDeadline := termDeadline.Add(2 * time.Second)
	for {
		pids := fixtureProcessPIDs(f.homeDir)
		if len(pids) == 0 {
			return
		}
		sig := syscall.SIGTERM
		if time.Now().After(termDeadline) {
			sig = syscall.SIGKILL
		}
		for _, pid := range pids {
			if proc, err := os.FindProcess(pid); err == nil {
				_ = proc.Signal(sig)
			}
		}
		if time.Now().After(hardDeadline) {
			// Absolute bound; the sweep is best-effort from here.
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// fixtureProcessPIDs lists the pids of processes whose command line
// references the fixture home. Every process the fixture owns starts
// with a path under the home (sshd config, remote coordinator shells,
// and the detached worker's binary), so a match is safe to terminate.
func fixtureProcessPIDs(homeDir string) []int {
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid == self {
			continue
		}
		if strings.Contains(strings.Join(fields[1:], " "), homeDir) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// waitForSSHRoundtrip runs `true` through the SSH alias until the
// sshd loopback accepts connections and the public key exchange
// completes. Returns nil when a round trip succeeds; surfaces the
// last error when the deadline elapses. ssh-key-set-up + listen-loop
// races already taken into account by fixture startSSHD; this gives
// sshd a moment to settle before relying on it. On failure, the
// fixture dumps sshd.log, ssh client stderr, and any private state to
// the calling test so the cause is precise.
func (f *SSHFixture) waitForSSHRoundtrip(ctx context.Context) error {
	var lastErr error
	var lastStderr []byte
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, stderr, err := f.ExecSSHCommand(ctx, "true")
		if err == nil {
			return nil
		}
		lastErr = err
		lastStderr = stderr
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = errors.New("ssh roundtrip failed without an error")
	}
	log, _ := os.ReadFile(filepath.Join(f.homeDir, ".ssh", "sshd.log"))
	f.t.Fatalf("controlled SSH roundtrip failed: %v\nsshd log:\n%s\nssh stderr:\n%s", lastErr, log, lastStderr)
	return lastErr
}

// repoRoot returns the absolute path to the module root as discovered
// by `git rev-parse --show-toplevel`. The integration test fixture
// builds the vci binary from the module root regardless of which file
// the test was launched from.
func repoRoot(t *testing.T) string {
	t.Helper()
	if dir, err := os.Getwd(); err == nil {
		for d := dir; d != "/" && d != "."; d = filepath.Dir(d) {
			if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
				return d
			}
		}
	}
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("fixture: git rev-parse: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// Controlled-sshd smoke test.
//
// One narrow assertion: the system ssh executable, driven only by
// ordinary SSH config + an SSH alias, executes one public vci JSON
// command on a coordinator root with `orchestrator = "self"`, and the
// response decodes as a Vci envelope.

// TestVciMachinesOverControlledSSHD executes the public
// coordinator command `vci machines` through ordinary system ssh +
// sshd on a loopback port generated by the fixture.
func TestVciMachinesOverControlledSSHD(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("controlled SSH: ssh roundtrip failed: %v", err)
	}

	wrapCmd := "PATH=" + fixture.binDir + ":$PATH vci machines"
	stdout, stderr, err := fixture.ExecSSHCommand(ctx, wrapCmd)
	if err != nil {
		t.Fatalf("ssh session exited non-zero: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}

	var envelope struct {
		SchemaVersion int             `json:"schema_version"`
		OK            bool            `json:"ok"`
		Command       string          `json:"command"`
		Data          json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(stdout, &envelope); err != nil {
		t.Fatalf("ssh session produced non-JSON stdout (%v):\n%s", err, stdout)
	}
	if envelope.SchemaVersion != 1 {
		t.Fatalf("unexpected schema_version %d in stdout:\n%s", envelope.SchemaVersion, stdout)
	}
	if envelope.Command != "machines" {
		t.Fatalf("expected command=machines, got %q in stdout:\n%s", envelope.Command, stdout)
	}
	if !envelope.OK {
		t.Fatalf("envelope not ok:\n%s", stdout)
	}
}
