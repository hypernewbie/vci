package app

// Plan 16 Phase 1 read-side artifact surface. CollectArtifacts owns
// the durable per-run copy under state/runs/<run>/artifacts/; these
// functions expose it read-only:
//
//   - ListArtifacts returns the collected relative paths plus the
//     durable result.json truncated flag (the 64 MiB per-run cap);
//   - GetArtifact streams one artifact's exact bytes. The relative
//     path is validated by validateArtifactRel before any filesystem
//     access, so a client-supplied string can never escape the run's
//     artifacts directory.
//
// No put/push/registry surface is added: `ls` is an inventory query
// and `get` is a single-file cat over the coordinator's durable copy.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/store"
)

// ErrArtifactNotFound is returned by GetArtifact when the requested
// relative path is not one of the run's collected artifacts.
var ErrArtifactNotFound = errors.New("artifact not found")

// ErrInvalidArtifactPath is wrapped by validateArtifactRel for every
// rejected relative path. The CLI maps it to the invalid_arguments
// envelope; app callers can distinguish validation failures from
// missing runs/artifacts with errors.Is.
var ErrInvalidArtifactPath = errors.New("invalid artifact path")

// ListArtifacts inventories the collected artifacts of a run:
// `state/runs/<id>/artifacts/` walk (sorted, slash-separated rel
// paths) plus the durable result.json `artifacts_truncated` flag.
// A missing run record is model.ErrRunNotFound; a run with no
// artifacts (or no result yet) lists empty with truncated=false.
func ListArtifacts(l layout.Layout, id model.RunID) ([]string, bool, error) {
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

// readArtifactsTruncated reads the `artifacts_truncated` flag from the
// run's durable result.json. A run with no result yet is not truncated;
// a malformed result is tolerated as false so a read-only inventory
// query never fails on a corrupt result file.
func readArtifactsTruncated(l layout.Layout, id model.RunID) (bool, error) {
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

// GetArtifact opens one collected artifact of a run for streaming. The
// relative path must pass validateArtifactRel and the file must be a
// regular file inside state/runs/<id>/artifacts/; anything else is
// ErrInvalidArtifactPath or ErrArtifactNotFound. The caller owns the
// returned ReadCloser and should close it even on partial reads.
func GetArtifact(l layout.Layout, id model.RunID, rel string) (io.ReadCloser, int64, error) {
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
	// Lstat so a swapped-in symlink is rejected before any read; the
	// collector never writes symlinks into the artifacts tree.
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

// validateArtifactRel rejects every relative path that could escape
// the run's artifacts directory or name a file the collector would
// never produce: absolute paths, `..`/`.`/empty segments, control and
// whitespace characters, leading `-`, segments that fail
// layout.ValidName (so `.git`, `.vci`, `.hidden`, and `-flag`
// segments are refused), and any path isExcludedArtifact rejects.
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
		if !layout.ValidName(segment) {
			return fmt.Errorf("%w: path segment %q is invalid", ErrInvalidArtifactPath, segment)
		}
	}
	if isExcludedArtifact(rel) {
		return fmt.Errorf("%w: path %q is excluded", ErrInvalidArtifactPath, rel)
	}
	return nil
}
