package source

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hypernewbie/vci/internal/process"
)

// LocalChanges is a client's working-tree delta against HEAD: a binary-safe
// patch for tracked modifications, deletions, and type or mode changes, plus
// the content of untracked non-ignored files. A coordinator reconstructs HEAD,
// then applies these changes to reproduce the client's buildable state.
type LocalChanges struct {
	Patch     []byte
	Untracked []UntrackedFile
}

// UntrackedFile is one non-ignored file absent from HEAD. Content holds the
// bytes of a regular file; Link holds the target of a symlink.
type UntrackedFile struct {
	Path    string
	Mode    os.FileMode
	Symlink bool
	Link    string
	Content []byte
}

// CaptureLocalChanges records the working-tree delta against HEAD without
// mutating the repository. Tracked changes become a binary-safe, rename-free
// patch; untracked non-ignored files are read in full, sorted by path.
func CaptureLocalChanges(ctx context.Context, repoRoot string, runner process.Runner) (LocalChanges, error) {
	patch, err := runGitBytes(ctx, runner, repoRoot, "diff", "HEAD", "--binary", "--no-renames")
	if err != nil {
		return LocalChanges{}, err
	}
	paths, err := runGitOutput(ctx, runner, repoRoot, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return LocalChanges{}, err
	}
	var files []UntrackedFile
	for _, p := range strings.Split(paths, "\x00") {
		if p == "" {
			continue
		}
		f, err := captureFile(repoRoot, p)
		if err != nil {
			return LocalChanges{}, err
		}
		files = append(files, f)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return LocalChanges{Patch: patch, Untracked: files}, nil
}

func captureFile(repoRoot, rel string) (UntrackedFile, error) {
	full := filepath.Join(repoRoot, rel)
	fi, err := os.Lstat(full)
	if err != nil {
		return UntrackedFile{}, fmt.Errorf("stat %q: %w", rel, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(full)
		if err != nil {
			return UntrackedFile{}, fmt.Errorf("readlink %q: %w", rel, err)
		}
		return UntrackedFile{Path: rel, Mode: fi.Mode(), Symlink: true, Link: target}, nil
	}
	if !fi.Mode().IsRegular() {
		return UntrackedFile{}, fmt.Errorf("untracked %q is not a regular file or symlink", rel)
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return UntrackedFile{}, fmt.Errorf("read %q: %w", rel, err)
	}
	return UntrackedFile{Path: rel, Mode: fi.Mode(), Content: data}, nil
}

// ApplyLC reproduces a client's working-tree delta in workspace. It applies the
// tracked-change patch with git, then writes each untracked file with its
// recorded mode or symlink. workspace must be a clean checkout at the client's
// HEAD. Supplied paths must stay within workspace; anything absolute, escaping
// the tree, or targeting .git or .vci is rejected.
func ApplyLC(ctx context.Context, workspace string, lc LocalChanges, runner process.Runner) error {
	if len(lc.Patch) > 0 {
		res, err := runner.Run(ctx, process.Command{
			Executable: "git",
			Args:       []string{"-C", workspace, "apply", "--binary", "--whitespace=nowarn", "-"},
			Stdin:      bytes.NewReader(lc.Patch),
		})
		if err != nil {
			return fmt.Errorf("git apply: %w", err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("git apply exited %d", res.ExitCode)
		}
	}
	for _, f := range lc.Untracked {
		if err := writeUntracked(workspace, f); err != nil {
			return err
		}
	}
	return nil
}

func writeUntracked(workspace string, f UntrackedFile) error {
	rel := filepath.ToSlash(filepath.Clean(f.Path))
	switch {
	case rel == ".", rel == "..", strings.HasPrefix(rel, "../"),
		strings.HasPrefix(rel, "/"),
		rel == ".git", strings.HasPrefix(rel, ".git/"),
		rel == ".vci", strings.HasPrefix(rel, ".vci/"):
		return fmt.Errorf("unsafe untracked path %q", f.Path)
	}
	full := filepath.Join(workspace, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	if f.Symlink {
		_ = os.Remove(full)
		return os.Symlink(f.Link, full)
	}
	if err := os.WriteFile(full, f.Content, f.Mode.Perm()); err != nil {
		return err
	}
	return os.Chmod(full, f.Mode.Perm())
}

// runGitBytes runs git and returns its raw stdout bytes for binary output.
func runGitBytes(ctx context.Context, runner process.Runner, repoRoot string, args ...string) ([]byte, error) {
	var buf bytes.Buffer
	res, err := runner.Run(ctx, process.Command{
		Executable: "git",
		Args:       append([]string{"-C", repoRoot}, args...),
		Stdout:     &buf,
	})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("git %s exited %d", strings.Join(args, " "), res.ExitCode)
	}
	return buf.Bytes(), nil
}
