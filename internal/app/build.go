package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/process"
	"github.com/hypernewbie/vci/internal/runtime"
	"github.com/hypernewbie/vci/internal/scheduler"
	"github.com/hypernewbie/vci/internal/source"
	"github.com/hypernewbie/vci/internal/store"
)

// Executor runs prepared runs.
type Executor interface {
	ExecuteSupervised(ctx context.Context, request runtime.Request, onStart func(process.Running) error) (runtime.Result, error)
}

type BuildResult struct {
	RunID              model.RunID     `json:"run_id"`
	Project            string          `json:"project"`
	Machine            string          `json:"machine"`
	State              model.RunState  `json:"state"`
	SourceDigest       string          `json:"source_digest"`
	ConfigDigest       string          `json:"config_digest"`
	ConfigSnapshot     json.RawMessage `json:"config_snapshot"`
	ExitCode           int             `json:"exit_code"`
	Failure            string          `json:"failure,omitempty"`
	StdoutPath         string          `json:"stdout_path,omitempty"`
	StderrPath         string          `json:"stderr_path,omitempty"`
	StdoutTruncated    bool            `json:"stdout_truncated"`
	StderrTruncated    bool            `json:"stderr_truncated"`
	Artifacts          []string        `json:"artifacts,omitempty"`
	ArtifactsTruncated bool            `json:"artifacts_truncated"`
	Executor           runtime.Result  `json:"executor"`
}
type PreparedRun struct {
	Record store.RunRecord
}
type runSnapshot struct {
	ProjectConfig config.Project            `json:"project_config"`
	Machine       string                    `json:"machine,omitempty"`
	SourceHead    string                    `json:"source_head,omitempty"`
	Machines      map[string]config.Machine `json:"machines"`
	LogLimits     config.LogLimits          `json:"log_limits"`
}

func Prepare(ctx context.Context, l model.Layout, sourcePath string) (PreparedRun, error) {
	if err := l.Ensure(); err != nil {
		return PreparedRun{}, err
	}
	cfg, err := config.Load(l.ConfigPath())
	if err != nil {
		return PreparedRun{}, err
	}
	repo, err := source.Discover(ctx, sourcePath, process.Native{})
	if err != nil {
		return PreparedRun{}, err
	}
	projectNames := make([]string, 0, len(cfg.Projects))
	for name := range cfg.Projects {
		projectNames = append(projectNames, name)
	}
	projectName, err := source.MatchProject(repo.Name, projectNames)
	if err != nil {
		return PreparedRun{}, err
	}
	project := cfg.Projects[projectName]
	if len(project.Machines) == 0 {
		return PreparedRun{}, fmt.Errorf("project %q has no attached machines", projectName)
	}

	// Reject uninitialized submodules and bad LFS pointers before staging.
	if _, err := source.SelectBuildInput(ctx, repo.Root, process.Native{}); err != nil {
		return PreparedRun{}, err
	}
	now := time.Now().UTC()
	runStore := store.Store{Layout: l}
	// Reserve one machine and publish a staged run atomically.
	draftRecord, err := store.NewRun(projectName, project.Machines[0], project.Command, "", buildDraftSnapshot(projectName, project, cfg), now)
	if err != nil {
		return PreparedRun{}, err
	}
	var staged store.RunRecord
	err = scheduler.ReserveAndPublish(l, runStore, cfg, draftRecord.ID, project.Machines, now, func(machineName string) error {
		record, buildErr := store.NewStagedRunFromID(draftRecord.ID, projectName, machineName, project.Command, "", buildStagedSnapshot(projectName, project, machineName, cfg, "", nil), now)
		if buildErr != nil {
			return buildErr
		}
		record.SourcePath = repo.Root
		if saveErr := runStore.Save(record); saveErr != nil {
			return saveErr
		}
		staged = record
		return nil
	})
	if err != nil {
		return PreparedRun{}, err
	}
	return PreparedRun{Record: staged}, nil
}

// buildDraftSnapshot returns the pre-staging config snapshot.
func buildDraftSnapshot(projectName string, project config.Project, cfg config.Config) map[string]any {
	return map[string]any{
		"schema_version": config.SchemaVersion,
		"project":        projectName,
		"project_config": project,
		"log_limits":     cfg.LogLimits,
		"retention":      cfg.Retention,
	}
}

