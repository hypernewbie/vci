package source

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hypernewbie/vci/internal/process"
)

// TestMaterializeSnapshotKeepsMinimalGitMarkers pins the snapshot
// contract from Plan 7 / Plan 11: the three documented top-level
// minimal git markers (.git/HEAD, .git/objects, .git/refs) must
// physically exist in the materialized snapshot so the remote
// `source.Discover` can resolve the repository root. The client tar
// producer takes its file list straight from the snapshot, so dropping
// these markers causes the remote-public `vci build .` to fail with
// `source is not a Git repository`.
//
// Nested `.git` components at any depth, `.gitmodules` at any depth,
// and arbitrary `.git` content (config, hooks, logs, packed-refs,
// objects/pack) must remain excluded — only the three minimal markers
// survive. This is the focused Plan 12 Fix regression test.
func TestMaterializeSnapshotKeepsMinimalGitMarkers(t *testing.T) {
	repo := t.TempDir()
	initGitRepo(t, repo)
	// Add a nested .git/objects/pack file that must NOT be carried.
	nestedPack := filepath.Join(repo, ".git", "objects", "pack", "deadbeef")
	if err := os.MkdirAll(filepath.Dir(nestedPack), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nestedPack, []byte("private-pack"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Add a .gitmodules file that must NOT be carried.
	if err := os.WriteFile(filepath.Join(repo, ".gitmodules"), []byte("[submodule]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	input, err := SelectBuildInput(t.Context(), repo, process.Native{})
	if err != nil {
		t.Fatal(err)
	}
	destParent := t.TempDir()
	snapRoot, err := MaterializeSnapshot(input, destParent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(snapRoot) })

	// The three minimal markers must physically exist.
	for _, marker := range []string{".git/HEAD", ".git/objects", ".git/refs"} {
		_, err := os.Lstat(filepath.Join(snapRoot, marker))
		if err != nil {
			t.Errorf("marker %s missing in snapshot: %v", marker, err)
		}
	}
	// .gitmodules and .git/objects/pack must NOT appear.
	for _, banned := range []string{".gitmodules", ".git/objects/pack"} {
		if _, err := os.Lstat(filepath.Join(snapRoot, banned)); err == nil {
			t.Errorf("%s leaked into snapshot", banned)
		}
	}
}
