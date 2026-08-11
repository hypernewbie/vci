package reaper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/host"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/process"
	"github.com/hypernewbie/vci/internal/scheduler"
	"github.com/hypernewbie/vci/internal/source"
	"github.com/hypernewbie/vci/internal/store"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Report struct {
	Removed                   int `json:"removed"`
	MarkedLost                int `json:"marked_lost"`
	QueuedAborted             int `json:"queued_aborted"`
	TransferRemoved           int `json:"transfer_removed"`
	PerRunTmpRemoved          int `json:"per_run_tmp_removed"`
	WorkspacesRemoved         int `json:"workspaces_removed"`
	RemoteCleaned             int `json:"remote_cleaned"`
	SchedulerClaimsReleased   int `json:"scheduler_claims_released"`
	ArtifactsReaped           int `json:"artifacts_reaped"`
	BundleCacheStaleRemoved   int `json:"bundle_cache_stale_removed"`
	BundleCacheEvictedRemoved int `json:"bundle_cache_evicted_removed"`
}

const (
	renewalGrace     = 10 * time.Minute
	transferStaleAge = 30 * time.Minute
	// preStartGrace is the short wait after staging publish for a worker lease.
	// If no lease appears in this window, the run is marked lost.
	preStartGrace = 60 * time.Second
)

func Run(l model.Layout, now time.Time) (Report, error) {
	var report Report
	runStore := store.Store{Layout: l}
	active := map[string]bool{}
	runs, err := os.ReadDir(l.RunsDir())
	if err != nil && !os.IsNotExist(err) {
		return report, err
	}
	for _, entry := range runs {
		if !entry.IsDir() {
			continue
		}
		id := model.RunID(entry.Name())
		record, loadErr := runStore.Load(id)
		if loadErr != nil {
			continue
		}
		// Legacy queued runs are not live without a lease; queueing occurs after
		// staging. A parented target is legitimately queued until dispatched, so it
		// is never reaped here; a stale reservation means its worker never started
		// and is released for the next dispatch pass.
		if record.State == model.RunQueued {
			if record.ParentRunID != "" {
				if res, err := scheduler.ReservationFor(l, record.Machine, id); err == nil && now.Sub(res.CreatedAt) >= preStartGrace {
					_ = scheduler.Release(l, record.Machine, id)
				}
				continue
			}
			if len(record.Children) == 0 && store.ReadHasNoLease(l, id) {
				if _, transitionErr := runStore.Transition(id, model.RunAborted, now); transitionErr == nil {
					report.QueuedAborted++
				}
			}
			continue
		}
		// Only staging, running, and committing states can hold a live lease.
		// Committing means publish is in progress; if lease is stale or
		// missing, mark lost so the scheduler can release the claim.
		if record.State != model.RunStaging && record.State != model.RunRunning && record.State != model.RunCommitting {
			continue
		}
		leaseRecord, leaseErr := store.Read(l, id)
		if leaseErr == nil {
			if leaseRecord.ExpiresAt.After(now.Add(-renewalGrace)) {
				active[string(id)] = true
				continue
			}
			// Stale lease is abandoned ownership; a live worker exits on renewal failure.
			// Reaper only updates state.
			if _, transitionErr := runStore.Transition(id, model.RunLost, now); transitionErr == nil {
				report.MarkedLost++
			}
			continue
		}
		if !os.IsNotExist(leaseErr) {
			// Ignore corrupt lease metadata until a later healthy lease file appears.
			continue
		}
		// No lease on staging: keep only while reservation is newer than preStartGrace.
		// Ignore claim age once a lease exists; active leases are handled above.
		if record.State == model.RunStaging {
			res, resErr := scheduler.ReservationFor(l, record.Machine, id)
			if resErr != nil {
				// Missing or corrupt reservation means no slot was claimed; mark lost to free it.
				if _, transitionErr := runStore.Transition(id, model.RunLost, now); transitionErr == nil {
					report.MarkedLost++
				}
				continue
			}
			if now.Sub(res.CreatedAt) < preStartGrace {
				// Within grace window, worker may still claim lease.
				continue
			}
			if _, transitionErr := runStore.Transition(id, model.RunLost, now); transitionErr == nil {
				report.MarkedLost++
			}
			continue
		}
		// Running/committing without lease is a stale orphan; mark lost and release claim.
		if _, transitionErr := runStore.Transition(id, model.RunLost, now); transitionErr == nil {
			report.MarkedLost++
		}
	}

	entries, err := os.ReadDir(l.WorkDir())
	if err == nil {
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".partial") {
				continue
			}
			info, infoErr := entry.Info()
			if infoErr != nil || info.ModTime().After(now.Add(-time.Minute)) {
				continue
			}
			id := strings.TrimSuffix(entry.Name(), ".partial")
			if active[id] {
				continue
			}
			if record, err := runStore.Load(model.RunID(id)); err == nil {
				if record.State == model.RunStaging || record.State == model.RunRunning {
					if _, leaseErr := store.Read(l, model.RunID(id)); leaseErr == nil {
						continue
					}
				}
			}
			if err := removeTree(filepath.Join(l.WorkDir(), entry.Name())); err != nil {
				return report, err
			}
			report.Removed++
		}
	} else if !os.IsNotExist(err) {
		return report, err
	}

	if removed, err := reapTransferDirs(l.TempDir(), now); err != nil {
		return report, err
	} else {
		report.TransferRemoved = removed
	}

	// Reap per-run workspace artifacts and remote mirrors after lease expiry.
	// Includes state/work/<run>/.tmp, .home, workspace, and remote copy for host-backed runs.
	if tmpRemoved, workspacesRemoved, err := reapWorkDirs(l, now); err != nil {
		return report, err
	} else {
		report.PerRunTmpRemoved = tmpRemoved
		report.WorkspacesRemoved = workspacesRemoved
	}
	report.RemoteCleaned = reapRemoteTurds(l, now)

	if released, err := scheduler.Reap(l, runStore); err != nil {
		return report, err
	} else {
		report.SchedulerClaimsReleased = released
	}

	// Reap artifacts for lost/aborted runs; other states follow normal retention.
	if reaped, err := ReapArtifacts(l, now); err != nil {
		return report, err
	} else {
		report.ArtifactsReaped = reaped
	}

	aggregateParents(l, runStore, now)
	return report, nil
}