// buildStagedSnapshot returns the staged config snapshot, optionally with source_provenance.
func buildStagedSnapshot(projectName string, project config.Project, machineName string, cfg config.Config, sourceHead string, provenance map[string]any) map[string]any {
	out := map[string]any{
		"schema_version": config.SchemaVersion,
		"project":        projectName,
		"project_config": project,
		"machine":        machineName,
		"machines":       cfg.Machines,
		"log_limits":     cfg.LogLimits,
		"retention":      cfg.Retention,
	}
	if sourceHead != "" {
		out["source_head"] = sourceHead
	}
	if provenance != nil {
		out["source_provenance"] = provenance
	}
	return out
}

// PrepareHosted runs `vci build --hosted <project>`.
// It validates the hosted fallback and runs the same validation/manifest flow as Prepare,
// with a temporary checkout and no source-cache admission.
func PrepareHosted(ctx context.Context, l model.Layout, projectName string) (PreparedRun, error) {
	if err := l.Ensure(); err != nil {
		return PreparedRun{}, err
	}
	cfg, err := config.Load(l.ConfigPath())
	if err != nil {
		return PreparedRun{}, err
	}
	if cfg.Orchestrator != config.OrchestratorSelf {
		return PreparedRun{}, fmt.Errorf("client root: orchestrator = %q", cfg.Orchestrator)
	}
	if !model.ValidName(projectName) {
		return PreparedRun{}, fmt.Errorf("%w: project name %q is invalid", config.ErrHostedFallbackInvalid, projectName)
	}
	project, ok := cfg.Projects[projectName]
	if !ok {
		return PreparedRun{}, fmt.Errorf("%w: project %q", config.ErrHostedFallbackNotConfigured, projectName)
	}
	if len(project.Machines) == 0 {
		return PreparedRun{}, fmt.Errorf("project %q has no attached machines", projectName)
	}
	validated, err := project.HostedFallback.Validate()
	if err != nil {
		return PreparedRun{}, err
	}
	// Checkout the pinned commit to a temp root, validate from it, then clean up.
	checkoutRoot, err := source.Checkout(ctx, process.Native{}, l, projectName, validated)
	if err != nil {
		return PreparedRun{}, err
	}
	defer func() { _ = source.CleanUnder(checkoutRoot, l.TempDir()) }()

	// Same validation pipeline as Prepare.
	graph, err := source.SelectBuildInput(ctx, checkoutRoot, process.Native{})
	if err != nil {
		return PreparedRun{}, err
	}
	snapParent, err := os.MkdirTemp(l.TempDir(), source.SnapshotPrefix)
	if err != nil {
		return PreparedRun{}, fmt.Errorf("prepare hosted snapshot dir: %w", err)
	}
	defer os.RemoveAll(snapParent)
	if _, err := source.MaterializeSnapshot(graph, snapParent); err != nil {
		return PreparedRun{}, err
	}
	manifest, blobs, err := source.BuildWithValidation(checkoutRoot, graph.LFSFiles)
	if err != nil {
		return PreparedRun{}, err
	}
	blobStore := source.BlobStore{Layout: l}
	if err := blobStore.PutManifestAndBlobs(manifest, blobs); err != nil {
		return PreparedRun{}, err
	}
	provenance := map[string]any{
		"kind":   "hosted_git",
		"url":    validated.URL,
		"commit": validated.Commit,
	}
	now := time.Now().UTC()
	runStore := store.Store{Layout: l}
	draftRecord, err := store.NewRun(projectName, project.Machines[0], project.Command, manifest.Digest, buildDraftSnapshot(projectName, project, cfg), now)
	if err != nil {
		return PreparedRun{}, err
	}
	var staged store.RunRecord
	err = scheduler.ReserveAndPublish(l, runStore, cfg, draftRecord.ID, project.Machines, now, func(machineName string) error {
		record, buildErr := store.NewStagedRunFromID(draftRecord.ID, projectName, machineName, project.Command, manifest.Digest, buildStagedSnapshot(projectName, project, machineName, cfg, validated.Commit, provenance), now)
		if buildErr != nil {
			return buildErr
		}
		if saveErr := runStore.Save(record); saveErr != nil {
			return saveErr
		}
		staged = record
		return nil
	})
	if err != nil {
		return PreparedRun{}, err
	}
	return PreparedRun{Record: staged}, nil
}

