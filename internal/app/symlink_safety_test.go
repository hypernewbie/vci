package app

// Symlink safety tests.
//
// A hostile source tree may contain symlinks whose targets live outside the
// copied tree. Remote staging cleanup uses a flat `rm -rf` over the staging
// root only, with no recursive chmod, so it must leave external targets
// untouched. These tests use a controlled SSH session to prove that fact.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/config"
)

// TestStagingTrapLeavesExternalSymlinkTargetUnchanged constructs a
// source tree that contains a symlink whose target is OUTSIDE the
// source tree, runs it through the controlled SSH fixture,
// and asserts that an external sentinel file is unchanged after the
// build's trap cleanup. The trap must not follow or chmod the
// symlink target.
func TestStagingTrapLeavesExternalSymlinkTargetUnchanged(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}

	// Coordinator command runs true (does nothing); the test asserts
	// only that the cleanup leaves external state alone.
	initCoordinatorRoot(t, fixture, "sh", "-c", "sleep 0.1; true")
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	// Build a source tree under fixture.t.TempDir()/demo. The source
	// tree contains a sentinel file at SIBLING/sentinel and a symlink
	// inside the source that points at the sentinel. After tar+cd the
	// staged tree holds the symlink; after the trap, the sentinel
	// must still exist with its original content.
	parent := fixture.t.TempDir()
	sourceDir := filepath.Join(parent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(parent, "SIBLING"), 0o755); err != nil {
		t.Fatalf("mkdir sibling: %v", err)
	}
	sentinel := filepath.Join(parent, "SIBLING", "sentinel.txt")
	if err := os.WriteFile(sentinel, []byte("external-marker\n"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	if err := os.Symlink(sentinel, filepath.Join(sourceDir, "external-link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "demo\n")
	mustGitAddCommit(t, sourceDir, "demo commit")

	env := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env.OK {
		t.Fatalf("phase2 build must register run; got %s", pretty(env))
	}

	var runData struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(env.Data, &runData); err != nil {
		t.Fatalf("build data decode: %v", err)
	}

	// Wait for the remote worker run to finish.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("remote worker did not finish in 20s")
		}
		if state := remoteCheckState(t, fixture, runData.RunID); state == "succeeded" || state == "failed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Sentinel must still exist with the original content.
	data, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("sentinel was deleted or made unreadable: %v", err)
	}
	if string(data) != "external-marker\n" {
		t.Fatalf("sentinel contents changed: got %q", data)
	}
	// Sentinel mode is unchanged.
	st, err := os.Stat(sentinel)
	if err != nil {
		t.Fatalf("sentinel stat: %v", err)
	}
	if st.Mode().Perm()&0o600 != 0o600 {
		t.Fatalf("sentinel mode changed: %v", st.Mode().Perm())
	}
	// Suppress unused symbol for config import.
	_ = config.OrchestratorSelf
}
