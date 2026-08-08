package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/host"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/process"
	"github.com/hypernewbie/vci/internal/reaper"
	"github.com/hypernewbie/vci/internal/runtime"
	"github.com/hypernewbie/vci/internal/source"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// RemoteConfigured reports whether this layout uses a client SSH target.
func RemoteConfigured(l model.Layout) (bool, error) {
	host, err := remoteOrchestrator(l)
	if err != nil {
		return false, err
	}
	return host != "", nil
}

// remoteOrchestrator returns the SSH destination for a client root and an
// empty string for a coordinator root.
func remoteOrchestrator(l model.Layout) (string, error) {
	cfg, err := config.Load(l.ConfigPath())
	if err != nil {
		return "", err
	}
	if cfg.Orchestrator == config.OrchestratorSelf {
		return "", nil
	}
	return cfg.Orchestrator, nil
}

// RemoteBuild snapshots selected input, computes its digest, and streams it
// to remote staging over SSH.
func RemoteBuild(ctx context.Context, l model.Layout, sourcePath string) ([]byte, bool, error) {
	host, remote, err := remoteTarget(l)
	if err != nil || !remote {
		return nil, remote, err
	}
	input, err := source.SelectBuildInput(ctx, sourcePath, process.Native{})
	if err != nil {
		return nil, true, fmt.Errorf("select build input: %w", err)
	}
	// Validate the repository basename immediately, before any
	// further work touches remote shell text or staging directories.
	if !model.ValidName(input.ProjectName) {
		return nil, true, fmt.Errorf("repository name %q cannot name a remote project", input.ProjectName)
	}

	// Materialize one snapshot so the digest and archive come from identical bytes.
	if err := os.MkdirAll(l.TempDir(), 0o700); err != nil {
		return nil, true, fmt.Errorf("prepare client snapshot dir: %w", err)
	}
	snapshot, err := source.MaterializeSnapshot(input, l.TempDir())
	if err != nil {
		return nil, true, fmt.Errorf("materialize source snapshot: %w", err)
	}
	defer func() { _ = os.RemoveAll(snapshot) }()

	digest, err := source.ComputeSnapshotDigest(snapshot)
	if err != nil {
		return nil, true, fmt.Errorf("compute source digest: %w", err)
	}
	key, err := validateCacheKey(digest, input.ProjectName)
	if err != nil {
		return nil, true, err
	}

	// Try remote source cache lookup over SSH before uploading the snapshot.
	cacheScript := buildCacheProbeScript(key)
	raw, err := runSSH(ctx, host, cacheScript)
	if err == nil && validEnvelope(raw) {
		fmt.Fprintln(os.Stderr, "vci: source cache hit")
		return raw, true, nil
	}

	raw, remote, tarBytes, err := buildOverStaging(ctx, host, input, snapshot, key)
	if tarBytes > 0 {
		fmt.Fprintf(os.Stderr, "vci: source tar bytes %d\n", tarBytes)
	}
	return raw, remote, err
}

// buildOverStaging uploads the tar stream over SSH, runs the remote build,
// and returns output plus tar bytes sent.
func buildOverStaging(ctx context.Context, host string, input source.SourceInput, snapshotRoot string, key safeCacheKey) ([]byte, bool, int64, error) {
	if !model.ValidName(input.ProjectName) {
		return nil, true, 0, fmt.Errorf("repository name %q cannot name a remote project", input.ProjectName)
	}

	tarCmd := exec.CommandContext(ctx, "tar", "-cf", "-", "-C", snapshotRoot, "--no-recursion", "--null", "-T", "-")
	var pathBuf bytes.Buffer
	for _, p := range input.Files {
		pathBuf.WriteString(p)
		pathBuf.WriteByte(0)
	}
	tarCmd.Stdin = &pathBuf

	script := stagingShellScript(key)
	sshCmd := exec.CommandContext(ctx, "ssh", host, script)
	tarOut, err := tarCmd.StdoutPipe()
	if err != nil {
		return nil, true, 0, fmt.Errorf("tar stdout: %w", err)
	}
	counter := &countingReader{r: tarOut}
	sshCmd.Stdin = counter
	var stdout, stderr bytes.Buffer
	sshCmd.Stdout = &stdout
	sshCmd.Stderr = &stderr
	if err := tarCmd.Start(); err != nil {
		return nil, true, 0, fmt.Errorf("start tar: %w", err)
	}
	sshErr := sshCmd.Run()
	tarErr := tarCmd.Wait()
	if tarErr != nil {
		return nil, true, counter.n, fmt.Errorf("archive source: %w", tarErr)
	}
	if sshErr != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = sshErr.Error()
		}
		if validEnvelope(stdout.Bytes()) {
			return stdout.Bytes(), true, counter.n, nil
		}
		return nil, true, counter.n, fmt.Errorf("ssh %s: %s", host, message)
	}
	if !validEnvelope(stdout.Bytes()) {
		return nil, true, counter.n, fmt.Errorf("remote vci returned invalid JSON")
	}
	return stdout.Bytes(), true, counter.n, nil
}

