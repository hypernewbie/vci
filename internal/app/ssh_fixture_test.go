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
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
	cmd := exec.CommandContext(ctx, sshdPath, "-f", cfg, "-E", filepath.Join(sshDir, "sshd.log"))
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	if out, err := cmd.CombinedOutput(); err != nil {
		f.t.Fatalf("fixture: sshd start: %v\n%s", err, out)
	}
	f.sshd = cmd

	// Wait until the listener accepts a connection so the alias has
	// something real to talk to.
	deadline := time.Now().Add(5 * time.Second)
	for {
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
		_ = f.sshd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() {
			_ = f.sshd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = f.sshd.Process.Kill()
			<-done
		}
	}
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
