// Package sourcecache owns the local coordinator source cache.
//
// It is filesystem-only, rooted under Vci state, with one fixed layout:
//
//	v1/<digest>/<project>/meta.json
//	v1/<digest>/<project>/complete
//	v1/<digest>/<project>/active/<claimID>/
//	v1/<digest>/<project>/<project>/
//	v1/locks/<digest>-<project>
//	v1/partial/<random>/
//
// Each entry is identified by (format_version, digest, project).

package sourcecache

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/hypernewbie/vci/internal/model"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FormatVersion identifies the active cache layout. Bumping it invalidates
// existing entries; old files remain until the reaper removes them.
const FormatVersion = "v1"

// publicationLockAge is the stale-claim threshold for lock recovery.
// A valid publisher should hold a lock only for one copy operation.
const publicationLockAge = 5 * time.Minute

// EntryMeta stores metadata for one cache entry.
// LastUse is updated on every hit so eviction uses it instead of tree mtimes.
type EntryMeta struct {
	FormatVersion string `json:"format_version"`
	Digest        string `json:"digest"`
	Project       string `json:"project"`
	LastUse       string `json:"last_use"`
}

// ErrAdmissionRejected signals admission quota failure; treat as cache miss.
var ErrAdmissionRejected = errors.New("sourcecache: cache admission rejected")

// ValidDigest enforces the strict sha256-<64 hex> cache key format.
// Shared by cache callers via exported usage.
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

// IsHit validates inputs and checks for a completed entry with matching metadata
// and a present tree for the exact (digest, project) pair.
func IsHit(root, digest, project string) (bool, *EntryMeta, error) {
	if !ValidDigest(digest) {
		return false, nil, fmt.Errorf("sourcecache: invalid digest %q", digest)
	}
	if !model.ValidName(project) {
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

var lockGuard sync.Mutex // serialize publication lock acquire/release within process

// PublicationLock acquires a per-entry publish lock and returns its unlock func.
// Caller must defer the function. Stale locks older than maxAge are reclaimed.
// A per-process guard keeps handoff safe across same-process goroutines.
func PublicationLock(root, digest, project string, now time.Time, maxAge time.Duration) (func(), error) {
	if !ValidDigest(digest) {
		return nil, fmt.Errorf("sourcecache: invalid digest %q", digest)
	}
	if !model.ValidName(project) {
		return nil, fmt.Errorf("sourcecache: invalid project name %q", project)
	}
	lockGuard.Lock()
	defer lockGuard.Unlock()
	lockPath := LockPath(root, digest, project)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, err
	}
	// Write a random owner token so unlock only deletes the lock it owns.
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, fmt.Errorf("sourcecache: lock token: %w", err)
	}
	token := hex.EncodeToString(raw[:])
	// Create if absent; if present and stale, remove and retry.
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
	if _, err := f.WriteString(token); err != nil {
		_ = f.Close()
		_ = os.Remove(lockPath)
		return nil, fmt.Errorf("sourcecache: write lock token: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(lockPath)
		return nil, fmt.Errorf("sourcecache: close lock: %w", err)
	}
	return func() {
		// Remove the lock only if our token is still present.
		// Guarded by lockGuard to avoid same-process reclaim races.
		lockGuard.Lock()
		defer lockGuard.Unlock()
		data, err := os.ReadFile(lockPath)
		if err == nil && string(data) == token {
			_ = os.Remove(lockPath)
		}
	}, nil
}

// AcquireActiveClaim creates (or reuses) the active-claim directory for a publish attempt.
func AcquireActiveClaim(root, digest, project, claimID string) error {
	if !ValidDigest(digest) || !model.ValidName(claimID) || !model.ValidName(project) {
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

// ReleaseActiveClaim removes one active claim after validating identifiers.
func ReleaseActiveClaim(root, digest, project, claimID string) {
	if !ValidDigest(digest) || !model.ValidName(claimID) || !model.ValidName(project) {
		return
	}
	_ = os.RemoveAll(ActiveClaimPath(root, digest, project, claimID))
}

// ActiveClaimsExist checks whether active claim directories exist for the key.
// Reapers use this to avoid evicting in-flight publish work.
func ActiveClaimsExist(root, digest, project string) (bool, error) {
	if !ValidDigest(digest) {
		return false, fmt.Errorf("sourcecache: invalid digest %q", digest)
	}
	if !model.ValidName(project) {
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

// EntryRootRel returns the per-project entry root relative path.
func EntryRootRel(digest, project string) string {
	return filepath.Join(FormatVersion, digest, project)
}

// EntryTreeRel returns the immutable tree path relative to cache root.
func EntryTreeRel(digest, project string) string {
	return filepath.Join(FormatVersion, digest, project, project)
}

// EntryPath returns the absolute per-project entry root. Digest and project
// should already be validated by the caller.
func EntryPath(root, digest, project string) string {
	return filepath.Join(root, EntryRootRel(digest, project))
}

// EntryTreePath returns the absolute tree directory inside an entry root.
func EntryTreePath(root, digest, project string) string {
	return filepath.Join(root, EntryTreeRel(digest, project))
}

// EntryMetaPath returns the absolute metadata file path for one entry.
func EntryMetaPath(root, digest, project string) string {
	return filepath.Join(EntryPath(root, digest, project), "meta.json")
}

// EntryCompletePath returns the completion marker path for one entry.
func EntryCompletePath(root, digest, project string) string {
	return filepath.Join(EntryPath(root, digest, project), "complete")
}

// ActiveClaimPath returns the per-attempt active-claim path.
func ActiveClaimPath(root, digest, project, claimID string) string {
	return filepath.Join(EntryPath(root, digest, project), "active", claimID)
}

// LockPath returns the per-key publication lock path.
func LockPath(root, digest, project string) string {
	return filepath.Join(root, FormatVersion, "locks", digest+"-"+project)
}

// PartialPath creates a random partial transfer scratch directory path.
func PartialPath(root string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return filepath.Join(root, FormatVersion, "partial", hex.EncodeToString(raw[:])), nil
}

// EntryListItem captures data the reaper uses to evict LRU entries.
type EntryListItem struct {
	Digest  string
	Project string
	Path    string
	Size    int64
	LastUse time.Time
	Active  bool
}

// List enumerates verified (digest, project) entries with size, last-use, and active
// status so the reaper can evict only non-active cache entries.
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
		// Each digest may map to multiple projects; list each project entry.
		projectDirs, rErr := os.ReadDir(filepath.Join(versionDir, digest))
		if rErr != nil {
			continue
		}
		for _, pd := range projectDirs {
			if !pd.IsDir() {
				continue
			}
			project := pd.Name()
			if !model.ValidName(project) {
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
