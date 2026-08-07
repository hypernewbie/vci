package source

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hypernewbie/vci/internal/process"
)

// TestBuildWithValidationRejectsLfsPointer pins that the local
// manifest construction rejects a formal LFS pointer before any
// blob is published. The validator checks the actual bytes that
// become a blob, so a post-validation change is a source-state
// error.
func TestBuildWithValidationRejectsLfsPointer(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "lfs")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, repo)
	mustWrite(t, filepath.Join(repo, ".gitattributes"), "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	const lfsPointer = "version https://git-lfs.github.com/spec/v1\noid sha256:4444444444444444444444444444444444444444444444444444444444444444\nsize 12\n"
	mustWrite(t, filepath.Join(repo, "data.bin"), lfsPointer)
	commitAll(t, repo, "lfs pointer")
	// BuildWithValidation observes the LFS pointer and rejects it.
	_, _, err := BuildWithValidation(repo, map[string]bool{"data.bin": true})
	if err == nil {
		t.Fatal("LFS pointer must fail BuildWithValidation")
	}
	if !errors.Is(err, ErrLFSContentUnavailable) {
		t.Fatalf("want ErrLFSContentUnavailable, got %v", err)
	}
}

// TestBuildWithValidationAcceptsHydratedBytes pins that genuinely
// hydrated LFS-attributed bytes are accepted as ordinary data.
func TestBuildWithValidationAcceptsHydratedBytes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "lfs")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, repo)
	mustWrite(t, filepath.Join(repo, ".gitattributes"), "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	mustWrite(t, filepath.Join(repo, "data.bin"), "actual hydrated bytes\n")
	commitAll(t, repo, "hydrated")
	manifest, blobs, err := BuildWithValidation(repo, map[string]bool{"data.bin": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Entries) == 0 {
		t.Fatal("hydrated file must produce a manifest entry")
	}
	if _, ok := blobs["data.bin"]; ok {
		// Sanity: the blob map is keyed by digest, not by path.
		_ = ok
	}
}

// TestBuildWithValidationDetectsPointerChangeAfterGraph pins that
// the local manifest path observes the actual bytes that become a
// blob. This is the inverse-hydrated case: the graph collector
// observed hydrated bytes, but a pointer-file change between
// collection and BuildWithValidation must be rejected.
func TestBuildWithValidationDetectsPointerChangeAfterGraph(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "lfs")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, repo)
	mustWrite(t, filepath.Join(repo, ".gitattributes"), "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	mustWrite(t, filepath.Join(repo, "data.bin"), "real hydrated\n")
	commitAll(t, repo, "hydrated")
	// Step 1: graph collection observes hydrated bytes; the
	// selector returns the LFS-attributed path.
	input, err := SelectBuildInput(context.Background(), repo, process.Native{})
	if err != nil {
		t.Fatal(err)
	}
	if !input.LFSFiles["data.bin"] {
		t.Fatal("data.bin must be LFS-attributed")
	}
	// Step 2: between collection and BuildWithValidation, the
	// committed bytes are replaced with a formal LFS pointer.
	const lfsPointer = "version https://git-lfs.github.com/spec/v1\noid sha256:5555555555555555555555555555555555555555555555555555555555555555\nsize 12\n"
	mustWrite(t, filepath.Join(repo, "data.bin"), lfsPointer)
	// BuildWithValidation reads the actual bytes and finds the
	// pointer, reporting it as a LFS failure.
	_, _, err = BuildWithValidation(repo, input.LFSFiles)
	if err == nil {
		t.Fatal("post-validation LFS pointer must be rejected")
	}
	if !errors.Is(err, ErrLFSContentUnavailable) {
		t.Fatalf("want ErrLFSContentUnavailable, got %v", err)
	}
}

// TestBuildWithValidationReportsMissingAttributedFile pins that a
// LFS-attributed path that is missing or non-regular in the
// working tree is a source-state error rather than a silent skip.
func TestBuildWithValidationReportsMissingAttributedFile(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	repo := filepath.Join(dir, "lfs")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, repo)
	mustWrite(t, filepath.Join(repo, ".gitattributes"), "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	mustWrite(t, filepath.Join(repo, "data.bin"), "hydrated\n")
	commitAll(t, repo, "hydrated")
	// Remove the file: the LFS-attributed set still references it.
	if err := os.Remove(filepath.Join(repo, "data.bin")); err != nil {
		t.Fatal(err)
	}
	_, _, err := BuildWithValidation(repo, map[string]bool{"data.bin": true})
	if err == nil {
		t.Fatal("missing LFS-attributed file must be a source-state error")
	}
}
