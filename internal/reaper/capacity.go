package reaper

import (
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/sourcecache"
)

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
func sourceCacheQuota(l model.Layout) int64 {
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
	// EnforceQuota gets the same bound for the retained-byte target and
	// the oversize ceiling, preserving reaper semantics: an entry larger
	// than the quota is retained and reported as rejected, never silently
	// removed.
	removed, totalBytes, rejected, err := sourcecache.EnforceQuota(cacheDir, maxBytes, maxBytes)
	if err != nil {
		return 0, 0, 0, maxBytes, 0, err
	}
	scratchRemoved, err := sourcecache.ReapStaleScratch(cacheDir, time.Now().UTC(), cacheScratchAge)
	if err != nil {
		return removed, 0, totalBytes, maxBytes, rejected, err
	}
	return removed, scratchRemoved, totalBytes, maxBytes, rejected, nil
}

type Entry struct {
	Path    string
	Size    int64
	ModTime time.Time
}
type RetentionReport struct {
	RemovedBytes   int64 `json:"removed_bytes"`
	RemovedEntries int   `json:"removed_entries"`
}

func Enforce(l model.Layout, policy config.Retention) (RetentionReport, error) {
	entries, err := files(l.BlobsDir())
	if err != nil && !os.IsNotExist(err) {
		return RetentionReport{}, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ModTime.Before(entries[j].ModTime) })
	var total int64
	for _, entry := range entries {
		total += entry.Size
	}
	var report RetentionReport
	for _, entry := range entries {
		if total <= policy.MaxBytes {
			break
		}
		if err := remove(entry.Path); err != nil {
			return report, err
		}
		total -= entry.Size
		report.RemovedBytes += entry.Size
		report.RemovedEntries++
	}
	return report, nil
}

func files(root string) ([]Entry, error) {
	var out []Entry
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			out = append(out, Entry{Path: path, Size: info.Size(), ModTime: info.ModTime()})
		}
		return nil
	})
	return out, err
}

func remove(path string) error {
	_ = filepath.Walk(path, func(current string, info os.FileInfo, err error) error {
		if err == nil {
			if info.IsDir() {
				_ = os.Chmod(current, 0o700)
			} else {
				_ = os.Chmod(current, 0o600)
			}
		}
		return nil
	})
	return os.RemoveAll(path)
}
