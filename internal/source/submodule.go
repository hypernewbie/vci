package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hypernewbie/vci/internal/process"
)

// ErrSubmoduleUnavailable is the typed error for any invalid or
// unverified submodule path. Messages include the top-root-relative
// path so callers can run `git submodule update --init --recursive`.
var ErrSubmoduleUnavailable = errors.New("source: submodule unavailable")

// ErrLFSContentUnavailable is the typed error for Git LFS pointer
// files still present as text in the workspace. Messages include the
// top-root-relative path so callers can run `git lfs pull`.
var ErrLFSContentUnavailable = errors.New("source: lfs content unavailable")

// InputRepository is one verified repository's contribution to SourceInput.
// Prefix is "" at the top and "child/sub/" for nested submodules.
// Files are repository-relative selected paths.
type InputRepository struct {
	Root   string
	Prefix string
	Files  []string
}

// SourceInput is the validated build input set.
// Files is the flattened top-relative entry list (files, directories,
// symlinks, top-level minimal .git markers). Repositories keeps
// per-repository decomposition for callers that need it. LFSFiles marks
// top-relative files attributed as filter=lfs in any verified repository.
type SourceInput struct {
	Root         string
	ProjectName  string
	Files        []string
	Repositories []InputRepository
	LFSFiles     map[string]bool
}

// buildGraph gathers the top repo and initialized submodules,
// returning SourceInput grouped by repository prefix. Any invalid
// gitlink returns ErrSubmoduleUnavailable.
func buildGraph(ctx context.Context, topRoot string, runner process.Runner) (SourceInput, error) {
	seen := map[string]bool{}
	files := []string{}
	repos := []InputRepository{}
	topRepo := InputRepository{Root: topRoot, Prefix: ""}
	if err := collectRepository(ctx, runner, topRepo, topRepo.Prefix, &files, &repos, seen); err != nil {
		return SourceInput{}, err
	}
	for _, marker := range []string{".git/HEAD", ".git/objects", ".git/refs"} {
		full := filepath.Join(topRoot, marker)
		if _, err := os.Lstat(full); err == nil {
			if !seen[marker] {
				seen[marker] = true
				files = append(files, marker)
			}
		}
	}
	attributed, err := collectLFSAttributes(ctx, runner, repos)
	if err != nil {
		return SourceInput{}, err
	}
	return ValidateInput(SourceInput{
		Root:         topRoot,
		ProjectName:  filepath.Base(topRoot),
		Files:        files,
		Repositories: repos,
		LFSFiles:     attributed,
	})
}

// collectRepository collects entries for one verified repository and
// recurses into initialized gitlinks.
// The list includes repo-root and submodule directory entries.
func collectRepository(ctx context.Context, runner process.Runner, repo InputRepository, prefix string, files *[]string, repos *[]InputRepository, seen map[string]bool) error {
	selected, err := selectRepositoryFiles(ctx, repo.Root, runner, prefix == "")
	if err != nil {
		return err
	}
	// Strip trailing slash from prefix for the directory entry.
	// Recursive callers use a slash-terminated prefix for joins, but
	// canonical validation rejects trailing slashes.
	dirEntry := strings.TrimSuffix(prefix, "/")
	if dirEntry != "" && !seen[dirEntry] {
		seen[dirEntry] = true
		*files = append(*files, dirEntry)
	}
	for _, rel := range selected {
		full := prefix + rel
		if !validateRepoRelativePath(full) {
			return fmt.Errorf("unsupported source: path %q from %s", full, repo.Root)
		}
		if !seen[full] {
			seen[full] = true
			*files = append(*files, full)
		}
	}
	*repos = append(*repos, InputRepository{Root: repo.Root, Prefix: dirEntry, Files: selected})
	gitlinks, err := listGitlinks(ctx, repo.Root, runner)
	if err != nil {
		return err
	}
	for _, gl := range gitlinks {
		if !validateRepoRelativePath(gl) {
			return fmt.Errorf("unsupported source: submodule path %q from %s", gl, repo.Root)
		}
		childRoot := filepath.Join(repo.Root, gl)
		if !prefixIsDirectory(childRoot) {
			return fmt.Errorf("%w: %s is not a directory", ErrSubmoduleUnavailable, topRelative(prefix+gl))
		}
		childRepoRoot, err := verifyChildTopLevel(ctx, childRoot, runner)
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrSubmoduleUnavailable, topRelative(prefix+gl), err)
		}
		if childRepoRoot != childRoot {
			return fmt.Errorf("%w: %s resolves to %s, not the expected child directory", ErrSubmoduleUnavailable, topRelative(prefix+gl), childRepoRoot)
		}
		// descendantPrefix is parent prefix + child path + "/" for
		// clean joins. The directory entry is recorded without "/".
		descendantPrefix := prefix + gl + "/"
		childPrefix := strings.TrimSuffix(descendantPrefix, "/")
		if err := collectRepository(ctx, runner, InputRepository{Root: childRepoRoot, Prefix: childPrefix}, descendantPrefix, files, repos, seen); err != nil {
			return err
		}
	}
	return nil
}

