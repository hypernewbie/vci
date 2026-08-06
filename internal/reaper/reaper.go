package reaper

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/lease"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/sourcecache"
	"github.com/hypernewbie/vci/internal/store"
)

type Report struct {
	Removed                   int   `json:"removed"`
	MarkedLost                int   `json:"marked_lost"`
	TransferRemoved           int   `json:"transfer_removed"`
	SourceCacheRemoved        int   `json:"source_cache_removed"`
	SourceCacheScratchRemoved int   `json:"source_cache_scratch_removed"`
	SourceCacheBytes          int64 `json:"source_cache_bytes"`
	SourceCacheLimitBytes     int64 `json:"source_cache_limit_bytes"`
	SourceCacheRejected       int   `json:"source_cache_rejected"`
}

const (
	renewalGrace     = 10 * time.Minute
	transferStaleAge = 30 * time.Minute
	// DefaultSourceCacheBytes is the documented default quota used
	// when no coordinator-owned retention setting supplies one.
	DefaultSourceCacheBytes = 500 * 1024 * 1024
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
		} else if os.IsNotExist(leaseErr) {
			if _, transitionErr := runStore.Transition(id, model.RunLost, now); transitionErr == nil {
				report.MarkedLost++
			}
		}
		// Corrupt metadata is left for a later pass; it is not proof of stale
		// ownership and must not trigger cleanup or a process signal.
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

	if removed, scratch, bytes, total, rejected, err := ReapSourceCache(l.SourceCacheDir(), sourceCacheQuota(l)); err != nil {
		return report, err
	} else {
		report.SourceCacheRemoved = removed
		report.SourceCacheScratchRemoved = scratch
		report.SourceCacheBytes = bytes
		report.SourceCacheLimitBytes = total
		report.SourceCacheRejected = rejected
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

// reapTransferDirs removes stale direct-SSH transfer staging directories
// and client materialization snapshots under the Vci-owned TempDir. It
// only matches the explicit `vci-source-`, legacy `vci-source.`, or
// `vci-snapshot-` prefixes and never traverses arbitrary TMPDIR content. Stale means the
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
		if !strings.HasPrefix(entry.Name(), "vci-source-") && !strings.HasPrefix(entry.Name(), "vci-source.") && !strings.HasPrefix(entry.Name(), "vci-snapshot-") {
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

func leaseExpired(l layout.Layout, id model.RunID, now time.Time) (bool, error) {
	current, err := lease.Read(l, id)
	if err != nil {
		return false, err
	}
	return !current.ExpiresAt.After(now), nil
}
func removeTree(path string) error {
	return os.RemoveAll(path)
}
