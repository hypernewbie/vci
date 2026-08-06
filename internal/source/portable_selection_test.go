package source

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypernewbie/vci/internal/process"
)

// TestPortableToolCapabilities verifies exact behavior of git and tar on macOS.
func TestPortableToolCapabilities(t *testing.T) {
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
