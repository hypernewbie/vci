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
		if rel == ".git" || strings.HasPrefix(rel, ".git/") {
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
