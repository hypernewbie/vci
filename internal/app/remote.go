package app

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// RemoteBuild captures the source identity and local changes, asks the
// coordinator for the deepest commit it already has, then streams a submission
// (the missing Git objects as a bundle plus the local changes) to the
// coordinator's build --from-submission over SSH stdin. It returns the
// coordinator's run-id envelope; the build executes asynchronously and the
// client polls or aborts it like any other run.
func RemoteBuild(ctx context.Context, l model.Layout, sourcePath string) ([]byte, bool, error) {
	host, remote, err := remoteTarget(l)
	if err != nil || !remote {
		return nil, remote, err
	}
	repo, err := source.Discover(ctx, sourcePath, process.Native{})
	if err != nil {
		return nil, true, fmt.Errorf("discover source: %w", err)
	}
	if !model.ValidName(repo.Name) {
		return nil, true, fmt.Errorf("repository name %q cannot name a remote project", repo.Name)
	}
	identity, err := source.CaptureIdentity(ctx, sourcePath, process.Native{})
	if err != nil {
		return nil, true, err
	}
	have, err := remoteSeedHead(ctx, host, repo.Name)
	if err != nil {
		return nil, true, err
	}
	have = deltaBase(ctx, sourcePath, have, identity.Head)
	bundleRC, err := source.CreateBundle(ctx, sourcePath, have, identity.Head, process.Native{})
	if err != nil {
		return nil, true, fmt.Errorf("create bundle: %w", err)
	}
	bundle, err := io.ReadAll(bundleRC)
	_ = bundleRC.Close()
	if err != nil {
		return nil, true, fmt.Errorf("read bundle: %w", err)
	}
	lc, err := source.CaptureLocalChanges(ctx, sourcePath, process.Native{})
	if err != nil {
		return nil, true, err
	}
	submission, err := source.PackageSubmission(source.Submission{
		Head:         identity.Head,
		Base:         identity.Base,
		RemoteURL:    identity.RemoteURL,
		Have:         have,
		Bundle:       bundle,
		LocalChanges: lc,
	})
	if err != nil {
		return nil, true, err
	}
	cmd := exec.CommandContext(ctx, "ssh", host, buildRemoteCommand("build", "--from-submission", repo.Name))
	cmd.Stdin = submission
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if validEnvelope(stdout.Bytes()) {
			return stdout.Bytes(), true, nil
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, true, fmt.Errorf("ssh %s: %s", host, message)
	}
	if !validEnvelope(stdout.Bytes()) {
		return nil, true, fmt.Errorf("remote vci returned invalid JSON")
	}
	return stdout.Bytes(), true, nil
}

