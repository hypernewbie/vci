package source

import (
	"fmt"
	"os"
	"path/filepath"
)

// Shared prefix/filename constants used by the Go-side writers and
// readers of Vci-owned staging trees and snapshot directories. Shell
// scripts that reference the same paths embed the literal strings by
// necessity; this is the single Go-side source of truth.
const (
	// SnapshotPrefix is the basename prefix of every Vci-owned
	// settled-snapshot directory under the coordinator/client temp
	// directory. The reaper sweeps only this prefix and never
	// traverses arbitrary TMPDIR content.
	SnapshotPrefix = "vci-snapshot-"
	// StagingPrefix is the basename prefix of every Vci-owned direct
	// SSH staging directory under the coordinator temp directory.
	StagingPrefix = "vci-source-"
	// HostedPrefix is the basename prefix of every Vci-owned pinned
	// Git checkout directory produced by `source.Checkout`. The
	// reaper sweeps only this prefix and never traverses arbitrary
	// TMPDIR content. A pinned checkout is never reused between
	// runs; a fresh rand suffix is generated per invocation.
	HostedPrefix = "vci-hosted-"
	// StagingMetaName is the basename of the metadata file the
	// staging fragment writes inside each staging directory so the
	// coordinator can rebuild the validated cache key.
	StagingMetaName = "vci-meta"
)

// allowedTopLevelGitMarkers is the closed set of `.git`-prefixed
// paths that may survive into the materialized snapshot at the top
// level. These three markers are the minimum required for the
// remote `source.Discover` to recognize the staged tree as a Git
// repository directory via `git rev-parse --show-toplevel`. They
// are the only signals the direct-SSH local source path appends to
// the selected file list at the top level (see buildGraph in
// submodule.go). All other `.git` content (config, hooks, logs,
// packed-refs, objects/pack, branches, index, info) stays excluded
// at every depth because gitlinks, not the `.git` directory, are
// the contracted path-restoration signal.
var allowedTopLevelGitMarkers = map[string]bool{
	".git/HEAD":    true,
	".git/objects": true,
	".git/refs":    true,
}

// MaterializeSnapshot copies the selected build input into a new
// Vci-owned directory under destParent, preserving relative paths,
// regular-file mode bits, and symlinks. Listed directories are created
// as empty directories; private configuration such as .git/config is
// not selected and therefore never copied.
//
// The input is validated against the canonical-entry contract before
// any file is written: every entry must be a safe relative path. The
// caller need not pre-validate; MaterializeSnapshot is the sole
// producer of the snapshot tree and enforces the contract locally.
//
// After the snapshot is populated, LFS-attributed regular files are
// checked against the formal pointer format. A pointer is rejected
// with ErrLFSContentUnavailable naming the top-relative path so the
// agent can run `git lfs pull`. Attribute semantics, not magic
// content alone, decide rejection: a non-LFS file with pointer-
// looking bytes is ordinary source data.
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
			// Defensive: validated input should never carry an
			// excluded component, but the consumer-trust contract
			// is preserved either way. The three top-level
			// minimal git markers are explicitly allowed because
			// the direct-SSH local source path appends them so
			// the remote `vci build .` can resolve the repository
			// root via `source.Discover`. Nested `.git` at any
			// depth, `.gitmodules` at any depth, and any
			// non-marker `.git` content stays excluded.
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
