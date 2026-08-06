// Package sourcecache implements the coordinator-owned, bounded,
// directly-published source cache. The cache is local: it lives under
// the Vci root and is queried/published through ordinary filesystem
// operations only. There is no Vci network service, listener, or
// per-remote protocol in this package.
//
// The cache layout under state/source-cache/ is:
//
//	v1/<digest>/<project>/meta.json     # {format_version, digest, project, last_use}
//	v1/<digest>/<project>/complete      # written last; cache hit requires this marker
//	v1/<digest>/<project>/active/<id>/  # short-lived claim while a public vci build captures the entry
//	v1/<digest>/<project>/<project>/    # immutable source tree; final basename is the project name
//	v1/locks/<digest>-<project>          # per-project publication lock
//	v1/partial/<random>/                # transfer scratch; never a cache hit
//
// Every cache mechanism — metadata, completion marker, active claims,
// lock, source tree, lookup, publication, LRU/reaping, and the SSH
// probe — keys on exactly one entry identity: (format_version, digest,
// project). Two projects with identical selected content therefore own
// independent entry roots under the shared digest; neither can
// overwrite the other's metadata or marker. Cache metadata lives
// outside the source tree (the tree is the nested
// <project>/<project>/ directory), and the tree's final basename is
// the configured project name so public `vci build .` discovers it.
//
// All paths in this package are Vci-owned (under layout.SourceCacheDir()).
// The versioned layout helpers (EntryRootRel, EntryTreeRel) are the one
// typed implementation shared by the Go path helpers and by the app's
// remote probe/staging fragments; no separately formatted shell string
// defines the layout.
package sourcecache

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/source"
)

// FormatVersion is the current cache layout version. Bumping this
// invalidates existing entries: they remain on disk until the reaper
// removes them, but they are never treated as a cache hit.
const FormatVersion = "v1"

// publicationLockAge bounds how old a lock must be before it is
// reclaimed. A live publication holds the lock only for the duration
// of one verified copy, so this age cannot be reached by a healthy
// publisher.
const publicationLockAge = 5 * time.Minute

// EntryMeta is the Vci-owned metadata for one cache entry. last_use
// is updated atomically on every cache hit so eviction follows the
// recorded use, not the file mtimes inside the project tree.
type EntryMeta struct {
	FormatVersion string `json:"format_version"`
	Digest        string `json:"digest"`
	Project       string `json:"project"`
	LastUse       string `json:"last_use"`
}

// ErrAdmissionRejected reports that an incoming entry cannot be
// admitted under the configured quota: it is oversize or no inactive
// capacity can be freed. The caller treats this as "no cache entry",
// not as a build failure.
var ErrAdmissionRejected = errors.New("sourcecache: cache admission rejected")

