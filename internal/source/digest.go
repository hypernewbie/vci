package source

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// CanonicalInput is one entry of the digest identity sequence: relative
// path, kind (regular file, symlink, directory), regular-file mode bits,
// symlink target, and file bytes. Directory modes and symlink modes are
// deliberately excluded: a materialized snapshot and its tar-extracted
// coordinator copy must canonicalize identically even though
// intermediate directories are created by tar with default modes and
// symlink modes are not meaningful.
type CanonicalInput struct {
	Path   string
	Kind   string // "file", "symlink", "dir"
	Mode   string // octal mode bits (regular files only)
	Target string // symlink target or empty
	Bytes  []byte // empty for non-regular entries
}

// CanonicalizeSnapshot walks a settled snapshot tree and returns the
// canonical entries for every entry under root, sorted by relative
// path. The set is exactly the materialized selected input: files,
// symlinks, listed directories, and the parent directories that hold
// them. Directory and symlink modes are excluded so the result matches
// a tar-extracted coordinator copy regardless of how tar created
// intermediate directories and of how symlink modes are recorded.
func CanonicalizeSnapshot(root string) ([]CanonicalInput, error) {
	var entries []CanonicalInput
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		entry := CanonicalInput{Path: filepath.ToSlash(rel)}
		switch {
		case d.Type()&os.ModeSymlink != 0:
			target, readErr := os.Readlink(path)
			if readErr != nil {
				return fmt.Errorf("snapshot digest readlink %s: %w", rel, readErr)
			}
			entry.Kind = "symlink"
			entry.Target = target
		case d.IsDir():
			entry.Kind = "dir"
		case info.Mode().IsRegular():
			entry.Kind = "file"
			entry.Mode = fmt.Sprintf("%o", info.Mode().Perm())
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return fmt.Errorf("snapshot digest read %s: %w", rel, readErr)
			}
			entry.Bytes = data
		default:
			return fmt.Errorf("snapshot digest: unsupported entry %s", rel)
		}
		entries = append(entries, entry)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

// CanonicalDigest returns "sha256-<64 lowercase hex>" of the canonical
// sequence. Two sequences that differ in any path, kind, regular-file
// mode, symlink target, or file bytes produce a different digest.
func CanonicalDigest(canonical []CanonicalInput) string {
	h := sha256.New()
	for _, entry := range canonical {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00", entry.Path, entry.Kind, entry.Mode, entry.Target)
		if entry.Kind == "file" {
			h.Write(entry.Bytes)
		}
		h.Write([]byte{0})
	}
	return "sha256-" + hex.EncodeToString(h.Sum(nil))
}

// ComputeSnapshotDigest calculates the digest over a settled snapshot
// tree. The client computes it over the materialized snapshot it will
// archive; the coordinator recomputes it over the received bytes. Both
// sides must produce the same value for a cache entry to be published.
func ComputeSnapshotDigest(root string) (string, error) {
	canonical, err := CanonicalizeSnapshot(root)
	if err != nil {
		return "", err
	}
	return CanonicalDigest(canonical), nil
}

// VerifySnapshot re-canonicalizes the snapshot tree at root and
// confirms the digest matches expected. It is the integrity check that
// runs after the snapshot bytes are settled: on the coordinator before
// cache publication, on the cache-hit path before a build reuses an
// entry, and inside publication over the copied partial.
func VerifySnapshot(root string, expected string) error {
	got, err := ComputeSnapshotDigest(root)
	if err != nil {
		return err
	}
	if got != expected {
		return fmt.Errorf("snapshot digest mismatch: want %s got %s", expected, got)
	}
	return nil
}
