package source

import (
	"fmt"
	"os"
	"path/filepath"
)

// MaterializeSnapshot copies the selected build input into a new
// Vci-owned directory under destParent, preserving relative paths,
// regular-file mode bits, and symlinks. Listed directories are created
// as empty directories; private configuration such as .git/config is
// not selected and therefore never copied.
//
// The returned root contains exactly the selected input, so a digest
// computed over it matches a digest recomputed over the coordinator's
// tar-extracted copy: the client archives this settled snapshot, never
// the live working tree, and no source mutation between digest
// computation and archive production can change the archived bytes.
//
// destParent must exist and be Vci-owned. The caller owns removal of
// the returned root on success, error, and cancellation.
func MaterializeSnapshot(input SourceInput, destParent string) (string, error) {
	root, err := os.MkdirTemp(destParent, "vci-snapshot-")
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
	for _, p := range input.Files {
		src := filepath.Join(input.Root, p)
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
	return root, nil
}
