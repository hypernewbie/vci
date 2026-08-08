// Package host executes project commands on a remote worker host via
// the ordinary system `ssh`, `tar`, and `scp` executables. No relay,
// daemon, framed protocol, or Go SSH client is involved: the detached
// worker stages the materialized workspace into the remote
// `~/.vci/state/work/<run>` tree, runs the selected runtime there,
// and fetches the workspace back when artifacts are configured. Only
// the workspace path and the project's environment map cross the SSH
// boundary; `~/.vci` state roots, `~/.ssh`, and `VCI_ROOT` never do.
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
	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/model"
)

// ValidateHost rejects an empty, option-like, whitespace, control-
// character, scheme, or `..` destination. The value is passed to the
// system `ssh` executable as a positional destination argument, so it
// must be safe as one shell word.
func ValidateHost(host string) error {
	if host == "" {
		return fmt.Errorf("remote host is empty")
	}
	return config.ValidateMachineHost(host)
}

// ValidateRemotePath enforces the exact Vci-owned remote tree
// grammar: `~/.vci/state/work/<run_id>`. The four fixed layout
// segments must appear in order and the trailing segment must be a
// layout.ValidName (a run id). No whitespace, control characters,
// option-like prefix, or `..` may appear. The value is embedded into
// remote `sh`/`scp` command lines, so it must be safe as a single
// shell word; `~` is the only shell-expanded part and it is a fixed
// literal.
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
	if !layout.ValidName(segments[4]) {
		return fmt.Errorf("remote path %q has invalid run segment %q", p, segments[4])
	}
	return nil
}

// RemoteWorkDir returns the deterministic remote tree a run's
// workspace is staged into on a machine's host: the same
// `state/work/<run>` layout the coordinator uses, mirrored under the
// remote session's home. `~` is expanded by the remote shell and by
// scp's remote-side expansion.
func RemoteWorkDir(id model.RunID) (string, error) {
	if !model.ValidRunID(id) {
		return "", fmt.Errorf("invalid run id %q", id)
	}
	return "~/.vci/state/work/" + string(id), nil
}

// RunRemote executes the given argv on the remote host via the
// ordinary system `ssh` executable: `ssh <host> <sh -c shell>` where
// `shell` is a single string that cd's into the staged remote
// workspace, isolates HOME/TMPDIR inside it (mirroring the local
// executor), exports the project environment, and execs the runtime
// argv. The exit code of the remote command is returned; ssh-level
// failures (missing binary, refused connection) are returned as an
// error. The remote command is cancelled with the context, exactly
// like a local supervised child.
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

// StageRemote streams the materialized local workspace into the
// remote work dir over ordinary SSH: `tar -cf - -C <workspace> .`
// piped into `ssh <host> "mkdir -p <workDir> && cd <workDir> &&
// tar -xpf -"`. The remote command text references only the
// validated work dir; the project bytes are tar data, never shell
// text.
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

// FetchRemote copies the remote work dir back to the coordinator via
// the system `scp` executable over the same SSH channel:
// `scp -r -q <host>:<workDir> <localDest>`. scp lays the remote tree
// at `<localDest>/<run_id>`, exactly like a local `scp -r` of the
// directory. Only the validated remote path appears in the command;
// the artifact globs are evaluated later by the local collector.
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

// composeShell builds the single `sh -c` string that runs on the
// remote host. The work dir is a validated Vci-owned path embedded as
// one shell word (`~` is expanded by the shell); environment keys and
// values are shell-quoted; argv elements are shell-quoted unless they
// are a shell-safe word containing the work dir (the docker/tart
// mount source, which must keep its unquoted `~` so the shell can
// expand it before the runtime client sees it).
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
	b.WriteString(" && export HOME=")
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
			b.WriteString(arg)
		} else {
			b.WriteString(shellQuote(arg))
		}
	}
	return b.String(), nil
}

// shellSafe reports whether a word is composed only of characters
// that are inert inside an unquoted shell word (alphanumerics plus
// `_`, `.`, `/`, `:`, `@`, `~`, `-`). A word that contains the
// work dir AND passes this check is embedded verbatim so its leading
// `~` is expanded by the remote shell; anything else is quoted.
var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:@~-]+$`).MatchString

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
