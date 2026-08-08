package app

// Coordinator-side driver for a machine whose `host` field selects a
// remote worker host via the ordinary system ssh. The local executor
// path is untouched; this file is the host branch of
// ExecutePrepared.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/executor"
	"github.com/hypernewbie/vci/internal/host"
	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/logs"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/reaper"
)

// executeRemote is the coordinator-side driver for a machine whose
// `host` field selects a remote SSH destination. It stages the
// materialized workspace into the remote `~/.vci/state/work/<run>`
// tree over the same `tar | ssh` channel the client already uses,
// runs the selected runtime remotely via `ssh <host> <sh -c ...>`,
// fetches the workspace back with `scp` when the project declares
// artifact globs (the local CollectArtifacts stays the collector; the
// remote tree is only a transfer), and best-effort removes the remote
// tree. No relay, daemon, framed protocol, or Go SSH client: only the
// system `ssh`, `tar`, and `scp` executables cross the boundary, and
// only the workspace path plus the project environment reach the
// remote shell.
//
// The return value mirrors the local executor path: an
// executor.Result whose ExitCode classifies job failures, the artifact
// matches the caller publishes, and the exec-level error (nil for a
// non-zero remote exit — that is a job failure, not infrastructure).
func executeRemote(ctx context.Context, l layout.Layout, id model.RunID, runDir string, machine config.Machine, workspace string, project config.Project, pair logs.Pair) (executor.Result, []string, bool, error) {
	remoteWorkDir, err := host.RemoteWorkDir(id)
	if err != nil {
		return executor.Result{}, nil, false, err
	}
	defer func() {
		// Best-effort remote turd cleanup on the common path. The
		// reaper owns the backstop sweep of `~/.vci/state/work/<run>`
		// for terminal runs; this removes the tree here so a
		// successful run does not wait for the next reap.
		_ = reaper.CleanupRemote(context.Background(), machine.Host, remoteWorkDir)
	}()

	if err := host.StageRemote(ctx, machine.Host, remoteWorkDir, workspace); err != nil {
		return executor.Result{}, nil, false, fmt.Errorf("stage remote workspace: %w", err)
	}
	argv, err := remoteArgv(machine, remoteWorkDir, project.Command)
	if err != nil {
		return executor.Result{}, nil, false, err
	}
	started := time.Now().UTC()
	exitCode, runErr := host.RunRemote(ctx, machine.Host, remoteWorkDir, argv, project.Environment, pair.Stdout, pair.Stderr)
	finished := time.Now().UTC()
	result := executor.Result{ExitCode: exitCode, ResolvedExecutable: argv[0], StartedAt: started, FinishedAt: finished, Duration: finished.Sub(started)}

	// Fetch artifact matches back with scp and let the local collector
	// publish them into runDir/artifacts. `scp -r` lays the remote
	// tree at <fetchParent>/<run_id>, so the collector walks that
	// directory. The fetch parent is a Vci-owned temp root under
	// state/tmp, so an abandoned fetch is reaper-owned.
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
