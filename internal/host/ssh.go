// Package host transfers workspaces over SSH and runs commands remotely.
// It stages workspace state and handles remote execution via system tools.
package host

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/process"
)

// ValidateHost checks the SSH host argument before use.
func ValidateHost(host string) error {
	if host == "" {
		return fmt.Errorf("remote host is empty")
	}
	return config.ValidateMachineHost(host)
}

// ValidateRemotePath checks remote paths are valid `~/.vci/state/work/<run_id>` values.
func ValidateRemotePath(p string) error {
	if p == "" {
		return fmt.Errorf("remote path is empty")
	}
	if strings.HasPrefix(p, "-") {
		return fmt.Errorf("remote path %q looks like an option flag", p)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("remote path %q contains control characters", p)
		}
	}
	segments := strings.Split(p, "/")
	if len(segments) != 5 {
		return fmt.Errorf("remote path %q is not under ~/.vci/state/work", p)
	}
	for i, fixed := range []string{"~", ".vci", "state", "work"} {
		if segments[i] != fixed {
			return fmt.Errorf("remote path %q is not under ~/.vci/state/work", p)
		}
	}
	if !model.ValidName(segments[4]) {
		return fmt.Errorf("remote path %q has invalid run segment %q", p, segments[4])
	}
	return nil
}

// RemoteWorkDir returns the remote `~/.vci/state/work/<run>` path for a run.
// Layout matches the coordinator's local work directory layout.
func RemoteWorkDir(id model.RunID) (string, error) {
	if !model.ValidRunID(id) {
		return "", fmt.Errorf("invalid run id %q", id)
	}
	return "~/.vci/state/work/" + string(id), nil
}

// Client runs remote commands over SSH through a process.Runner.
// Callers must set Runner; the zero value has no runner to invoke.
type Client struct {
	Runner process.Runner
}

// RunRemote executes a remote command over SSH.
// It returns the remote exit code; transport failures return an error.
func (c Client) RunRemote(ctx context.Context, host, workDir string, argv []string, env map[string]string, stdout, stderr io.Writer) (int, error) {
	if err := ValidateHost(host); err != nil {
		return 0, err
	}
	shell, err := composeShell(workDir, argv, env)
	if err != nil {
		return 0, err
	}
	if c.Runner == nil {
		return 0, fmt.Errorf("ssh runner is required")
	}
	result, err := c.Runner.Run(ctx, process.Command{Executable: "ssh", Args: []string{host, shell}, Stdout: stdout, Stderr: stderr})
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if err != nil {
		if result.ExitCode != 0 {
			return result.ExitCode, nil
		}
		return 0, fmt.Errorf("ssh %s: %w", host, err)
	}
	return result.ExitCode, nil
}

// RunRemote executes a remote command over SSH with the native runner.
func RunRemote(ctx context.Context, host, workDir string, argv []string, env map[string]string, stdout, stderr io.Writer) (int, error) {
	return (Client{Runner: process.Native{}}).RunRemote(ctx, host, workDir, argv, env, stdout, stderr)
}

// ProbeSeedHead asks a remote machine which commit is checked out at
// its seed path, running `ssh <host> git -C <seed> rev-parse HEAD`
// through the runner, where the seed is rendered as a `$HOME`-rooted
// shell word. It returns the trimmed stdout on success. A
// nonzero remote exit means the seed is not a Git checkout and yields
// an empty head; only ssh-level failures with no remote exit are
// reported as transport errors.
func (c Client) ProbeSeedHead(ctx context.Context, host, seed string) (string, error) {
	if err := ValidateHost(host); err != nil {
		return "", err
	}
	if err := validateSeed(seed); err != nil {
		return "", err
	}
	if c.Runner == nil {
		return "", fmt.Errorf("ssh runner is required")
	}
	var stdout bytes.Buffer
	result, err := c.Runner.Run(ctx, process.Command{
		Executable: "ssh",
		Args:       []string{host, "git", "-C", shellSeed(seed), "rev-parse", "HEAD"},
		Stdout:     &stdout,
	})
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil && result.ExitCode == 0 {
		return "", fmt.Errorf("ssh %s: %w", host, err)
	}
	if result.ExitCode != 0 {
		return "", nil
	}
	return strings.TrimSpace(stdout.String()), nil
}