// remoteSeedHead probes the coordinator for the HEAD commit of its local seed
// for a project; empty means the coordinator has no usable seed.
func remoteSeedHead(ctx context.Context, host, project string) (string, error) {
	raw, err := runSSH(ctx, host, buildRemoteCommand("probe-seed", project))
	if err != nil {
		return "", err
	}
	var resp struct {
		Data struct {
			Have string `json:"have"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", fmt.Errorf("decode probe-seed response: %w", err)
	}
	return resp.Data.Have, nil
}

// deltaBase returns the commit to bundle from: have when it is an ancestor of
// head, otherwise empty so the client sends its full history.
func deltaBase(ctx context.Context, repoRoot, have, head string) string {
	if have == "" {
		return ""
	}
	res, err := (process.Native{}).Run(ctx, process.Command{Executable: "git", Args: []string{"-C", repoRoot, "merge-base", "--is-ancestor", have, head}})
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	return have
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

// workerPayload assembles the private tar streamed to a worker machine for a
// submission-backed remote build. It carries the submitted commit as "head",
// the Git objects the worker lacks as "bundle" (omitted when the worker
// already has head), and the run's durable local-change archive as "lc.tar",
// copied byte-for-byte so the worker reconstructs exactly the submitted
// state. sourceRoot is the submitted source checkout; CreateBundle only
// touches transient refs, never the working tree or index.
func workerPayload(ctx context.Context, sourceRoot, have, head, lcPath string) (io.ReadCloser, error) {
	var bundle []byte
	bundleRC, err := source.CreateBundle(ctx, sourceRoot, have, head, process.Native{})
	if err != nil && !errors.Is(err, source.ErrBundleEmpty) {
		return nil, fmt.Errorf("create worker bundle: %w", err)
	}
	if bundleRC != nil {
		bundle, err = io.ReadAll(bundleRC)
		_ = bundleRC.Close()
		if err != nil {
			return nil, fmt.Errorf("read worker bundle: %w", err)
		}
	}
	lc, err := os.ReadFile(lcPath)
	if err != nil {
		return nil, fmt.Errorf("read durable local-change archive: %w", err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := writePayloadEntry(tw, "head", []byte(strings.TrimSpace(head)+"\n")); err != nil {
		return nil, err
	}
	if len(bundle) > 0 {
		if err := writePayloadEntry(tw, "bundle", bundle); err != nil {
			return nil, err
		}
	}
	if err := writePayloadEntry(tw, "lc.tar", lc); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(buf.Bytes())), nil
}

// writePayloadEntry writes one regular-file member into a worker payload tar.
func writePayloadEntry(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

// seededReconstructionEligible reports whether a machine can reconstruct the
// submitted local changes onto its own source checkout: the machine must have
// a source path seeded for the project, the project must declare no hard
// workspace exclusions (which the seeded overlay cannot honor), and the
// durable local-change archive must be present as a regular file.
func seededReconstructionEligible(machine config.Machine, project config.Project, projectName, lcPath string) bool {
	if machine.SourcePaths[projectName] == "" {
		return false
	}
	if len(project.ExcludedPaths) != 0 {
		return false
	}
	info, err := os.Stat(lcPath)
	return err == nil && info.Mode().IsRegular()
}

// stageOrReconstruct prepares a worker's remote workspace for a
// submission-backed remote build. When the machine is eligible for seeded
// reconstruction it probes the machine's seed checkout for its head and
// streams a reconstruction payload; any probe, payload, or stream failure
// falls back to full workspace staging exactly as executeRemote stages it.
// runner supplies the ssh transport for the probe and the stream.
// submittedHead is the submitted commit used as the payload head; the
// probed seed head is the bundle base, so the worker receives exactly the
// objects it lacks.
func stageOrReconstruct(ctx context.Context, runner process.Runner, machine config.Machine, project config.Project, projectName, lcPath, sourceRoot, workspace, submittedHead, remoteWorkDir string) error {
	if !seededReconstructionEligible(machine, project, projectName, lcPath) {
		if err := host.StageRemote(ctx, machine.Host, remoteWorkDir, workspace); err != nil {
			return fmt.Errorf("stage remote workspace: %w", err)
		}
		return nil
	}
	seed := machine.SourcePaths[projectName]
	client := host.Client{Runner: runner}
	seedHead, err := client.ProbeSeedHead(ctx, machine.Host, seed)
	if err != nil || seedHead == "" {
		if err := host.StageRemote(ctx, machine.Host, remoteWorkDir, workspace); err != nil {
			return fmt.Errorf("stage remote workspace: %w", err)
		}
		return nil
	}
	payload, err := workerPayload(ctx, sourceRoot, seedHead, submittedHead, lcPath)
	if err != nil {
		if err := host.StageRemote(ctx, machine.Host, remoteWorkDir, workspace); err != nil {
			return fmt.Errorf("stage remote workspace: %w", err)
		}
		return nil
	}
	if err := client.StreamReconstruct(ctx, machine.Host, seed, remoteWorkDir, payload); err != nil {
		if err := host.StageRemote(ctx, machine.Host, remoteWorkDir, workspace); err != nil {
			return fmt.Errorf("stage remote workspace: %w", err)
		}
		return nil
	}
	return nil
}

// executeRemote stages workspace on a remote host via ssh, runs the command,
// fetches artifacts if requested, and best-effort cleans the remote workspace.
// Uses `ssh`, `tar`, and `scp` only and returns runtime.Result with matches.
func executeRemote(ctx context.Context, l model.Layout, id model.RunID, runDir string, machine config.Machine, sourceRoot string, workspace string, project config.Project, projectName, lcPath, submittedHead string, pair process.Pair) (runtime.Result, []string, bool, error) {
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

	if err := stageOrReconstruct(ctx, process.Native{}, machine, project, projectName, lcPath, sourceRoot, workspace, submittedHead, remoteWorkDir); err != nil {
		return runtime.Result{}, nil, false, err
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
