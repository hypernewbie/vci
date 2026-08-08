package app

// Plan 17 Phase 1 read-side log surface. The worker writes the
// durable per-run log files `state/runs/<id>/stdout.log` and
// `state/runs/<id>/stderr.log` (mode 0600) before execution, and
// `vci check` references them as `stdout_path`/`stderr_path`.
// ReadLog exposes them read-only:
//
//   - `stream` must be "stdout" or "stderr"; anything else is
//     ErrInvalidLogStream;
//   - a missing run record is model.ErrRunNotFound;
//   - a missing log file is ErrLogNotFound;
//   - Lstat rejects a swapped-in symlink (logs are always regular
//     files written by the worker) before any read.
//
// No streaming protocol, no follow/tail daemon, no relay: this is a
// single-file cat, and the bounded `--tail` is a CLI presentation
// concern on top of the returned reader.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/store"
)

// ErrInvalidLogStream is returned by ReadLog when stream is neither
// "stdout" nor "stderr". The CLI maps it to the invalid_arguments
// envelope.
var ErrInvalidLogStream = errors.New("invalid log stream")

// ErrLogNotFound is returned by ReadLog when the requested stream's
// log file is missing or is not a regular file. The CLI maps it to
// the not_found envelope.
var ErrLogNotFound = errors.New("log not found")

// ReadLog opens one of a run's durable log files for streaming:
// `state/runs/<id>/{stdout,stderr}.log`. stream is "stdout" or
// "stderr"; anything else is ErrInvalidLogStream. A missing run
// record is model.ErrRunNotFound; a missing log file is
// ErrLogNotFound. The file must be a regular file — Lstat rejects a
// swapped-in symlink before any read. The caller owns the returned
// ReadCloser and should close it even on partial reads.
func ReadLog(l layout.Layout, id model.RunID, stream string) (io.ReadCloser, int64, error) {
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
