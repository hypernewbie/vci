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

// ErrSubmoduleUnavailable is the typed sentinel for an
// uninitialized, missing, symlinked, escaping, conflicted, or
// otherwise unverifiable gitlink. The error message names the
// top-root-relative path so the agent can run the ordinary user
// action `git submodule update --init --recursive`.
var ErrSubmoduleUnavailable = errors.New("source: submodule unavailable")

// ErrLFSContentUnavailable is the typed sentinel for a Git LFS
// pointer file whose bytes are present in the working tree but not
// hydrated. The error message names the top-root-relative path so
// the agent can run the ordinary user action `git lfs pull`.
var ErrLFSContentUnavailable = errors.New("source: lfs content unavailable")

// InputRepository is one verified repository's contribution to a
// SourceInput. The prefix is "" at the top and "child/sub/" for a
// nested submodule. Files is repository-relative selected paths.
type InputRepository struct {
	Root   string
	Prefix string
	Files  []string
}

// SourceInput holds the validated finite build input for transfer.
// The Files list is the top-relative flattened path list (regular
// files, directories, symlinks, and the top-level minimal .git
// markers). The Repositories list preserves the per-repository
// decomposition for callers that need it. LFSFiles is the
// top-relative set of regular files Git attributes as filter=lfs in
// any verified repository.
type SourceInput struct {
	Root         string
	ProjectName  string
	Files        []string
	Repositories []InputRepository
	LFSFiles     map[string]bool
}

// buildGraph is the recursive source-owned input graph builder. It
// produces a SourceInput by walking the top-level repository, every
// initialized submodule, and every nested initialized submodule.
// Every verified repository contributes its own selected files under
// its own prefix. Uninitialized, missing, symlinked, escaping, or
// conflicted gitlinks fail with ErrSubmoduleUnavailable before any
// snapshot is taken.
//
// The result is run through ValidateInput before being returned so
// that every entry reaching the snapshot, manifest, cache entry, or
// tar stream is a single source of truth validated path.
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

// collectRepository selects every finite working-tree entry from
// one verified repository, recurses into each initialized gitlink,
// and appends the per-repository decomposition to repos. The
// returned files list is the union of prefixed paths from this
// repository and every nested submodule. The dir entries for the
// repository root and every submodule path are always present so an
// empty submodule still creates its directory in the snapshot.
func collectRepository(ctx context.Context, runner process.Runner, repo InputRepository, prefix string, files *[]string, repos *[]InputRepository, seen map[string]bool) error {
	selected, err := selectRepositoryFiles(ctx, repo.Root, runner, prefix == "")
	if err != nil {
		return err
	}
	// The directory entry is the prefix without a trailing slash.
	// The recursive caller passes a prefix that ends in "/" so it
	// can join cleanly with descendant entries; we strip the
	// trailing slash here so the canonical validator (which
	// rejects trailing slashes) accepts the entry.
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
		// descendantPrefix joins the parent prefix with the child
		// path and ends in "/" so descendant entries compose
		// cleanly. The directory entry itself is recorded without
		// the trailing slash.
		descendantPrefix := prefix + gl + "/"
		childPrefix := strings.TrimSuffix(descendantPrefix, "/")
		if err := collectRepository(ctx, runner, InputRepository{Root: childRepoRoot, Prefix: childPrefix}, descendantPrefix, files, repos, seen); err != nil {
			return err
		}
	}
	return nil
}

// selectRepositoryFiles runs git ls-files with the documented flags
// and returns the validated relative paths. Newline-containing names
// are rejected. Locally-deleted tracked files are dropped. The
// top-level .git must be a directory; submodule children may use
// either form (file or directory) and their verified top-level is
// asserted by verifyChildTopLevel.
//
// .gitmodules is excluded at every depth: it can contain remote URLs
// and embedded credentials, which Plan 11 forbids from any source
// path. The submodule's gitlink reference is sufficient to know the
// submodule exists; the agent does not need checkout URLs at build
// time.
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
		// .gitmodules is excluded at every depth. The selection
		// shape is a flat top-relative entry; submodule prefixes
		// are applied at the parent call site, so a literal
		// basename match is sufficient.
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

// listGitlinks parses `git ls-files -z --stage` and returns the
// mode-160000 (gitlink) entries. These are the verified submodule
// paths registered in the parent index. Other entries are ignored
// here; the per-repository file selection already covered them.
//
// Stage records are strictly parsed: a malformed record, a non-zero
// stage (unmerged), or a duplicate gitlink path is an error wrapped
// in ErrSubmoduleUnavailable rather than a silent skip. Vci does not
// recurse twice into the same submodule and tolerates a stage
// conflict by failing closed.
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

// verifyChildTopLevel returns the git-reported top-level of
// childRoot, asserting it equals the expected childRoot itself.
// A linked-worktree resolution that escapes or a mis-anchored
// submodule that resolves to the parent both fail this check.
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

// validateRepoRelativePath is the central safety gate. The path is
// rejected if it contains a newline, an absolute component, a ".."
// segment, or a path that is not local/contained under its prefix.
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

// prefixIsDirectory reports whether the path is a directory (not a
// symlink, not a missing path, not a regular file).
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

// topRelative converts a flat top-relative path back to its
// top-root-relative form for error messages. The input is already
// top-relative; this is just a label helper.
func topRelative(p string) string { return p }

// lfsPointer is the formal shape of a Git LFS pointer file. A real
// hydrated LFS object is ordinary bytes; only the pointer form is
// rejected by ValidateLFSPointers.
const lfsPointerVersion = "https://git-lfs.github.com/spec/v1"

// isLFSPointer reports whether body is the exact textual form of a
// Git LFS pointer. The check is strict on version line, sha256 OID
// shape, and decimal size; any deviation is treated as ordinary
// working-tree content.
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

// collectLFSAttributes runs git check-attr -z --stdin filter on the
// selected paths in each verified repository. It returns a flat
// top-relative-path set of files that Git attributes as filter=lfs.
// Submodule attribution is computed inside each child repository
// using its own .gitattributes; the result is then re-keyed to the
// top-relative path under the parent's prefix.
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
		// Records are triples: path\0attr\0value\0path\0attr\0value...
		// The trailing partial triple (after the final NUL) is
		// dropped by Split, which is correct.
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

// ValidateLFSPointers walks the settled snapshot root and rejects
// every LFS-attributed regular file whose bytes are a formal
// pointer. The snapshot is the Vci-owned materialized copy at
// destParent/<snapPrefix><rand>; the top-relative keys in
// attributed match its on-disk layout. A pointer fails with a typed
// ErrLFSContentUnavailable naming the top-relative path so the
// agent can run `git lfs pull`. Attribute semantics, not magic
// content alone, decide rejection: a non-LFS file with pointer-
// looking bytes is ordinary source data.
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
