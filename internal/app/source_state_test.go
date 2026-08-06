package app

// Source-state tests.
//
// Build input is a finite snapshot of the local working tree selected via
// `source.SelectBuildInput`:
//   - Tracked files (HEAD, modified, staged) are included with current bytes and modes.
//   - Locally deleted tracked files are excluded (absent from archive).
//   - Untracked non-ignored files are included.
//   - Ignored files and directories (.gitignore) are excluded.
//   - Executable permission bits are preserved by tar.
//   - Symlinks are preserved as symlinks without dereferencing.
//   - Minimal repository markers (.git/HEAD, .git/objects, .git/refs) are included
//     for remote source.Discover.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypernewbie/vci/internal/process"
	"github.com/hypernewbie/vci/internal/source"
)

func TestSourceCleanToplevelPathRespected(t *testing.T) {
	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "hello\n")
	mustWriteFile(t, filepath.Join(sourceDir, "lib.go"), "package demo\n")
	mustGitAddCommit(t, sourceDir, "initial")

	tarOut := t.TempDir()
	if err := archiveSelectedSource(t, sourceDir, tarOut); err != nil {
		t.Fatalf("archive source: %v", err)
	}
	mustContain(t, tarOut, "README.md")
	mustContain(t, tarOut, "lib.go")
	mustContain(t, tarOut, ".git/HEAD")
}

func TestSourceIncludesUntrackedAndExcludesIgnored(t *testing.T) {
	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "tracked.txt"), "tracked\n")
	mustGitAddCommit(t, sourceDir, "tracked")
	mustWriteFile(t, filepath.Join(sourceDir, "untracked.txt"), "untracked\n")
	mustWriteFile(t, filepath.Join(sourceDir, ".gitignore"), "ignored.bin\n")
	if err := os.WriteFile(filepath.Join(sourceDir, "ignored.bin"), []byte("ignored"), 0o600); err != nil {
		t.Fatalf("write ignored: %v", err)
	}
	tarOut := t.TempDir()
	if err := archiveSelectedSource(t, sourceDir, tarOut); err != nil {
		t.Fatalf("archive source: %v", err)
	}
	mustContain(t, tarOut, "tracked.txt")
	mustContain(t, tarOut, "untracked.txt")
	mustContain(t, tarOut, ".gitignore")
	mustNotContain(t, tarOut, "ignored.bin")
}

func TestSourceStagePreservesSymlinkBytes(t *testing.T) {
	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "x")
	if err := os.Symlink("README.md", filepath.Join(sourceDir, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	mustGitAddCommit(t, sourceDir, "symlink")

	tarOut := t.TempDir()
	if err := archiveSelectedSource(t, sourceDir, tarOut); err != nil {
		t.Fatalf("archive source: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(tarOut, "link")); err != nil {
		t.Fatalf("symlink must be preserved as a symlink: %v", err)
	}
}

func TestSourceExecutableModePreserved(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skipf("tar not available: %v", err)
	}
	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "x")
	binary := filepath.Join(sourceDir, "bin")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	mustGitAddCommit(t, sourceDir, "init")

	tarOut := t.TempDir()
	if err := archiveSelectedSource(t, sourceDir, tarOut); err != nil {
		t.Fatalf("archive source: %v", err)
	}
	got, err := os.Stat(filepath.Join(tarOut, "bin"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got.Mode()&0o111 == 0 {
		t.Fatalf("executable bit must survive tar; got mode %v", got.Mode())
	}
}

func TestSourceWorkingTreeDeltaRecordedAboveGit(t *testing.T) {
	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "config.txt"), "committed\n")
	mustGitAddCommit(t, sourceDir, "init")
	mustWriteFile(t, filepath.Join(sourceDir, "config.txt"), "modified-after-commit\n")

	tarOut := t.TempDir()
	if err := archiveSelectedSource(t, sourceDir, tarOut); err != nil {
		t.Fatalf("archive source: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(tarOut, "config.txt"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "modified-after-commit") {
		t.Fatalf("working-tree changes after last commit must reach the archive; got %q", data)
	}
}

func archiveSelectedSource(t *testing.T, sourceDir, outDir string) error {
	t.Helper()
	input, err := source.SelectBuildInput(context.Background(), sourceDir, process.Native{})
	if err != nil {
		return err
	}

	tarCmd := exec.Command("tar", "-cf", "-", "-C", input.Root, "--null", "-T", "-", "--no-recursion")
	var pathBuf bytes.Buffer
	for _, p := range input.Files {
		pathBuf.WriteString(p)
		pathBuf.WriteByte(0)
	}
	tarCmd.Stdin = &pathBuf

	untarCmd := exec.Command("tar", "-C", outDir, "-xf", "-")
	untarCmd.Stdin, _ = tarCmd.StdoutPipe()

	if err := untarCmd.Start(); err != nil {
		return err
	}
	if err := tarCmd.Run(); err != nil {
		return err
	}
	return untarCmd.Wait()
}

func mustContain(t *testing.T, dir, rel string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
		t.Fatalf("tar archive missing %s: %v", rel, err)
	}
}

func mustNotContain(t *testing.T, dir, rel string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, rel)); err == nil {
		t.Fatalf("tar archive MUST NOT contain %s", rel)
	}
}
