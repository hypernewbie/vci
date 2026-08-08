package source

import (
	"context"
	"fmt"
	"github.com/hypernewbie/vci/internal/process"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SelectBuildInput validates source and returns SourceInput for direct SSH build.
// It uses git-tracked, unignored, non-symlink workspace files.
// It rejects linked worktrees, escaped paths, bad gitlinks, and bad names.
// Any snapshot/archive error aborts before run ID generation.
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

type Repository struct {
	Root string `json:"root"`
	Name string `json:"name"`
}

func Discover(ctx context.Context, path string, runner process.Runner) (Repository, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return Repository{}, fmt.Errorf("resolve source path: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return Repository{}, fmt.Errorf("inspect source path: %w", err)
	}
	if !info.IsDir() {
		absolute = filepath.Dir(absolute)
	}
	var out strings.Builder
	result, runErr := runner.Run(ctx, process.Command{Executable: "git", Args: []string{"-C", absolute, "rev-parse", "--show-toplevel"}, Stdout: &out})
	if runErr != nil || result.ExitCode != 0 {
		return Repository{}, fmt.Errorf("source is not a Git repository: %w", runErr)
	}
	root := strings.TrimSpace(out.String())
	if root == "" {
		return Repository{}, fmt.Errorf("git returned an empty source root")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return Repository{}, fmt.Errorf("resolve Git root: %w", err)
	}
	name := filepath.Base(root)
	return Repository{Root: root, Name: name}, nil
}

func MatchProject(repositoryName string, configured []string) (string, error) {
	for _, name := range configured {
		if name == repositoryName {
			return name, nil
		}
	}
	for _, name := range configured {
		if strings.EqualFold(name, repositoryName) {
			return name, nil
		}
	}
	return "", fmt.Errorf("project %q is not configured", repositoryName)
}

// ValidateInput keeps only safe relative paths and deduplicates them.
// It validates every entry before it reaches snapshot, manifest,
// cache, or tar.
// Any invalid path is rejected.
func ValidateInput(input SourceInput) (SourceInput, error) {
	seen := map[string]bool{}
	cleaned := make([]string, 0, len(input.Files))
	for _, p := range input.Files {
		clean, err := canonicalEntry(input.Root, p)
		if err != nil {
			return SourceInput{}, err
		}
		if seen[clean] {
			continue
		}
		seen[clean] = true
		cleaned = append(cleaned, clean)
	}
	sort.Strings(cleaned)
	// Reject a file entry that collides with a directory entry on
	// the same on-disk path. A directory entry plus its descendants
	// is supported because the directory legitimately exists on
	// disk and the descendants are files inside it.
	for _, p := range cleaned {
		for _, other := range cleaned {
			if other == p {
				continue
			}
			if isStrictAncestor(p, other) || isStrictAncestor(other, p) {
				if err := fileVsDirCollision(input.Root, p, other); err != nil {
					return SourceInput{}, err
				}
			}
		}
	}
	// Rebuild the per-repository decomposition from the validated,
	// sorted top-relative entries.
	cleanedRepos := make([]InputRepository, len(input.Repositories))
	for i, r := range input.Repositories {
		prefix := r.Prefix
		if prefix != "" && !strings.HasSuffix(prefix, "/") {
			prefix = prefix + "/"
		}
		repoFiles := make([]string, 0, len(r.Files))
		repoSeen := map[string]bool{}
		for _, f := range r.Files {
			clean, err := canonicalEntry(input.Root, f)
			if err != nil {
				return SourceInput{}, err
			}
			if prefix != "" && !strings.HasPrefix(clean, prefix) {
				continue
			}
			if repoSeen[clean] {
				continue
			}
			repoSeen[clean] = true
			repoFiles = append(repoFiles, clean)
		}
		sort.Strings(repoFiles)
		cleanedRepos[i] = InputRepository{Root: r.Root, Prefix: r.Prefix, Files: repoFiles}
	}
	cleanedLFS := map[string]bool{}
	for k := range input.LFSFiles {
		clean, err := canonicalEntry(input.Root, k)
		if err != nil {
			return SourceInput{}, err
		}
		cleanedLFS[clean] = true
	}
	return SourceInput{
		Root:         input.Root,
		ProjectName:  input.ProjectName,
		Files:        cleaned,
		Repositories: cleanedRepos,
		LFSFiles:     cleanedLFS,
	}, nil
}

// canonicalEntry returns the cleaned top-relative path for an
// entry. It rejects empty, absolute, escaped, dotted, newline-bearing,
// or trailing-slash forms. The raw path is inspected for ".."
// segments before filepath.Clean collapses them, so a caller cannot
// smuggle a parent traversal past the validator.
func canonicalEntry(root, p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("source: empty entry rejected")
	}
	if strings.Contains(p, "\n") {
		return "", fmt.Errorf("source: filename containing newline is not supported: %q", p)
	}
	if strings.HasPrefix(p, "/") || filepath.IsAbs(p) {
		return "", fmt.Errorf("source: absolute path rejected: %q", p)
	}
	if strings.HasSuffix(p, "/") {
		return "", fmt.Errorf("source: trailing-slash entry rejected: %q", p)
	}
	// Reject any ".." segment up-front. filepath.Clean collapses
	// these, so the cleaned version would silently hide the
	// traversal.
	for _, segment := range strings.Split(p, "/") {
		if segment == ".." {
			return "", fmt.Errorf("source: relative-segment entry rejected: %q", p)
		}
		if segment == "" {
			return "", fmt.Errorf("source: empty path segment in %q", p)
		}
	}
	clean := filepath.ToSlash(filepath.Clean(p))
	if clean == "." || clean == ".." {
		return "", fmt.Errorf("source: relative-segment entry rejected: %q", p)
	}
	if strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") || strings.HasSuffix(clean, "/..") {
		return "", fmt.Errorf("source: path escapes input root: %q", p)
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	fullAbs, err := filepath.Abs(filepath.Join(rootAbs, filepath.FromSlash(clean)))
	if err != nil {
		return "", err
	}
	rootAbs = filepath.Clean(rootAbs)
	fullAbs = filepath.Clean(fullAbs)
	if fullAbs != rootAbs && !strings.HasPrefix(fullAbs, rootAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("source: path escapes root: %q", p)
	}
	return clean, nil
}

