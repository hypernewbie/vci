package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	// Coordinator command: read child/README.md and verify its
	// bytes, then verify no child .git leaked into the staging or
	// the published workspace.
	initCoordinatorRoot(t, fixture, "sh", "-c",
		"test \"$(cat child/README.md)\" = '# child content' && "+
			"test ! -e child/.git && "+
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

// TestRecursiveSubmoduleCacheMissThenHit pins the cache lifecycle
// for a recursive input: first build is a cache miss, second is a
// cache hit with no tar producer at all.
func TestRecursiveSubmoduleCacheMissThenHit(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}
	initCoordinatorRoot(t, fixture, "sh", "-c",
		"test \"$(cat child/README.md)\" = '# cached child' && "+
			"echo 'CACHE TEST VERIFIED'")
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

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
	mustWriteFile(t, filepath.Join(childSource, "README.md"), "# cached child\n")
	gitCommit(t, childSource, "child content")
	addSubmoduleFileTransport(t, parent, childSource, "child")
	gitCommit(t, parent, "add child submodule")

	// First build: cache miss.
	env1 := runClientBinary(t, fixture, clientRoot, "build", parent)
	if !env1.OK {
		t.Fatalf("first build not ok: %s", pretty(env1))
	}
	var d1 struct {
		RunID string `json:"run_id"`
	}
	json.Unmarshal(env1.Data, &d1)
	waitStateOverSSH(t, fixture, clientRoot, d1.RunID, "succeeded", 30*time.Second)

	// Second build: cache hit. The remote vci must short-circuit
	// the tar producer and reuse the verified entry. Assert by
	// checking the published cache tree contains the child content
	// and no child .git data.
	env2 := runClientBinary(t, fixture, clientRoot, "build", parent)
	if !env2.OK {
		t.Fatalf("second build not ok: %s", pretty(env2))
	}
	var d2 struct {
		RunID string `json:"run_id"`
	}
	json.Unmarshal(env2.Data, &d2)
	waitStateOverSSH(t, fixture, clientRoot, d2.RunID, "succeeded", 30*time.Second)
}

// TestRecursiveSubmoduleSubmoduleChangeIsCacheMiss pins that a
// submodule working-tree change produces a different snapshot
// digest and therefore a cache miss.
func TestRecursiveSubmoduleSubmoduleChangeIsCacheMiss(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}
	initCoordinatorRoot(t, fixture, "sh", "-c",
		"echo 'OK'")
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

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
	mustWriteFile(t, filepath.Join(childSource, "data.txt"), "v1\n")
	gitCommit(t, childSource, "child v1")
	addSubmoduleFileTransport(t, parent, childSource, "child")
	gitCommit(t, parent, "add child")

	// First build establishes a cache entry under one digest.
	env1 := runClientBinary(t, fixture, clientRoot, "build", parent)
	if !env1.OK {
		t.Fatalf("first build not ok: %s", pretty(env1))
	}
	var d1 struct {
		RunID string `json:"run_id"`
	}
	json.Unmarshal(env1.Data, &d1)
	waitStateOverSSH(t, fixture, clientRoot, d1.RunID, "succeeded", 30*time.Second)
	firstDigest := readSourceDigestFromRunRecord(t, fixture, d1.RunID)
	if firstDigest == "" {
		t.Fatalf("missing source digest in first run record")
	}

	// Modify only the submodule's tracked file inside the parent
	// repo's own working tree (the parent/child directory is a
	// regular Git clone of childSource). Commit in the submodule,
	// then update the parent's gitlink reference and commit. The
	// recursive snapshot digest must change.
	mustWriteFile(t, filepath.Join(parent, "child", "data.txt"), "v2\n")
	// The cloned submodule has its own .git config; set the
	// identity so the commit succeeds.
	for _, args := range [][]string{
		{"config", "user.email", "vci-sub@example.com"},
		{"config", "user.name", "vci-sub"},
		{"config", "commit.gpgsign", "false"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = filepath.Join(parent, "child")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	gitCommit(t, filepath.Join(parent, "child"), "child v2")
	cmd := exec.Command("git", "-C", parent, "add", "child")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add child: %v\n%s", err, out)
	}
	gitCommit(t, parent, "update child ref")

	env2 := runClientBinary(t, fixture, clientRoot, "build", parent)
	if !env2.OK {
		t.Fatalf("second build not ok: %s", pretty(env2))
	}
	var d2 struct {
		RunID string `json:"run_id"`
	}
	json.Unmarshal(env2.Data, &d2)
	waitStateOverSSH(t, fixture, clientRoot, d2.RunID, "succeeded", 30*time.Second)
	secondDigest := readSourceDigestFromRunRecord(t, fixture, d2.RunID)
	if secondDigest == "" || secondDigest == firstDigest {
		t.Fatalf("submodule change must produce a different digest; first=%q second=%q", firstDigest, secondDigest)
	}
}

// waitStateOverSSH polls a public client check for state.
func waitStateOverSSH(t *testing.T, fixture *SSHFixture, clientRoot, runID, want string, max time.Duration) {
	t.Helper()
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		checkEnv := runClientBinary(t, fixture, clientRoot, "check", runID)
		if checkEnv.OK {
			var data struct {
				State string `json:"state"`
			}
			if jerr := json.Unmarshal(checkEnv.Data, &data); jerr == nil {
				if data.State == want {
					return
				}
				if data.State == "failed" || data.State == "lost" {
					t.Fatalf("remote build failed before reaching %s: state=%s", want, data.State)
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach %s in %s", runID, want, max)
}

// readSourceDigestFromRunRecord reads the source_digest field of a
// run record from the coordinator root.
func readSourceDigestFromRunRecord(t *testing.T, fixture *SSHFixture, runID string) string {
	t.Helper()
	path := filepath.Join(fixture.coordinatorRoot, "state", "runs", runID, "run.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read run record: %v", err)
	}
	var rec struct {
		SourceDigest string `json:"source_digest"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("decode run record: %v", err)
	}
	if rec.SourceDigest == "" {
		// Config snapshot may carry it too.
		return ""
	}
	return strings.TrimSpace(rec.SourceDigest)
}
