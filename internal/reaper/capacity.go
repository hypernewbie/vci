package reaper

import (
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
)

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
