package source

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hypernewbie/vci/internal/process"
)

// gitSha returns the full object id of ref in root, exercising the same
// runGitOutput code path as CaptureIdentity.
func gitSha(t *testing.T, root, ref string) string {
	t.Helper()
	sha, err := runGitOutput(context.Background(), process.Native{}, root, "rev-parse", ref)
	if err != nil {
		t.Fatalf("rev-parse %s: %v", ref, err)
	}
	return sha
}

func TestCaptureIdentityReportsHeadBaseRemote(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "a.txt")
	runGit(t, root, "commit", "-q", "-m", "base")
	base := gitSha(t, root, "HEAD")
	runGit(t, root, "remote", "add", "origin", "https://example.com/vci.git")
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("2"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "b.txt")
	runGit(t, root, "commit", "-q", "-m", "head")
	head := gitSha(t, root, "HEAD")
	id, err := CaptureIdentity(context.Background(), root, process.Native{})
	if err != nil {
		t.Fatalf("CaptureIdentity: %v", err)
	}
	if id.Head != head {
		t.Fatalf("Head = %q, want %q", id.Head, head)
	}
	if id.Base != base {
		t.Fatalf("Base = %q, want %q", id.Base, base)
	}
	if id.RemoteURL != "https://example.com/vci.git" {
		t.Fatalf("RemoteURL = %q, want %q", id.RemoteURL, "https://example.com/vci.git")
	}
}

func TestCaptureIdentityRejectsNonRepository(t *testing.T) {
	root := t.TempDir()
	_, err := CaptureIdentity(context.Background(), root, process.Native{})
	if !errors.Is(err, ErrNotGitRepository) {
		t.Fatalf("err = %v, want ErrNotGitRepository", err)
	}
}

func TestCaptureIdentityRootCommitEmptyBaseAndNoRemote(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "f.txt")
	runGit(t, root, "commit", "-q", "-m", "root")
	rootSha := gitSha(t, root, "HEAD")
	id, err := CaptureIdentity(context.Background(), root, process.Native{})
	if err != nil {
		t.Fatalf("CaptureIdentity: %v", err)
	}
	if id.Head != rootSha {
		t.Fatalf("Head = %q, want %q", id.Head, rootSha)
	}
	if id.Base != "" {
		t.Fatalf("Base = %q, want empty for root commit", id.Base)
	}
	if id.RemoteURL != "" {
		t.Fatalf("RemoteURL = %q, want empty when origin is unset", id.RemoteURL)
	}
}

func TestCaptureIdentityDoesNotMutate(t *testing.T) {
	root := t.TempDir()
	runGit(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, root, "add", "keep.txt")
	runGit(t, root, "commit", "-q", "-m", "one")
	runGit(t, root, "remote", "add", "origin", "https://example.com/x.git")
	before := gitSha(t, root, "HEAD")
	contentBefore, err := os.ReadFile(filepath.Join(root, "keep.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureIdentity(context.Background(), root, process.Native{}); err != nil {
		t.Fatalf("CaptureIdentity: %v", err)
	}
	if after := gitSha(t, root, "HEAD"); after != before {
		t.Fatalf("HEAD changed from %q to %q", before, after)
	}
	contentAfter, err := os.ReadFile(filepath.Join(root, "keep.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contentAfter, contentBefore) {
		t.Fatalf("keep.txt changed: %q -> %q", contentBefore, contentAfter)
	}
}