// selectRepositoryFiles runs git ls-files and validates paths.
// It skips newline names and locally-deleted tracked files.
// Top-level .git must be a directory; submodule checks happen elsewhere.
// .gitmodules is excluded at all depths.
func selectRepositoryFiles(ctx context.Context, repoRoot string, runner process.Runner, topLevel bool) ([]string, error) {
	gitPath := filepath.Join(repoRoot, ".git")
	fi, err := os.Lstat(gitPath)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", gitPath, err)
	}
	if topLevel {
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return nil, fmt.Errorf("unsupported source: linked Git worktrees (.git file) are not supported")
		}
	}
	var stdout, stderr bytes.Buffer
	res, err := runner.Run(ctx, process.Command{
		Executable: "git",
		Args:       []string{"-C", repoRoot, "ls-files", "-z", "--cached", "--others", "--exclude-standard"},
		Stdout:     &stdout,
		Stderr:     &stderr,
	})
	if err != nil || res.ExitCode != 0 {
		return nil, fmt.Errorf("git ls-files: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	rawPaths := strings.Split(stdout.String(), "\x00")
	var selected []string
	seen := make(map[string]bool)
	for _, p := range rawPaths {
		if p == "" {
			continue
		}
		if strings.Contains(p, "\n") {
			return nil, fmt.Errorf("unsupported source: filename containing newline is not supported: %q", p)
		}
		// .gitmodules is excluded at every depth.
		// Path matching is by basename because parent callsites apply
		// submodule prefixes.
		if filepath.Base(p) == ".gitmodules" {
			continue
		}
		full := filepath.Join(repoRoot, p)
		if _, err := os.Lstat(full); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("stat source file %s: %w", p, err)
		}
		if !seen[p] {
			seen[p] = true
			selected = append(selected, p)
		}
	}
	return selected, nil
}

