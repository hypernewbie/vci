package source

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
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

func TestPackageUnpackageLCRoundTrip(t *testing.T) {
	src := t.TempDir()
	runGit(t, src, "init", "-q")
	lcWriteFile(t, src, "keep.txt", "v")
	runGit(t, src, "add", "keep.txt")
	runGit(t, src, "commit", "-q", "-m", "base")
	lcWriteFile(t, src, "keep.txt", "changed")
	lcWriteFile(t, src, "untracked.txt", "new")
	if err := os.Symlink("/tmp/p", filepath.Join(src, "sym")); err != nil {
		t.Fatal(err)
	}

	original, err := CaptureLocalChanges(context.Background(), src, process.Native{})
	if err != nil {
		t.Fatalf("capture: %v", err)
	}
	rc, err := PackageLC(original)
	if err != nil {
		t.Fatalf("package: %v", err)
	}
	got, err := UnpackageLC(rc)
	if err != nil {
		t.Fatalf("unpackage: %v", err)
	}

	dst := t.TempDir()
	lcCloneInto(t, src, dst)
	if err := ApplyLC(context.Background(), dst, got, process.Native{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	lcAssertContent(t, dst, "keep.txt", "changed")
	lcAssertContent(t, dst, "untracked.txt", "new")
	if target, err := os.Readlink(filepath.Join(dst, "sym")); err != nil || target != "/tmp/p" {
		t.Fatalf("sym = %q, %v", target, err)
	}
}

func TestPackageLCAppliesToCleanCheckoutAtHead(t *testing.T) {
	src := t.TempDir()
	runGit(t, src, "init", "-q")
	lcWriteFile(t, src, "keep.txt", "original")
	lcWriteFile(t, src, "del.txt", "gone")
	runGit(t, src, "add", "-A")
	runGit(t, src, "commit", "-q", "-m", "base")

	// Client change set against HEAD: tracked modification and deletion plus
	// untracked regular, executable, and symlink entries.
	lcWriteFile(t, src, "keep.txt", "modified")
	if err := os.Remove(filepath.Join(src, "del.txt")); err != nil {
		t.Fatal(err)
	}
	lcWriteFile(t, src, "new.txt", "untracked")
	lcWriteFile(t, src, "exec.sh", "#!/bin/sh\necho hi\n")
	if err := os.Chmod(filepath.Join(src, "exec.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	lc, err := CaptureLocalChanges(context.Background(), src, process.Native{})
	if err != nil {
		t.Fatalf("CaptureLocalChanges: %v", err)
	}
	rc, err := PackageLC(lc)
	if err != nil {
		t.Fatalf("PackageLC: %v", err)
	}
	packed, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read packaged LC: %v", err)
	}

	ws := t.TempDir()
	lcCloneInto(t, src, ws)

	// Worker-compatible apply: extract lc.tar into a private staging dir, run
	// git apply on staging/patch, then restore the f/ archive into the
	// workspace, stripping its leading component.
	staging := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	lcTar := filepath.Join(t.TempDir(), "lc.tar")
	if err := os.WriteFile(lcTar, packed, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("tar", "-xpf", lcTar, "-C", staging).CombinedOutput(); err != nil {
		t.Fatalf("extract lc.tar: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(staging, "patch")); err != nil {
		t.Fatalf("staging/patch: %v", err)
	}
	runGit(t, ws, "apply", "--binary", "--whitespace=nowarn", filepath.Join(staging, "patch"))
	archive, err := exec.Command("tar", "-C", staging, "-cf", "-", "f").Output()
	if err != nil {
		t.Fatalf("archive staging/f: %v", err)
	}
	restore := exec.Command("tar", "-C", ws, "-xpf", "-", "--strip-components=1")
	restore.Stdin = bytes.NewReader(archive)
	if out, err := restore.CombinedOutput(); err != nil {
		t.Fatalf("restore f/ into workspace: %v\n%s", err, out)
	}

	lcAssertContent(t, ws, "keep.txt", "modified")
	if _, err := os.Stat(filepath.Join(ws, "del.txt")); !os.IsNotExist(err) {
		t.Fatalf("del.txt should be absent, stat err = %v", err)
	}
	lcAssertContent(t, ws, "new.txt", "untracked")
	lcAssertContent(t, ws, "exec.sh", "#!/bin/sh\necho hi\n")
	if fi, err := os.Stat(filepath.Join(ws, "exec.sh")); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm() != 0o755 {
		t.Fatalf("exec.sh mode = %o, want 755", fi.Mode().Perm())
	}
	if target, err := os.Readlink(filepath.Join(ws, "link")); err != nil || target != "target.txt" {
		t.Fatalf("link = %q, %v; want target.txt", target, err)
	}
}