// ValidDigest is the strict sha256-<64-hex> shape the cache key must
// satisfy. Anything else is rejected. Exported so the app-side
// validateCacheKey and any other consumer can share a single rule.
// Every call site in this package routes through this single rule.
func ValidDigest(digest string) bool {
	if !strings.HasPrefix(digest, "sha256-") {
		return false
	}
	if len(digest) != len("sha256-")+64 {
		return false
	}
	for _, r := range digest[len("sha256-"):] {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

// EntryRootRel returns the versioned per-project entry root for one
// key relative to the cache root, e.g. "v1/<digest>/<project>". All
// entry state (meta, complete, active, tree) lives under this root.
func EntryRootRel(digest, project string) string {
	return filepath.Join(FormatVersion, digest, project)
}

// EntryTreeRel returns the versioned immutable source-tree path for
// one key relative to the cache root, e.g.
// "v1/<digest>/<project>/<project>". The tree's final basename is the
// project name so the remote public `vci build .` discovers the
// repository and matches the configured project.
func EntryTreeRel(digest, project string) string {
	return filepath.Join(FormatVersion, digest, project, project)
}

// EntryPath returns the Vci-owned absolute per-project entry root for
// one cache key. The caller must have already validated digest and
// project.
func EntryPath(root, digest, project string) string {
	return filepath.Join(root, EntryRootRel(digest, project))
}

// EntryTreePath returns the Vci-owned absolute source-tree directory
// inside one entry root.
func EntryTreePath(root, digest, project string) string {
	return filepath.Join(root, EntryTreeRel(digest, project))
}

// EntryMetaPath is the JSON-serialized metadata location for one
// (digest, project) entry.
func EntryMetaPath(root, digest, project string) string {
	return filepath.Join(EntryPath(root, digest, project), "meta.json")
}

// EntryCompletePath is the completion marker for one (digest, project)
// entry. A hit requires this marker.
func EntryCompletePath(root, digest, project string) string {
	return filepath.Join(EntryPath(root, digest, project), "complete")
}

// ActiveClaimPath is the per-run claim file created when an entry is
// handed to a public `vci build .`. The reaper must protect entries
// holding an active claim.
func ActiveClaimPath(root, digest, project, claimID string) string {
	return filepath.Join(EntryPath(root, digest, project), "active", claimID)
}

// LockPath is the publication lock for one (digest, project) tuple.
func LockPath(root, digest, project string) string {
	return filepath.Join(root, FormatVersion, "locks", digest+"-"+project)
}

// PartialPath is the transfer scratch directory for one miss.
func PartialPath(root string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return filepath.Join(root, FormatVersion, "partial", hex.EncodeToString(raw[:])), nil
}

// IsHit reports whether a completed cache entry exists for the exact
// (digest, project) key. Cache hit requires the completion marker,
// matching metadata, and a present source tree, all under the
// per-project entry root. A bare digest-level directory or another
// project's entry is never a hit.
func IsHit(root, digest, project string) (bool, *EntryMeta, error) {
	if !ValidDigest(digest) {
		return false, nil, fmt.Errorf("sourcecache: invalid digest %q", digest)
	}
	if !layout.ValidName(project) {
		return false, nil, fmt.Errorf("sourcecache: invalid project name %q", project)
	}
	completeInfo, err := os.Stat(EntryCompletePath(root, digest, project))
	if err != nil || !completeInfo.Mode().IsRegular() {
		return false, nil, nil
	}
	data, err := os.ReadFile(EntryMetaPath(root, digest, project))
	if err != nil {
		return false, nil, fmt.Errorf("sourcecache: read meta: %w", err)
	}
	var meta EntryMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return false, nil, fmt.Errorf("sourcecache: decode meta: %w", err)
	}
	if meta.FormatVersion != FormatVersion || meta.Digest != digest || meta.Project != project {
		return false, nil, nil
	}
	if info, err := os.Stat(EntryTreePath(root, digest, project)); err != nil || !info.IsDir() {
		return false, nil, nil
	}
	return true, &meta, nil
}

// ReadSnapshotFiles is removed: the package tests enumerate entry
// files directly and production never needs the list (verification
// walks the tree). Keeping a disconnected list helper would violate
// the one-caller contract.

// Publication primitives: locks, partial write, atomically-publish
// a verified entry, and update last-use metadata.

var lockGuard sync.Mutex // serialize publication lock acquisition in-process

// PublicationLock acquires the publication lock for (digest, project).
// The returned unlock function must be deferred. A second publisher
// observes the existing lock and backs off; stale locks (older than
// maxAge) can be reclaimed. The in-process guard serializes callers in
// this process; the lock file serializes cross-process publishers whose
// acquisition does not overlap a stale-reap race.
func PublicationLock(root, digest, project string, now time.Time, maxAge time.Duration) (func(), error) {
	if !ValidDigest(digest) {
		return nil, fmt.Errorf("sourcecache: invalid digest %q", digest)
	}
	if !layout.ValidName(project) {
		return nil, fmt.Errorf("sourcecache: invalid project name %q", project)
	}
	lockGuard.Lock()
	defer lockGuard.Unlock()
	lockPath := LockPath(root, digest, project)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, err
	}
	// Try to create the lock; if it exists, check staleness.
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			info, statErr := os.Stat(lockPath)
			if statErr == nil && now.Sub(info.ModTime()) > maxAge {
				_ = os.Remove(lockPath)
				f, err = os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("sourcecache: publication busy: %w", err)
		}
	}
	_, _ = f.WriteString(now.UTC().Format(time.RFC3339Nano))
	_ = f.Close()
	return func() { _ = os.Remove(lockPath) }, nil
}

