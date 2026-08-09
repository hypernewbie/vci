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

// Prefixes for Vci-owned snapshot and staging names used by scripts.
const (
	// SnapshotPrefix names Vci-owned snapshot temp directories.
	SnapshotPrefix = "vci-snapshot-"
	// StagingPrefix names Vci-owned direct-SSH staging directories.
	StagingPrefix = "vci-source-"
	// HostedPrefix names Vci-owned pinned Git checkout directories.
	HostedPrefix = "vci-hosted-"
	// StagingMetaName is the metadata filename used inside each staging directory.
	StagingMetaName = "vci-meta"
)

// allowedTopLevelGitMarkers keeps only repository-root markers for discovery:
// .git/HEAD, .git/objects, .git/refs; all other git paths are excluded.
var allowedTopLevelGitMarkers = map[string]bool{
	".git/HEAD":    true,
	".git/objects": true,
	".git/refs":    true,
}

// MaterializeSnapshot validates and copies selected input into a new snapshot directory,
// runs LFS checks, and returns the snapshot path.
// Caller must remove the snapshot root; destParent must exist.
func MaterializeSnapshot(input SourceInput, destParent string) (string, error) {
	validated, err := ValidateInput(input)
	if err != nil {
		return "", err
	}
	root, err := os.MkdirTemp(destParent, SnapshotPrefix)
	if err != nil {
		return "", fmt.Errorf("snapshot mkdir: %w", err)
	}
	cleanup := func() {
		_ = os.RemoveAll(root)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		cleanup()
		return "", fmt.Errorf("snapshot protect: %w", err)
	}
	for _, p := range validated.Files {
		if isExcludedComponent(p) && !allowedTopLevelGitMarkers[p] {
			// Allow required top-level git markers for discovery; exclude all other
			// nested git paths, .gitmodules, and non-marker .git contents.
			continue
		}
		src := filepath.Join(validated.Root, p)
		fi, err := os.Lstat(src)
		if err != nil {
			cleanup()
			return "", fmt.Errorf("snapshot stat %s: %w", p, err)
		}
		dest := filepath.Join(root, p)
		switch {
		case fi.IsDir():
			if err := os.MkdirAll(dest, 0o700); err != nil {
				cleanup()
				return "", fmt.Errorf("snapshot mkdir %s: %w", p, err)
			}
		case fi.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(src)
			if err != nil {
				cleanup()
				return "", fmt.Errorf("snapshot readlink %s: %w", p, err)
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
				cleanup()
				return "", fmt.Errorf("snapshot mkdir parent %s: %w", p, err)
			}
			if err := os.Symlink(target, dest); err != nil {
				cleanup()
				return "", fmt.Errorf("snapshot symlink %s: %w", p, err)
			}
		case fi.Mode().IsRegular():
			data, err := os.ReadFile(src)
			if err != nil {
				cleanup()
				return "", fmt.Errorf("snapshot read %s: %w", p, err)
			}
			if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
				cleanup()
				return "", fmt.Errorf("snapshot mkdir parent %s: %w", p, err)
			}
			if err := os.WriteFile(dest, data, fi.Mode().Perm()); err != nil {
				cleanup()
				return "", fmt.Errorf("snapshot write %s: %w", p, err)
			}
		default:
			cleanup()
			return "", fmt.Errorf("snapshot: unsupported source entry %s", p)
		}
	}
	if err := ValidateLFSPointers(root, validated.LFSFiles); err != nil {
		cleanup()
		return "", err
	}
	return root, nil
}

// CanonicalInput is the hash-relevant snapshot entry payload used for digests.
// It keeps only fields meaningful for byte-for-byte reproducibility.
type CanonicalInput struct {
	Path   string
	Kind   string // "file", "symlink", "dir"
	Mode   string // octal mode bits (regular files only)
	Target string // symlink target or empty
	Bytes  []byte // empty for non-regular entries
}

// CanonicalizeSnapshot walks root, captures normalized file/dir/symlink entries,
// sorts them by path, and omits directory/symlink mode details.
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

// CanonicalDigest returns a sha256 hash of canonical entries as "sha256-<64 hex>".
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
