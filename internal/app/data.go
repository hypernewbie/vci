package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/store"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// ArtifactCapBytes is the per-run artifact cap; overflow marks collection as truncated.
const ArtifactCapBytes int64 = 64 << 20

// CollectArtifacts copies matching workspace files into `<runDir>/artifacts/<rel>`.
// It returns collected paths and whether collection was truncated.
// Only regular workspace files are copied; symlinks, special files,
// `.git`, and `.vci` entries are skipped.
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
			// Lstat rejects symlinks before any read to avoid traversing targets.
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

// matchWorkspaceGlob matches workspace-relative paths by segment.
// It uses path.Match only and returns workspace-relative results.
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

// matchGlobSegments applies per-segment glob matching rules.
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

// isExcludedArtifact filters `.git` and `.vci` path segments.
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

// ReadLog reads durable run logs for stdout or stderr.

// ErrInvalidLogStream is returned for non-stdout/stderr log streams.
var ErrInvalidLogStream = errors.New("invalid log stream")

// ErrLogNotFound is returned when the requested log is missing or not regular.
var ErrLogNotFound = errors.New("log not found")

// ReadLog opens `stdout.log` or `stderr.log` for a run.
// Missing run or missing file returns typed errors.
// Caller must close returned reader.
func ReadLog(l model.Layout, id model.RunID, stream string) (io.ReadCloser, int64, error) {
	if !model.ValidRunID(id) {
		return nil, 0, fmt.Errorf("invalid run id %q", id)
	}
	if stream != "stdout" && stream != "stderr" {
		return nil, 0, fmt.Errorf("%w: %q", ErrInvalidLogStream, stream)
	}
	runStore := store.Store{Layout: l}
	if _, err := runStore.Load(id); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, model.ErrRunNotFound
		}
		return nil, 0, err
	}
	runDir, err := l.RunDir(string(id))
	if err != nil {
		return nil, 0, err
	}
	path := filepath.Join(runDir, stream+".log")
	// Lstat so a swapped-in symlink is rejected before any read; the
	// worker only ever writes regular log files.
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, fmt.Errorf("%w: %s", ErrLogNotFound, stream)
		}
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%w: %s", ErrLogNotFound, stream)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// Artifact functions read and stream run artifacts from state/runs/<run>/artifacts.

// ErrArtifactNotFound is returned when a requested artifact is missing.
var ErrArtifactNotFound = errors.New("artifact not found")

// ErrInvalidArtifactPath marks artifact requests that fail path validation.
var ErrInvalidArtifactPath = errors.New("invalid artifact path")

// ListArtifacts returns sorted artifact paths and truncated flag for a run.
func ListArtifacts(l model.Layout, id model.RunID) ([]string, bool, error) {
	if !model.ValidRunID(id) {
		return nil, false, fmt.Errorf("invalid run id %q", id)
	}
	runStore := store.Store{Layout: l}
	if _, err := runStore.Load(id); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, model.ErrRunNotFound
		}
		return nil, false, err
	}
	runDir, err := l.RunDir(string(id))
	if err != nil {
		return nil, false, err
	}
	artDir := filepath.Join(runDir, "artifacts")
	var files []string
	err = filepath.WalkDir(artDir, func(p string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(artDir, p)
		if relErr != nil {
			return relErr
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, false, err
	}
	sort.Strings(files)
	truncated, err := readArtifactsTruncated(l, id)
	if err != nil {
		return nil, false, err
	}
	return files, truncated, nil
}

// readArtifactsTruncated reads the `artifacts_truncated` flag from result.json.
// Missing or malformed result files are treated as not truncated.
func readArtifactsTruncated(l model.Layout, id model.RunID) (bool, error) {
	data, err := (store.Store{Layout: l}).ReadResult(id)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	var result struct {
		ArtifactsTruncated bool `json:"artifacts_truncated"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return false, nil
	}
	return result.ArtifactsTruncated, nil
}

// GetArtifact opens one artifact for streaming from a run's artifact directory.
// The caller must close the returned ReadCloser.
func GetArtifact(l model.Layout, id model.RunID, rel string) (io.ReadCloser, int64, error) {
	if !model.ValidRunID(id) {
		return nil, 0, fmt.Errorf("invalid run id %q", id)
	}
	if err := validateArtifactRel(rel); err != nil {
		return nil, 0, err
	}
	runStore := store.Store{Layout: l}
	if _, err := runStore.Load(id); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, model.ErrRunNotFound
		}
		return nil, 0, err
	}
	runDir, err := l.RunDir(string(id))
	if err != nil {
		return nil, 0, err
	}
	path := filepath.Join(runDir, "artifacts", filepath.FromSlash(rel))
	// Lstat rejects swapped-in symlinks before reading.
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, fmt.Errorf("%w: %s", ErrArtifactNotFound, rel)
		}
		return nil, 0, err
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%w: %s", ErrArtifactNotFound, rel)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// validateArtifactRel rejects unsafe artifact paths.
// It blocks absolute, empty, dot, parent, control/whitespace, leading '-',
// and model.ValidName/isExcludedArtifact-rejected segments.
// Every rejection wraps ErrInvalidArtifactPath.
func validateArtifactRel(rel string) error {
	if rel == "" {
		return fmt.Errorf("%w: path is empty", ErrInvalidArtifactPath)
	}
	if strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "\\") || filepath.IsAbs(rel) {
		return fmt.Errorf("%w: path %q is absolute", ErrInvalidArtifactPath, rel)
	}
	if strings.HasPrefix(rel, "-") {
		return fmt.Errorf("%w: path %q starts with -", ErrInvalidArtifactPath, rel)
	}
	for _, segment := range strings.Split(rel, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("%w: path %q contains an empty, dot, or parent segment", ErrInvalidArtifactPath, rel)
		}
		if strings.ContainsAny(segment, "\x00\t\n\r ") {
			return fmt.Errorf("%w: path %q contains control or whitespace characters", ErrInvalidArtifactPath, rel)
		}
		if !model.ValidName(segment) {
			return fmt.Errorf("%w: path segment %q is invalid", ErrInvalidArtifactPath, segment)
		}
	}
	if isExcludedArtifact(rel) {
		return fmt.Errorf("%w: path %q is excluded", ErrInvalidArtifactPath, rel)
	}
	return nil
}