// countingReader counts bytes read from an underlying reader.
// Used to measure exact tar bytes sent to SSH.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// RemoteCheck and RemoteAbort forward the remote run ID unchanged.
// Caller context controls timeout.
func RemoteCheck(ctx context.Context, l model.Layout, id model.RunID) ([]byte, bool, error) {
	host, remote, err := remoteTarget(l)
	if err != nil || !remote {
		return nil, remote, err
	}
	raw, err := runSSH(ctx, host, "vci check "+shellQuote(string(id)))
	return raw, true, err
}

func RemoteAbort(ctx context.Context, l model.Layout, id model.RunID) ([]byte, bool, error) {
	host, remote, err := remoteTarget(l)
	if err != nil || !remote {
		return nil, remote, err
	}
	raw, err := runSSH(ctx, host, "vci abort "+shellQuote(string(id)))
	return raw, true, err
}

// remoteTarget returns the orchestrator host for client layouts.
// Empty host means this layout is a coordinator.
func remoteTarget(l model.Layout) (string, bool, error) {
	host, err := remoteOrchestrator(l)
	if err != nil {
		return "", false, err
	}
	return host, host != "", nil
}

// runSSH runs `ssh <host> <command>` and returns stdout bytes.
// Valid Vci envelopes are returned even on remote non-zero exits.
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

// runSSHRaw runs `ssh <host> <command>` and returns raw stdout bytes.
// Valid Vci envelopes are returned even on remote non-zero exits.
func runSSHRaw(ctx context.Context, host, command string) ([]byte, error) {
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
	return stdout.Bytes(), nil
}

// validEnvelope checks if bytes decode to a valid Vci envelope
// with the expected schema version.
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

// RemoteCommand runs a public Vci command on the configured remote host.
// It returns the raw remote JSON envelope.
func RemoteCommand(ctx context.Context, l model.Layout, name string, args ...string) ([]byte, bool, error) {
	host, remote, err := remoteTarget(l)
	if err != nil || !remote {
		return nil, remote, err
	}
	raw, err := runSSH(ctx, host, buildRemoteCommand(name, args...))
	return raw, true, err
}

// RemoteLog proxies `vci logs` to the remote host and returns raw log bytes.
func RemoteLog(ctx context.Context, l model.Layout, id model.RunID, stream string, tail int) ([]byte, bool, error) {
	host, remote, err := remoteTarget(l)
	if err != nil || !remote {
		return nil, remote, err
	}
	args := []string{"logs", string(id)}
	if stream == "stderr" {
		args = append(args, "--stderr")
	}
	if tail > 0 {
		args = append(args, "--tail", strconv.Itoa(tail))
	}
	raw, err := runSSHRaw(ctx, host, buildRemoteCommand(args[0], args[1:]...))
	return raw, true, err
}

