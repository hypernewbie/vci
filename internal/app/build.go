package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/executor"
	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/lease"
	"github.com/hypernewbie/vci/internal/logs"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/process"
	"github.com/hypernewbie/vci/internal/source"
	"github.com/hypernewbie/vci/internal/store"
)

type BuildResult struct {
	RunID           model.RunID     `json:"run_id"`
	Project         string          `json:"project"`
	Machine         string          `json:"machine"`
	State           model.RunState  `json:"state"`
	SourceDigest    string          `json:"source_digest"`
	ConfigDigest    string          `json:"config_digest"`
	ConfigSnapshot  json.RawMessage `json:"config_snapshot"`
	ExitCode        int             `json:"exit_code"`
	Failure         string          `json:"failure,omitempty"`
	StdoutPath      string          `json:"stdout_path,omitempty"`
	StderrPath      string          `json:"stderr_path,omitempty"`
	StdoutTruncated bool            `json:"stdout_truncated"`
	StderrTruncated bool            `json:"stderr_truncated"`
	Executor        executor.Result `json:"executor"`
}
type PreparedRun struct {
	Record store.RunRecord
}
type runSnapshot struct {
	ProjectConfig config.Project   `json:"project_config"`
	LogLimits     config.LogLimits `json:"log_limits"`
}

func Prepare(ctx context.Context, l layout.Layout, sourcePath string) (PreparedRun, error) {
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
	if len(project.Machines) != 1 {
		return PreparedRun{}, fmt.Errorf("project %q must have exactly one local machine in the first build slice", projectName)
	}
	machineName := project.Machines[0]
	manifest, blobs, err := source.Build(repo.Root)
	if err != nil {
		return PreparedRun{}, err
	}
	blobStore := source.BlobStore{Layout: l}
	if err := blobStore.PutManifestAndBlobs(manifest, blobs); err != nil {
		return PreparedRun{}, err
	}
	snapshot := map[string]any{"schema_version": config.SchemaVersion, "project": projectName, "project_config": project, "machine": machineName, "log_limits": cfg.LogLimits, "retention": cfg.Retention}
	runStore := store.Store{Layout: l}
	record, err := store.NewRun(projectName, machineName, project.Command, manifest.Digest, snapshot, time.Now().UTC())
	if err != nil {
		return PreparedRun{}, err
	}
	if err := runStore.Save(record); err != nil {
		return PreparedRun{}, err
	}
	staged, err := runStore.Transition(record.ID, model.RunStaging, time.Now().UTC())
	if err != nil {
		return PreparedRun{}, err
	}
	return PreparedRun{Record: staged}, nil
}

func Build(ctx context.Context, l layout.Layout, sourcePath string) (BuildResult, error) {
	prepared, err := Prepare(ctx, l, sourcePath)
	if err != nil {
		return BuildResult{}, err
	}
	return ExecutePrepared(ctx, l, prepared.Record.ID)
}

