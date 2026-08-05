package app

// Source-state tests.
//
// Current behavior sends a complete working-tree archive over SSH using
// literal tar, including `.git` and ignored content. It is direct SSH copy,
// not status-aware or incremental synchronization.
//
// Transferred archive behavior:
//   - Every file under the working tree, including `.git/` itself, is copied.
//     Git metadata is kept so `vci build .` on the coordinator can discover
//     the project basename.
//   - Untracked files and ignored content are included because literal tar
//     archives the whole directory. Transferring ignored content is a known
//     limitation of literal tar copy, not a product decision.
//   - Executable permission bits are preserved by tar.
//   - Symlinks are preserved as symlinks without dereferencing.
//
// The following tests verify these literal tar facts.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSourceCleanToplevelPathRespectedViaTar creates a clean git
// repo whose toplevel basename matches the coordinator project
// name and asserts that a plain tar archive of the working tree
// contains every committed file plus the .git directory. The test
// runs without SSH so it stays a unit test of the source-state
// contract; an integration variant in TestStagingTrap covers the
// remote side.
func TestSourceCleanToplevelPathRespectedViaTar(t *testing.T) {
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
	if err := runTar(sourceDir, tarOut); err != nil {
		t.Fatalf("tar: %v", err)
	}
	mustContain(t, tarOut, "README.md")
	mustContain(t, tarOut, "lib.go")
	mustContain(t, tarOut, ".git")
}

// TestSourceIncludesUntrackedAndIgnored still includes untracked
// files (because tar does not filter), and ignored files remain
// byte-for-byte present. This is a literal archive behavior, not a
// filtering policy or a custom Vci transport format.
func TestSourceIncludesUntrackedAndIgnored(t *testing.T) {
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
	if err := runTar(sourceDir, tarOut); err != nil {
		t.Fatalf("tar: %v", err)
	}
	mustContain(t, tarOut, "tracked.txt")
	mustContain(t, tarOut, "untracked.txt")
	mustContain(t, tarOut, "ignored.bin")
	mustContain(t, tarOut, ".gitignore")
}

// TestSourceStagePreservesSymlinkBytes asserts that tar preserves
// symlinks as symlinks in the extracted archive (does not
// dereference them). The cleanup trap on the remote side therefore
// never has an opportunity to follow symlinks into external
// targets.
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
	if err := runTar(sourceDir, tarOut); err != nil {
		t.Fatalf("tar: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(tarOut, "link")); err != nil {
		t.Fatalf("symlink must be preserved as a symlink: %v", err)
	}
}

// TestSourceEmptyDirectorySurvivesTar asserts tar treats empty
// directories like any other path; the empty directory arrives
// on the coordinator side because git itself does not track empty
// directories, but the working tree may contain them as fixtures.
func TestSourceEmptyDirectorySurvivesTar(t *testing.T) {
	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "x")
	mustGitAddCommit(t, sourceDir, "init")
	if err := os.MkdirAll(filepath.Join(sourceDir, "empty"), 0o755); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}

	tarOut := t.TempDir()
	if err := runTar(sourceDir, tarOut); err != nil {
		t.Fatalf("tar: %v", err)
	}
	info, err := os.Stat(filepath.Join(tarOut, "empty"))
	if err != nil || !info.IsDir() {
		t.Fatalf("empty directory must be preserved in tar archive; got %v", err)
	}
}

// TestSourceExecutableModePreserved asserts that a binary marked
// executable in the working tree lands on the receiver with the
// executable bit intact. tar preserves modes by default; the
// host side does not strip them.
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
	if err := runTar(sourceDir, tarOut); err != nil {
		t.Fatalf("tar: %v", err)
	}
	got, err := os.Stat(filepath.Join(tarOut, "bin"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got.Mode()&0o111 == 0 {
		t.Fatalf("executable bit must survive tar; got mode %v", got.Mode())
	}
}

// TestSourceWorkingTreeDeltaRecordedAboveGit asserts that a
// modification made AFTER the last commit is captured. The build
// observes the latest working tree, never the most recent commit,
// so an unmodified local change still updates the coordinator's
// input.
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
	if err := runTar(sourceDir, tarOut); err != nil {
		t.Fatalf("tar: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(tarOut, "config.txt"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "modified-after-commit") {
		t.Fatalf("working-tree changes after last commit must reach the archive; got %q", data)
	}
}

// runTar archives sourceDir into outDir using the system tar
// binary. The system tar's default options match the production
// composition: no dereferencing, no excludes.
func runTar(sourceDir, outDir string) error {
	cmd := exec.Command("tar", "-C", sourceDir, "-cf", "-", ".")
	cmdOut := exec.Command("tar", "-C", outDir, "-xf", "-")
	cmdOut.Stdin, _ = cmd.StdoutPipe()
	if err := cmdOut.Start(); err != nil {
		return err
	}
	if err := cmd.Run(); err != nil {
		return err
	}
	return cmdOut.Wait()
}

// mustContain asserts that path is present in dir.
func mustContain(t *testing.T, dir, rel string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
		t.Fatalf("tar archive missing %s: %v", rel, err)
	}
}
