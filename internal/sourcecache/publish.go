package sourcecache

import (
	"encoding/json"
	"fmt"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/source"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// PublishTree validates input and publishes one cache entry atomically.
func PublishTree(root, digest, project, srcDir string, quota int64) (bool, error) {
	if !ValidDigest(digest) {
		return false, fmt.Errorf("sourcecache: invalid digest %q", digest)
	}
	if !model.ValidName(project) {
		return false, fmt.Errorf("sourcecache: invalid project name %q", project)
	}
	// Wait briefly for competing publishers; stale locks are reclaimed by publicationLockAge.
	deadline := time.Now().Add(2 * time.Second)
	var unlock func()
	var lockErr error
	for {
		unlock, lockErr = PublicationLock(root, digest, project, time.Now(), publicationLockAge)
		if lockErr == nil {
			break
		}
		if time.Now().After(deadline) {
			return false, lockErr
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer unlock()

	// Exit early if an identical complete entry already exists.
	if hit, _, err := IsHit(root, digest, project); err != nil {
		return false, err
	} else if hit {
		return false, nil
	}

	size, err := treeSize(srcDir)
	if err != nil {
		return false, err
	}

	// Enforce quota by evicting inactive LRU entries as needed.
	// Reject immediately if snapshot exceeds quota.
	if quota > 0 {
		if size > quota {
			return false, ErrAdmissionRejected
		}
		if _, total, _, err := EnforceQuota(root, quota-size, quota); err != nil {
			return false, err
		} else if total+size > quota {
			return false, ErrAdmissionRejected
		}
	}

	// Build a same-filesystem partial tree.
	partialRoot, err := PartialPath(root)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(partialRoot, 0o700); err != nil {
		return false, err
	}
	partialProject := filepath.Join(partialRoot, project)
	if err := copyTree(srcDir, partialProject); err != nil {
		_ = os.RemoveAll(partialRoot)
		return false, fmt.Errorf("sourcecache: copy partial: %w", err)
	}
	// Verify the copied snapshot before publishing.
	if err := source.VerifySnapshot(partialProject, digest); err != nil {
		_ = os.RemoveAll(partialRoot)
		return false, fmt.Errorf("sourcecache: partial verification: %w", err)
	}
	if err := PublishVerifiedEntry(root, digest, project, partialRoot); err != nil {
		_ = os.RemoveAll(partialRoot)
		return false, err
	}
	_ = os.RemoveAll(partialRoot)
	return true, nil
}

// PublishVerifiedEntry writes metadata, renames the tree into place, and then
// writes the complete marker.
// A stale incomplete tree may be replaced under the publication lock;
// a completed entry is never removed or overwritten.
func PublishVerifiedEntry(root, digest, project, partialPath string) error {
	if !ValidDigest(digest) {
		return fmt.Errorf("sourcecache: invalid digest %q", digest)
	}
	if !model.ValidName(project) {
		return fmt.Errorf("sourcecache: invalid project name %q", project)
	}
	entryPath := EntryPath(root, digest, project)
	if err := os.MkdirAll(entryPath, 0o700); err != nil {
		return err
	}
	meta := EntryMeta{FormatVersion: FormatVersion, Digest: digest, Project: project, LastUse: time.Now().UTC().Format(time.RFC3339Nano)}
	metaData, err := json.Marshal(&meta)
	if err != nil {
		return err
	}
	metaPath := EntryMetaPath(root, digest, project)
	metaTmp := metaPath + ".tmp"
	if err := os.WriteFile(metaTmp, metaData, 0o600); err != nil {
		return err
	}
	if err := os.Rename(metaTmp, metaPath); err != nil {
		_ = os.Remove(metaTmp)
		return err
	}
	partialProject := filepath.Join(partialPath, project)
	if _, err := os.Stat(partialProject); err != nil {
		return fmt.Errorf("sourcecache: partial project missing: %w", err)
	}
	treePath := EntryTreePath(root, digest, project)
	if _, err := os.Lstat(treePath); err == nil {
		// Replace stale incomplete entries only under publication lock.
		if _, err := os.Stat(EntryCompletePath(root, digest, project)); err == nil {
			return fmt.Errorf("sourcecache: entry %s/%s already complete", digest, project)
		}
		if err := os.RemoveAll(treePath); err != nil {
			return err
		}
	}
	if err := os.Rename(partialProject, treePath); err != nil {
		return err
	}
	completeTmp := EntryCompletePath(root, digest, project) + ".tmp"
	if err := os.WriteFile(completeTmp, []byte("complete\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(completeTmp, EntryCompletePath(root, digest, project)); err != nil {
		_ = os.Remove(completeTmp)
		return err
	}
	return nil
}

// UpdateLastUse refreshes last_use on a verified entry.
// Reapers read this value; tmp+rename provides atomic update semantics.
func UpdateLastUse(root, digest, project string, now time.Time) error {
	if !ValidDigest(digest) {
		return fmt.Errorf("sourcecache: invalid digest %q", digest)
	}
	if !model.ValidName(project) {
		return fmt.Errorf("sourcecache: invalid project name %q", project)
	}
	hit, _, err := IsHit(root, digest, project)
	if err != nil || !hit {
		return err
	}
	metaPath := EntryMetaPath(root, digest, project)
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return err
	}
	var meta EntryMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return err
	}
	meta.LastUse = now.UTC().Format(time.RFC3339Nano)
	out, err := json.Marshal(&meta)
	if err != nil {
		return err
	}
	tmp := metaPath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, metaPath)
}

// copyTree copies a source tree into dst, preserving modes and symlinks (0700 dirs).
func copyTree(src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(src, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, readErr := os.Readlink(path)
			if readErr != nil {
				return readErr
			}
			return os.Symlink(link, target)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("sourcecache: unsupported entry type %s", rel)
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

// EnforceQuota evicts inactive LRU entries while size exceeds maxBytes.
// Active entries are kept; oversized entries are skipped and counted rejected.
func EnforceQuota(root string, maxBytes, oversizeCeiling int64) (int, int64, int, error) {
	items, err := List(root)
	if err != nil {
		return 0, 0, 0, err
	}
	var totalBytes int64
	for _, it := range items {
		totalBytes += it.Size
	}
	if totalBytes <= maxBytes {
		return 0, totalBytes, 0, nil
	}
	sort.Slice(items, func(i, j int) bool { return items[i].LastUse.Before(items[j].LastUse) })
	removed := 0
	rejected := 0
	for _, it := range items {
		if totalBytes <= maxBytes {
			break
		}
		if it.Active {
			continue
		}
		if it.Size > oversizeCeiling {
			rejected++
			continue
		}
		if err := PurgeEntry(root, it.Digest, it.Project); err != nil {
			return removed, totalBytes, rejected, err
		}
		totalBytes -= it.Size
		removed++
	}
	return removed, totalBytes, rejected, nil
}

// ReapStaleScratch deletes stale owned partial dirs and lock files older than maxAge.
func ReapStaleScratch(root string, now time.Time, maxAge time.Duration) (int, error) {
	removed := 0
	partialRoot := filepath.Join(root, FormatVersion, "partial")
	entries, err := os.ReadDir(partialRoot)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			info, infoErr := e.Info()
			if infoErr != nil || now.Sub(info.ModTime()) <= maxAge {
				continue
			}
			if err := os.RemoveAll(filepath.Join(partialRoot, e.Name())); err != nil {
				return removed, err
			}
			removed++
		}
	} else if !os.IsNotExist(err) {
		return removed, err
	}
	locksRoot := filepath.Join(root, FormatVersion, "locks")
	locks, err := os.ReadDir(locksRoot)
	if err == nil {
		for _, e := range locks {
			if e.IsDir() {
				continue
			}
			info, infoErr := e.Info()
			if infoErr != nil || now.Sub(info.ModTime()) <= maxAge {
				continue
			}
			if err := os.Remove(filepath.Join(locksRoot, e.Name())); err != nil {
				return removed, err
			}
			removed++
		}
	} else if !os.IsNotExist(err) {
		return removed, err
	}
	return removed, nil
}

// dirSizeAndLastUse returns recursive regular-file size and parsed lastUse timestamp.
func dirSizeAndLastUse(path, lastUseStr string) (int64, time.Time, error) {
	var total int64
	err := filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	lastUse, _ := time.Parse(time.RFC3339Nano, lastUseStr)
	return total, lastUse, err
}

// PurgeEntry deletes one entry only when no active claims exist.
func PurgeEntry(root, digest, project string) error {
	if !ValidDigest(digest) {
		return fmt.Errorf("sourcecache: invalid digest %q", digest)
	}
	if !model.ValidName(project) {
		return fmt.Errorf("sourcecache: invalid project name %q", project)
	}
	active, _ := ActiveClaimsExist(root, digest, project)
	if active {
		return fmt.Errorf("sourcecache: cannot purge %s/%s: active claim present", digest, project)
	}
	return os.RemoveAll(EntryPath(root, digest, project))
}

// treeSize returns total bytes for regular files under root; symlinks and dirs are skipped.
func treeSize(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || info.IsDir() {
			return nil
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}
