package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/executor"
	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/lease"
	"github.com/hypernewbie/vci/internal/logs"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/process"
	"github.com/hypernewbie/vci/internal/reaper"
	"github.com/hypernewbie/vci/internal/scheduler"
	"github.com/hypernewbie/vci/internal/source"
	"github.com/hypernewbie/vci/internal/sourcecache"
	"github.com/hypernewbie/vci/internal/store"
)

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
	Executor           executor.Result `json:"executor"`
}
type PreparedRun struct {
	Record store.RunRecord
}
type runSnapshot struct {
	ProjectConfig config.Project            `json:"project_config"`
	Machine       string                    `json:"machine,omitempty"`
	Machines      map[string]config.Machine `json:"machines"`
	LogLimits     config.LogLimits          `json:"log_limits"`
}

// stagingMeta is the Vci-owned identity record the staging shell writes
// as a sibling of the staged project directory. It carries only the
// validated cache key fields and is never part of the project tree, so
// it cannot become build input.
type stagingMeta struct {
	FormatVersion string
	Digest        string
	Project       string
}

// reconcileSourceCache is the sole production call path for digest
// verification, cache lookup, publication, and active claims. It
// distinguishes three source classes by path containment under this
// Vci root:
//
//   - a completed cache entry tree (state/source-cache/v1/<digest>/<project>):
//     validate the entry, refresh last-use, and hold an active claim
//     while the caller captures the tree into the run's independent
//     source;
//   - a direct staging tree (state/tmp/vci-source-*/<project>): read
//     the Vci-owned staging meta, recompute the canonical snapshot
//     digest from the received bytes, fail as infrastructure on any
//     mismatch before a run exists, and publish the verified tree
//     (best effort; a quota rejection or a competing winner is not a
//     build failure);
//   - any other path: an ordinary local build with no cache behavior.
//
// The returned release function, when non-nil, must be deferred until
// the cache capture is complete.
func reconcileSourceCache(ctx context.Context, l layout.Layout, quota int64, repoRoot, projectName string) (func(), error) {
	// 1. Completed cache entry tree.
	if digest, ok := cacheEntryDigestAt(l, repoRoot, projectName); ok {
		cacheRoot := l.SourceCacheDir()
		hit, _, err := sourcecache.IsHit(cacheRoot, digest, projectName)
		if err != nil {
			return nil, fmt.Errorf("source cache check failed: %w", err)
		}
		if !hit {
			return nil, fmt.Errorf("source cache entry %s is not complete", digest)
		}
		// Cache-hit validation: the tree must still canonicalize to
		// its recorded digest before it is captured.
		if err := source.VerifySnapshot(repoRoot, digest); err != nil {
			return nil, fmt.Errorf("source cache entry %s failed verification: %w", digest, err)
		}
		claimID, err := newClaimID()
		if err != nil {
			return nil, fmt.Errorf("source cache claim: %w", err)
		}
		if err := sourcecache.AcquireActiveClaim(cacheRoot, digest, projectName, claimID); err != nil {
			return nil, fmt.Errorf("source cache claim: %w", err)
		}
		// A real cache hit refreshes the recorded last-use; the reaper
		// evicts by this metadata, not by file mtimes.
		_ = sourcecache.UpdateLastUse(cacheRoot, digest, projectName, time.Now().UTC())
		return func() { sourcecache.ReleaseActiveClaim(cacheRoot, digest, projectName, claimID) }, nil
	}

	// 2. Direct staging tree under this root's Vci-owned temporary dir.
	if metaPath, ok := stagingMetaPathAt(l, repoRoot); ok {
		meta, err := readStagingMeta(metaPath)
		if err != nil {
			return nil, fmt.Errorf("read staging meta: %w", err)
		}
		if meta.Project != projectName {
			return nil, fmt.Errorf("staged project %q does not match repository %q", meta.Project, projectName)
		}
		if meta.FormatVersion != sourcecache.FormatVersion {
			return nil, fmt.Errorf("staging format version %q is not supported", meta.FormatVersion)
		}
		// Recompute the canonical snapshot digest from the received
		// bytes. A mismatch is an infrastructure failure: no complete
		// entry is created and no run ID is returned.
		if err := source.VerifySnapshot(repoRoot, meta.Digest); err != nil {
			return nil, fmt.Errorf("source cache digest mismatch: %w", err)
		}
		// Publish the verified tree. The cache is an optimization: an
		// admission rejection or a competing winner leaves the build
		// on the direct one-shot path without an entry.
		if _, pubErr := sourcecache.PublishTree(l.SourceCacheDir(), meta.Digest, meta.Project, repoRoot, quota); pubErr != nil && !errors.Is(pubErr, sourcecache.ErrAdmissionRejected) {
			_ = pubErr
		}
		return nil, nil
	}

	// 3. Ordinary local build.
	return nil, nil
}

