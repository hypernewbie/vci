package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/process"
	"github.com/hypernewbie/vci/internal/source"
)

// RemoteConfigured reports whether this root selects direct SSH for any
// command. The answer is driven by the top-level `orchestrator`
// selector. `self` (the coordinator) returns false; any other value
// is treated as the SSH destination for a client root.
func RemoteConfigured(l layout.Layout) (bool, error) {
	host, err := remoteOrchestrator(l)
	if err != nil {
		return false, err
	}
	return host != "", nil
}

// remoteOrchestrator returns the SSH destination for a client root and an
// empty string for a coordinator root.
func remoteOrchestrator(l layout.Layout) (string, error) {
	cfg, err := config.Load(l.ConfigPath())
	if err != nil {
		return "", err
	}
	if cfg.Orchestrator == config.OrchestratorSelf {
		return "", nil
	}
	return cfg.Orchestrator, nil
}

// RemoteBuild copies the local source tree into a remote staging directory
// that lives under the remote Vci root's TempDir, then invokes the public
// remote `vci build .` from the copied source. The remote shell creates
// the staging directory with private permissions, populates it from a tar
// archive piped over ssh, runs the public build, and removes the staging
// directory with a trap covering normal, error, interrupt, and termination
// paths.
//
// The remote run identity is returned unchanged; no local run record is
// created and no remote run files are inspected.
func RemoteBuild(ctx context.Context, l layout.Layout, sourcePath string) ([]byte, bool, error) {
	host, repo, remote, err := remoteBuildTarget(ctx, l, sourcePath)
	if err != nil || !remote {
		return nil, remote, err
	}
	return buildOverStaging(ctx, host, repo.Root, repo.Name)
}

// buildOverStaging composes a single ssh session that owns the entire
// staging lifecycle: mkdir, populate from a client tar stream, run the
// public `vci build .`, and trap-based cleanup on every exit path.
//
// The staging directory is named after the project (repoName), which has
// already passed layout.ValidName. The remote `vci build .` therefore finds
// the configured project by git-root basename lookup without interpreting
// client-controlled shell syntax.
func buildOverStaging(ctx context.Context, host, sourceRoot, repoName string) ([]byte, bool, error) {
	// repoName is interpolated into the remote shell only after this
	// coordinator-compatible validation. A repository basename outside this
	// alphabet cannot name a configured project either, so reject it before
	// starting tar or SSH rather than treating it as shell text.
	if !layout.ValidName(repoName) {
		return nil, true, fmt.Errorf("repository name %q cannot name a remote project", repoName)
	}
	tarCmd := exec.CommandContext(ctx, "tar", "-C", sourceRoot, "-cf", "-", ".")
	script := stagingShellScript(repoName)
	sshCmd := exec.CommandContext(ctx, "ssh", host, script)
	sshCmd.Stdin, _ = tarCmd.StdoutPipe()
	var stdout, stderr bytes.Buffer
	sshCmd.Stdout = &stdout
	sshCmd.Stderr = &stderr
	if err := tarCmd.Start(); err != nil {
		return nil, true, fmt.Errorf("start tar: %w", err)
	}
	sshErr := sshCmd.Run()
	tarErr := tarCmd.Wait()
	if tarErr != nil {
		return nil, true, fmt.Errorf("archive source: %w", tarErr)
	}
	if sshErr != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = sshErr.Error()
		}
		if validEnvelope(stdout.Bytes()) {
			return stdout.Bytes(), true, nil
		}
		return nil, true, fmt.Errorf("ssh %s: %s", host, message)
	}
	if !validEnvelope(stdout.Bytes()) {
		return nil, true, fmt.Errorf("remote vci returned invalid JSON")
	}
	return stdout.Bytes(), true, nil
}

// stagingShellScript returns the shell fragment run on the remote host.
// It derives the Vci root from the remote's own environment, creates a
// private staging directory under that root's TempDir with an
// unpredictable mktemp suffix (so two SSH sessions back-to-back can
// never collide), receives a tar archive on stdin, runs `vci build .`
// from the copied source under a project-named subdirectory, and
// traps removal of the entire staging tree on every exit path.
//
// The project-named subdirectory is what the remote `vci build .`
// runs from, so `git rev-parse --show-toplevel` returns the project
// basename and the coordinator's project lookup succeeds. The mktemp
// parent directory holds only the temporary copy and is removed as
// a whole on exit; nothing else is touched.
//
// Cleanup is a flat `rm -rf` over the staging root only. There is no
// recursive chmod, so a hostile source-tree symlink cannot trick the
// trap into touching an external path. The script trusts `mktemp -d`
// rather than re-deriving the same name to guarantee uniqueness across
// re-entries in the same SSH session.
//
// The fragment is composed only of literal shell syntax and quoted
// client-controlled values; it never expands client source text.
func stagingShellScript(repoName string) string {
	// buildOverStaging rejects values outside layout.ValidName before this
	// function is reached. Keep the fallback literal safe for direct unit
	// callers too; never interpolate an unsafe name into a shell fragment.
	name := repoName
	if !layout.ValidName(name) {
		name = "source"
	}
	prefix := "vci-source-" + name
	return fmt.Sprintf(`set -eu
ROOT="${VCI_ROOT:-$HOME/.vci}"
TMP_PARENT="$ROOT/state/tmp"
mkdir -m 700 -p "$TMP_PARENT"
STAGING=$(mktemp -d -p "$TMP_PARENT" -t %s.XXXXXXXX)
chmod 700 "$STAGING"
trap 'rm -rf "$STAGING" 2>/dev/null || true' EXIT INT TERM
PROJECT="$STAGING/%s"
mkdir -m 700 "$PROJECT"
tar -C "$PROJECT" -xf -
cd "$PROJECT"
vci build .
`, prefix, name)
}