func ExecutePrepared(ctx context.Context, l layout.Layout, id model.RunID) (BuildResult, error) {
	runStore := store.Store{Layout: l}
	record, err := runStore.Load(id)
	if err != nil {
		return BuildResult{}, err
	}
	if record.State == model.RunAborted {
		return BuildResult{}, fmt.Errorf("run %s was aborted", id)
	}
	var snapshot runSnapshot
	if err := json.Unmarshal(record.ConfigSnapshot, &snapshot); err != nil {
		return BuildResult{}, fmt.Errorf("decode run configuration snapshot: %w", err)
	}
	manifest, err := (source.BlobStore{Layout: l}).LoadManifest(record.SourceDigest)
	if err != nil {
		return BuildResult{}, err
	}
	owner, err := process.NewOwner()
	if err != nil {
		return BuildResult{}, err
	}
	const ttl = 30 * time.Minute
	if err := lease.Claim(l, id, owner, time.Now().UTC(), ttl); err != nil {
		return BuildResult{}, err
	}
	defer func() { _ = lease.Release(l, id, owner) }()

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
	blobStore := source.BlobStore{Layout: l}
	partial := filepath.Join(l.WorkDir(), string(record.ID)+".partial")
	workspace := filepath.Join(l.WorkDir(), string(record.ID))
	if err := blobStore.Materialize(manifest, partial); err != nil {
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
	pair := logs.NewPair(stdoutFile, stderrFile, snapshot.LogLimits.StdoutBytes, snapshot.LogLimits.StderrBytes)
	project := snapshot.ProjectConfig
	if len(project.Command) == 0 {
		project.Command = record.Command
	}
	executorLocal := executor.Local{}
	execResult, execErr := executorLocal.ExecuteSupervised(workerCtx, executor.Request{Executable: project.Command[0], Args: project.Command[1:], Workspace: workspace, Environment: project.Environment, Stdout: pair.Stdout, Stderr: pair.Stderr}, func(running process.Running) error {
		execution := process.Execution{SchemaVersion: model.SchemaVersion, RunID: id, Owner: owner, PID: running.PID, PGID: running.PGID, StartedAt: running.StartedAt, CancellationPhase: process.CancellationNone}
		return process.WriteExecution(filepath.Join(runDir, "execution.json"), execution)
	})
	// The child has been waited/reaped before this point. No external process is
	// ever signalled; only Native's retained child supervisor can do that.
	executionPath := filepath.Join(runDir, "execution.json")
	if execution, readErr := process.ReadExecution(executionPath, id); readErr == nil {
		execution.CancellationPhase = process.CancellationReaped
		_ = process.WriteExecution(executionPath, execution)
	}
	if ownershipLost.Load() {
		_ = removeOwned(workspace)
		_ = process.RemoveExecution(filepath.Join(runDir, "execution.json"))
		return BuildResult{}, fmt.Errorf("run %s lost worker ownership", id)
	}
	latest, loadErr := runStore.Load(id)
	cancelled := loadErr == nil && latest.CancellationRequestedAt != nil
	if workerCtx.Err() != nil && cancelled {
		cancelled = true
	}
	state, failure := model.RunSucceeded, ""
	if cancelled {
		state = model.RunAborted
	} else if execErr != nil {
		state, failure = model.RunFailed, "infrastructure"
	} else if execResult.ExitCode != 0 {
		state, failure = model.RunFailed, "job"
	}
	result := BuildResult{RunID: id, Project: record.Project, Machine: record.Machine, State: state, SourceDigest: record.SourceDigest, ConfigDigest: record.ConfigDigest, ConfigSnapshot: record.ConfigSnapshot, ExitCode: execResult.ExitCode, Failure: failure, StdoutPath: filepath.Join(runDir, "stdout.log"), StderrPath: filepath.Join(runDir, "stderr.log"), StdoutTruncated: pair.Stdout.Truncated(), StderrTruncated: pair.Stderr.Truncated(), Executor: execResult}
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
	if err := removeOwned(workspace); err != nil {
		_ = os.WriteFile(filepath.Join(runDir, "cleanup.pending"), []byte(err.Error()), 0o600)
	}
	return result, nil
}

func watchCancellation(ctx context.Context, cancel context.CancelFunc, runStore store.Store, id model.RunID, executionPath string, stop <-chan struct{}) {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			record, err := runStore.Load(id)
			if err != nil {
				continue
			}
			if record.CancellationRequestedAt != nil {
				if execution, readErr := process.ReadExecution(executionPath, id); readErr == nil {
					execution.CancellationPhase = process.CancellationTerminating
					_ = process.WriteExecution(executionPath, execution)
				}
				cancel()
				return
			}
		}
	}
}
func renewLease(ctx context.Context, cancel context.CancelFunc, l layout.Layout, id model.RunID, owner string, ttl time.Duration, lost *atomic.Bool, stop <-chan struct{}) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := lease.Renew(l, id, owner, now.UTC(), ttl); err != nil {
				lost.Store(true)
				cancel()
				return
			}
		}
	}
}
func cancellationRequested(s store.Store, id model.RunID) bool {
	r, err := s.Load(id)
	return err == nil && r.CancellationRequestedAt != nil
}

func Check(l layout.Layout, id model.RunID) (any, error) {
	runStore := store.Store{Layout: l}
	record, err := runStore.Load(id)
	if err != nil {
		return nil, err
	}
	if record.State == model.RunSucceeded || record.State == model.RunFailed || record.State == model.RunLost || record.State == model.RunAborted {
		if data, err := runStore.ReadResult(id); err == nil {
			var result any
			if json.Unmarshal(data, &result) == nil {
				return result, nil
			}
		}
	}
	return record, nil
}
