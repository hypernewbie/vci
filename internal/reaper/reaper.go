package reaper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/host"
	"github.com/hypernewbie/vci/internal/model"
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
	Removed                   int   `json:"removed"`
	MarkedLost                int   `json:"marked_lost"`
	QueuedAborted             int   `json:"queued_aborted"`
	TransferRemoved           int   `json:"transfer_removed"`
	PerRunTmpRemoved          int   `json:"per_run_tmp_removed"`
	WorkspacesRemoved         int   `json:"workspaces_removed"`
	RemoteCleaned             int   `json:"remote_cleaned"`
	SourceCacheRemoved        int   `json:"source_cache_removed"`
	SourceCacheScratchRemoved int   `json:"source_cache_scratch_removed"`
	SourceCacheBytes          int64 `json:"source_cache_bytes"`
	SourceCacheLimitBytes     int64 `json:"source_cache_limit_bytes"`
	SourceCacheRejected       int   `json:"source_cache_rejected"`
	SchedulerClaimsReleased   int   `json:"scheduler_claims_released"`
	ArtifactsReaped           int   `json:"artifacts_reaped"`
}

const (
	renewalGrace     = 10 * time.Minute
	transferStaleAge = 30 * time.Minute
	// DefaultSourceCacheBytes is the documented default quota used
	// when no coordinator-owned retention setting supplies one.
	DefaultSourceCacheBytes = 500 * 1024 * 1024
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
		// Legacy queued records are not live without a lease; queueing occurs after staging.
		// Mark as aborted (distinct counter) and let scheduler reap orphan claims.
		if record.State == model.RunQueued {
			if store.ReadHasNoLease(l, id) {
				if _, transitionErr := runStore.Transition(id, model.RunAborted, now); transitionErr == nil {
					report.QueuedAborted++
				}
				continue
			}
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

	if removed, scratch, bytes, total, rejected, err := ReapSourceCache(l.SourceCacheDir(), sourceCacheQuota(l)); err != nil {
		return report, err
	} else {
		report.SourceCacheRemoved = removed
		report.SourceCacheScratchRemoved = scratch
		report.SourceCacheBytes = bytes
		report.SourceCacheLimitBytes = total
		report.SourceCacheRejected = rejected
	}

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

	return report, nil
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
		hostName, ok := remoteHostFor(record)
		if !ok {
			continue
		}
		remoteWorkDir, err := host.RemoteWorkDir(id)
		if err != nil {
			continue
		}
		if err := CleanupRemote(context.Background(), hostName, remoteWorkDir); err == nil {
			cleaned++
		}
	}
	return cleaned
}

// remoteHostFor reads the durable config snapshot and returns the reserved machine host.
// Returns ok=false for missing/malformed snapshot, unknown machine, or empty host.
func remoteHostFor(record store.RunRecord) (string, bool) {
	var snap struct {
		Machine  string                    `json:"machine"`
		Machines map[string]config.Machine `json:"machines"`
	}
	if err := json.Unmarshal(record.ConfigSnapshot, &snap); err != nil {
		return "", false
	}
	name := snap.Machine
	if name == "" {
		return "", false
	}
	machine, ok := snap.Machines[name]
	if !ok || machine.Host == "" {
		return "", false
	}
	return machine.Host, true
}

// CleanupRemote validates host and path then runs `ssh <host> rm -rf -- <path>`.
// Inputs are validated and execution is bounded by a 30-second timeout.
func CleanupRemote(ctx context.Context, hostName, remotePath string) error {
	if err := host.ValidateHost(hostName); err != nil {
		return err
	}
	if err := host.ValidateRemotePath(remotePath); err != nil {
		return err
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cleanupCtx, "ssh", hostName, "rm -rf -- "+remotePath)
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
