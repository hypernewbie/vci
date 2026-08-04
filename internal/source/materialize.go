package source

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

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
