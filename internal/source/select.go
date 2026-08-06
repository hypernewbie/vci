package source

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hypernewbie/vci/internal/process"
)

// SourceInput holds the selected finite build input for transfer.
type SourceInput struct {
	Root        string   // Absolute path to local repository root
	ProjectName string   // Project basename
	Files       []string // Relative paths of selected files and minimal git markers
}

// SelectBuildInput inspects the local working tree at sourcePath and returns a typed
// SourceInput representing direct-SSH client build inputs for remote transmission.
//
// Selection rules for direct-SSH client build inputs (read during archive streaming):
//  1. Tracked Files (HEAD, modified, staged): Included using current working tree bytes and mode bits.
//  2. Locally Deleted Tracked Files: Excluded (absent from staged input).
//  3. Untracked Non-Ignored Files: Included using current working tree bytes and mode bits.
//  4. Ignored Files and Directories (.gitignore): Excluded from transfer.
//  5. Minimal Git Markers (.git/HEAD, .git/objects, .git/refs): Included so remote `vci build .`
//     can discover the repository root and project name via `source.Discover`. Private `.git/config`
//     and object pack history are excluded.
//  6. Unsupported Source Forms: Linked Git worktrees (.git file) or filenames containing newlines
//     are rejected locally before network transmission with a deterministic infrastructure error.
//
// Any read or archive failure during selection/streaming aborts client submission before returning a remote run ID.
// Local coordinator builds retain their existing source-manifest behavior.
func SelectBuildInput(ctx context.Context, sourcePath string, runner process.Runner) (SourceInput, error) {
	absPath, err := filepath.Abs(sourcePath)
	if err == nil {
		gitPath := filepath.Join(absPath, ".git")
		if fi, lerr := os.Lstat(gitPath); lerr == nil && !fi.IsDir() {
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
	if !fi.IsDir() {
		return SourceInput{}, fmt.Errorf("unsupported source: linked Git worktrees (.git file) are not supported")
	}

	var stdout, stderr bytes.Buffer
	res, err := runner.Run(ctx, process.Command{
		Executable: "git",
		Args:       []string{"-C", repo.Root, "ls-files", "-z", "--cached", "--others", "--exclude-standard"},
		Stdout:     &stdout,
		Stderr:     &stderr,
	})
	if err != nil || res.ExitCode != 0 {
		return SourceInput{}, fmt.Errorf("git ls-files: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}

	rawPaths := strings.Split(stdout.String(), "\x00")
	var selected []string
	seen := make(map[string]bool)

	for _, p := range rawPaths {
		if p == "" {
			continue
		}
		if strings.Contains(p, "\n") {
			return SourceInput{}, fmt.Errorf("unsupported source: filename containing newline is not supported: %q", p)
		}
		full := filepath.Join(repo.Root, p)
		_, err := os.Lstat(full)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return SourceInput{}, fmt.Errorf("stat source file %s: %w", p, err)
		}
		if !seen[p] {
			seen[p] = true
			selected = append(selected, p)
		}
	}

	// Minimal repository markers for remote source.Discover (.git/HEAD, .git/objects, .git/refs)
	for _, gitMarker := range []string{".git/HEAD", ".git/objects", ".git/refs"} {
		full := filepath.Join(repo.Root, gitMarker)
		if _, err := os.Lstat(full); err == nil {
			if !seen[gitMarker] {
				seen[gitMarker] = true
				selected = append(selected, gitMarker)
			}
		}
	}

	return SourceInput{
		Root:        repo.Root,
		ProjectName: repo.Name,
		Files:       selected,
	}, nil
}
