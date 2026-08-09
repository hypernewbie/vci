package source

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/hypernewbie/vci/internal/process"
)

// CopyWorkspace recursively copies src into dst, preserving file modes and
// recreating symlinks without following them. skip is called on each entry's
// base name; returning true drops a file or prunes a directory. A build
// workspace never wants a .vci, and a workspace that will undergo no Git
// operations should also skip .git to avoid copying history.
func CopyWorkspace(src, dst string, skip func(name string) bool) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if skip != nil && skip(d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		mode := info.Mode()
		switch {
		case mode&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case mode.IsDir():
			return os.MkdirAll(target, mode.Perm())
		case mode.IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if err := os.WriteFile(target, data, mode.Perm()); err != nil {
				return err
			}
			return os.Chmod(target, mode.Perm())
		default:
			return fmt.Errorf("copy %q: unsupported file type", rel)
		}
	})
}

// AdvanceToHead moves a clean Git workspace that already has head's base commit
// up to head by importing the staged bundle and checking head out. The
// workspace working tree and index must be clean so the checkout can advance
// them.
func AdvanceToHead(ctx context.Context, workspace, head string, bundle io.Reader, runner process.Runner) error {
	tmp, err := os.CreateTemp("", "vci-recv-*.bundle")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := io.Copy(tmp, bundle); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("stage bundle: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close staged bundle: %w", err)
	}
	defer os.Remove(tmpPath)
	if _, err := runGitOutput(ctx, runner, workspace, "bundle", "unbundle", tmpPath); err != nil {
		return fmt.Errorf("git bundle unbundle: %w", err)
	}
	if _, err := runGitOutput(ctx, runner, workspace, "checkout", "-q", head); err != nil {
		return fmt.Errorf("git checkout %s: %w", head, err)
	}
	return nil
}

// ReconstructWorkspace builds a buildable workspace for a run. With a seed it
// copies the seed read-only and advances the copy to the client head through the
// bundle; without a seed it initializes an empty repository and imports the
// bundle, which must then cover the full reachable history. It then applies the
// client local changes and prunes coordinator-owned exclusions. A seed is never
// mutated. bundle is nil only for a seeded submission that needs no Git advance.
func ReconstructWorkspace(ctx context.Context, seedPath, workspace, head string, bundle io.Reader, lc LocalChanges, exclusions []string, runner process.Runner) error {
	if seedPath == "" {
		if bundle == nil {
			return fmt.Errorf("cannot reconstruct without a seed or a bundle")
		}
		if err := os.MkdirAll(workspace, 0o755); err != nil {
			return err
		}
		if _, err := runGitOutput(ctx, runner, workspace, "init", "-q"); err != nil {
			return fmt.Errorf("init workspace: %w", err)
		}
	} else {
		skipVCI := func(name string) bool { return name == ".vci" }
		if err := CopyWorkspace(seedPath, workspace, skipVCI); err != nil {
			return fmt.Errorf("copy seed: %w", err)
		}
	}
	if bundle != nil {
		if err := AdvanceToHead(ctx, workspace, head, bundle, runner); err != nil {
			return err
		}
	}
	if err := ApplyLC(ctx, workspace, lc, runner); err != nil {
		return err
	}
	return ApplyExclusions(workspace, exclusions)
}