// submissionLCPath names the private per-run file that holds a submission's
// local-change archive. PrepareFromSubmission writes the exact serialized
// local changes there so the submitted delta stays available until
// ExecutePrepared removes it during end-of-run cleanup.
func submissionLCPath(l model.Layout, id model.RunID) (string, error) {
	dir, err := l.RunDir(string(id))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "submission-lc.tar"), nil
}

// PrepareFromSubmission reconstructs a workspace from a framed client
// submission and stages the run, returning the prepared record. The
// reconstructed staging dir persists until the run executes.
func PrepareFromSubmission(ctx context.Context, l model.Layout, projectName string, r io.Reader) (PreparedRun, error) {
	if err := l.Ensure(); err != nil {
		return PreparedRun{}, err
	}
	cfg, err := config.Load(l.ConfigPath())
	if err != nil {
		return PreparedRun{}, err
	}
	if cfg.Orchestrator != config.OrchestratorSelf {
		return PreparedRun{}, fmt.Errorf("client root: orchestrator = %q", cfg.Orchestrator)
	}
	project, ok := cfg.Projects[projectName]
	if !ok {
		return PreparedRun{}, fmt.Errorf("project %q is not configured", projectName)
	}
	if len(project.Machines) == 0 {
		return PreparedRun{}, fmt.Errorf("project %q has no attached machines", projectName)
	}
	seed, seedErr := localSeed(cfg, projectName)
	if seedErr != nil {
		seed = ""
	}
	sub, err := source.UnpackageSubmission(r)
	if err != nil {
		return PreparedRun{}, err
	}
	tempRoot, err := os.MkdirTemp(l.TempDir(), "vci-recon-*")
	if err != nil {
		return PreparedRun{}, fmt.Errorf("create reconstruction dir: %w", err)
	}
	reconDir := filepath.Join(tempRoot, projectName)
	var bundle io.Reader
	if len(sub.Bundle) > 0 {
		bundle = bytes.NewReader(sub.Bundle)
	}
	if err := source.ReconstructWorkspace(ctx, seed, reconDir, sub.Head, bundle, sub.LocalChanges, project.ExcludedPaths, process.Native{}); err != nil {
		_ = os.RemoveAll(tempRoot)
		return PreparedRun{}, fmt.Errorf("reconstruct workspace: %w", err)
	}
	prepared, err := Prepare(ctx, l, reconDir)
	if err != nil {
		_ = os.RemoveAll(tempRoot)
		return PreparedRun{}, err
	}
	lcPath, err := submissionLCPath(l, prepared.Record.ID)
	if err != nil {
		_ = os.RemoveAll(tempRoot)
		return PreparedRun{}, err
	}
	lcRC, err := source.PackageLC(sub.LocalChanges)
	if err != nil {
		_ = os.RemoveAll(tempRoot)
		return PreparedRun{}, err
	}
	lcBytes, err := io.ReadAll(lcRC)
	_ = lcRC.Close()
	if err != nil {
		_ = os.RemoveAll(tempRoot)
		return PreparedRun{}, err
	}
	if err := os.WriteFile(lcPath, lcBytes, 0o600); err != nil {
		_ = os.RemoveAll(tempRoot)
		return PreparedRun{}, err
	}
	return prepared, nil
}

// BuildFromSubmission reconstructs and runs a submission build to completion.
func BuildFromSubmission(ctx context.Context, l model.Layout, projectName string, r io.Reader) (BuildResult, error) {
	prepared, err := PrepareFromSubmission(ctx, l, projectName, r)
	if err != nil {
		return BuildResult{}, err
	}
	return ExecutePrepared(ctx, l, prepared.Record.ID)
}

