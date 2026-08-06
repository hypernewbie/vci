package app

// Phase 0 — direct staging recovery proof.
//
// These tests pin the exact staged tree received by the remote public
// `vci build .` after the Plan 8 Fix staging regression. The staging
// shell must extract the tar into the Vci-owned project directory and
// run the public command from inside that directory, so the staged
// tree is the repository root at the point `source.Discover` executes.
//
// The probe replaces only the final remote `vci` invocation with a
// script that asserts the tree in place of the real public command;
// everything before that boundary (real client binary, real system
// ssh, real sshd, real mktemp/trap/tar) is the production path.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStagedTreeAtPublicCommandBoundary drives the full public client
// build path through controlled SSH and replaces the final remote
// `vci build .` invocation with a probe that asserts, at the point the
// public command would execute:
//
//   - .git/HEAD, .git/objects/ and .git/refs/ are present and the
//     staged tree resolves as a Git repository (git rev-parse works);
//   - a tracked input file, a modified tracked file, and an untracked
//     file carry their expected bytes;
//   - an executable file keeps its mode and a symlink keeps its target;
//   - private .git/config and ignored files were never staged.
//
// The probe then returns the standard Vci envelope so the client sees
// a successful submission.
func TestStagedTreeAtPublicCommandBoundary(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}

	remoteVciPath := filepath.Join(fixture.homeDir, "bin", "vci")
	originalBinary, err := os.ReadFile(remoteVciPath)
	if err != nil {
		t.Fatalf("read original remote vci: %v", err)
	}
	t.Cleanup(func() { _ = os.WriteFile(remoteVciPath, originalBinary, 0o755) })

	// The probe runs in place of `vci build .` from inside the staged
	// project directory. Every assertion is a hard failure so a wrong
	// staged tree surfaces the exact missing fact on stderr and a
	// non-zero exit (which the client reports as an SSH failure).
	probe := `#!/bin/sh
set -eu
root=$(git rev-parse --show-toplevel)
[ -n "$root" ]
[ -f "$root/.git/HEAD" ]
[ -d "$root/.git/objects" ]
[ -d "$root/.git/refs" ]
[ "$(cat "$root/tracked.txt")" = "tracked" ]
[ "$(cat "$root/modified.txt")" = "modified content" ]
[ "$(cat "$root/untracked.txt")" = "untracked" ]
[ ! -f "$root/.git/config" ]
[ ! -f "$root/ignored.log" ]
[ ! -d "$root/ignored_dir" ]
[ -x "$root/script.sh" ]
[ -L "$root/link.txt" ]
[ "$(readlink "$root/link.txt")" = "tracked.txt" ]
printf '%s\n' '{"schema_version":1,"command":"build","ok":true,"data":{"run_id":"run_staged_probe","state":"succeeded"}}'
`
	if err := os.WriteFile(remoteVciPath, []byte(probe), 0o755); err != nil {
		t.Fatalf("write probe vci: %v", err)
	}

	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}

	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "tracked.txt"), "tracked")
	mustWriteFile(t, filepath.Join(sourceDir, "modified.txt"), "original")
	mustWriteFile(t, filepath.Join(sourceDir, "deleted.txt"), "deleted")
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "# demo\n")
	mustGitAddCommit(t, sourceDir, "init")

	// Private .git/config is written after the commit so the commit
	// uses the fixture identity; the sentinel must not reach the
	// staged tree.
	mustWriteFile(t, filepath.Join(sourceDir, ".git", "config"), "[user]\n  name = PrivateSentinelUser\n")

	// Modified tracked file (current working-tree bytes must arrive).
	mustWriteFile(t, filepath.Join(sourceDir, "modified.txt"), "modified content")
	// Locally deleted tracked file must not arrive.
	if err := os.Remove(filepath.Join(sourceDir, "deleted.txt")); err != nil {
		t.Fatalf("remove deleted.txt: %v", err)
	}
	// Untracked non-ignored file must arrive.
	mustWriteFile(t, filepath.Join(sourceDir, "untracked.txt"), "untracked")
	// Ignored file and directory must not arrive.
	mustWriteFile(t, filepath.Join(sourceDir, ".gitignore"), "ignored.log\nignored_dir/\n")
	mustWriteFile(t, filepath.Join(sourceDir, "ignored.log"), "secret")
	if err := os.MkdirAll(filepath.Join(sourceDir, "ignored_dir"), 0o755); err != nil {
		t.Fatalf("mkdir ignored_dir: %v", err)
	}
	mustWriteFile(t, filepath.Join(sourceDir, "ignored_dir", "cached.bin"), "bin")
	// Executable mode must arrive.
	execPath := filepath.Join(sourceDir, "script.sh")
	mustWriteFile(t, execPath, "#!/bin/sh\necho hi")
	if err := os.Chmod(execPath, 0o755); err != nil {
		t.Fatalf("chmod script.sh: %v", err)
	}
	// Symlink target must arrive as a symlink.
	if err := os.Symlink("tracked.txt", filepath.Join(sourceDir, "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	env := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env.OK {
		t.Fatalf("staged probe build must succeed; got %s", pretty(env))
	}
	var data struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("json decode run_id: %v", err)
	}
	if !strings.HasPrefix(data.RunID, "run_") {
		t.Fatalf("staged probe must return a run id; got %q", data.RunID)
	}

	// The staging dir is removed by the trap after the probe exits.
	rootTmp := filepath.Join(fixture.coordinatorRoot, "state", "tmp")
	if left := findStagingArtifacts(rootTmp); len(left) > 0 {
		t.Fatalf("staging directories were not cleaned up: %v", left)
	}
}

// TestStagedTreeRecognizedBySourceDiscover proves the staged tree is a
// Git repository at the point the public command executes by running
// the real remote `vci build .` against it: the build can only reach
// the coordinator project command if `source.Discover` accepted the
// staged `.git` markers.
func TestStagedTreeRecognizedBySourceDiscover(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}

	initCoordinatorRoot(t, fixture, "sh", "-c", "echo 'DISCOVER ACCEPTED STAGED TREE'")
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "# demo\n")
	mustGitAddCommit(t, sourceDir, "init")

	env := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env.OK {
		t.Fatalf("real remote build must accept the staged tree; got %s", pretty(env))
	}
	var data struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("json decode run_id: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for remoteCheckState(t, fixture, data.RunID) != "succeeded" {
		if time.Now().After(deadline) {
			t.Fatalf("remote build %s did not succeed", data.RunID)
		}
		if remoteCheckState(t, fixture, data.RunID) == "failed" {
			t.Fatalf("remote build %s failed against the staged tree", data.RunID)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
