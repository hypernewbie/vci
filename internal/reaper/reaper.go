package reaper

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/host"
	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/lease"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/scheduler"
	"github.com/hypernewbie/vci/internal/source"
	"github.com/hypernewbie/vci/internal/sourcecache"
	"github.com/hypernewbie/vci/internal/store"
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
}

const (
	renewalGrace     = 10 * time.Minute
	transferStaleAge = 30 * time.Minute
	// DefaultSourceCacheBytes is the documented default quota used
	// when no coordinator-owned retention setting supplies one.
	DefaultSourceCacheBytes = 500 * 1024 * 1024
	// preStartGrace is the bounded window a freshly-published staging
	// record may wait for its detached worker to claim a normal
	// worker lease. During the grace a valid reservation protects the
	// record from `setup reap`; after the grace expires with no lease,
	// the reaper marks the run lost and the scheduler reaps the
	// claim. An active lease overrides claim age entirely: workers
	// that have already claimed their lease are governed by the
	// existing lease/renewal rules, never by this grace.
	preStartGrace = 60 * time.Second
)

func Run(l layout.Layout, now time.Time) (Report, error) {
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
		// Legacy `queued` records: the public build path only spawns
		// after staging, so a queued record with no lease cannot be a
		// live submission. Terminalize it as aborted (a distinct
		// counter from worker losses) and let scheduler reaping free
		// any orphan claim below.
		if record.State == model.RunQueued {
			if lease.ReadHasNoLease(l, id) {
				if _, transitionErr := runStore.Transition(id, model.RunAborted, now); transitionErr == nil {
					report.QueuedAborted++
				}
				continue
			}
		}
		if record.State != model.RunStaging && record.State != model.RunRunning {
			continue
		}
		leaseRecord, leaseErr := lease.Read(l, id)
		if leaseErr == nil {
			if leaseRecord.ExpiresAt.After(now.Add(-renewalGrace)) {
				active[string(id)] = true
				continue
			}
			// A valid but stale lease is abandoned. The worker, if still alive,
			// must self-terminate when its renewal fails; reaper never signals it.
			if _, transitionErr := runStore.Transition(id, model.RunLost, now); transitionErr == nil {
				report.MarkedLost++
			}
			continue
		}
		if !os.IsNotExist(leaseErr) {
			// Corrupt lease metadata: leave the run untouched. A later
			// pass with a healthy lease file is responsible for the
			// terminal decision.
			continue
		}
		// No lease. For a staging record, the only thing that can
		// justify retaining it is a fresh, valid scheduler
		// reservation younger than the pre-start grace. Claim age is
		// never used to kill a run that has already claimed its
		// worker lease; an active lease path is handled above.
		if record.State == model.RunStaging {
			res, resErr := scheduler.ReservationFor(l, record.Machine, id)
			if resErr != nil {
				// Missing or corrupt reservation: the detached worker
				// never had a slot to claim. Terminalize as lost so
				// the slot is freed for the next submission.
				if _, transitionErr := runStore.Transition(id, model.RunLost, now); transitionErr == nil {
					report.MarkedLost++
				}
				continue
			}
			if now.Sub(res.CreatedAt) < preStartGrace {
				// Within the grace; the worker may still be racing to
				// claim its lease.
				continue
			}
			if _, transitionErr := runStore.Transition(id, model.RunLost, now); transitionErr == nil {
				report.MarkedLost++
			}
			continue
		}
		// Running with no lease is a stale orphan: mark lost.
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
			if _, err := runStore.Load(model.RunID(id)); err == nil {
				if record, _ := runStore.Load(model.RunID(id)); record.State == model.RunStaging || record.State == model.RunRunning {
					if _, leaseErr := lease.Read(l, model.RunID(id)); leaseErr == nil {
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

	// Per-run workspace and remote turd ownership. The reaper owns
	// state/work/<run>/.tmp, .home, and the workspace itself after a
	// terminal run's lease is gone, plus the mirrored remote tree for
	// host machines. See reapWorkDirs and reapRemoteTurds.
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

	return report, nil
}

// SourceCacheQuota returns the effective source-cache quota for a
// coordinator config: the configured retention.source_cache_bytes when
// positive, otherwise the documented default. Admission before
// publication and the reaper share this rule, so a config that omits
// the setting cannot publish without a bound.
func SourceCacheQuota(cfg config.Config) int64 {
	if cfg.Retention.SourceCacheBytes <= 0 {
		return DefaultSourceCacheBytes
	}
	return cfg.Retention.SourceCacheBytes
}

// sourceCacheQuota returns the configured source-cache quota in
// bytes, falling back to DefaultSourceCacheBytes when the
// coordinator-owned setting is zero. Loading the config is best-
// effort: a malformed config simply gets the documented default.
func sourceCacheQuota(l layout.Layout) int64 {
	if l.Root == "" {
		return DefaultSourceCacheBytes
	}
	cfg, err := config.Load(l.ConfigPath())
	if err != nil {
		return DefaultSourceCacheBytes
	}
	return SourceCacheQuota(cfg)
}

// cacheScratchAge is how old a cache partial or lock must be before
// the reaper removes it. Publication holds locks only for the duration
// of one verified copy, and partials are either published or discarded
// synchronously, so anything older is abandoned scratch.
const cacheScratchAge = time.Hour

// ReapSourceCache enforces the configured quota and removes stale
// Vci-owned cache scratch (partials and locks). The return values give
// the machine-readable maintenance report: removed entries, removed
// scratch items, retained bytes (active entries always counted), the
// configured quota, and how many oversize candidates could not be
// admitted by eviction.
func ReapSourceCache(cacheDir string, maxBytes int64) (int, int, int64, int64, int, error) {
	removed, totalBytes, rejected, err := sourcecache.EnforceQuota(cacheDir, maxBytes)
	if err != nil {
		return 0, 0, 0, maxBytes, 0, err
	}
	scratchRemoved, err := sourcecache.ReapStaleScratch(cacheDir, time.Now().UTC(), cacheScratchAge)
	if err != nil {
		return removed, 0, totalBytes, maxBytes, rejected, err
	}
	return removed, scratchRemoved, totalBytes, maxBytes, rejected, nil
}

// reapTransferDirs removes stale direct-SSH transfer staging directories,
// client materialization snapshots, and coordinator-owned pinned Git
// checkouts under the Vci-owned TempDir. It only matches the explicit
// `vci-source-`, legacy `vci-source.`, `vci-snapshot-`, or `vci-hosted-`
// prefixes and never traverses arbitrary TMPDIR content. Stale means the
// directory has not been modified within transferStaleAge. The reaper
// never signals any process.
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

// isTerminal reports whether a run state is final and immutable.
func isTerminal(state model.RunState) bool {
	return state == model.RunSucceeded || state == model.RunFailed || state == model.RunLost || state == model.RunAborted
}

// reapWorkDirs owns per-run workspace cleanup under state/work:
//   - `.tmp`/`.home` subdirs inside a workspace that is no longer
//     protected by a live worker lease are removed when older than
//     transferStaleAge (a live run owns its temp roots);
//   - a whole workspace is removed when its run record is terminal
//     and its worker lease is gone — the worker already released the
//     lease only after removeOwned, so a terminal record with no
//     live lease cannot race a live worker.
//
// Returns the number of per-run temp roots removed and the number of
// whole workspaces removed.
func reapWorkDirs(l layout.Layout, now time.Time) (int, int, error) {
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
		leaseRecord, leaseErr := lease.Read(l, id)
		liveLease := leaseErr == nil && leaseRecord.ExpiresAt.After(now.Add(-renewalGrace))
		terminal := loadErr == nil && isTerminal(record.State)
		workDir := filepath.Join(l.WorkDir(), entry.Name())
		// 1. Stale per-run temp roots. Only when no live lease: a
		// live worker owns .tmp/.home and may still be writing them.
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
		// 2. Whole workspace: terminal run, lease gone.
		if terminal && !liveLease {
			if err := removeTree(workDir); err != nil {
				return tmpRemoved, workspacesRemoved, err
			}
			workspacesRemoved++
		}
	}
	return tmpRemoved, workspacesRemoved, nil
}

// reapRemoteTurds sweeps the mirrored remote workspace trees of
// terminal runs whose reserved machine declared a remote host. A
// terminal run whose worker lease is gone cannot have a live remote
// worker, so the remote `~/.vci/state/work/<run>` tree is a turd
// owned by the reaper. Cleanup is best-effort: an unreachable or
// misconfigured host counts as nothing to report, and every ssh call
// is bounded by CleanupRemote's timeout.
func reapRemoteTurds(l layout.Layout, now time.Time) int {
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
		if loadErr != nil || !isTerminal(record.State) {
			continue
		}
		leaseRecord, leaseErr := lease.Read(l, id)
		if leaseErr == nil && leaseRecord.ExpiresAt.After(now.Add(-renewalGrace)) {
			// The worker may still be terminalizing; its lease is
			// released only after removeOwned completes.
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

// remoteHostFor returns the machine host of a run's reserved machine
// by decoding the durable config snapshot. A missing or malformed
// snapshot, an unknown machine, or an empty host yields ok=false: the
// reaper never invokes ssh on guesswork.
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

// CleanupRemote removes a Vci-owned remote tree via the ordinary
// system `ssh` executable: `ssh <host> rm -rf -- <path>`. Both the
// host and the path are strictly validated before any subprocess
// runs, and the validated path is embedded as exactly one shell word
// (a fixed layout plus a validated run id), so a crafted machine host
// or run id can never inject shell text. The call is bounded by a
// 30-second timeout so an unreachable host cannot wedge maintenance.
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