// cacheEntryDigestAt returns the digest when repoRoot is exactly the
// immutable source tree of a Vci-owned cache entry
// (state/source-cache/v1/<digest>/<project>/<project>). Path comparison
// resolves symlinks so macOS /tmp-style indirection cannot hide a
// match.
func cacheEntryDigestAt(l layout.Layout, repoRoot, projectName string) (string, bool) {
	cacheRoot, err := filepath.EvalSymlinks(l.SourceCacheDir())
	if err != nil {
		return "", false
	}
	realRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(cacheRoot, realRoot)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", false
	}
	parts := strings.Split(rel, string(os.PathSeparator))
	// The tree sits at v1/<digest>/<project>/<project>; the entry root
	// is its parent.
	if len(parts) != 4 || parts[0] != sourcecache.FormatVersion {
		return "", false
	}
	if parts[2] != projectName || parts[3] != projectName || !sourcecache.ValidDigest(parts[1]) {
		return "", false
	}
	return parts[1], true
}

// stagingMetaPathAt returns the Vci-owned staging meta path when
// repoRoot is the project directory of a direct staging tree
// (state/tmp/vci-source-*/<project>).
func stagingMetaPathAt(l layout.Layout, repoRoot string) (string, bool) {
	tmpDir, err := filepath.EvalSymlinks(l.TempDir())
	if err != nil {
		return "", false
	}
	realRoot, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return "", false
	}
	parent := filepath.Dir(realRoot)
	if filepath.Dir(parent) != tmpDir {
		return "", false
	}
	if !strings.HasPrefix(filepath.Base(parent), source.StagingPrefix) {
		return "", false
	}
	meta := filepath.Join(parent, source.StagingMetaName)
	if _, err := os.Stat(meta); err != nil {
		return "", false
	}
	return meta, true
}

// readStagingMeta parses and validates the staging meta record written
// by the staging shell. Only validated fields are accepted.
func readStagingMeta(path string) (stagingMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return stagingMeta{}, err
	}
	fields := strings.Fields(string(data))
	if len(fields) != 3 {
		return stagingMeta{}, fmt.Errorf("malformed staging meta at %s", path)
	}
	meta := stagingMeta{FormatVersion: fields[0], Digest: fields[1], Project: fields[2]}
	if meta.FormatVersion != sourcecache.FormatVersion {
		return stagingMeta{}, fmt.Errorf("unsupported staging format version %q", meta.FormatVersion)
	}
	if !sourcecache.ValidDigest(meta.Digest) {
		return stagingMeta{}, fmt.Errorf("staging meta has invalid digest %q", meta.Digest)
	}
	if !layout.ValidName(meta.Project) {
		return stagingMeta{}, fmt.Errorf("staging meta has invalid project %q", meta.Project)
	}
	return meta, nil
}

