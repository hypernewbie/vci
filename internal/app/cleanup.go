package app

import (
	"io/fs"
	"os"
	"path/filepath"
)

func removeOwned(path string) error {
	// Build tools may deliberately make cache files read-only. Restore ownership
	// permissions before removing a Vci-owned tree.
	_ = filepath.WalkDir(path, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			_ = os.Chmod(current, 0o700)
		} else {
			_ = os.Chmod(current, 0o600)
		}
		return nil
	})
	return os.RemoveAll(path)
}
