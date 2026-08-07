package source

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hypernewbie/vci/internal/process"
)

// SelectBuildInput inspects the local working tree at sourcePath and
// returns a typed SourceInput representing direct-SSH client build
// inputs for remote transmission.
//
// Selection rules for direct-SSH client build inputs (read during
// archive streaming):
//  1. Tracked files (HEAD, modified, staged) of the top-level
//     repository: included using current working tree bytes and mode
//     bits.
//  2. Locally deleted tracked files: excluded (absent from staged
//     input).
//  3. Untracked non-ignored files: included.
//  4. Ignored files and directories (.gitignore): excluded.
//  5. Initialized submodules (mode-160000 gitlinks): recursively
//     inspected. Each verified child contributes its own selected
//     files under its validated prefix. The child directory itself
//     is preserved so empty submodules still create a directory in
//     the snapshot.
//  6. Uninitialized, missing, symlinked, escaping, or conflicted
//     gitlinks fail with ErrSubmoduleUnavailable before any snapshot
//     or archive is produced. The error names the top-root-relative
//     path so the agent can run the ordinary user action `git
//     submodule update --init --recursive`. Vci never executes that
//     command, fetches a URL, reads .git/config, or contacts a host.
//  7. Minimal top-level git markers (.git/HEAD, .git/objects,
//     .git/refs) are included so the remote `vci build .` can
//     discover the repository root via `source.Discover`. No child
//     git marker is ever added; child .git at any depth is excluded
//     from the snapshot, manifest, cache entry, and tar stream.
//  8. Linked Git worktrees (.git file) are rejected locally before
//     network transmission with a deterministic infrastructure error.
//  9. Filenames containing newlines are rejected locally before any
//     archive.
//  10. Every Git-produced path is validated as a safe relative path
//     before snapshot and tar composition; Git-printed paths are not
//     trusted on their own.
//  11. `.gitmodules` is excluded at every depth. The submodule's
//     gitlink reference is the only approved path-restoration
//     signal; a tracked `.gitmodules` cannot leak remote URLs or
//     embedded credentials into the build. Vci never reads
//     `.gitmodules`, fetches a URL, or writes back a checkout URL.
//
// Any read or archive failure during selection/streaming aborts
// client submission before returning a remote run ID. Local
// coordinator builds retain their existing source-manifest behavior,
// extended to skip every .git component at any depth.
func SelectBuildInput(ctx context.Context, sourcePath string, runner process.Runner) (SourceInput, error) {
	absPath, err := filepath.Abs(sourcePath)
	if err != nil {
		return SourceInput{}, err
	}
	if fi, lerr := os.Lstat(filepath.Join(absPath, ".git")); lerr == nil {
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return SourceInput{}, fmt.Errorf("unsupported source: linked Git worktrees (.git file) are not supported")
		}
	}
	repo, err := Discover(ctx, sourcePath, runner)
	if err != nil {
		return SourceInput{}, err
	}
	gitPath := filepath.Join(repo.Root, ".git")
	fi, err := os.Lstat(gitPath)
	if err != nil {
		return SourceInput{}, fmt.Errorf("stat .git: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return SourceInput{}, fmt.Errorf("unsupported source: linked Git worktrees (.git file) are not supported")
	}
	return buildGraph(ctx, repo.Root, runner)
}