// validateSeed rejects seed paths that could be misread as ssh option
// flags or carry shell-injected control bytes.
func validateSeed(seed string) error {
	if seed == "" {
		return fmt.Errorf("seed path is empty")
	}
	if strings.HasPrefix(seed, "-") {
		return fmt.Errorf("seed path %q looks like an option flag", seed)
	}
	for _, r := range seed {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("seed path %q contains control characters", seed)
		}
	}
	return nil
}

// StreamReconstruct sends a worker payload tar over SSH stdin to a remote
// reconstruction shell. The shell seeds a VCI-owned work directory from the
// machine's seed checkout with tar, imports the bundle the payload carries
// (when present), checks out the submitted head, applies the durable
// local-change archive, and removes the extracted payload. Any remote failure
// is an error so the caller can fall back to full workspace staging.
func (c Client) StreamReconstruct(ctx context.Context, host, seed, workDir string, payload io.Reader) error {
	if err := ValidateHost(host); err != nil {
		return err
	}
	if payload == nil {
		return fmt.Errorf("reconstruction payload is required")
	}
	if c.Runner == nil {
		return fmt.Errorf("ssh runner is required")
	}
	shell, err := reconstructShell(seed, workDir)
	if err != nil {
		return err
	}
	result, err := c.Runner.Run(ctx, process.Command{
		Executable: "ssh",
		Args:       []string{host, shell},
		Stdin:      payload,
	})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		if result.ExitCode != 0 {
			return fmt.Errorf("reconstruct %s: remote exit %d", host, result.ExitCode)
		}
		return fmt.Errorf("ssh %s: %w", host, err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("reconstruct %s: remote exit %d", host, result.ExitCode)
	}
	return nil
}

// StageRemote clears the validated remote work dir, then tars a local
// workspace and streams it to remoteWorkDir over SSH. The command uses
// tar data only, with no shell content from the workspace payload.
func StageRemote(ctx context.Context, host, remoteWorkDir, localWorkspace string) error {
	if err := ValidateHost(host); err != nil {
		return err
	}
	if err := ValidateRemotePath(remoteWorkDir); err != nil {
		return err
	}
	if localWorkspace == "" {
		return fmt.Errorf("local workspace is required")
	}
	info, err := os.Stat(localWorkspace)
	if err != nil {
		return fmt.Errorf("local workspace: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("local workspace is not a directory")
	}
	tarCmd := exec.CommandContext(ctx, "tar", "-cf", "-", "-C", localWorkspace, ".")
	sshCmd := exec.CommandContext(ctx, "ssh", host, "rm -rf "+remoteWorkDir+" && mkdir -p "+remoteWorkDir+" && cd "+remoteWorkDir+" && tar -xpf -")
	tarOut, err := tarCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("tar stdout: %w", err)
	}
	sshCmd.Stdin = tarOut
	var stderr bytes.Buffer
	sshCmd.Stderr = &stderr
	if err := tarCmd.Start(); err != nil {
		return fmt.Errorf("start tar: %w", err)
	}
	sshErr := sshCmd.Run()
	tarErr := tarCmd.Wait()
	if tarErr != nil {
		return fmt.Errorf("archive workspace: %w", tarErr)
	}
	if sshErr != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = sshErr.Error()
		}
		return fmt.Errorf("ssh %s: %s", host, message)
	}
	return nil
}

// FetchRemote copies remote workdir contents back to the local destination using scp.
// Command format: `scp -r -q <host>:<workDir> <localDest>`.
func FetchRemote(ctx context.Context, host, remoteWorkDir, localDest string) error {
	if err := ValidateHost(host); err != nil {
		return err
	}
	if err := ValidateRemotePath(remoteWorkDir); err != nil {
		return err
	}
	if localDest == "" {
		return fmt.Errorf("local fetch destination is required")
	}
	cmd := exec.CommandContext(ctx, "scp", "-r", "-q", host+":"+remoteWorkDir, localDest)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("scp %s: %s", host, message)
	}
	return nil
}

