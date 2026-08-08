// Package host transfers workspaces over SSH and runs commands remotely.
// It stages workspace state and handles remote execution via system tools.
package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
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

// RunRemote executes a remote command over SSH.
// It returns the remote exit code; transport failures return an error.
func RunRemote(ctx context.Context, host, workDir string, argv []string, env map[string]string, stdout, stderr io.Writer) (int, error) {
	if err := ValidateHost(host); err != nil {
		return 0, err
	}
	shell, err := composeShell(workDir, argv, env)
	if err != nil {
		return 0, err
	}
	cmd := exec.CommandContext(ctx, "ssh", host, shell)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err = cmd.Run()
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 0, fmt.Errorf("ssh %s: %w", host, err)
	}
	return 0, nil
}

// StageRemote tars a local workspace and streams it to remoteWorkDir over SSH.
// The command uses tar data only, with no shell content from the workspace payload.
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
	sshCmd := exec.CommandContext(ctx, "ssh", host, "mkdir -p "+remoteWorkDir+" && cd "+remoteWorkDir+" && tar -xpf -")
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