// listGitlinks parses `git ls-files -z --stage` and returns mode-160000
// gitlink paths from the index; other entries are ignored.
//
// Malformed records, non-zero stage, or duplicate paths are errors and
// fail with ErrSubmoduleUnavailable (no silent skips).
func listGitlinks(ctx context.Context, repoRoot string, runner process.Runner) ([]string, error) {
	var stdout, stderr bytes.Buffer
	res, err := runner.Run(ctx, process.Command{
		Executable: "git",
		Args:       []string{"-C", repoRoot, "ls-files", "-z", "--stage"},
		Stdout:     &stdout,
		Stderr:     &stderr,
	})
	if err != nil || res.ExitCode != 0 {
		return nil, fmt.Errorf("git ls-files --stage: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	var gitlinks []string
	seen := map[string]bool{}
	for _, record := range strings.Split(stdout.String(), "\x00") {
		if record == "" {
			continue
		}
		// Format: "<mode> <hash> <stage>\t<path>"
		tab := strings.IndexByte(record, '\t')
		if tab < 0 {
			return nil, fmt.Errorf("%w: malformed stage record (no tab): %q", ErrSubmoduleUnavailable, truncate(record, 64))
		}
		meta := record[:tab]
		path := record[tab+1:]
		fields := strings.SplitN(meta, " ", 3)
		if len(fields) != 3 {
			return nil, fmt.Errorf("%w: malformed stage record (need mode/hash/stage): %q", ErrSubmoduleUnavailable, truncate(meta, 64))
		}
		if fields[0] != "160000" {
			continue
		}
		if path == "" {
			return nil, fmt.Errorf("%w: empty gitlink path", ErrSubmoduleUnavailable)
		}
		if strings.Contains(path, "\n") {
			return nil, fmt.Errorf("%w: newline in gitlink path", ErrSubmoduleUnavailable)
		}
		// Stage must be 0: a non-zero stage is an unmerged gitlink.
		if fields[2] != "0" {
			return nil, fmt.Errorf("%w: unmerged gitlink %q (stage=%s)", ErrSubmoduleUnavailable, path, fields[2])
		}
		if seen[path] {
			return nil, fmt.Errorf("%w: duplicate gitlink %q", ErrSubmoduleUnavailable, path)
		}
		seen[path] = true
		gitlinks = append(gitlinks, path)
	}
	return gitlinks, nil
}

// truncate caps a stage record error preview to a small bound.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// verifyChildTopLevel resolves git's top-level directory for childRoot and
// verifies it matches childRoot itself, rejecting linked-worktree or
// mis-anchored escapes.
func verifyChildTopLevel(ctx context.Context, childRoot string, runner process.Runner) (string, error) {
	var out bytes.Buffer
	res, err := runner.Run(ctx, process.Command{
		Executable: "git",
		Args:       []string{"-C", childRoot, "rev-parse", "--show-toplevel"},
		Stdout:     &out,
	})
	if err != nil || res.ExitCode != 0 {
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	root := strings.TrimSpace(out.String())
	if root == "" {
		return "", fmt.Errorf("empty toplevel")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", err
	}
	childRoot, err = filepath.Abs(childRoot)
	if err != nil {
		return "", err
	}
	return root, nil
}

// validateRepoRelativePath is the safety gate for gitlink-relative paths.
// It rejects empty, newline-containing, absolute, and '.'/'..' segments.
func validateRepoRelativePath(p string) bool {
	if p == "" {
		return false
	}
	if strings.Contains(p, "\n") {
		return false
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") {
		return false
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// prefixIsDirectory checks that the path exists, is not a symlink,
// and is a directory.
func prefixIsDirectory(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return false
	}
	return fi.IsDir()
}

// topRelative is a no-op label helper for top-root-relative error
// messages.
func topRelative(p string) string { return p }

// lfsPointer is the required header format for a Git LFS pointer
// file; only this pointer form is rejected by ValidateLFSPointers.
const lfsPointerVersion = "https://git-lfs.github.com/spec/v1"

// isLFSPointer validates exact Git LFS pointer syntax:
// version, sha256 oid, and decimal size. Any deviation is treated as
// normal content.
func isLFSPointer(body []byte) bool {
	lines := strings.Split(string(body), "\n")
	if len(lines) < 3 {
		return false
	}
	if lines[0] != "version "+lfsPointerVersion {
		return false
	}
	oidLine := strings.SplitN(lines[1], " ", 2)
	if len(oidLine) != 2 || oidLine[0] != "oid" {
		return false
	}
	hex := strings.TrimPrefix(oidLine[1], "sha256:")
	if len(hex) != 64 {
		return false
	}
	for _, r := range hex {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	sizeLine := strings.SplitN(lines[2], " ", 2)
	if len(sizeLine) != 2 || sizeLine[0] != "size" || sizeLine[1] == "" {
		return false
	}
	for _, r := range sizeLine[1] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// collectLFSAttributes runs git check-attr on selected paths in each
// verified repository and returns top-relative files marked filter=lfs.
// Child repos are attributed locally, then re-keyed under the parent
// prefix.
func collectLFSAttributes(ctx context.Context, runner process.Runner, repos []InputRepository) (map[string]bool, error) {
	attributed := map[string]bool{}
	for _, repo := range repos {
		if len(repo.Files) == 0 {
			continue
		}
		var stdin bytes.Buffer
		for _, rel := range repo.Files {
			stdin.WriteString(rel)
			stdin.WriteByte(0)
		}
		var stdout, stderr bytes.Buffer
		res, err := runner.Run(ctx, process.Command{
			Executable: "git",
			Args:       []string{"-C", repo.Root, "check-attr", "-z", "--stdin", "filter"},
			Stdin:      &stdin,
			Stdout:     &stdout,
			Stderr:     &stderr,
		})
		if err != nil || res.ExitCode != 0 {
			return nil, fmt.Errorf("git check-attr: %w (%s)", err, strings.TrimSpace(stderr.String()))
		}
		records := strings.Split(stdout.String(), "\x00")
		// Output is path\0attr\0value... triples. Ignore any trailing
		// partial triple after Split.
		prefix := repo.Prefix
		if prefix != "" && !strings.HasSuffix(prefix, "/") {
			prefix = prefix + "/"
		}
		for i := 0; i+2 < len(records); i += 3 {
			path := records[i]
			attr := records[i+1]
			value := records[i+2]
			if path == "" || attr != "filter" {
				continue
			}
			if value == "lfs" {
				attributed[prefix+path] = true
			}
		}
	}
	return attributed, nil
}

// ValidateLFSPointers checks attributed regular files in a settled
// snapshot and fails if any content matches a formal LFS pointer.
// Only files marked filter=lfs are checked, and errors use
// ErrLFSContentUnavailable with the top-relative path. Non-LFS files
// with pointer-like bytes are ignored.
func ValidateLFSPointers(snapRoot string, attributed map[string]bool) error {
	if len(attributed) == 0 {
		return nil
	}
	for topRel := range attributed {
		full := filepath.Join(snapRoot, filepath.FromSlash(topRel))
		fi, err := os.Lstat(full)
		if err != nil {
			continue
		}
		if !fi.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return fmt.Errorf("read lfs-attributed file %s: %w", topRel, err)
		}
		if isLFSPointer(data) {
			return fmt.Errorf("%w: %s", ErrLFSContentUnavailable, topRel)
		}
	}
	return nil
}
