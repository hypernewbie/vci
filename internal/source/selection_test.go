package source

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypernewbie/vci/internal/process"
)

func TestSelectBuildInputContract(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "myproject")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	runGit(t, sourceDir, "init")

	// 1. Tracked file
	mustWrite(t, filepath.Join(sourceDir, "tracked.go"), "package main")
	// 2. Tracked file to be deleted locally
	mustWrite(t, filepath.Join(sourceDir, "deleted.go"), "package main")
	runGit(t, sourceDir, "add", ".")
	runGit(t, sourceDir, "commit", "-m", "init")

	// Delete deleted.go locally
	if err := os.Remove(filepath.Join(sourceDir, "deleted.go")); err != nil {
		t.Fatalf("remove deleted.go: %v", err)
	}

	// 3. Untracked file
	mustWrite(t, filepath.Join(sourceDir, "untracked.go"), "package main")

	// 4. Ignored file
	mustWrite(t, filepath.Join(sourceDir, ".gitignore"), "*.log\n")
	mustWrite(t, filepath.Join(sourceDir, "app.log"), "log data")

	// 5. Unique sentinel in .git/config
	mustWrite(t, filepath.Join(sourceDir, ".git", "config"), "[user]\n  name = PrivateSentinelUser\n")

	// Select build input
	input, err := SelectBuildInput(context.Background(), sourceDir, process.Native{})
	if err != nil {
		t.Fatalf("SelectBuildInput failed: %v", err)
	}

	if input.ProjectName != "myproject" {
		t.Fatalf("expected project name myproject, got %s", input.ProjectName)
	}

	set := make(map[string]bool)
	for _, f := range input.Files {
		set[f] = true
	}

	if !set["tracked.go"] {
		t.Errorf("expected tracked.go in input")
	}
	if !set["untracked.go"] {
		t.Errorf("expected untracked.go in input")
	}
	if set["deleted.go"] {
		t.Errorf("deleted.go MUST NOT be in input")
	}
	if set["app.log"] {
		t.Errorf("app.log MUST NOT be in input")
	}

	// Markers
	if !set[".git/HEAD"] {
		t.Errorf("expected .git/HEAD marker")
	}
	if !set[".git/objects"] {
		t.Errorf("expected .git/objects marker")
	}
	if set[".git/config"] {
		t.Errorf(".git/config MUST NOT be in input")
	}
}