// aggregateParents terminalizes build requests whose targets are all terminal,
// removing the shared temp source once a request is done. A request stays
// queued while any target is still running.
func aggregateParents(l model.Layout, runStore store.Store, now time.Time) {
	entries, err := os.ReadDir(l.RunsDir())
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		parent, err := runStore.Load(model.RunID(entry.Name()))
		if err != nil || len(parent.Children) == 0 || model.IsTerminal(parent.State) {
			continue
		}
		children, err := runStore.LoadChildren(parent.ID)
		if err != nil {
			continue
		}
		state, _ := store.AggregateState(children, parent.State == model.RunAborted)
		if state == model.RunAggregating {
			continue
		}
		// The parent is a container; its terminal state is set when all targets
		// finish, bypassing the per-target CanTransition table via Mutate.
		if _, err := runStore.Mutate(parent.ID, func(r *store.RunRecord) error {
			if model.IsTerminal(r.State) {
				return nil
			}
			r.State = state
			r.UpdatedAt = now
			return nil
		}); err == nil {
			cleanSharedSource(l, parent.SourcePath)
		}
	}
}

// cleanSharedSource removes a build request's shared temp source once the
// request is terminal. It only removes paths inside the Vci temp directory so
// a live source checkout is never touched.
func cleanSharedSource(l model.Layout, sourcePath string) {
	if sourcePath == "" {
		return
	}
	rel, err := filepath.Rel(l.TempDir(), sourcePath)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return
	}
	_ = os.RemoveAll(sourcePath)
}

