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

// parentDirs returns the directory paths that must hold a file path,
// from the shallowest to the deepest, excluding the root itself. These
// entries make the file-list canonicalization equal to the tree-walk
// canonicalization over a materialized snapshot.
func parentDirs(rel string) []string {
	var dirs []string
	dir := filepath.Dir(rel)
	for dir != "." && dir != "" && dir != string(filepath.Separator) {
		dirs = append(dirs, dir)
		dir = filepath.Dir(dir)
	}
	// Reverse so parents sort as ancestors (shallowest first).
	for i, j := 0, len(dirs)-1; i < j; i, j = i+1, j-1 {
		dirs[i], dirs[j] = dirs[j], dirs[i]
	}
	return dirs
}

// Canonicalize materializes the SourceInput as the immutable byte
// sequence the digest is computed over. It reads each selected file
// exactly once and returns a deterministic slice, sorted by relative
// path, that contains only canonical fields. Parent directories of
// selected files are included as directory entries without modes so the
// result matches a tree-walk over a materialized snapshot of the same
// input. Ignored files, deleted files, private .git/config, object-pack
// history, and host mtimes are excluded from the identity.
func Canonicalize(input SourceInput) ([]CanonicalInput, error) {
	files := make([]string, 0, len(input.Files)*2)
	for _, p := range input.Files {
		files = append(files, parentDirs(p)...)
		files = append(files, p)
	}
	sort.Strings(files)
	canonical := make([]CanonicalInput, 0, len(files))
	seen := make(map[string]bool)
	for _, p := range files {
		if seen[p] {
			continue
		}
		seen[p] = true
		fullPath := filepath.Join(input.Root, p)
		fi, err := os.Lstat(fullPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("digest stat %s: %w", p, err)
		}
		entry := CanonicalInput{Path: p}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			target, readErr := os.Readlink(fullPath)
			if readErr != nil {
				return nil, fmt.Errorf("digest readlink %s: %w", p, readErr)
			}
			entry.Kind = "symlink"
			entry.Target = target
		case fi.IsDir():
			entry.Kind = "dir"
		default:
			entry.Kind = "file"
			entry.Mode = fmt.Sprintf("%o", fi.Mode().Perm())
			data, readErr := os.ReadFile(fullPath)
			if readErr != nil {
				return nil, fmt.Errorf("digest read %s: %w", p, readErr)
			}
			entry.Bytes = data
		}
		canonical = append(canonical, entry)
	}
	return canonical, nil
}

// CanonicalizeSnapshot walks a settled snapshot tree and returns the
// canonical entries for every entry under root, sorted by relative
// path. The set is exactly the materialized selected input: files,
// symlinks, listed directories, and the parent directories that hold
// them. Directory and symlink modes are excluded for the same reason as
// in Canonicalize.
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

// ComputeDigest calculates a deterministic SHA-256 hash over the defined
// build input (files, modes, symlink targets, and file content bytes).
// The canonical form matches the digest of a materialized snapshot of
// the same input.
func ComputeDigest(input SourceInput) (string, error) {
	canonical, err := Canonicalize(input)
	if err != nil {
		return "", err
	}
	return CanonicalDigest(canonical), nil
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
