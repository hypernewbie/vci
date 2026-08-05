package reaper

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/lease"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/store"
)

type Report struct {
	Removed         int `json:"removed"`
	MarkedLost      int `json:"marked_lost"`
	TransferRemoved int `json:"transfer_removed"`
}

const (
	renewalGrace     = 10 * time.Minute
	transferStaleAge = 30 * time.Minute
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

	return report, nil
}

// reapTransferDirs removes stale direct-SSH transfer staging directories
// under the Vci-owned TempDir. It only matches the explicit `vci-source-`
// or legacy `vci-source.` prefix and never traverses arbitrary TMPDIR content. Stale means the
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
		if !strings.HasPrefix(entry.Name(), "vci-source-") && !strings.HasPrefix(entry.Name(), "vci-source.") {
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