// PublishTree admits and publishes one verified tree under the cache.
// It serializes same-key publishers with a bounded deadline, lets a
// pre-existing completed identical entry win, enforces the configured
// quota before publication, copies the incoming tree into a unique
// partial on the same filesystem, verifies the copy against the claimed
// digest, and atomically makes the complete entry visible.
//
// A quota rejection returns ErrAdmissionRejected and never creates an
// entry. Any other error leaves no complete entry behind.
func PublishTree(root, digest, project, srcDir string, quota int64) (bool, error) {
	if !ValidDigest(digest) {
		return false, fmt.Errorf("sourcecache: invalid digest %q", digest)
	}
	if !layout.ValidName(project) {
		return false, fmt.Errorf("sourcecache: invalid project name %q", project)
	}
	// Bounded wait for a competing publisher of the same key. The wait
	// has a deadline so a stalled owner cannot hang the build; a stale
	// lock older than publicationLockAge is reclaimed by the lock.
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

	// A completed identical entry wins; the competitor discards its own
	// partial without touching the winner.
	if hit, _, err := IsHit(root, digest, project); err != nil {
		return false, err
	} else if hit {
		return false, nil
	}

	size, err := treeSize(srcDir)
	if err != nil {
		return false, err
	}

	// Admission before publication.
	if quota > 0 {
		if size > quota {
			return false, ErrAdmissionRejected
		}
		if _, total, _, err := EnforceQuota(root, quota-size); err != nil {
			return false, err
		} else if total+size > quota {
			return false, ErrAdmissionRejected
		}
	}

	// Unique partial on the same filesystem.
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
	// Verify the copied partial before it can become visible.
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

// PublishVerifiedEntry atomically publishes the verified project tree
// found at <partialPath>/<project> under the per-project entry root.
// meta.json lands first, the source tree is renamed into place without
// clobbering a completed entry, and the `complete` marker is the final
// atomic step: the entry is visible only after every byte has landed
// and the marker rename succeeds. A stale tree left by a crashed
// publication is replaced only under the caller's held publication
// lock; a completed entry is never removed or overwritten.
func PublishVerifiedEntry(root, digest, project, partialPath string) error {
	if !ValidDigest(digest) {
		return fmt.Errorf("sourcecache: invalid digest %q", digest)
	}
	if !layout.ValidName(project) {
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
		// A completed entry wins: never remove or overwrite it. Without
		// a complete marker the leftover tree is stale crashed scratch
		// and is replaced under the held publication lock.
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

// UpdateLastUse refreshes the recorded last_use on a verified entry.
// The reaper uses this metadata, not file mtimes inside the project
// tree. The update is atomic via a tmp+rename so a partial read sees
// either the old or new value.
func UpdateLastUse(root, digest, project string, now time.Time) error {
	if !ValidDigest(digest) {
		return fmt.Errorf("sourcecache: invalid digest %q", digest)
	}
	if !layout.ValidName(project) {
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

// AcquireActiveClaim creates a short-lived claim directory under the
// per-project entry root (state/source-cache/v1/<digest>/<project>/active/<claim>/).
// The reaper must preserve entries with an active claim. The acquire
// is idempotent: repeated calls for the same claimID return nil.
func AcquireActiveClaim(root, digest, project, claimID string) error {
	if !ValidDigest(digest) || !layout.ValidName(claimID) || !layout.ValidName(project) {
		return fmt.Errorf("sourcecache: invalid claim inputs")
	}
	claimPath := ActiveClaimPath(root, digest, project, claimID)
	if err := os.MkdirAll(filepath.Dir(claimPath), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(claimPath, 0o700); err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	return nil
}

// ReleaseActiveClaim removes the short-lived claim directory.
func ReleaseActiveClaim(root, digest, project, claimID string) {
	_ = os.RemoveAll(ActiveClaimPath(root, digest, project, claimID))
}

// ActiveClaimsExist reports whether any claim ID lives under the
// per-project entry root's active/ subdirectory. The reaper uses this
// to protect entries that an in-flight public `vci build .` is still
// capturing.
func ActiveClaimsExist(root, digest, project string) (bool, error) {
	if !ValidDigest(digest) {
		return false, fmt.Errorf("sourcecache: invalid digest %q", digest)
	}
	if !layout.ValidName(project) {
		return false, fmt.Errorf("sourcecache: invalid project name %q", project)
	}
	activeDir := filepath.Join(EntryPath(root, digest, project), "active")
	entries, err := os.ReadDir(activeDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	for _, e := range entries {
		if e.IsDir() {
			return true, nil
		}
	}
	return false, nil
}

// EntryListItem is the metadata used by the reaper's LRU eviction
// decision. The modTime is sourced from the meta.json LastUse field,
// not from file mtimes inside the project tree. Active entries are
// counted in total capacity but must never be evicted. The Project
// field makes the entry identity exact: (digest, project).
type EntryListItem struct {
	Digest  string
	Project string
	Path    string
	Size    int64
	LastUse time.Time
	Active  bool
}

// List returns every verified per-project entry under the cache root
// with its recorded last-use time, total byte size, and active-claim
// flag. Active entries are included so total capacity always counts
// them; eviction must skip them. Each item is one exact (digest,
// project) entry.
func List(root string) ([]EntryListItem, error) {
	versionDir := filepath.Join(root, FormatVersion)
	entries, err := os.ReadDir(versionDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var items []EntryListItem
	for _, d := range entries {
		if !d.IsDir() {
			continue
		}
		digest := d.Name()
		if digest == "partial" || digest == "locks" {
			continue
		}
		if !ValidDigest(digest) {
			continue
		}
		// One digest may hold several independent per-project entry
		// roots; each is a separate list item.
		projectDirs, rErr := os.ReadDir(filepath.Join(versionDir, digest))
		if rErr != nil {
			continue
		}
		for _, pd := range projectDirs {
			if !pd.IsDir() {
				continue
			}
			project := pd.Name()
			if !layout.ValidName(project) {
				continue
			}
			completeInfo, cErr := os.Stat(EntryCompletePath(root, digest, project))
			if cErr != nil || !completeInfo.Mode().IsRegular() {
				continue
			}
			metaData, mErr := os.ReadFile(EntryMetaPath(root, digest, project))
			if mErr != nil {
				continue
			}
			var meta EntryMeta
			if jErr := json.Unmarshal(metaData, &meta); jErr != nil {
				continue
			}
			if meta.FormatVersion != FormatVersion || meta.Digest != digest || meta.Project != project {
				continue
			}
			active, _ := ActiveClaimsExist(root, digest, project)
			entryPath := EntryTreePath(root, digest, project)
			size, _, _ := dirSizeAndLastUse(entryPath, meta.LastUse)
			lastUse, _ := time.Parse(time.RFC3339Nano, meta.LastUse)
			items = append(items, EntryListItem{Digest: digest, Project: project, Path: EntryPath(root, digest, project), Size: size, LastUse: lastUse, Active: active})
		}
	}
	return items, nil
}

// EnforceQuota evicts least-recently-used inactive entries while total
// bytes exceed maxBytes. Active entries are always counted in total and
// never evicted. Oversize singles that cannot be admitted by eviction
// are counted as rejected and retained (their bytes remain counted).
func EnforceQuota(root string, maxBytes int64) (int, int64, int, error) {
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
		if it.Size > maxBytes {
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

// ReapStaleScratch removes exact Vci-owned stale cache scratch:
// v1/partial/<random>/ directories and v1/locks/<key> files older than
// maxAge. It never touches external symlink targets (removal is flat
// RemoveAll over owned scratch) and never removes a live lock newer
// than maxAge.
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

// dirSizeAndLastUse returns the recursive size and parses the lastUse
// timestamp; errors are swallowed so the reaper can still enumerate
// entries whose internals are partially readable.
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

// PurgeEntry removes one exact (digest, project) entry wholly. Entries
// with active claims are not removed.
func PurgeEntry(root, digest, project string) error {
	if !ValidDigest(digest) {
		return fmt.Errorf("sourcecache: invalid digest %q", digest)
	}
	if !layout.ValidName(project) {
		return fmt.Errorf("sourcecache: invalid project name %q", project)
	}
	active, _ := ActiveClaimsExist(root, digest, project)
	if active {
		return fmt.Errorf("sourcecache: cannot purge %s/%s: active claim present", digest, project)
	}
	return os.RemoveAll(EntryPath(root, digest, project))
}

// copyTree copies a settled tree into dst preserving regular-file mode
// bits and symlinks. Intermediate directories are private (0700).
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

// treeSize returns the byte size of regular files under root. Symlink
// targets and directory entries are metadata, not content bytes.
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