// localSeed returns the configured local source checkout for a project: the
// SourcePaths entry of the project's first hostless machine. It is the
// reconstruction seed for submission-driven builds.
func localSeed(cfg config.Config, projectName string) (string, error) {
	for _, name := range cfg.Projects[projectName].Machines {
		machine := cfg.Machines[name]
		if machine.Host != "" {
			continue
		}
		if path := machine.SourcePaths[projectName]; path != "" {
			return path, nil
		}
	}
	return "", fmt.Errorf("no local source path configured for project %q", projectName)
}

// ProbeSeed reports the HEAD commit of a project's local seed checkout, or an
// empty string when no seed is configured or the seed is not a Git repository.
// A remote client uses it to compute the smallest bundle that advances the
// coordinator's seed to the client head.
func ProbeSeed(ctx context.Context, l model.Layout, projectName string) (string, error) {
	cfg, err := config.Load(l.ConfigPath())
	if err != nil {
		return "", err
	}
	if cfg.Orchestrator != config.OrchestratorSelf {
		return "", fmt.Errorf("client root: orchestrator = %q", cfg.Orchestrator)
	}
	seed, err := localSeed(cfg, projectName)
	if err != nil {
		return "", nil
	}
	var out strings.Builder
	if _, err := (process.Native{}).Run(ctx, process.Command{Executable: "git", Args: []string{"-C", seed, "rev-parse", "HEAD"}, Stdout: &out}); err != nil {
		return "", nil
	}
	return strings.TrimSpace(out.String()), nil
}

