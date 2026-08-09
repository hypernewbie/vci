// Package bundlecache implements a bounded on-disk LRU cache of Git bundles
// on a build worker, keyed by (project, base commit). A worker that has
// admitted a bundle for a base can answer a coordinator probe with "I already
// have base" (the complete marker is a hit) and hold an active claim so the
// coordinator sends only the delta and eviction skips the entry.
//
// Layout under a cache root:
//
//	<root>/v1/<project>/<base>/bundle           the bundle bytes
//	<root>/v1/<project>/<base>/meta.json        {"bytes":N,"last_used":"<RFC3339>"}
//	<root>/v1/<project>/<base>/complete         empty file; presence == a hit
//	<root>/v1/<project>/<base>/claims/<claimID> active-claim markers
package bundlecache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Version is the on-disk layout version segment beneath the cache root.
const Version = "v1"

// Policy bounds bundle admission and retention for one project.
type Policy struct {
	MaxEntries     int   // per project; <=0 means unlimited
	MaxBytes       int64 // total bundle bytes per project; <=0 means unlimited
	AdmissionBytes int64 // bundles larger than this are not admitted; <=0 admits all
}

// Entry describes one complete cached bundle.
type Entry struct {
	Base     string
	Bytes    int64
	LastUsed time.Time
}

// meta is the persisted shape of meta.json.
type meta struct {
	Bytes    int64     `json:"bytes"`
	LastUsed time.Time `json:"last_used"`
}

