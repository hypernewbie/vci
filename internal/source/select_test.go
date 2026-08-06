package source

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/hypernewbie/vci/internal/process"
)

func TestSelectBuildInputContract(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "myproject")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	runGit(t, sourceDir, "init")

	// 1. Tracked file
	mustWrite(t, filepath.Join(sourceDir, "tracked.go"), "package main")
	// 2. Tracked file to be deleted locally
	mustWrite(t, filepath.Join(sourceDir, "deleted.go"), "package main")
	runGit(t, sourceDir, "add", ".")
	runGit(t, sourceDir, "commit", "-m", "init")

	// Delete deleted.go locally
	if err := os.Remove(filepath.Join(sourceDir, "deleted.go")); err != nil {
		t.Fatalf("remove deleted.go: %v", err)
	}

	// 3. Untracked file
	mustWrite(t, filepath.Join(sourceDir, "untracked.go"), "package main")

	// 4. Ignored file
	mustWrite(t, filepath.Join(sourceDir, ".gitignore"), "*.log\n")
	mustWrite(t, filepath.Join(sourceDir, "app.log"), "log data")

	// 5. Unique sentinel in .git/config
	mustWrite(t, filepath.Join(sourceDir, ".git", "config"), "[user]\n  name = PrivateSentinelUser\n")

	// Select build input
	input, err := SelectBuildInput(context.Background(), sourceDir, process.Native{})
	if err != nil {
		t.Fatalf("SelectBuildInput failed: %v", err)
	}

	if input.ProjectName != "myproject" {
		t.Fatalf("expected project name myproject, got %s", input.ProjectName)
	}

	set := make(map[string]bool)
	for _, f := range input.Files {
		set[f] = true
	}

	if !set["tracked.go"] {
		t.Errorf("expected tracked.go in input")
	}
	if !set["untracked.go"] {
		t.Errorf("expected untracked.go in input")
	}
	if set["deleted.go"] {
		t.Errorf("deleted.go MUST NOT be in input")
	}
	if set["app.log"] {
		t.Errorf("app.log MUST NOT be in input")
	}

	// Markers
	if !set[".git/HEAD"] {
		t.Errorf("expected .git/HEAD marker")
	}
	if !set[".git/objects"] {
		t.Errorf("expected .git/objects marker")
	}
	if set[".git/config"] {
		t.Errorf(".git/config MUST NOT be in input")
	}
}

func TestSelectBuildInputRejectsLinkedWorktree(t *testing.T) {
	dir := t.TempDir()
	sourceDir := filepath.Join(dir, "worktree")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write a .git file (linked worktree)
	mustWrite(t, filepath.Join(sourceDir, ".git"), "gitdir: /some/path")

	_, err := SelectBuildInput(context.Background(), sourceDir, process.Native{})
	if err == nil {
		t.Fatalf("expected error for linked worktree (.git file), got nil")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
