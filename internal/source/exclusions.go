package source

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
)

// ApplyExclusions removes entries under dir that match any of the
// coordinator-owned exclusion globs. A path is excluded when a glob matches its
// dir-relative path or its base name (path.Match does not cross separators, so
// base-name matching covers any depth). .git and .vci are never traversed, so
// internal Git or VCI state is never excluded. An empty glob list is a no-op.
func ApplyExclusions(dir string, globs []string) error {
	if len(globs) == 0 {
		return nil
	}
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if d.Name() == ".git" || d.Name() == ".vci" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !excludedPath(rel, d.Name(), globs) {
			return nil
		}
		if d.IsDir() {
			if err := os.RemoveAll(p); err != nil {
				return err
			}
			return fs.SkipDir
		}
		return os.Remove(p)
	})
}

func excludedPath(rel, name string, globs []string) bool {
	rel = filepath.ToSlash(rel)
	for _, g := range globs {
		if m, _ := path.Match(g, rel); m {
			return true
		}
		if m, _ := path.Match(g, name); m {
			return true
		}
	}
	return false
}
