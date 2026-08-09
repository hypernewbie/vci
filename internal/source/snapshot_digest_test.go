package source

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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

// TestPortableToolCapabilities verifies exact behavior of git and tar on macOS.
func TestPortableToolCapabilities(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("tar with Windows paths")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git tool not available")
	}
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skip("tar tool not available")
	}

	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Init git repo
	cmd := exec.Command("git", "init")
	cmd.Dir = sourceDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}

	// 1. Tracked file
	mustWrite(t, filepath.Join(sourceDir, "tracked.txt"), "tracked content")
	cmd = exec.Command("git", "add", "tracked.txt")
	cmd.Dir = sourceDir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "commit", "-m", "init")
	cmd.Dir = sourceDir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	// 2. Untracked non-ignored file
	mustWrite(t, filepath.Join(sourceDir, "untracked.txt"), "untracked content")

	// 3. Ignored file
	mustWrite(t, filepath.Join(sourceDir, ".gitignore"), "ignored.log\n")
	mustWrite(t, filepath.Join(sourceDir, "ignored.log"), "should be excluded")

	// 4. Executable file
	execPath := filepath.Join(sourceDir, "script.sh")
	mustWrite(t, execPath, "#!/bin/sh\necho hi")
	if err := os.Chmod(execPath, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	// 5. Symlink
	symlinkPath := filepath.Join(sourceDir, "link.txt")
	if err := os.Symlink("tracked.txt", symlinkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// Run git ls-files -z --cached --others --exclude-standard
	lsCmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	lsCmd.Dir = sourceDir
	out, err := lsCmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}

	paths := strings.Split(string(out), "\x00")
	var selected []string
	for _, p := range paths {
		if p == "" {
			continue
		}
		selected = append(selected, p)
	}

	// Verify path selection
	pathSet := make(map[string]bool)
	for _, p := range selected {
		pathSet[p] = true
	}

	if !pathSet["tracked.txt"] {
		t.Errorf("expected tracked.txt in selection")
	}
	if !pathSet["untracked.txt"] {
		t.Errorf("expected untracked.txt in selection")
	}
	if !pathSet["script.sh"] {
		t.Errorf("expected script.sh in selection")
	}
	if !pathSet["link.txt"] {
		t.Errorf("expected link.txt in selection")
	}
	if pathSet["ignored.log"] {
		t.Errorf("ignored.log MUST NOT be in selection")
	}

	// 6. Test tar --null -T - archiving
	var inputBuffer bytes.Buffer
	for _, p := range selected {
		inputBuffer.WriteString(p)
		inputBuffer.WriteByte(0)
	}

	destDir := t.TempDir()
	tarCmd := exec.Command("tar", "-cf", "-", "-C", sourceDir, "--null", "-T", "-")
	tarCmd.Stdin = &inputBuffer
	tarOut, err := tarCmd.Output()
	if err != nil {
		t.Fatalf("tar -cf: %v", err)
	}

	// Extract tar
	untarCmd := exec.Command("tar", "-xf", "-", "-C", destDir)
	untarCmd.Stdin = bytes.NewReader(tarOut)
	if out, err := untarCmd.CombinedOutput(); err != nil {
		t.Fatalf("tar -xf: %v\n%s", err, out)
	}

	// Assert extracted contents
	if data, err := os.ReadFile(filepath.Join(destDir, "tracked.txt")); err != nil || string(data) != "tracked content" {
		t.Errorf("tracked.txt mismatch: %v, %s", err, string(data))
	}
	if data, err := os.ReadFile(filepath.Join(destDir, "untracked.txt")); err != nil || string(data) != "untracked content" {
		t.Errorf("untracked.txt mismatch: %v, %s", err, string(data))
	}
	if _, err := os.Stat(filepath.Join(destDir, "ignored.log")); err == nil {
		t.Errorf("ignored.log should not exist in extracted archive")
	}

	// Assert executable bit preserved
	info, err := os.Stat(filepath.Join(destDir, "script.sh"))
	if err != nil {
		t.Fatalf("stat script.sh: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("script.sh expected executable mode, got %v", info.Mode())
	}

	// Assert symlink preserved
	lInfo, err := os.Lstat(filepath.Join(destDir, "link.txt"))
	if err != nil {
		t.Fatalf("lstat link.txt: %v", err)
	}
	if lInfo.Mode()&os.ModeSymlink == 0 {
		t.Errorf("link.txt expected symlink mode, got %v", lInfo.Mode())
	}

	// 7. Verify minimal git marker works for Discover
	minGitDir := filepath.Join(destDir, ".git")
	if err := os.MkdirAll(filepath.Join(minGitDir, "objects"), 0o755); err != nil {
		t.Fatalf("mkdir git/objects: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(minGitDir, "refs"), 0o755); err != nil {
		t.Fatalf("mkdir git/refs: %v", err)
	}
	mustWrite(t, filepath.Join(minGitDir, "HEAD"), "ref: refs/heads/main\n")

	repo, err := Discover(context.Background(), destDir, process.Native{})
	if err != nil {
		t.Fatalf("Discover on minimal git marker failed: %v", err)
	}
	if repo.Name != filepath.Base(destDir) {
		t.Fatalf("expected repo name %s, got %s", filepath.Base(destDir), repo.Name)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
