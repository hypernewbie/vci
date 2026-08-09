package source

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/hypernewbie/vci/internal/process"
)

// ErrBundleEmpty is returned by CreateBundle when the requested range carries
// no commits the coordinator lacks, so no bundle transfer is needed.
var ErrBundleEmpty = errors.New("source bundle range is empty")

// CreateBundle produces a Git bundle of the commits reachable from head but not
// from have: the objects a coordinator that already has have lacks. When have
// is empty the bundle carries the full history reachable from head. When have
// equals head, or the range is otherwise empty, CreateBundle returns
// ErrBundleEmpty. Have and head must be full object ids.
//
// Git bundles are addressed by ref, so CreateBundle writes transient refs under
// refs/vci/ for the duration of bundle creation and removes them before
// returning; the working tree and index are never touched. The returned reader
// streams the bundle from a temporary file removed when the reader is closed.
func CreateBundle(ctx context.Context, repoRoot, have, head string, runner process.Runner) (io.ReadCloser, error) {
	if have != "" && have == head {
		return nil, ErrBundleEmpty
	}
	var rid [8]byte
	if _, err := rand.Read(rid[:]); err != nil {
		return nil, err
	}
	ns := "refs/vci/" + hex.EncodeToString(rid[:]) + "/"
	headRef := ns + "head"
	baseRef := ns + "base"
	if err := updateRef(ctx, runner, repoRoot, headRef, head); err != nil {
		return nil, err
	}
	defer func() {
		delRef(ctx, runner, repoRoot, headRef)
		delRef(ctx, runner, repoRoot, baseRef)
	}()
	rangeArg := headRef
	if have != "" {
		if err := updateRef(ctx, runner, repoRoot, baseRef, have); err != nil {
			return nil, err
		}
		count, err := runGitOutput(ctx, runner, repoRoot, "rev-list", "--count", baseRef+".."+headRef)
		if err != nil {
			return nil, err
		}
		n, convErr := strconv.Atoi(count)
		if convErr != nil || n <= 0 {
			return nil, ErrBundleEmpty
		}
		rangeArg = baseRef + ".." + headRef
	}
	tmp, err := os.CreateTemp("", "vci-bundle-*.bundle")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return nil, err
	}
	res, runErr := runner.Run(ctx, process.Command{
		Executable: "git",
		Args:       []string{"-C", repoRoot, "bundle", "create", tmpPath, rangeArg},
	})
	if runErr != nil {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("git bundle create: %w", runErr)
	}
	if res.ExitCode != 0 {
		_ = os.Remove(tmpPath)
		return nil, fmt.Errorf("git bundle create exited %d", res.ExitCode)
	}
	file, err := os.Open(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return nil, err
	}
	return &bundleReader{File: file, path: tmpPath}, nil
}

// updateRef points ref at sha. delRef removes ref and ignores a missing ref so
// it can clean up unconditionally.
func updateRef(ctx context.Context, runner process.Runner, repoRoot, ref, sha string) error {
	res, err := runner.Run(ctx, process.Command{
		Executable: "git",
		Args:       []string{"-C", repoRoot, "update-ref", ref, sha},
	})
	if err != nil {
		return fmt.Errorf("update-ref %s: %w", ref, err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("update-ref %s exited %d", ref, res.ExitCode)
	}
	return nil
}

func delRef(ctx context.Context, runner process.Runner, repoRoot, ref string) {
	_, _ = runner.Run(ctx, process.Command{
		Executable: "git",
		Args:       []string{"-C", repoRoot, "update-ref", "-d", ref},
	})
}

// bundleReader streams a bundle file and removes it when closed.
type bundleReader struct {
	*os.File
	path string
}

func (b *bundleReader) Close() error {
	if err := b.File.Close(); err != nil {
		_ = os.Remove(b.path)
		return err
	}
	return os.Remove(b.path)
}