func ExecutePrepared(ctx context.Context, l model.Layout, id model.RunID) (BuildResult, error) {
	runStore := store.Store{Layout: l}
	record, err := runStore.Load(id)
	if err != nil {
		return BuildResult{}, err
	}
	if record.State == model.RunAborted {
		return BuildResult{}, fmt.Errorf("run %s was aborted", id)
	}
	// Only staging runs may start.
	if record.State != model.RunStaging {
		return BuildResult{}, fmt.Errorf("run %s is not in staging state (state=%s)", id, record.State)
	}
	// Verify the scheduler reservation before starting.
	if _, err := scheduler.ReservationFor(l, record.Machine, id); err != nil {
		return BuildResult{}, fmt.Errorf("scheduler reservation missing for %s on %s: %w", id, record.Machine, err)
	}
	defer func() { _ = scheduler.Release(l, record.Machine, id) }()
	// Claim the worker lease so reaper and cancellation can track liveness.
	owner, err := process.NewOwner()
	if err != nil {
		return BuildResult{}, err
	}
	const ttl = 30 * time.Minute
	if err := store.Claim(l, id, owner, time.Now().UTC(), ttl); err != nil {
		return BuildResult{}, err
	}
	defer func() { _ = store.Release(l, id, owner) }()
	// Re-check state after claiming the lease.
	recheck, err := runStore.Load(id)
	if err != nil {
		return BuildResult{}, err
	}
	if recheck.State != model.RunStaging {
		return BuildResult{}, fmt.Errorf("run %s left staging state before lease (state=%s)", id, recheck.State)
	}
	var snapshot runSnapshot
	if err := json.Unmarshal(record.ConfigSnapshot, &snapshot); err != nil {
		return BuildResult{}, fmt.Errorf("decode run configuration snapshot: %w", err)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var ownershipLost atomic.Bool
	stop := make(chan struct{})
	defer close(stop)
	runDir, _ := l.RunDir(string(id))
	go watchCancellation(workerCtx, cancel, runStore, id, filepath.Join(runDir, "execution.json"), stop)
	go renewLease(workerCtx, cancel, l, id, owner, ttl, &ownershipLost, stop)

	if cancellationRequested(runStore, id) {
		_, _ = runStore.Transition(id, model.RunAborted, time.Now().UTC())
		return BuildResult{}, fmt.Errorf("run %s was cancelled before execution", id)
	}
	partial := filepath.Join(l.WorkDir(), string(record.ID)+".partial")
	workspace := filepath.Join(l.WorkDir(), string(record.ID))
	if err := populateWorkspace(l, record, snapshot.ProjectConfig, partial); err != nil {
		_ = removeOwned(partial)
		if cancellationRequested(runStore, id) {
			_, _ = runStore.Transition(id, model.RunAborted, time.Now().UTC())
		} else {
			_, _ = runStore.Transition(id, model.RunLost, time.Now().UTC())
		}
		return BuildResult{}, err
	}
	if err := os.Rename(partial, workspace); err != nil {
		_ = removeOwned(partial)
		_, _ = runStore.Transition(id, model.RunLost, time.Now().UTC())
		return BuildResult{}, err
	}
	defer cleanReconstructStaging(l, record.SourcePath)
	if cancellationRequested(runStore, id) {
		_ = removeOwned(workspace)
		_, _ = runStore.Transition(id, model.RunAborted, time.Now().UTC())
		return BuildResult{}, fmt.Errorf("run %s was cancelled during staging", id)
	}
	if _, err := runStore.Transition(id, model.RunRunning, time.Now().UTC()); err != nil {
		_ = removeOwned(workspace)
		return BuildResult{}, err
	}

	stdoutFile, err := os.OpenFile(filepath.Join(runDir, "stdout.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		_ = removeOwned(workspace)
		return BuildResult{}, err
	}
	defer stdoutFile.Close()
	stderrFile, err := os.OpenFile(filepath.Join(runDir, "stderr.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		_ = removeOwned(workspace)
		return BuildResult{}, err
	}
	defer stderrFile.Close()
	pair := process.NewPair(stdoutFile, stderrFile, snapshot.LogLimits.StdoutBytes, snapshot.LogLimits.StderrBytes)
	project := snapshot.ProjectConfig
	if len(project.Command) == 0 {
		project.Command = record.Command
	}
	execRunner := selectExecutor(snapshot)
	machine := resolvedMachine(snapshot)
	var execResult runtime.Result
	var execErr error
	var collectedArtifacts []string
	var artifactsTruncated bool
	if machine.Host != "" {
		// Remote path: run via ssh on the target host.
		lcPath, _ := submissionLCPath(l, id)
		submittedHead := snapshot.SourceHead
		if submittedHead == "" && record.SourcePath != "" {
			identity, err := source.CaptureIdentity(workerCtx, record.SourcePath, process.Native{})
			if err == nil {
				submittedHead = identity.Head
			}
		}
		// Leave an unresolvable submittedHead empty and proceed to
		// executeRemote: stageOrReconstruct falls back to full workspace
		// staging whenever reconstruction cannot proceed.
		execResult, collectedArtifacts, artifactsTruncated, execErr = executeRemote(workerCtx, l, id, runDir, machine, record.SourcePath, workspace, project, record.Project, lcPath, submittedHead, pair)
	} else {
		execResult, execErr = execRunner.ExecuteSupervised(workerCtx, runtime.Request{Executable: project.Command[0], Args: project.Command[1:], Workspace: workspace, Environment: project.Environment, Stdout: pair.Stdout, Stderr: pair.Stderr}, func(running process.Running) error {
			execution := process.Execution{SchemaVersion: model.SchemaVersion, RunID: id, Owner: owner, PID: running.PID, PGID: running.PGID, StartedAt: running.StartedAt, CancellationPhase: process.CancellationNone}
			return process.WriteExecution(filepath.Join(runDir, "execution.json"), execution)
		})
		// Child is already reaped; only the Native retained-child supervisor can signal it.
		executionPath := filepath.Join(runDir, "execution.json")
		if execution, readErr := process.ReadExecution(executionPath, id); readErr == nil {
			execution.CancellationPhase = process.CancellationReaped
			_ = process.WriteExecution(executionPath, execution)
		}
	}
	if ownershipLost.Load() {
		_ = removeOwned(workspace)
		_ = process.RemoveExecution(filepath.Join(runDir, "execution.json"))
		return BuildResult{}, fmt.Errorf("run %s lost worker ownership", id)
	}
	// Collect artifacts before publishing. Local runs only.
	if machine.Host == "" && len(snapshot.ProjectConfig.Artifacts) > 0 {
		var collectErr error
		collectedArtifacts, artifactsTruncated, collectErr = CollectArtifacts(workspace, runDir, snapshot.ProjectConfig.Artifacts)
		if collectErr != nil {
			_ = removeOwned(workspace)
			return BuildResult{}, fmt.Errorf("collect artifacts: %w", collectErr)
		}
	}
	latest, loadErr := runStore.Load(id)
	cancelled := cancelledByWorker(workerCtx, latest, loadErr)
	state, failure := model.RunSucceeded, ""
	if cancelled {
		state = model.RunAborted
	} else if execErr != nil {
		state, failure = model.RunFailed, "infrastructure"
	} else if execResult.ExitCode != 0 {
		state, failure = model.RunFailed, "job"
	}
	result := BuildResult{RunID: id, Project: record.Project, Machine: record.Machine, State: state, SourceDigest: record.SourceDigest, ConfigDigest: record.ConfigDigest, ConfigSnapshot: record.ConfigSnapshot, ExitCode: execResult.ExitCode, Failure: failure, StdoutPath: filepath.Join(runDir, "stdout.log"), StderrPath: filepath.Join(runDir, "stderr.log"), StdoutTruncated: pair.Stdout.Truncated(), StderrTruncated: pair.Stderr.Truncated(), Artifacts: collectedArtifacts, ArtifactsTruncated: artifactsTruncated, Executor: execResult}
	if _, err := runStore.Transition(id, model.RunCommitting, time.Now().UTC()); err != nil {
		_ = removeOwned(workspace)
		return BuildResult{}, err
	}
	if err := runStore.PublishResult(id, result); err != nil {
		_ = removeOwned(workspace)
		return BuildResult{}, err
	}
	if _, err := runStore.Transition(id, state, time.Now().UTC()); err != nil {
		_ = removeOwned(workspace)
		return BuildResult{}, err
	}
	_ = process.RemoveExecution(filepath.Join(runDir, "execution.json"))
	// Drop the persisted submission archive with the rest of the run's cleanup;
	// a prepared-but-unexecuted run keeps it available for inspection.
	if lcPath, lcErr := submissionLCPath(l, id); lcErr == nil {
		_ = os.Remove(lcPath)
	}
	if err := removeOwned(workspace); err != nil {
		_ = os.WriteFile(filepath.Join(runDir, "cleanup.pending"), []byte(err.Error()), 0o600)
	}
	return result, nil
}

// cleanReconstructStaging removes the reconstructed staging dir once the run
// workspace has been populated from it. It only removes paths strictly under
// the Vci temp directory, so a local build's source checkout is never touched.
func cleanReconstructStaging(l model.Layout, sourcePath string) {
	if sourcePath == "" {
		return
	}
	parent := filepath.Dir(sourcePath)
	rel, err := filepath.Rel(l.TempDir(), parent)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return
	}
	_ = os.RemoveAll(parent)
}

// populateWorkspace fills a partial workspace directory for a run. A run with a
// recorded source path is reconstructed by copying that source and pruning
// coordinator-owned exclusions; otherwise it is materialized from its manifest.
func populateWorkspace(l model.Layout, record store.RunRecord, project config.Project, partial string) error {
	if record.SourcePath != "" {
		skip := func(name string) bool { return name == ".vci" || name == ".git" }
		if err := source.CopyWorkspace(record.SourcePath, partial, skip); err != nil {
			return err
		}
		return source.ApplyExclusions(partial, project.ExcludedPaths)
	}
	blobStore := source.BlobStore{Layout: l}
	manifest, err := blobStore.LoadManifest(record.SourceDigest)
	if err != nil {
		return err
	}
	return blobStore.Materialize(manifest, partial)
}

func Check(l model.Layout, id model.RunID) (any, error) {
	runStore := store.Store{Layout: l}
	record, err := runStore.Load(id)
	if err != nil {
		return nil, err
	}
	if model.IsTerminal(record.State) {
		if data, err := runStore.ReadResult(id); err == nil {
			var result any
			if json.Unmarshal(data, &result) == nil {
				return result, nil
			}
		}
	}
	return record, nil
}

func removeOwned(path string) error {
	// Restore permissions first; build tools may mark cache files read-only.
	_ = filepath.WalkDir(path, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			_ = os.Chmod(current, 0o700)
		} else {
			_ = os.Chmod(current, 0o600)
		}
		return nil
	})
	return os.RemoveAll(path)
}