// isStrictAncestor reports whether a is a strict directory ancestor
// of b (a is a prefix of b joined by a slash separator).
func isStrictAncestor(a, b string) bool {
	if a == b {
		return false
	}
	return strings.HasPrefix(b, a+"/")
}

// fileVsDirCollision returns a descriptive error when one entry is
// a regular file and the other is a directory on disk under the
// configured root, and the file entry is a strict descendant of the
// directory. The inverse case (a directory entry under a file
// entry) and the directory+descendant case are both supported.
func fileVsDirCollision(root, a, b string) error {
	aInfo, errA := os.Lstat(filepath.Join(root, filepath.FromSlash(a)))
	bInfo, errB := os.Lstat(filepath.Join(root, filepath.FromSlash(b)))
	if errA != nil || errB != nil {
		return nil
	}
	aIsDir := aInfo.IsDir()
	bIsDir := bInfo.IsDir()
	if aIsDir == bIsDir {
		return nil
	}
	// A file entry that is a strict descendant of a directory is
	// allowed (foo is a directory, foo/bar is a file inside it).
	// Any other mismatch is a real collision.
	if aIsDir && isStrictAncestor(a, b) {
		return nil
	}
	if bIsDir && isStrictAncestor(b, a) {
		return nil
	}
	return fmt.Errorf("source: %q (%s) conflicts with %q (%s)", a, kindLabel(aIsDir), b, kindLabel(bIsDir))
}

func kindLabel(isDir bool) string {
	if isDir {
		return "directory"
	}
	return "file"
}