// RemoteGetArtifact proxies `vci artifacts get` and returns raw artifact bytes.
func RemoteGetArtifact(ctx context.Context, l model.Layout, id model.RunID, rel string) ([]byte, bool, error) {
	host, remote, err := remoteTarget(l)
	if err != nil || !remote {
		return nil, remote, err
	}
	raw, err := runSSHRaw(ctx, host, buildRemoteCommand("artifacts", "get", string(id), rel))
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

// executeRemote stages workspace on a remote host via ssh, runs the command,
// fetches artifacts if requested, and best-effort cleans the remote workspace.
// Uses `ssh`, `tar`, and `scp` only and returns runtime.Result with matches.
func executeRemote(ctx context.Context, l model.Layout, id model.RunID, runDir string, machine config.Machine, workspace string, project config.Project, pair process.Pair) (runtime.Result, []string, bool, error) {
	remoteWorkDir, err := host.RemoteWorkDir(id)
	if err != nil {
		return runtime.Result{}, nil, false, err
	}
	defer func() {
		// Best-effort remote workspace cleanup on the success path.
		// The reaper does terminal sweeps of `~/.vci/state/work/<run>`;
		// this removes the tree immediately.
		_ = reaper.CleanupRemote(context.Background(), machine.Host, remoteWorkDir)
	}()

	if err := host.StageRemote(ctx, machine.Host, remoteWorkDir, workspace); err != nil {
		return runtime.Result{}, nil, false, fmt.Errorf("stage remote workspace: %w", err)
	}
	argv, err := remoteArgv(machine, remoteWorkDir, project.Command)
	if err != nil {
		return runtime.Result{}, nil, false, err
	}
	started := time.Now().UTC()
	exitCode, runErr := host.RunRemote(ctx, machine.Host, remoteWorkDir, argv, project.Environment, pair.Stdout, pair.Stderr)
	finished := time.Now().UTC()
	result := runtime.Result{ExitCode: exitCode, ResolvedExecutable: argv[0], StartedAt: started, FinishedAt: finished, Duration: finished.Sub(started)}

	// Fetch matching artifacts via scp into a temporary parent,
	// then let local collection publish them into runDir/artifacts.
	var collected []string
	var truncated bool
	if len(project.Artifacts) > 0 {
		fetchParent, mkErr := os.MkdirTemp(l.TempDir(), "vci-fetch-")
		if mkErr != nil {
			return result, nil, false, fmt.Errorf("artifact fetch dir: %w", mkErr)
		}
		defer func() { _ = os.RemoveAll(fetchParent) }()
		if err := host.FetchRemote(ctx, machine.Host, remoteWorkDir, fetchParent); err != nil {
			return result, nil, false, err
		}
		collected, truncated, err = CollectArtifacts(filepath.Join(fetchParent, string(id)), runDir, project.Artifacts)
		if err != nil {
			return result, nil, false, fmt.Errorf("collect remote artifacts: %w", err)
		}
	}
	return result, collected, truncated, runErr
}

// selectExecutor selects runtime based on the snapshot's machine.
// Defaults to runtime.Local when machine runtime is empty.
func selectExecutor(snapshot runSnapshot) Executor {
	machine := resolvedMachine(snapshot)
	switch machine.Runtime {
	case "docker":
		return runtime.Docker{Image: machine.Image}
	case "vm":
		return runtime.VM{Snapshot: machine.Snapshot, Binary: "tart"}
	}
	return runtime.Local{}
}

// resolvedMachine resolves the machine reserved by the snapshot,
// falling back to ProjectConfig.Machines[0] for legacy records.
func resolvedMachine(snapshot runSnapshot) config.Machine {
	name := snapshot.Machine
	if name == "" && len(snapshot.ProjectConfig.Machines) > 0 {
		name = snapshot.ProjectConfig.Machines[0]
	}
	return lookupMachine(snapshot, name)
}

// remoteArgv builds the remote command argv.
// Docker/VM use runtime-specific remote arg builders; others run directly.
func remoteArgv(machine config.Machine, remoteWorkDir string, command []string) ([]string, error) {
	switch machine.Runtime {
	case "docker":
		args, err := (runtime.Docker{Image: machine.Image}).CommandArgvRemote(remoteWorkDir, command)
		if err != nil {
			return nil, err
		}
		return append([]string{"docker"}, args...), nil
	case "vm":
		return (runtime.VM{Snapshot: machine.Snapshot, Binary: "tart"}).CommandArgvRemote(remoteWorkDir, command)
	}
	return command, nil
}

// lookupMachine resolves a machine by name from snapshot data.
// Snapshot state is used, so live config changes do not rewrite history.
func lookupMachine(snapshot runSnapshot, machineName string) config.Machine {
	if machineName == "" {
		return config.Machine{}
	}
	if machine, ok := snapshot.Machines[machineName]; ok {
		return machine
	}
	return config.Machine{}
}
