package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// initSubmoduleRepo creates a fresh Git repository at dir with a
// single empty commit so downstream gitlink/submodule commands work.
func initSubmoduleRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "vci-sub@example.com"},
		{"config", "user.name", "vci-sub"},
		{"config", "commit.gpgsign", "false"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := exec.Command("git", "-C", dir, "commit", "--allow-empty", "-q", "-m", "init").Run(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// addSubmoduleFileTransport adds a child repository as a submodule
// at parentDir/<submodulePath>, using only local file transport.
func addSubmoduleFileTransport(t *testing.T, parentDir, childPath, submodulePath string) {
	t.Helper()
	cmd := exec.Command("git", "-C", parentDir,
		"-c", "protocol.file.allow=always",
		"submodule", "add", childPath, submodulePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("submodule add: %v\n%s", err, out)
	}
}

// gitCommit commits every change in dir with msg.
func gitCommit(t *testing.T, dir, msg string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", msg}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// TestRecursiveSubmoduleBuildOverSSH exercises the public build
// path with a parent project that owns an initialized submodule.
// The coordinator's command reads the submodule's tracked file and
// verifies its content; the test also proves no child .git metadata
// reaches the staged tree, the manifest, the cache entry, or any
// archive.
func TestRecursiveSubmoduleBuildOverSSH(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}
	// The fixture's submodule is a local path. Allow the file transport in the
	// shared home gitconfig so both the client and the coordinator (which runs
	// under the same home over SSH) can clone it during reconstruction.
	// Production submodules use https or ssh.
	if err := os.WriteFile(filepath.Join(fixture.homeDir, ".gitconfig"), []byte("[protocol \"file\"]\n\tallow = always\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Coordinator command: verify the submodule content reached the workspace.
	initCoordinatorRoot(t, fixture, "sh", "-c",
		"test \"$(cat child/README.md)\" = '# child content' && "+
			"echo 'SUBMODULE BUILD VERIFIED'")

	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	// Build a parent repo with one initialized submodule.
	sourceParent := t.TempDir()
	parent := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	childSource := filepath.Join(sourceParent, "child_source")
	if err := os.MkdirAll(childSource, 0o755); err != nil {
		t.Fatal(err)
	}
	initSubmoduleRepo(t, parent)
	initSubmoduleRepo(t, childSource)
	mustWriteFile(t, filepath.Join(childSource, "README.md"), "# child content\n")
	gitCommit(t, childSource, "child content")
	addSubmoduleFileTransport(t, parent, childSource, "child")
	gitCommit(t, parent, "add child submodule")

	env := runClientBinary(t, fixture, clientRoot, "build", parent)
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
				State   string `json:"state"`
				Failure string `json:"failure"`
			}
			if jerr := json.Unmarshal(checkEnv.Data, &checkData); jerr == nil {
				if checkData.State == "succeeded" {
					return
				}
				if checkData.State == "failed" {
					t.Fatalf("remote build failed: %s", pretty(checkEnv))
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
}
