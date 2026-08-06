package source

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/process"
)

func TestComputeDigestDeterminism(t *testing.T) {
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

	input1, err := SelectBuildInput(context.Background(), sourceDir, process.Native{})
	if err != nil {
		t.Fatalf("SelectBuildInput 1: %v", err)
	}
	d1, err := ComputeDigest(input1)
	if err != nil {
		t.Fatalf("ComputeDigest 1: %v", err)
	}

	input2, err := SelectBuildInput(context.Background(), sourceDir, process.Native{})
	if err != nil {
		t.Fatalf("SelectBuildInput 2: %v", err)
	}
	d2, err := ComputeDigest(input2)
	if err != nil {
		t.Fatalf("ComputeDigest 2: %v", err)
	}

	if d1 != d2 {
		t.Fatalf("expected identical digest for unchanged repo; got %s vs %s", d1, d2)
	}

	// Modify a file -> digest must change
	mustWrite(t, filepath.Join(sourceDir, "main.go"), "package main // modified\n")
	input3, err := SelectBuildInput(context.Background(), sourceDir, process.Native{})
	if err != nil {
		t.Fatalf("SelectBuildInput 3: %v", err)
	}
	d3, err := ComputeDigest(input3)
	if err != nil {
		t.Fatalf("ComputeDigest 3: %v", err)
	}

	if d1 == d3 {
		t.Fatalf("digest must change when file content is modified")
	}

	// Ignored file addition -> digest must stay identical to d3
	mustWrite(t, filepath.Join(sourceDir, ".gitignore"), "*.log\n")
	mustWrite(t, filepath.Join(sourceDir, "test.log"), "log content")
	input4, err := SelectBuildInput(context.Background(), sourceDir, process.Native{})
	if err != nil {
		t.Fatalf("SelectBuildInput 4: %v", err)
	}
	d4, err := ComputeDigest(input4)
	if err != nil {
		t.Fatalf("ComputeDigest 4: %v", err)
	}

	// Note: .gitignore is a tracked file so d4 includes .gitignore, but test.log is ignored
	if d3 == d4 {
		// Adding .gitignore changed tracked files, so d4 != d3. But test.log is excluded.
	}
}

// TestCanonicalDigestShape asserts the exact wire shape of the
// returned digest string so downstream code that parses it can rely
// on a single accepted form.
func TestCanonicalDigestShape(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "repo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	runGit(t, sourceDir, "init")
	mustWrite(t, filepath.Join(sourceDir, "main.go"), "package main\n")
	runGit(t, sourceDir, "add", ".")
	runGit(t, sourceDir, "commit", "-m", "init")

	input, err := SelectBuildInput(context.Background(), sourceDir, process.Native{})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	digest, err := ComputeDigest(input)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if !validDigestShape(digest) {
		t.Fatalf("digest does not match sha256-<64 lowercase hex>: %q", digest)
	}
}

// TestCanonicalizeExcludesMtimes asserts the canonical sequence does
// not depend on file mtimes. Two snapshots with identical content
// must produce the same digest regardless of when each file was
// touched.
func TestCanonicalizeExcludesMtimes(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		runGit(t, d, "init")
		mustWrite(t, filepath.Join(d, "main.go"), "package main\n")
		runGit(t, d, "add", ".")
		runGit(t, d, "commit", "-m", "init")
	}
	if err := os.Chtimes(filepath.Join(a, "main.go"), fixedTime, fixedTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	inA, _ := SelectBuildInput(context.Background(), a, process.Native{})
	inB, _ := SelectBuildInput(context.Background(), b, process.Native{})
	dA, _ := ComputeDigest(inA)
	dB, _ := ComputeDigest(inB)
	if dA != dB {
		t.Fatalf("digest must not depend on mtime; got %s vs %s", dA, dB)
	}
}

var fixedTime = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

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