// RemoteCheck and RemoteAbort use the one configured remote target directly.
// They take the run ID returned by the remote public CLI unchanged. No
// fixed timeout is applied: cancellation is owned by the caller context
// and the controlled test deadline.
func RemoteCheck(ctx context.Context, l layout.Layout, id model.RunID) ([]byte, bool, error) {
	host, remote, err := remoteTarget(l)
	if err != nil || !remote {
		return nil, remote, err
	}
	raw, err := runSSH(ctx, host, "vci check "+shellQuote(string(id)))
	return raw, true, err
}

func RemoteAbort(ctx context.Context, l layout.Layout, id model.RunID) ([]byte, bool, error) {
	host, remote, err := remoteTarget(l)
	if err != nil || !remote {
		return nil, remote, err
	}
	raw, err := runSSH(ctx, host, "vci abort "+shellQuote(string(id)))
	return raw, true, err
}

// remoteTarget returns the configured remote host from the orchestrator
// selector. A client root carries only the selector; the coordinator
// owned project and machine configuration that the build needs lives
// on the remote side and is resolved by the remote `vci build .`
// itself.
func remoteTarget(l layout.Layout) (string, bool, error) {
	host, err := remoteOrchestrator(l)
	if err != nil {
		return "", false, err
	}
	return host, host != "", nil
}

// remoteBuildTarget resolves the local source repository and returns
// the SSH destination from the orchestrator selector. Project matching
// is performed by the remote vci against its configured projects.
func remoteBuildTarget(ctx context.Context, l layout.Layout, sourcePath string) (string, source.Repository, bool, error) {
	host, remote, err := remoteTarget(l)
	if err != nil || !remote {
		return "", source.Repository{}, remote, err
	}
	repo, err := source.Discover(ctx, sourcePath, process.Native{})
	if err != nil {
		return "", source.Repository{}, true, err
	}
	return host, repo, true, nil
}

// runSSH executes `ssh <host> <command>` via the ordinary system ssh
// binary and returns the remote stdout. When the remote process
// exits non-zero, a valid Vci JSON envelope is preserved so the caller
// can report the remote error rather than relabeling it as SSH
// failure. Only no response, malformed response, schema mismatch, or
// genuine SSH failure is classified as infrastructure.
func runSSH(ctx context.Context, host, command string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ssh", host, command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if validEnvelope(stdout.Bytes()) {
			return stdout.Bytes(), nil
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("ssh %s: %s", host, message)
	}
	if !validEnvelope(stdout.Bytes()) {
		return nil, fmt.Errorf("remote vci returned invalid JSON")
	}
	return stdout.Bytes(), nil
}

// validEnvelope returns true when the given bytes decode as a Vci response
// envelope with the expected schema version. Used to decide whether a
// non-zero remote exit should be propagated as an envelope or treated as an
// SSH infrastructure failure.
func validEnvelope(data []byte) bool {
	var envelope struct {
		SchemaVersion int             `json:"schema_version"`
		Command       *string         `json:"command"`
		OK            *bool           `json:"ok"`
		Data          json.RawMessage `json:"data"`
		Error         json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return false
	}
	if envelope.SchemaVersion != model.SchemaVersion || envelope.Command == nil || *envelope.Command == "" || envelope.OK == nil || len(envelope.Data) == 0 {
		return false
	}
	if *envelope.OK {
		return len(envelope.Error) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Error), []byte("null"))
	}
	if len(envelope.Error) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Error), []byte("null")) {
		return false
	}
	var remoteErr struct {
		Code    string `json:"code"`
		Class   string `json:"class"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(envelope.Error, &remoteErr); err != nil {
		return false
	}
	return remoteErr.Code != "" && remoteErr.Class != "" && remoteErr.Message != ""
}

// RemoteCommand runs an ordinary public Vci command on the configured
// remote host. It returns the remote JSON envelope unchanged; a non-zero
// remote exit with a valid envelope is preserved. Only no response,
// malformed response, schema mismatch, or genuine SSH failure is
// classified as infrastructure.
func RemoteCommand(ctx context.Context, l layout.Layout, name string, args ...string) ([]byte, bool, error) {
	host, remote, err := remoteTarget(l)
	if err != nil || !remote {
		return nil, remote, err
	}
	raw, err := runSSH(ctx, host, buildRemoteCommand(name, args...))
	return raw, true, err
}

// buildRemoteCommand composes a shell-safe `vci <name> <args...>` line
// for the system `ssh` executable. It quotes each argument so the
// remote shell sees the same set of arguments the client passed.
func buildRemoteCommand(name string, args ...string) string {
	parts := make([]string, 0, 1+len(args))
	parts = append(parts, shellQuote(name))
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	return "vci " + strings.Join(parts, " ")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