func TestSelectBuildInputRejectsLinkedWorktree(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "worktree")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write a .git file (linked worktree)
	mustWrite(t, filepath.Join(sourceDir, ".git"), "gitdir: /some/path")

	_, err := SelectBuildInput(context.Background(), sourceDir, process.Native{})
	if err == nil {
		t.Fatalf("expected error for linked worktree (.git file), got nil")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestDiscoverCurrentRepository(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (process.Native{}).Run(context.Background(), process.Command{Executable: "git", Args: []string{"init", "-q", dir}}); err != nil {
		t.Fatal(err)
	}
	repo, err := Discover(context.Background(), dir, process.Native{})
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := filepath.EvalSymlinks(dir)
	if repo.Root != expected || repo.Name != filepath.Base(expected) {
		t.Fatalf("repo: %+v", repo)
	}
}

func TestMatchProjectIsCaseInsensitiveFallback(t *testing.T) {
	got, err := MatchProject("vci", []string{"Vci"})
	if err != nil || got != "Vci" {
		t.Fatalf("%q, %v", got, err)
	}
}

func TestDiscoverRejectsNonRepository(t *testing.T) {
	dir := t.TempDir()
	if _, err := Discover(context.Background(), dir, process.Native{}); err == nil {
		t.Fatal("non-repository accepted")
	}
}

// TestValidateInputRejectsParentTraversal pins that .. segments
// fail before any write outside the snapshot root. The validator is
// the single source of truth: a parent traversal path never reaches
// the snapshot, manifest, cache entry, or tar stream.
func TestValidateInputRejectsParentTraversal(t *testing.T) {
	root := t.TempDir()
	cases := []string{
		"../escape",
		"foo/../bar",
		"foo/..",
		"..",
	}
	for _, p := range cases {
		_, err := ValidateInput(SourceInput{
			Root:  root,
			Files: []string{p},
		})
		if err == nil {
			t.Fatalf("path %q must be rejected", p)
		}
	}
}

// TestValidateInputRejectsAbsolutePath pins that absolute paths
// fail before any write outside the snapshot root.
func TestValidateInputRejectsAbsolutePath(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(root, "foo")
	_, err := ValidateInput(SourceInput{
		Root:  root,
		Files: []string{abs},
	})
	if err == nil {
		t.Fatalf("absolute path %q must be rejected", abs)
	}
}

// TestValidateInputRejectsTrailingSlash pins that directory entries
// with a trailing slash are normalized to a single spelling.
func TestValidateInputRejectsTrailingSlash(t *testing.T) {
	root := t.TempDir()
	_, err := ValidateInput(SourceInput{
		Root:  root,
		Files: []string{"foo/"},
	})
	if err == nil {
		t.Fatal("trailing slash must be rejected")
	}
}

// TestValidateInputDeduplicatesAndSorts pins that the output is
// deduped and sorted, regardless of input order.
func TestValidateInputDeduplicatesAndSorts(t *testing.T) {
	root := t.TempDir()
	out, err := ValidateInput(SourceInput{
		Root:  root,
		Files: []string{"b", "a", "b", "c"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(out.Files, ",") != "a,b,c" {
		t.Fatalf("output: %v", out.Files)
	}
}

// TestValidateInputAcceptsDirectoryAndDescendants pins that a
// directory entry plus its descendants is the supported shape. The
// filesystem precludes a real file/directory collision on a single
// path: the resolved stat is the only ground truth, and the
// validator trusts it implicitly. The supported caller-facing
// shape is "directory + descendants" which is what recursive
// selection produces in production.
func TestValidateInputAcceptsDirectoryAndDescendants(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "foo", "bar"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "foo", "bar", "baz"), []byte("baz"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := ValidateInput(SourceInput{
		Root:  root,
		Files: []string{"foo", "foo/bar", "foo/bar/baz"},
	})
	if err != nil {
		t.Fatalf("directory + descendants is supported: %v", err)
	}
	if len(out.Files) != 3 {
		t.Fatalf("output: %v", out.Files)
	}
}

// TestValidateInputRejectsNewlineEntry pins that newline-containing
// filenames never reach the snapshot.
func TestValidateInputRejectsNewlineEntry(t *testing.T) {
	root := t.TempDir()
	_, err := ValidateInput(SourceInput{
		Root:  root,
		Files: []string{"foo\nbar"},
	})
	if err == nil {
		t.Fatal("newline in entry must be rejected")
	}
}

// TestCanonicalEntryRejectsEmptySegment pins that a path with an
// empty segment (e.g. "foo//bar") is rejected.
func TestCanonicalEntryRejectsEmptySegment(t *testing.T) {
	_, err := canonicalEntry("/tmp", "foo//bar")
	if err == nil {
		t.Fatal("empty segment must be rejected")
	}
}

// TestCanonicalEntryAcceptsTopLevelFile pins that a top-level file
// is accepted.
func TestCanonicalEntryAcceptsTopLevelFile(t *testing.T) {
	out, err := canonicalEntry("/tmp", "README.md")
	if err != nil {
		t.Fatal(err)
	}
	if out != "README.md" {
		t.Fatalf("output: %q", out)
	}
}

// TestCanonicalEntryRejectsTrailingSlash pins that a trailing slash
// is rejected as a malformed entry.
func TestCanonicalEntryRejectsTrailingSlash(t *testing.T) {
	_, err := canonicalEntry("/tmp", "child/")
	if err == nil {
		t.Fatal("trailing slash must be rejected")
	}
	if !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("error: %v", err)
	}
}

// TestSelectBuildInputExcludesGitmodulesAtTopLevel pins that the
// top-level recursive selection refuses to include .gitmodules even
// when the file is committed. The submodule's gitlink reference is
// the only approved path-restoration signal.
func TestSelectBuildInputExcludesGitmodulesAtTopLevel(t *testing.T) {
	root := setupSingleRepo(t)
	mustWrite(t, filepath.Join(root, ".gitmodules"),
		"[submodule \"child\"]\n\tpath = child\n\turl = https://SENTINEL-EXAMPLE-CRED@example.com/sentinel.git\n")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# top\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commitAll(t, root, "gitmodules")
	input, err := SelectBuildInput(testContext(), root, processRunner())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range input.Files {
		if filepath.Base(f) == ".gitmodules" {
			t.Fatalf("top-level .gitmodules must not be selected: %q", f)
		}
		if strings.Contains(f, "example.com") || strings.Contains(f, "SENTINEL-EXAMPLE-CRED") {
			t.Fatalf("sentinel URL leaked into %q", f)
		}
	}
}

// TestSelectBuildInputExcludesGitmodulesInSubmodule pins that a
// .gitmodules inside an initialized submodule is also excluded. The
// selector's recursion is the unit under test.
func TestSelectBuildInputExcludesGitmodulesInSubmodule(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "parent")
	childSource := filepath.Join(dir, "child_source")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(childSource, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, parent)
	initGitRepo(t, childSource)
	mustWrite(t, filepath.Join(childSource, "README.md"), "# child\n")
	commitAll(t, childSource, "child")
	mustWrite(t, filepath.Join(childSource, ".gitmodules"),
		"[submodule \"nested\"]\n\tpath = nested\n\turl = https://SENTINEL-CHILD-CRED@example.com/sentinel.git\n")
	commitAll(t, childSource, "gitmodules")
	addSubmoduleLocalFileTransport(t, parent, childSource, "child")
	commitAll(t, parent, "add child")
	input, err := SelectBuildInput(testContext(), parent, processRunner())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range input.Files {
		if filepath.Base(f) == ".gitmodules" {
			t.Fatalf("nested .gitmodules must not be selected: %q", f)
		}
		if strings.Contains(f, "SENTINEL-CHILD-CRED") {
			t.Fatalf("sentinel URL leaked into %q", f)
		}
	}
}

// TestSelectBuildInputSentinelNotVisibleInManifest pins that the
// local manifest also excludes any .gitmodules content. The
// sentinel URL/token must never enter a manifest entry.
func TestSelectBuildInputSentinelNotVisibleInManifest(t *testing.T) {
	root := setupSingleRepo(t)
	mustWrite(t, filepath.Join(root, ".gitmodules"),
		"[submodule \"x\"]\n\tpath = x\n\turl = https://SENTINEL-MANIFEST@example.com/x.git\n")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# top\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	commitAll(t, root, "gitmodules")
	manifest, blobs, err := Build(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range manifest.Entries {
		if filepath.Base(e.Path) == ".gitmodules" {
			t.Fatalf("local manifest must not include .gitmodules: %q", e.Path)
		}
		if strings.Contains(e.Path, "SENTINEL-MANIFEST") {
			t.Fatalf("sentinel URL leaked into manifest: %q", e.Path)
		}
	}
	for _, data := range blobs {
		if strings.Contains(string(data), "SENTINEL-MANIFEST") {
			t.Fatalf("sentinel URL leaked into a blob")
		}
	}
}

// setupSingleRepo is a small helper that creates one empty repo
// with a single commit. Tests that exercise .gitmodules and similar
// top-level files use it to avoid re-implementing the init dance.
func setupSingleRepo(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, root)
	return root
}

func testContext() context.Context  { return context.Background() }
func processRunner() process.Runner { return process.Native{} }
