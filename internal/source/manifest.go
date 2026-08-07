package source

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hypernewbie/vci/internal/layout"
)

type Entry struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size,omitempty"`
	Digest string `json:"digest,omitempty"`
	Target string `json:"target,omitempty"`
}

type Manifest struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
	Digest  string  `json:"digest"`
}

func Build(root string) (Manifest, map[string][]byte, error) {
	entries := make([]Entry, 0)
	blobs := make(map[string][]byte)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		// Skip every .git component at any depth (directory or
		// file). Submodule .git directories carry the child's
		// gitdir data; the Plan 11 contract excludes that data
		// from manifests, snapshots, and cache entries. The
		// top-level minimal .git markers needed for source.Discover
		// are produced by the staging path, not the local
		// manifest, which walks the working tree as-is.
		if isExcludedComponent(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entry := Entry{Path: rel, Mode: uint32(info.Mode().Perm())}
		switch {
		case d.Type()&os.ModeSymlink != 0:
			entry.Kind = "symlink"
			entry.Target, err = os.Readlink(path)
		case d.IsDir():
			entry.Kind = "dir"
		case info.Mode().IsRegular():
			entry.Kind, entry.Size = "file", info.Size()
			before := info.ModTime()
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			afterInfo, statErr := os.Stat(path)
			if statErr != nil {
				return statErr
			}
			if afterInfo.Size() != info.Size() || !afterInfo.ModTime().Equal(before) {
				return fmt.Errorf("source_changed_during_staging: %s", rel)
			}
			digest := sha256.Sum256(data)
			entry.Digest = hex.EncodeToString(digest[:])
			blobs[entry.Digest] = data
		default:
			return fmt.Errorf("unsupported source file: %s", rel)
		}
		if err != nil {
			return err
		}
		entries = append(entries, entry)
		return nil
	})
	if err != nil {
		return Manifest{}, nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	manifest := Manifest{Version: 1, Entries: entries}
	canonical, err := json.Marshal(struct {
		Version int     `json:"version"`
		Entries []Entry `json:"entries"`
	}{manifest.Version, manifest.Entries})
	if err != nil {
		return Manifest{}, nil, err
	}
	digest := sha256.Sum256(canonical)
	manifest.Digest = hex.EncodeToString(digest[:])
	return manifest, blobs, nil
}

type BlobStore struct{ Layout layout.Layout }

func (s BlobStore) PutManifestAndBlobs(manifest Manifest, blobs map[string][]byte) error {
	if err := s.Layout.Ensure(); err != nil {
		return err
	}
	for digest, data := range blobs {
		if err := s.putBlob(digest, data); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(s.Layout.ManifestsDir(), manifest.Digest+".json"), data, 0o600)
}

func (s BlobStore) putBlob(digest string, data []byte) error {
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != digest {
		return fmt.Errorf("blob digest mismatch: %s", digest)
	}
	path := filepath.Join(s.Layout.BlobsDir(), digest)
	if existing, err := os.ReadFile(path); err == nil {
		if string(existing) == string(data) {
			return nil
		}
		return fmt.Errorf("existing blob is corrupt: %s", digest)
	}
	return atomicWrite(path, data, 0o400)
}

// isGitComponent reports whether the top-relative path is a .git
// entry at any depth. The first segment is checked first; nested
// .git directories inside submodule working trees are also skipped.
func isGitComponent(rel string) bool {
	for _, segment := range strings.Split(rel, "/") {
		if segment == ".git" {
			return true
		}
	}
	return false
}

// isExcludedComponent reports whether the top-relative path is
// excluded at every depth: any .git component (directory or file),
// or a .gitmodules file. .gitmodules is excluded because it can
// carry remote URLs and embedded credentials; Vci needs gitlinks
// from index stage records, not checkout URLs.
func isExcludedComponent(rel string) bool {
	if isGitComponent(rel) {
		return true
	}
	return filepath.Base(rel) == ".gitmodules"
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".vci-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return nil
}

// BuildWithValidation builds a manifest from the working tree at
// root and validates every LFS-attributed regular file against the
// formal pointer format. The validation runs against the same
// bytes that become a blob: a typed pointer failure is reported
// before any blob is published, reservation is taken, or run
// record is created.
//
// `lfsFiles` is the set of LFS-attributed paths declared by the
// upstream graph collector. The set is consulted via relative path
// match; an LFS-attributed path that is missing or non-regular in
// the local working tree is a source-state error rather than a
// silent skip, because the agent's tool said it was there.
//
// Local manifest inclusion rules (ignored files, locally-deleted
// tracked files, executable modes, symlinks) are unchanged from
// Build; the only addition is the LFS check at the byte level.
func BuildWithValidation(root string, lfsFiles map[string]bool) (Manifest, map[string][]byte, error) {
	manifest, blobs, err := Build(root)
	if err != nil {
		return Manifest{}, nil, err
	}
	for rel := range lfsFiles {
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil {
			return Manifest{}, nil, fmt.Errorf("lfs-attributed file %s: %w", rel, err)
		}
		if !info.Mode().IsRegular() {
			return Manifest{}, nil, fmt.Errorf("lfs-attributed file %s is not regular", rel)
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return Manifest{}, nil, fmt.Errorf("read lfs-attributed file %s: %w", rel, err)
		}
		if isLFSPointer(data) {
			return Manifest{}, nil, fmt.Errorf("%w: %s", ErrLFSContentUnavailable, rel)
		}
	}
	return manifest, blobs, nil
}
