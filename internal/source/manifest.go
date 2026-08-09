package source

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/hypernewbie/vci/internal/model"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
		// Skip .git components at any depth (dir or file). Manifests ignore
		// git metadata for staging; required top-level markers are added separately.
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

type BlobStore struct{ Layout model.Layout }

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

// isGitComponent reports whether any path segment is ".git".
func isGitComponent(rel string) bool {
	for _, segment := range strings.Split(rel, "/") {
		if segment == ".git" {
			return true
		}
	}
	return false
}

// isExcludedComponent skips .git artifacts and .gitmodules files.
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

// BuildWithValidation builds the manifest and validates LFS-attributed files.
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

func (s BlobStore) LoadManifest(digest string) (Manifest, error) {
	if digest == "" {
		return Manifest{}, fmt.Errorf("manifest digest is empty")
	}
	data, err := os.ReadFile(filepath.Join(s.Layout.ManifestsDir(), digest+".json"))
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, err
	}
	if manifest.Digest != digest {
		return Manifest{}, fmt.Errorf("manifest digest mismatch: %s", digest)
	}
	return manifest, nil
}

func (s BlobStore) Materialize(manifest Manifest, destination string) error {
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return err
	}
	entries := append([]Entry(nil), manifest.Entries...)
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Kind == entries[j].Kind {
			return entries[i].Path < entries[j].Path
		}
		if entries[i].Kind == "dir" {
			return true
		}
		if entries[j].Kind == "dir" {
			return false
		}
		return entries[i].Path < entries[j].Path
	})
	for _, entry := range entries {
		path, err := safeJoin(destination, entry.Path)
		if err != nil {
			return err
		}
		switch entry.Kind {
		case "dir":
			if err := os.MkdirAll(path, 0o700); err != nil {
				return err
			}
			if err := os.Chmod(path, os.FileMode(entry.Mode)); err != nil {
				return err
			}
		case "file":
			data, err := os.ReadFile(filepath.Join(s.Layout.BlobsDir(), entry.Digest))
			if err != nil {
				return fmt.Errorf("read blob %s: %w", entry.Digest, err)
			}
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != entry.Digest {
				return fmt.Errorf("source blob corrupt: %s", entry.Digest)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(path, data, os.FileMode(entry.Mode)); err != nil {
				return err
			}
			// os.WriteFile applies the process umask. Restore the manifest mode
			// explicitly so a Windows 0666 source materializes identically on a
			// Unix coordinator with umask 022.
			if err := os.Chmod(path, os.FileMode(entry.Mode)); err != nil {
				return err
			}
		case "symlink":
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			if err := os.Symlink(entry.Target, path); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported manifest kind %q", entry.Kind)
		}
	}
	actual, _, err := Build(destination)
	if err != nil {
		return err
	}
	if actual.Digest != manifest.Digest {
		return fmt.Errorf("materialized workspace does not match manifest")
	}
	return nil
}

func safeJoin(root, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || strings.HasPrefix(filepath.ToSlash(rel), "../") || filepath.Clean(rel) == ".." {
		return "", fmt.Errorf("unsafe source path %q", rel)
	}
	path := filepath.Join(root, filepath.FromSlash(rel))
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	cleanPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if cleanPath != cleanRoot && !strings.HasPrefix(cleanPath, cleanRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("source path escapes workspace: %q", rel)
	}
	return path, nil
}