// validSegment reports whether s is a single path segment of letters, digits,
// dot, dash, and underscore: non-empty, no slashes, and not "." or "..".
func validSegment(s string) bool {
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, `/\`) {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// checkSegment rejects a project, base, or claimID that is not a valid single
// path segment. A base is a Git object id; a project is a configured name.
func checkSegment(what, s string) error {
	if !validSegment(s) {
		return fmt.Errorf("bundlecache: invalid %s %q: must be a single path segment of letters, digits, '.', '-', '_'", what, s)
	}
	return nil
}

// entryDir returns the on-disk entry directory for project/base, validating
// both names first.
func entryDir(cacheRoot, project, base string) (string, error) {
	if err := checkSegment("project", project); err != nil {
		return "", err
	}
	if err := checkSegment("base", base); err != nil {
		return "", err
	}
	return filepath.Join(cacheRoot, Version, project, base), nil
}

// IsHit reports whether a complete bundle entry exists for project/base.
func IsHit(cacheRoot, project, base string) (bool, error) {
	dir, err := entryDir(cacheRoot, project, base)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(filepath.Join(dir, "complete"))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// Admit stores bundle under project/base and marks the entry complete. A bundle
// larger than policy.AdmissionBytes (when positive) is rejected without error.
// The complete marker is written last so an interrupted admission is never a hit.
func Admit(cacheRoot, project, base string, bundle []byte, policy Policy, now time.Time) (admitted bool, err error) {
	dir, err := entryDir(cacheRoot, project, base)
	if err != nil {
		return false, err
	}
	if policy.AdmissionBytes > 0 && int64(len(bundle)) > policy.AdmissionBytes {
		return false, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	if err := os.WriteFile(filepath.Join(dir, "bundle"), bundle, 0o644); err != nil {
		return false, err
	}
	raw, err := json.Marshal(meta{Bytes: int64(len(bundle)), LastUsed: now})
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), raw, 0o644); err != nil {
		return false, err
	}
	if err := os.WriteFile(filepath.Join(dir, "complete"), nil, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// AcquireActiveClaim writes an active-claim marker for entry project/base. The
// entry dir must already exist; the claims dir is created on demand.
func AcquireActiveClaim(cacheRoot, project, base, claimID string) error {
	dir, err := entryDir(cacheRoot, project, base)
	if err != nil {
		return err
	}
	if err := checkSegment("claimID", claimID); err != nil {
		return err
	}
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("bundlecache: no entry for project %q base %q", project, base)
		}
		return err
	}
	claimsDir := filepath.Join(dir, "claims")
	if err := os.MkdirAll(claimsDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(claimsDir, claimID), nil, 0o644)
}

// ReleaseActiveClaim removes a claim marker; a missing marker is not an error.
func ReleaseActiveClaim(cacheRoot, project, base, claimID string) error {
	dir, err := entryDir(cacheRoot, project, base)
	if err != nil {
		return err
	}
	if err := checkSegment("claimID", claimID); err != nil {
		return err
	}
	err = os.Remove(filepath.Join(dir, "claims", claimID))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// UpdateLastUse rewrites meta.json with last_used=now. A missing entry is an error.
func UpdateLastUse(cacheRoot, project, base string, now time.Time) error {
	dir, err := entryDir(cacheRoot, project, base)
	if err != nil {
		return err
	}
	metaPath := filepath.Join(dir, "meta.json")
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return err
	}
	var m meta
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	m.LastUsed = now
	raw, err = json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(metaPath, raw, 0o644)
}

// List returns the complete entries under v1/<project>, sorted by Base.
// Incomplete or unreadable entries are skipped.
func List(cacheRoot, project string) ([]Entry, error) {
	if err := checkSegment("project", project); err != nil {
		return nil, err
	}
	projDir := filepath.Join(cacheRoot, Version, project)
	dirs, err := os.ReadDir(projDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Entry
	for _, d := range dirs {
		if !d.IsDir() || !validSegment(d.Name()) {
			continue
		}
		dir := filepath.Join(projDir, d.Name())
		if _, err := os.Stat(filepath.Join(dir, "complete")); err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
		if err != nil {
			continue
		}
		var m meta
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		out = append(out, Entry{Base: d.Name(), Bytes: m.Bytes, LastUsed: m.LastUsed})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Base < out[j].Base })
	return out, nil
}

// EvictLRU removes the least-recently-used claim-free entries of a project,
// oldest first, re-checking the limits after each removal, until the project is
// within both policy limits or no claim-free entries remain. It returns the
// number of entries removed.
func EvictLRU(cacheRoot, project string, policy Policy) (int, error) {
	entries, err := List(cacheRoot, project)
	if err != nil {
		return 0, err
	}
	if withinLimits(entries, policy) {
		return 0, nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].LastUsed.Before(entries[j].LastUsed) })
	removed := 0
	for i := 0; i < len(entries) && !withinLimits(entries, policy); {
		e := entries[i]
		claims, err := activeClaims(cacheRoot, project, e.Base)
		if err != nil {
			return removed, err
		}
		if claims > 0 {
			i++
			continue
		}
		dir, err := entryDir(cacheRoot, project, e.Base)
		if err != nil {
			return removed, err
		}
		if err := os.RemoveAll(dir); err != nil {
			return removed, err
		}
		removed++
		entries = append(entries[:i], entries[i+1:]...)
	}
	return removed, nil
}

// withinLimits reports whether entries satisfy both positive policy limits.
func withinLimits(entries []Entry, p Policy) bool {
	if p.MaxEntries > 0 && len(entries) > p.MaxEntries {
		return false
	}
	if p.MaxBytes > 0 {
		var total int64
		for _, e := range entries {
			total += e.Bytes
		}
		if total > p.MaxBytes {
			return false
		}
	}
	return true
}

// activeClaims counts the claim markers under entry project/base's claims dir.
func activeClaims(cacheRoot, project, base string) (int, error) {
	dir, err := entryDir(cacheRoot, project, base)
	if err != nil {
		return 0, err
	}
	items, err := os.ReadDir(filepath.Join(dir, "claims"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	for _, it := range items {
		if !it.IsDir() && validSegment(it.Name()) {
			n++
		}
	}
	return n, nil
}

// ReapStale walks every project and base, removing incomplete entries whose
// content is older than partialAge and claim markers older than claimTTL. It
// returns the total number of entries and claims removed.
func ReapStale(cacheRoot string, now time.Time, partialAge, claimTTL time.Duration) (int, error) {
	root := filepath.Join(cacheRoot, Version)
	projects, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	removed := 0
	for _, p := range projects {
		if !p.IsDir() || !validSegment(p.Name()) {
			continue
		}
		projDir := filepath.Join(root, p.Name())
		bases, err := os.ReadDir(projDir)
		if err != nil {
			return removed, err
		}
		for _, b := range bases {
			if !b.IsDir() || !validSegment(b.Name()) {
				continue
			}
			entryPath := filepath.Join(projDir, b.Name())
			if _, err := os.Stat(filepath.Join(entryPath, "complete")); os.IsNotExist(err) {
				age, err := entryAge(entryPath, now)
				if err != nil {
					return removed, err
				}
				if age > partialAge {
					if err := os.RemoveAll(entryPath); err != nil {
						return removed, err
					}
					removed++
					continue
				}
			}
			claimsDir := filepath.Join(entryPath, "claims")
			claims, err := os.ReadDir(claimsDir)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return removed, err
			}
			for _, c := range claims {
				if c.IsDir() {
					continue
				}
				info, err := c.Info()
				if err != nil {
					continue
				}
				if now.Sub(info.ModTime()) > claimTTL {
					if err := os.Remove(filepath.Join(claimsDir, c.Name())); err != nil {
						return removed, err
					}
					removed++
				}
			}
		}
	}
	return removed, nil
}

// entryAge returns how long ago the entry's content was last written, using
// meta.json's mtime when present (it is written alongside the bundle at
// admission) and the entry dir's mtime otherwise.
func entryAge(entryPath string, now time.Time) (time.Duration, error) {
	info, err := os.Stat(filepath.Join(entryPath, "meta.json"))
	if err == nil {
		return now.Sub(info.ModTime()), nil
	}
	if !os.IsNotExist(err) {
		return 0, err
	}
	info, err = os.Stat(entryPath)
	if err != nil {
		return 0, err
	}
	return now.Sub(info.ModTime()), nil
}