// reapTransferDirs prunes TempDir subdirectories with prefixes vci-source-,
// vci-source., vci-snapshot-, or vci-hosted-. A directory is removed
// when its mod time is older than transferStaleAge.
func reapTransferDirs(tempDir string, now time.Time) (int, error) {
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	threshold := now.Add(-transferStaleAge)
	var removed int
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if !strings.HasPrefix(entry.Name(), source.StagingPrefix) && !strings.HasPrefix(entry.Name(), "vci-source.") && !strings.HasPrefix(entry.Name(), source.SnapshotPrefix) && !strings.HasPrefix(entry.Name(), source.HostedPrefix) {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			continue
		}
		if info.ModTime().After(threshold) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(tempDir, entry.Name())); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func removeTree(path string) error {
	return os.RemoveAll(path)
}

// ReapArtifacts removes artifact directories for lost/aborted runs older than transferStaleAge.
// Succeeded and failed runs keep artifacts for other retention passes.
// Returns removed directory count.
func ReapArtifacts(l model.Layout, now time.Time) (int, error) {
	entries, err := os.ReadDir(l.RunsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	runStore := store.Store{Layout: l}
	threshold := now.Add(-transferStaleAge)
	reaped := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := model.RunID(entry.Name())
		if !model.ValidRunID(id) {
			continue
		}
		record, loadErr := runStore.Load(id)
		if loadErr != nil {
			continue
		}
		if record.State != model.RunLost && record.State != model.RunAborted {
			continue
		}
		age := record.UpdatedAt
		if age.IsZero() {
			info, infoErr := entry.Info()
			if infoErr != nil {
				continue
			}
			age = info.ModTime()
		}
		if !age.Before(threshold) {
			continue
		}
		artDir := filepath.Join(l.RunsDir(), string(id), "artifacts")
		if _, statErr := os.Stat(artDir); statErr != nil {
			continue
		}
		if err := removeTree(artDir); err != nil {
			return reaped, err
		}
		reaped++
	}
	return reaped, nil
}

// reapWorkDirs removes stale temp roots and terminal workspaces without live leases.
// Returns counts of temp-root and workspace removals.
func reapWorkDirs(l model.Layout, now time.Time) (int, int, error) {
	entries, err := os.ReadDir(l.WorkDir())
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	runStore := store.Store{Layout: l}
	tmpRemoved, workspacesRemoved := 0, 0
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasSuffix(entry.Name(), ".partial") {
			continue
		}
		id := model.RunID(entry.Name())
		if !model.ValidRunID(id) {
			continue
		}
		record, loadErr := runStore.Load(id)
		leaseRecord, leaseErr := store.Read(l, id)
		liveLease := leaseErr == nil && leaseRecord.ExpiresAt.After(now.Add(-renewalGrace))
		terminal := loadErr == nil && model.IsTerminal(record.State)
		workDir := filepath.Join(l.WorkDir(), entry.Name())
		// Remove stale .tmp/.home only when no live lease exists.
		if !liveLease {
			for _, sub := range []string{".tmp", ".home"} {
				p := filepath.Join(workDir, sub)
				info, statErr := os.Stat(p)
				if statErr != nil {
					continue
				}
				if info.ModTime().After(now.Add(-transferStaleAge)) {
					continue
				}
				if err := removeTree(p); err == nil {
					tmpRemoved++
				}
			}
		}
		// Remove whole workspace for terminal runs without a live lease.
		if terminal && !liveLease {
			if err := removeTree(workDir); err != nil {
				return tmpRemoved, workspacesRemoved, err
			}
			workspacesRemoved++
		}
	}
	return tmpRemoved, workspacesRemoved, nil
}

// reapRemoteTurds removes mirrored remote workspace trees for terminal
// runs on remote hosts. If host is unreachable or misconfigured, cleanup is
// best-effort and ignored. All ssh calls are timeout-bound in CleanupRemote.
func reapRemoteTurds(l model.Layout, now time.Time) int {
	runStore := store.Store{Layout: l}
	entries, err := os.ReadDir(l.RunsDir())
	if err != nil {
		return 0
	}
	cleaned := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := model.RunID(entry.Name())
		if !model.ValidRunID(id) {
			continue
		}
		record, loadErr := runStore.Load(id)
		if loadErr != nil || !model.IsTerminal(record.State) {
			continue
		}
		leaseRecord, leaseErr := store.Read(l, id)
		if leaseErr == nil && leaseRecord.ExpiresAt.After(now.Add(-renewalGrace)) {
			// Skip if worker lease is still active while terminalization finishes.
			continue
		}
		hostName, osKind, ok := remoteHostFor(record)
		if !ok {
			continue
		}
		remoteWorkDir, err := host.RemoteWorkDir(id)
		if err != nil {
			continue
		}
		if err := CleanupRemote(context.Background(), hostName, remoteWorkDir, osKind); err == nil {
			cleaned++
		}
	}
	return cleaned
}

