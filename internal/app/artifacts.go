package app

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ArtifactCapBytes is the per-run total byte cap for collected
// artifacts. When the cumulative size exceeds this value, further
// matches are dropped and the truncated flag is set on the build
// result. The default 64 MiB is documented in the README.
const ArtifactCapBytes int64 = 64 << 20

// CollectArtifacts walks the workspace, matches each configured
// glob, copies every regular file to <runDir>/artifacts/<rel>, and
// returns the list of relative paths actually collected and a
// truncated flag. The collector never crosses out of the
// workspace; symlinks, device files, and `.git`/`.vci` paths are
// rejected. Glob matching is per path segment: a trailing bare `*`
// collects the whole subtree (`build/*` matches
// `build/sub/file.txt`), while a constrained final segment is
// single-level (`build/*.bin` matches only files directly inside
// `build/`); see matchWorkspaceGlob.
//
// `projectArtifacts` is the verified list of workspace-relative
// globs from the durable snapshot (validated at config load time).
// `workspace` is the per-run source root. `runDir` is the run
// state directory.
func CollectArtifacts(workspace, runDir string, projectArtifacts []string) (collected []string, truncated bool, err error) {
	if len(projectArtifacts) == 0 {
		return nil, false, nil
	}
	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return nil, false, fmt.Errorf("resolve workspace: %w", err)
	}
	absRunDir, err := filepath.Abs(runDir)
	if err != nil {
		return nil, false, fmt.Errorf("resolve run dir: %w", err)
	}
	artDir := filepath.Join(absRunDir, "artifacts")
	if err := os.MkdirAll(artDir, 0o700); err != nil {
		return nil, false, fmt.Errorf("mkdir artifacts: %w", err)
	}
	used := int64(0)
	for _, glob := range projectArtifacts {
		matches, matchErr := matchWorkspaceGlob(absWorkspace, glob)
		if matchErr != nil {
			return collected, truncated, matchErr
		}
		for _, rel := range matches {
			srcPath := filepath.Join(absWorkspace, rel)
			// Use Lstat so a symlink is detected and rejected
			// before any read; os.Stat would follow the link and
			// copy bytes from the symlink target outside the
			// workspace containment rule.
			info, statErr := os.Lstat(srcPath)
			if statErr != nil {
				continue
			}
			if info.Mode()&os.ModeSymlink != 0 {
				continue
			}
			if !info.Mode().IsRegular() {
				continue
			}
			if used+info.Size() > ArtifactCapBytes {
				truncated = true
				continue
			}
			dst := filepath.Join(artDir, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return collected, truncated, err
			}
			if err := copyRegular(srcPath, dst, info.Mode().Perm()); err != nil {
				return collected, truncated, err
			}
			used += info.Size()
			collected = append(collected, filepath.ToSlash(rel))
		}
	}
	return collected, truncated, nil
}

// matchWorkspaceGlob evaluates a workspace-relative glob against
// the workspace root. Matching is per path segment: every glob
// segment except the last must path.Match exactly one leading
// path segment of the artifact, and the final glob segment must
// path.Match every remaining path segment (one or more). The
// semantics are therefore explicit about depth: `build/*`
// collects the entire `build/` tree (the trailing `*` matches
// each deeper segment), while `build/*.bin` collects only files
// whose trailing segments each match `*.bin`, i.e. files
// directly inside `build/`. `**` has no special recursive
// meaning; it is matched literally as an ordinary segment.
// path.Match is the only pattern engine — no doublestar or
// recursive `**` extension is introduced. Matched paths are
// restricted to files inside the workspace.
func matchWorkspaceGlob(workspace, glob string) ([]string, error) {
	globSegments := strings.Split(glob, "/")
	var matches []string
	err := filepath.WalkDir(workspace, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !d.IsDir() {
			rel, err := filepath.Rel(workspace, p)
			if err != nil {
				return err
			}
			relSlash := filepath.ToSlash(rel)
			if isExcludedArtifact(relSlash) {
				return nil
			}
			ok, err := matchGlobSegments(globSegments, relSlash)
			if err != nil {
				return err
			}
			if ok {
				matches = append(matches, rel)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return matches, nil
}

// matchGlobSegments applies the per-segment rule: leading glob
// segments match exactly one path segment each; the final glob
// segment must match every remaining path segment. A glob with
// more segments than the path never matches.
func matchGlobSegments(globSegments []string, rel string) (bool, error) {
	relSegments := strings.Split(rel, "/")
	if len(relSegments) < len(globSegments) {
		return false, nil
	}
	last := len(globSegments) - 1
	for i := 0; i < last; i++ {
		ok, err := path.Match(globSegments[i], relSegments[i])
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	for _, segment := range relSegments[last:] {
		ok, err := path.Match(globSegments[last], segment)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// isExcludedArtifact rejects paths the collector must never copy:
// `.git` components at any depth and `.vci` roots. The collector
// runs after the executor returns, so any escape the workspace
// already permits would be a source-graph defect, not an artifact
// collector defect.
func isExcludedArtifact(rel string) bool {
	for _, segment := range strings.Split(filepath.ToSlash(rel), "/") {
		if segment == ".git" || segment == ".vci" {
			return true
		}
	}
	return false
}

func copyRegular(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return nil
}