// reconstructShell builds the remote `sh -c` body that turns a streamed
// worker payload into a reconstructed workspace at workDir. The payload tar
// carries head, an optional bundle, and the durable lc.tar; the shell extracts
// it into a private dot-directory, seeds the workspace from the machine's
// checkout with tar, imports the bundle when present, checks out the submitted
// head, validates the complete lc.tar archive with `tar -tf` redirected to
// /dev/null, applies the tracked-change patch, restores untracked files by
// stripping the lc.tar "f/" prefix, and removes the payload directory. Every
// step is chained with && so any failure aborts the reconstruction instead of
// leaving a half-built workspace.
func reconstructShell(seed, workDir string) (string, error) {
	if err := ValidateRemotePath(workDir); err != nil {
		return "", err
	}
	if err := validateSeed(seed); err != nil {
		return "", err
	}
	return "mkdir -p " + workDir +
		" && cd " + workDir +
		" && mkdir -p .vci-payload" +
		" && tar -xf - -C .vci-payload" +
		" && tar -cf - -C " + shellSeed(seed) + " . | tar -xf -" +
		" && head=$(cat .vci-payload/head)" +
		" && if [ -s .vci-payload/bundle ]; then git bundle unbundle .vci-payload/bundle >/dev/null 2>&1; fi" +
		" && git checkout -q \"$head\"" +
		" && tar -tf .vci-payload/lc.tar >/dev/null" +
		" && if tar -xOf .vci-payload/lc.tar patch > .vci-payload/patch 2>/dev/null; then git apply --binary --whitespace=nowarn .vci-payload/patch; fi" +
		" && if tar -tf .vci-payload/lc.tar f/ >/dev/null 2>&1; then tar -xf .vci-payload/lc.tar -C . --strip-components=1 f/; fi" +
		" && rm -rf .vci-payload", nil
}

// shellSeed renders a seed path as a remote shell word. A leading `~/` is
// rewritten to `$HOME` so the tilde is never single-quoted and a suffix
// holding spaces or metacharacters stays intact; the suffix is single-quoted
// only when it is not shell-safe. Other seeds pass through unquoted when
// shell-safe.
func shellSeed(seed string) string {
	if strings.HasPrefix(seed, "~/") {
		suffix := strings.TrimPrefix(seed, "~")
		if shellSafe(suffix) {
			return "$HOME" + suffix
		}
		return "$HOME" + shellQuote(suffix)
	}
	if shellSafe(seed) {
		return seed
	}
	return shellQuote(seed)
}

// composeShell builds a single remote shell command.
// It validates the workDir, sets HOME/TMPDIR, applies env vars, and execs argv.
// It preserves safe `~`-based expansion for staged-workspace paths.
func composeShell(workDir string, argv []string, env map[string]string) (string, error) {
	if err := ValidateRemotePath(workDir); err != nil {
		return "", err
	}
	if len(argv) == 0 {
		return "", fmt.Errorf("remote argv is empty")
	}
	var b strings.Builder
	b.WriteString("cd ")
	b.WriteString(workDir)
	b.WriteString(" && __vci_login_home=$HOME && export HOME=")
	b.WriteString(workDir)
	b.WriteString("/.home TMPDIR=")
	b.WriteString(workDir)
	b.WriteString("/.tmp && mkdir -p ")
	b.WriteString(workDir)
	b.WriteString("/.home ")
	b.WriteString(workDir)
	b.WriteString("/.tmp")
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if key == "" {
			continue
		}
		b.WriteString(" && export ")
		b.WriteString(shellQuote(key))
		b.WriteString("=")
		b.WriteString(shellQuote(env[key]))
	}
	b.WriteString(" && exec ")
	for i, arg := range argv {
		if i > 0 {
			b.WriteString(" ")
		}
		if strings.Contains(arg, workDir) && shellSafe(arg) {
			b.WriteString(remoteWord(arg))
		} else {
			b.WriteString(shellQuote(arg))
		}
	}
	return b.String(), nil
}

// remoteWord rewrites `~`-prefixed argv words to use captured login home.
func remoteWord(arg string) string {
	if !strings.HasPrefix(arg, "~") {
		return arg
	}
	return `"$__vci_login_home"` + strings.TrimPrefix(arg, "~")
}

// shellSafe checks whether a token is safe to pass unquoted to the remote shell.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:@~-]+$`).MatchString

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