// remoteHostFor reads the durable config snapshot and returns the reserved
// machine host and OS kind. Returns ok=false for missing/malformed snapshot,
// unknown machine, or empty host.
func remoteHostFor(record store.RunRecord) (host string, osKind string, ok bool) {
	var snap struct {
		Machine  string                    `json:"machine"`
		Machines map[string]config.Machine `json:"machines"`
	}
	if err := json.Unmarshal(record.ConfigSnapshot, &snap); err != nil {
		return "", "", false
	}
	name := snap.Machine
	if name == "" {
		return "", "", false
	}
	machine, ok := snap.Machines[name]
	if !ok || machine.Host == "" {
		return "", "", false
	}
	return machine.Host, machine.OS, true
}

// CleanupRemote validates host and path then removes the remote workspace
// tree. A POSIX worker is cleaned with `ssh <host> rm -rf -- <path>`; a
// Windows worker (cmd.exe login shell) with `rmdir /S /Q "<path>"` over a
// %USERPROFILE%-rooted path, because cmd.exe has no rm and cannot expand ~.
// Inputs are validated and execution is bounded by a 30-second timeout.
func CleanupRemote(ctx context.Context, hostName, remotePath, osKind string) error {
	if err := host.ValidateHost(hostName); err != nil {
		return err
	}
	if err := host.ValidateRemotePath(remotePath); err != nil {
		return err
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var remoteCmd string
	if host.IsWindowsOS(osKind) {
		remoteCmd = `rmdir /S /Q "` + host.WindowsRemotePath(remotePath) + `"`
	} else {
		remoteCmd = "rm -rf -- " + remotePath
	}
	cmd := exec.CommandContext(cleanupCtx, "ssh", hostName, remoteCmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("cleanup remote %s: %s", hostName, message)
	}
	return nil
}

// workerCacheRoot is the worker-side bundle-cache root the sweep addresses on
// remote machines, mirroring the reconstruction path's ~/.vci/state layout.
const workerCacheRoot = "~/.vci/state/bundle-cache"

// remoteReapTimeout bounds each remote cache sweep so an unresponsive worker
// cannot stall maintenance.
const remoteReapTimeout = 30 * time.Second

// ReapRemoteBundleCaches sweeps the worker-side bundle cache of every
// coordinator-configured remote machine for each project attached to it,
// adding the reported removal counts to the sweep report. A machine is remote
// when it declares a host; hostless machines hold no worker cache and are
// skipped. Remote workers are best-effort: an unreachable or failing host
// contributes nothing and never fails the sweep, so maintenance completes and
// only successful removals are reported.
func ReapRemoteBundleCaches(report *Report, cfg config.Config, now time.Time) {
	client := host.Client{Runner: process.Native{}}
	for machineName, machine := range cfg.Machines {
		if machine.Host == "" {
			continue
		}
		policy := config.EffectiveBundleCache(machine.BundleCache)
		for projectName, project := range cfg.Projects {
			if !attachedTo(project, machineName) {
				continue
			}
			reapCtx, cancel := context.WithTimeout(context.Background(), remoteReapTimeout)
			result, err := client.ReapBundleCache(reapCtx, machine.Host, workerCacheRoot, projectName, policy, now)
			cancel()
			if err != nil {
				continue
			}
			report.BundleCacheStaleRemoved += result.Stale
			report.BundleCacheEvictedRemoved += result.Evicted
		}
	}
}

// attachedTo reports whether the project declares the machine in its
// configured machine set.
func attachedTo(project config.Project, machine string) bool {
	for _, name := range project.Machines {
		if name == machine {
			return true
		}
	}
	return false
}
