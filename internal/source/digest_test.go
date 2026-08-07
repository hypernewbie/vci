package source

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/process"
)

var fixedTime = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

func snapshotForRepo(t *testing.T, repoDir string) string {
	t.Helper()
	input, err := SelectBuildInput(context.Background(), repoDir, process.Native{})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	snapshot, err := MaterializeSnapshot(input, t.TempDir())
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return snapshot
}

// TestSnapshotDigestDeterminism asserts the snapshot digest is
// stable for an unchanged input and changes when any selected file
// changes. The cache key cannot be reused for changed bytes.
func TestSnapshotDigestDeterminism(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "repo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	runGit(t, sourceDir, "init")
	mustWrite(t, filepath.Join(sourceDir, "main.go"), "package main\n")
	mustWrite(t, filepath.Join(sourceDir, "README.md"), "# repo\n")
	runGit(t, sourceDir, "add", ".")
	runGit(t, sourceDir, "commit", "-m", "init")

	d1, err := ComputeSnapshotDigest(snapshotForRepo(t, sourceDir))
	if err != nil {
		t.Fatalf("digest 1: %v", err)
	}
	d2, err := ComputeSnapshotDigest(snapshotForRepo(t, sourceDir))
	if err != nil {
		t.Fatalf("digest 2: %v", err)
	}
	if d1 != d2 {
		t.Fatalf("expected identical digest for unchanged repo; got %s vs %s", d1, d2)
	}

	mustWrite(t, filepath.Join(sourceDir, "main.go"), "package main // modified\n")
	d3, err := ComputeSnapshotDigest(snapshotForRepo(t, sourceDir))
	if err != nil {
		t.Fatalf("digest 3: %v", err)
	}
	if d1 == d3 {
		t.Fatalf("digest must change when file content is modified")
	}
}

// TestSnapshotDigestShape asserts the exact wire shape of the
// returned digest string so downstream code that parses it can rely
// on a single accepted form.
func TestSnapshotDigestShape(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "repo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	runGit(t, sourceDir, "init")
	mustWrite(t, filepath.Join(sourceDir, "main.go"), "package main\n")
	runGit(t, sourceDir, "add", ".")
	runGit(t, sourceDir, "commit", "-m", "init")

	digest, err := ComputeSnapshotDigest(snapshotForRepo(t, sourceDir))
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if !validDigestShape(digest) {
		t.Fatalf("digest does not match sha256-<64 lowercase hex>: %q", digest)
	}
}

// TestSnapshotDigestExcludesMtimes asserts the digest does not
// depend on file mtimes. Two snapshots of identical content produced
// at different times must produce the same digest.
func TestSnapshotDigestExcludesMtimes(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "repo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	runGit(t, sourceDir, "init")
	mustWrite(t, filepath.Join(sourceDir, "main.go"), "package main\n")
	runGit(t, sourceDir, "add", ".")
	runGit(t, sourceDir, "commit", "-m", "init")
	if err := os.Chtimes(filepath.Join(sourceDir, "main.go"), fixedTime, fixedTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	snapA := snapshotForRepo(t, sourceDir)
	dA, err := ComputeSnapshotDigest(snapA)
	if err != nil {
		t.Fatalf("digest a: %v", err)
	}

	// Rebuild the same content under a different mtime and confirm
	// the digest is stable. The two snapshots have identical bytes.
	sourceDir2 := filepath.Join(dir, "repo2")
	if err := os.MkdirAll(sourceDir2, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	runGit(t, sourceDir2, "init")
	mustWrite(t, filepath.Join(sourceDir2, "main.go"), "package main\n")
	runGit(t, sourceDir2, "add", ".")
	runGit(t, sourceDir2, "commit", "-m", "init")
	snapB := snapshotForRepo(t, sourceDir2)
	dB, err := ComputeSnapshotDigest(snapB)
	if err != nil {
		t.Fatalf("digest b: %v", err)
	}
	if dA != dB {
		t.Fatalf("digest must not depend on mtime; got %s vs %s", dA, dB)
	}
}

// TestSnapshotDigestIndependentOfSelectedMtimes proves the snapshot
// digest excludes mtimes on files inside the materialized snapshot
// itself, not only on the working tree before materialization. The
// previous test only set mtime on the working-tree file; this test
// sets distinct, non-default mtimes on the selected files of two
// otherwise-identical snapshots immediately before digesting and
// requires equal digests.
func TestSnapshotDigestIndependentOfSelectedMtimes(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "repo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	runGit(t, sourceDir, "init")
	mustWrite(t, filepath.Join(sourceDir, "main.go"), "package main\n")
	mustWrite(t, filepath.Join(sourceDir, "lib.go"), "package main\n")
	runGit(t, sourceDir, "add", ".")
	runGit(t, sourceDir, "commit", "-m", "init")

	snapA := snapshotForRepo(t, sourceDir)
	tA := time.Date(2021, 6, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(snapA, "main.go"), tA, tA); err != nil {
		t.Fatalf("chtimes snapA: %v", err)
	}
	if err := os.Chtimes(filepath.Join(snapA, "lib.go"), tA, tA); err != nil {
		t.Fatalf("chtimes snapA lib: %v", err)
	}
	dA, err := ComputeSnapshotDigest(snapA)
	if err != nil {
		t.Fatalf("digest a: %v", err)
	}

	snapB := snapshotForRepo(t, sourceDir)
	tB := time.Date(2024, 11, 30, 23, 59, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(snapB, "main.go"), tB, tB); err != nil {
		t.Fatalf("chtimes snapB: %v", err)
	}
	if err := os.Chtimes(filepath.Join(snapB, "lib.go"), tB, tB); err != nil {
		t.Fatalf("chtimes snapB lib: %v", err)
	}
	dB, err := ComputeSnapshotDigest(snapB)
	if err != nil {
		t.Fatalf("digest b: %v", err)
	}
	if dA != dB {
		t.Fatalf("digest must not depend on snapshot file mtimes; got %s vs %s", dA, dB)
	}
}

// TestVerifySnapshotMatchesExpectedDigest proves the post-archive
// verifier rejects a snapshot whose bytes differ from the digest
// expectation.
func TestVerifySnapshotMatchesExpectedDigest(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"README.md", "lib.go"} {
		mustWrite(t, filepath.Join(dir, name), name+"\n")
	}
	digest, err := ComputeSnapshotDigest(dir)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if err := VerifySnapshot(dir, digest); err != nil {
		t.Fatalf("snapshot verify: %v", err)
	}
	// Modify one file after the digest was computed; verify must
	// reject.
	mustWrite(t, filepath.Join(dir, "lib.go"), "tampered\n")
	if err := VerifySnapshot(dir, digest); err == nil {
		t.Fatalf("snapshot verify must fail after modification")
	}
}

func validDigestShape(s string) bool {
	if len(s) != len("sha256-")+64 {
		return false
	}
	if s[:len("sha256-")] != "sha256-" {
		return false
	}
	for _, r := range s[len("sha256-"):] {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