// newClaimID returns a fresh random claim identifier for an active
// cache capture.
func newClaimID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "claim-" + hex.EncodeToString(raw[:]), nil
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
	if len(project.Machines) == 0 {
		return PreparedRun{}, fmt.Errorf("project %q has no attached machines", projectName)
	}

	// Coordinator-owned source-cache handling at the public build
	// boundary. No wire protocol is involved: the direct staging tree
	// and the cache-hit tree are both Vci-owned paths under this root.
	// On the cache-hit path, the claim is held only while the entry is
	// captured into the run's independent source below. The effective
	// quota applies the documented default when
	// retention.source_cache_bytes is omitted, so admission is always
	// bounded.
	releaseClaim, err := reconcileSourceCache(ctx, l, reaper.SourceCacheQuota(cfg), repo.Root, projectName)
	if err != nil {
		return PreparedRun{}, err
	}
	if releaseClaim != nil {
		defer releaseClaim()
	}

	// Local coordinator builds share the same recursive source
	// validation as direct-SSH: an uninitialized submodule or an
	// LFS pointer fails before any source manifest is produced. The
	// attribute-graph + LFS-pointer rules live in internal/source
	// and are reused here so local and remote paths agree.
	graph, err := source.SelectBuildInput(ctx, repo.Root, process.Native{})
	if err != nil {
		return PreparedRun{}, err
	}
	snapParent, err := os.MkdirTemp(l.TempDir(), source.SnapshotPrefix)
	if err != nil {
		return PreparedRun{}, fmt.Errorf("prepare local snapshot dir: %w", err)
	}
	defer os.RemoveAll(snapParent)
	if _, err := source.MaterializeSnapshot(graph, snapParent); err != nil {
		return PreparedRun{}, err
	}

	// BuildWithValidation checks LFS-attributed files against the
	// formal pointer format using the same bytes that become a
	// blob. The local manifest construction is the source of truth
	// for what the run will execute against; the validation cannot
	// be skipped.
	manifest, blobs, err := source.BuildWithValidation(repo.Root, graph.LFSFiles)
	if err != nil {
		return PreparedRun{}, err
	}
	blobStore := source.BlobStore{Layout: l}
	if err := blobStore.PutManifestAndBlobs(manifest, blobs); err != nil {
		return PreparedRun{}, err
	}
	now := time.Now().UTC()
	runStore := store.Store{Layout: l}
	// Reserve a slot on an eligible machine and publish the staging
	// record atomically. The transaction holds the scheduler lock
	// from sweep-through-save, so a crashed caller cannot leave an
	// orphan claim. Prepare never persists a queued record under the
	// public build path: the reservation transaction publishes the
	// record directly in `staging` state.
	draftRecord, err := store.NewRun(projectName, project.Machines[0], project.Command, manifest.Digest, buildDraftSnapshot(projectName, project, cfg), now)
	if err != nil {
		return PreparedRun{}, err
	}
	var staged store.RunRecord
	err = scheduler.ReserveAndPublish(l, runStore, cfg, draftRecord.ID, project.Machines, now, func(machineName string) error {
		record, buildErr := store.NewStagedRunFromID(draftRecord.ID, projectName, machineName, project.Command, manifest.Digest, buildStagedSnapshot(projectName, project, machineName, cfg, nil), now)
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
		if errors.Is(err, scheduler.ErrNoCapacity) {
			return PreparedRun{}, err
		}
		return PreparedRun{}, err
	}
	return PreparedRun{Record: staged}, nil
}

// buildDraftSnapshot composes the public build path's draft config
// snapshot. Direct/local builds get no source_provenance block; the
// hosted entrypoint injects one via buildStagedSnapshot. The draft's
// config digest is discarded; only the run ID is reused.
func buildDraftSnapshot(projectName string, project config.Project, cfg config.Config) map[string]any {
	return map[string]any{
		"schema_version": config.SchemaVersion,
		"project":        projectName,
		"project_config": project,
		"log_limits":     cfg.LogLimits,
		"retention":      cfg.Retention,
	}
}

// buildStagedSnapshot composes the staged config snapshot. The
// additive `source_provenance` block is the only place a hosted
// build differs from a direct/local build; its presence is the
// signal that no local source path was used. When `provenance` is
// nil, the block is omitted.
func buildStagedSnapshot(projectName string, project config.Project, machineName string, cfg config.Config, provenance map[string]any) map[string]any {
	out := map[string]any{
		"schema_version": config.SchemaVersion,
		"project":        projectName,
		"project_config": project,
		"machine":        machineName,
		"machines":       cfg.Machines,
		"log_limits":     cfg.LogLimits,
		"retention":      cfg.Retention,
	}
	if provenance != nil {
		out["source_provenance"] = provenance
	}
	return out
}

