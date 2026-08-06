package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDirectBuildInputSourceIntegrityOverSSH exercises the full end-to-end SSH path
// from client binary -> system ssh -> remote sshd -> staging shell -> public vci build .
// and verifies observed content, mode, symlink target, and path exclusion on the remote coordinator.
func TestDirectBuildInputSourceIntegrityOverSSH(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}

	// Coordinator script verifies observed content, mode, symlink target, and exclusions.
	initCoordinatorRoot(t, fixture, "sh", "-c",
		"test \"$(cat modified.txt)\" = 'modified content' && "+
			"test \"$(cat untracked.txt)\" = 'untracked' && "+
			"test ! -f deleted.txt && test ! -f ignored.log && test ! -d ignored_dir && "+
			"test ! -f .git/config && test -x script.sh && test -L link.txt && "+
			"test \"$(readlink link.txt)\" = 'tracked.txt' && "+
			"echo 'SOURCE INTEGRITY VERIFIED'")

	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}

	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, ".git", "config"), "[user]\n  name = PrivateSentinelUser\n  email = sentinel@example.com\n")
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "# demo\n")
	mustWriteFile(t, filepath.Join(sourceDir, "tracked.txt"), "tracked")
	mustWriteFile(t, filepath.Join(sourceDir, "modified.txt"), "original")
	mustWriteFile(t, filepath.Join(sourceDir, "deleted.txt"), "deleted")
	mustGitAddCommit(t, sourceDir, "init")

	// 1. Modify tracked file
	mustWriteFile(t, filepath.Join(sourceDir, "modified.txt"), "modified content")
	// 2. Delete tracked file locally
	if err := os.Remove(filepath.Join(sourceDir, "deleted.txt")); err != nil {
		t.Fatalf("remove deleted.txt: %v", err)
	}
	// 3. Untracked non-ignored file
	mustWriteFile(t, filepath.Join(sourceDir, "untracked.txt"), "untracked")
	// 4. Ignored file and directory
	mustWriteFile(t, filepath.Join(sourceDir, ".gitignore"), "ignored.log\nignored_dir/\n")
	mustWriteFile(t, filepath.Join(sourceDir, "ignored.log"), "secret")
	if err := os.MkdirAll(filepath.Join(sourceDir, "ignored_dir"), 0o755); err != nil {
		t.Fatalf("mkdir ignored_dir: %v", err)
	}
	mustWriteFile(t, filepath.Join(sourceDir, "ignored_dir", "cached.bin"), "bin")
	// 5. Executable file
	execPath := filepath.Join(sourceDir, "script.sh")
	mustWriteFile(t, execPath, "#!/bin/sh\necho hi")
	if err := os.Chmod(execPath, 0o755); err != nil {
		t.Fatalf("chmod script.sh: %v", err)
	}
	// 6. Symlink
	if err := os.Symlink("tracked.txt", filepath.Join(sourceDir, "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	env := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env.OK {
		t.Fatalf("build submission failed: %s", pretty(env))
	}
	var data struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("json decode run_id: %v", err)
	}

	// Read public check response through client
	deadline := time.Now().Add(20 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("remote build worker timed out")
		}
		checkEnv := runClientBinary(t, fixture, clientRoot, "check", data.RunID)
		if checkEnv.OK {
			var checkData struct {
				State string `json:"state"`
			}
			if jerr := json.Unmarshal(checkEnv.Data, &checkData); jerr == nil {
				if checkData.State == "succeeded" {
					break
				}
				if checkData.State == "failed" {
					t.Fatalf("remote build failed (observed facts check failed)")
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Verify no staging directory remains under remote Vci root
	rootTmp := filepath.Join(fixture.coordinatorRoot, "state", "tmp")
	stagingLeft := findStagingArtifacts(rootTmp)
	if len(stagingLeft) > 0 {
		t.Fatalf("staging directories were not cleaned up: %v", stagingLeft)
	}
}

func TestDirectBuildInputSpecialFilenamesOverSSH(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}

	// Verify nested paths with spaces, quotes, and Unicode metacharacters
	initCoordinatorRoot(t, fixture, "sh", "-c",
		"test -f \"sub dir/file with space.txt\" && "+
			"test -f \"sub dir/special_quote'name.txt\" && "+
			"test -f \"sub dir/unicode_🚀_file.txt\" && "+
			"echo 'SPECIAL FILENAMES VERIFIED'")

	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	subDir := filepath.Join(sourceDir, "sub dir")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatalf("mkdir sub dir: %v", err)
	}

	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "# demo\n")
	mustWriteFile(t, filepath.Join(subDir, "file with space.txt"), "space")
	mustWriteFile(t, filepath.Join(subDir, "special_quote'name.txt"), "quotes")
	mustWriteFile(t, filepath.Join(subDir, "unicode_🚀_file.txt"), "unicode")
	mustGitAddCommit(t, sourceDir, "init")

	env := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env.OK {
		t.Fatalf("build submission failed: %s", pretty(env))
	}
	var data struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("json decode run_id: %v", err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("remote build worker timed out")
		}
		checkEnv := runClientBinary(t, fixture, clientRoot, "check", data.RunID)
		if checkEnv.OK {
			var checkData struct {
				State string `json:"state"`
			}
			if jerr := json.Unmarshal(checkEnv.Data, &checkData); jerr == nil {
				if checkData.State == "succeeded" {
					break
				}
				if checkData.State == "failed" {
					t.Fatalf("remote build failed for special filenames")
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestDirectBuildInputRejectsLinkedWorktreeBeforeSSH(t *testing.T) {
	fixture := NewSSHFixture(t)
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}

	// Create linked worktree marker (.git file)
	mustWriteFile(t, filepath.Join(sourceDir, ".git"), "gitdir: /some/external/path")

	env := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if env.OK {
		t.Fatalf("build must fail for linked worktree (.git file); got %s", pretty(env))
	}
	if env.Error == nil || env.Error.Class != "infrastructure" || !strings.Contains(env.Error.Message, "linked Git worktrees") {
		t.Fatalf("expected infrastructure error mentioning linked Git worktrees; got %s", pretty(env))
	}
}
