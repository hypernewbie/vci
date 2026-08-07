package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypernewbie/vci/internal/process"
)

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

func testContext() context.Context { return context.Background() }
func processRunner() process.Runner { return process.Native{} }