// PrepareHosted is the coordinator-only entrypoint for
// `vci build --hosted <project>`. It validates the configured
// pinned hosted fallback, performs a single-shot pinned Git
// checkout of the configured commit into a fresh
// `l.TempDir()/vci-hosted-<rand>/<project>` root, runs the same
// source-validation + manifest pipeline as Prepare against the
// checked-out root, and emits a run record with additive hosted
// provenance. The checkout is removed on every exit path. The
// source-cache path is intentionally skipped: hosted checkouts do
// not participate in source-cache admission (the temp root matches
// neither the staging nor the cache-entry shape).
//
// The returned PreparedRun is identical in shape to Prepare's
// result; the only difference is the additive source_provenance
// block in the staged record's ConfigSnapshot.
func PrepareHosted(ctx context.Context, l layout.Layout, projectName string) (PreparedRun, error) {
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
	if !layout.ValidName(projectName) {
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
	// Checkout the pinned commit into a fresh coordinator-owned root.
	// The root is removed on every exit path; the source-validation
	// pipeline below consumes it before cleanup. A successful
	// return leaves the manifest/blobs persisted in the blob store
	// and the checkout removed.
	checkoutRoot, err := source.Checkout(ctx, process.Native{}, l, projectName, validated)
	if err != nil {
		return PreparedRun{}, err
	}
	defer func() { _ = source.CleanUnder(checkoutRoot, l.TempDir()) }()

	// Same source-validation pipeline as Prepare: an uninitialized
	// submodule or an LFS pointer fails before any source manifest
	// is produced. The attribute-graph + LFS-pointer rules live in
	// internal/source and run unchanged on the checked-out root.
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
		record, buildErr := store.NewStagedRunFromID(draftRecord.ID, projectName, machineName, project.Command, manifest.Digest, buildStagedSnapshot(projectName, project, machineName, cfg, provenance), now)
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
		if errors.Is(err, scheduler.ErrNoCapacity) {
			return PreparedRun{}, err
		}
		return PreparedRun{}, err
	}
	return PreparedRun{Record: staged}, nil
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
	// The detached worker must only begin work when the run record
	// is still in `staging` state. A reaper terminalization or a
	// competing failing path can move the record out of staging
	// between the reservation and the worker start; refusing here
	// means the slot is freed without a half-built workspace.
	if record.State != model.RunStaging {
		return BuildResult{}, fmt.Errorf("run %s is not in staging state (state=%s)", id, record.State)
	}
	// Verify the reservation before any work begins. The detached
	// worker must not create an unreserved execution. The reservation
	// is validated by the scheduler, not by a raw Stat: a missing
	// claim surfaces as os.ErrNotExist and a corrupt claim surfaces
	// as a wrapped error.
	if _, err := scheduler.ReservationFor(l, record.Machine, id); err != nil {
		return BuildResult{}, fmt.Errorf("scheduler reservation missing for %s on %s: %w", id, record.Machine, err)
	}
	defer func() { _ = scheduler.Release(l, record.Machine, id) }()
	// Claim the worker lease before any workspace or process work.
	// The lease is what the reaper and the cancellation path use to
	// recognize a live worker; without it, the reaper would free the
	// slot on the next sweep and a duplicate worker could start.
	owner, err := process.NewOwner()
	if err != nil {
		return BuildResult{}, err
	}
	const ttl = 30 * time.Minute
	if err := lease.Claim(l, id, owner, time.Now().UTC(), ttl); err != nil {
		return BuildResult{}, err
	}
	defer func() { _ = lease.Release(l, id, owner) }()
	// Re-check state after claiming the lease. A reaper that
	// terminalized the record between the initial load and the lease
	// claim must not see a half-built workspace. Release the lease
	// and the reservation cleanly so the slot is freed for the next
	// submission.
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
	manifest, err := (source.BlobStore{Layout: l}).LoadManifest(record.SourceDigest)
	if err != nil {
		return BuildResult{}, err
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
	execRunner := selectExecutor(snapshot)
	machine := resolvedMachine(snapshot)
	var execResult executor.Result
	var execErr error
	var collectedArtifacts []string
	var artifactsTruncated bool
	if machine.Host != "" {
		// Remote host execution: the detached worker delegates to the
		// machine's host via the ordinary system `ssh`. The remote
		// lifecycle (stage, run, fetch artifacts, cleanup) lives in
		// executeRemote; the local executor and its execution.json
		// bookkeeping are skipped because the supervised child is the
		// ssh process, not a locally-retained process.
		execResult, collectedArtifacts, artifactsTruncated, execErr = executeRemote(workerCtx, l, id, runDir, machine, workspace, project, pair)
	} else {
		execResult, execErr = execRunner.ExecuteSupervised(workerCtx, executor.Request{Executable: project.Command[0], Args: project.Command[1:], Workspace: workspace, Environment: project.Environment, Stdout: pair.Stdout, Stderr: pair.Stderr}, func(running process.Running) error {
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
	}
	if ownershipLost.Load() {
		_ = removeOwned(workspace)
		_ = process.RemoveExecution(filepath.Join(runDir, "execution.json"))
		return BuildResult{}, fmt.Errorf("run %s lost worker ownership", id)
	}
	// Collect artifacts before publishing. The run state is
	// `running`; artifact collection is read-only against the
	// source workspace and writes only into the run's owned
	// artifacts directory. The collector caps at 64 MiB and sets
	// ArtifactsTruncated if any match overflows. Local runs only:
	// the remote path already fetched and collected inside
	// executeRemote.
	if machine.Host == "" && len(snapshot.ProjectConfig.Artifacts) > 0 {
		var collectErr error
		collectedArtifacts, artifactsTruncated, collectErr = CollectArtifacts(workspace, runDir, snapshot.ProjectConfig.Artifacts)
		if collectErr != nil {
			_ = removeOwned(workspace)
			return BuildResult{}, fmt.Errorf("collect artifacts: %w", collectErr)
		}
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
