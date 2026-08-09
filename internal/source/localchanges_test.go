package source

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hypernewbie/vci/internal/process"
)

func lcWriteFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func lcAssertContent(t *testing.T, dir, name, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if string(got) != want {
		t.Fatalf("%s = %q, want %q", name, got, want)
	}
}

func lcCloneInto(t *testing.T, src, dst string) {
	t.Helper()
	runGit(t, t.TempDir(), "clone", "-q", src, dst)
}

func TestCaptureApplyReproducesWorkingTree(t *testing.T) {
	src := t.TempDir()
	runGit(t, src, "init", "-q")
	lcWriteFile(t, src, "keep.txt", "original")
	lcWriteFile(t, src, "del.txt", "gone")
	lcWriteFile(t, src, "mode.txt", "plain")
	lcWriteFile(t, src, "untouched.txt", "same")
	runGit(t, src, "add", "-A")
	runGit(t, src, "commit", "-q", "-m", "base")

	// Working-tree changes: modify, delete, mode change, untracked file, and an
	// untracked symlink. untouched.txt stays at HEAD.
	lcWriteFile(t, src, "keep.txt", "modified")
	if err := os.Remove(filepath.Join(src, "del.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(src, "mode.txt"), 0o755); err != nil {
		t.Fatal(err)
	}
	lcWriteFile(t, src, "untracked.txt", "newfile")
	if err := os.Symlink("/tmp/lc-target", filepath.Join(src, "untracked_sym")); err != nil {
		t.Fatal(err)
	}

	lc, err := CaptureLocalChanges(context.Background(), src, process.Native{})
	if err != nil {
		t.Fatalf("CaptureLocalChanges: %v", err)
	}
	if len(lc.Patch) == 0 {
		t.Fatal("patch empty despite tracked changes")
	}
	if len(lc.Untracked) != 2 {
		t.Fatalf("untracked count = %d, want 2", len(lc.Untracked))
	}

	dst := t.TempDir()
	lcCloneInto(t, src, dst)
	if err := ApplyLC(context.Background(), dst, lc, process.Native{}); err != nil {
		t.Fatalf("ApplyLC: %v", err)
	}

	lcAssertContent(t, dst, "keep.txt", "modified")
	if _, err := os.Stat(filepath.Join(dst, "del.txt")); !os.IsNotExist(err) {
		t.Fatalf("del.txt should be absent: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dst, "mode.txt"))
	if err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("mode.txt should be executable: %v %v", fi, err)
	}
	lcAssertContent(t, dst, "untouched.txt", "same")
	lcAssertContent(t, dst, "untracked.txt", "newfile")
	if target, err := os.Readlink(filepath.Join(dst, "untracked_sym")); err != nil || target != "/tmp/lc-target" {
		t.Fatalf("untracked_sym = %q, %v", target, err)
	}
}

func TestCaptureCleanRepoIsEmpty(t *testing.T) {
	src := t.TempDir()
	runGit(t, src, "init", "-q")
	lcWriteFile(t, src, "f.txt", "v")
	runGit(t, src, "add", "f.txt")
	runGit(t, src, "commit", "-q", "-m", "one")
	lc, err := CaptureLocalChanges(context.Background(), src, process.Native{})
	if err != nil {
		t.Fatalf("CaptureLocalChanges: %v", err)
	}
	if len(lc.Patch) != 0 || len(lc.Untracked) != 0 {
		t.Fatalf("clean repo produced changes: patch=%d untracked=%d", len(lc.Patch), len(lc.Untracked))
	}
}

func TestApplyLCRejectsUnsafePaths(t *testing.T) {
	dst := t.TempDir()
	for _, p := range []string{"../escape", "/abs/path", ".git/config", ".vci/state", "a/../../b"} {
		lc := LocalChanges{Untracked: []UntrackedFile{{Path: p, Mode: 0o644, Content: []byte("x")}}}
		if err := ApplyLC(context.Background(), dst, lc, process.Native{}); err == nil {
			t.Fatalf("unsafe path %q accepted", p)
		}
	}
}
