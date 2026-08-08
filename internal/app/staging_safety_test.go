package app

// Staging safety tests.
//
// The literal direct-copy path is tar-over-SSH into the staging
// directory owned by the remote Vci root. These tests assert the
// composition contains only ordinary tar, ssh, and the public vci
// invocation; preserves Git metadata; applies no fixed size-based
// timeout; keeps the staging path under the remote Vci root with a
// unique mktemp directory; refuses unsafe repository names before
// tar or SSH; traps every exit during cleanup without recursive
// chmod or client source interpolation; and leaves external symlink
// targets untouched.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/source"
)

// safeTestKey returns a digest + project pair that already passes
// validateCacheKey so existing shape tests still build the staging
// fragment without depending on a real digest computation.
func safeTestKey(project, digest string) safeCacheKey {
	return safeCacheKey{Digest: digest, Project: project}
}

// Staging composition tests.
//
// The literal direct-copy path is tar-over-SSH into the staging
// directory owned by the remote Vci root. These tests assert the
// composition contains only ordinary tar, ssh, and the public vci
// invocation; preserves Git metadata; and applies no fixed size-based
// timeout.

// TestStagingShellUsesOnlyTarSshVci asserts the staged shell fragment
// contains only the literal tar, ssh (via system remote shell), and
// public vci invocation. It must not add a source receiver, framed
// transport, rsync-specific state, or any other external tool.
func TestStagingShellUsesOnlyTarSshVci(t *testing.T) {
	script := stagingShellScript(safeTestKey("demo", "sha256-0000000000000000000000000000000000000000000000000000000000000000"))
	mustContain := []string{"set -eu", "mkdir -m 700", "tar -C \"$PROJECT\" -xpf -", "vci build ."}
	for _, want := range mustContain {
		if !strings.Contains(script, want) {
			t.Fatalf("shell missing required fragment %q; got:\n%s", want, script)
		}
	}
	for _, banned := range []string{"rsync", "ssh -o ", "vci build --stdio", "framed", "relay"} {
		if strings.Contains(script, banned) {
			t.Fatalf("shell must not contain %q; got:\n%s", banned, script)
		}
	}
}

// TestStagingShellPreservesGitMetadata asserts the script extracts the
// tar archive verbatim into the staging project directory. Git
// metadata (.git/) is included because tar -xf - does not filter;
// the existing public 'vci build .' source-discovery rule (git
// rev-parse --show-toplevel) finds the repository root on the
// remote because the staging project directory IS the project
// root.
func TestStagingShellPreservesGitMetadata(t *testing.T) {
	script := stagingShellScript(safeTestKey("demo", "sha256-0000000000000000000000000000000000000000000000000000000000000000"))
	// The extract line must be a literal `tar -xf -` with no
	// excludes, targeting the project subdirectory.
	if !strings.Contains(script, `tar -C "$PROJECT" -xpf -`) {
		t.Fatalf("extract line missing or filtered; got:\n%s", script)
	}
	for _, banned := range []string{"--exclude", "--exclude-vcs", "--exclude-vcs-ignores", "--no-recursion"} {
		if strings.Contains(script, banned) {
			t.Fatalf("shell must not exclude files: %q; got:\n%s", banned, script)
		}
	}
}

// TestRemoteCommandsHaveNoArbitraryTimeout asserts that the public
// command proxy and the staged transfer do not apply a fixed
// per-call timeout. Cancellation is owned by the caller context and
// the real SSH failure.
func TestRemoteCommandsHaveNoArbitraryTimeout(t *testing.T) {
	body := stagingShellScript(safeTestKey("demo", "sha256-0000000000000000000000000000000000000000000000000000000000000000"))
	if strings.Contains(body, "sleep") || strings.Contains(body, "timeout") {
		t.Fatalf("staging shell must not embed a timeout; got:\n%s", body)
	}
}

// TestStagingShellScriptPlacesUnderRemoteVciRoot asserts the remote
// shell fragment derives the staging path from the remote's own Vci
// root (or its default) and never hard-codes an absolute /tmp staging.
func TestStagingShellScriptPlacesUnderRemoteVciRoot(t *testing.T) {
	script := stagingShellScript(safeTestKey("demo", "sha256-0000000000000000000000000000000000000000000000000000000000000000"))
	if !strings.Contains(script, `${VCI_ROOT:-$HOME/.vci}`) {
		t.Fatalf("script must derive remote Vci root; got:\n%s", script)
	}
	if !strings.Contains(script, "demo") {
		t.Fatalf("script must use the project basename as staging prefix; got:\n%s", script)
	}
	if strings.Contains(script, "/tmp/vci-build-") {
		t.Fatalf("script must not use the old hard-coded /tmp/vci-build staging path; got:\n%s", script)
	}
}

// TestStagingShellScriptDoesNotNestUnderSource asserts the staging
// layout no longer uses the old `$STAGING/source` directory.
func TestStagingShellScriptDoesNotNestUnderSource(t *testing.T) {
	script := stagingShellScript(safeTestKey("demo", "sha256-0000000000000000000000000000000000000000000000000000000000000000"))
	if strings.Contains(script, "$STAGING/source") {
		t.Fatalf("script must not use the obsolete $STAGING/source layout; got:\n%s", script)
	}
}

// TestStagingShellScriptUsesMktempForUniqueness asserts that the
// staging directory is created via `mktemp -d` so concurrent or
// rapid re-entry stages cannot collide on the same remote.
func TestStagingShellScriptUsesMktempForUniqueness(t *testing.T) {
	script := stagingShellScript(safeTestKey("demo", "sha256-0000000000000000000000000000000000000000000000000000000000000000"))
	if !strings.Contains(script, "mktemp -d") {
		t.Fatalf("script must create staging via mktemp -d for uniqueness; got:\n%s", script)
	}
	if !strings.Contains(script, "-p \"$TMP_PARENT\"") {
		t.Fatalf("script must pin mktemp's parent directory under the remote Vci root; got:\n%s", script)
	}
}

// TestStagingShellScriptDoesNotInterpolateUnsafeName asserts that direct
// callers cannot make an unsafe repository name part of the shell fragment.
func TestStagingShellScriptDoesNotInterpolateUnsafeName(t *testing.T) {
	for _, unsafe := range []string{"$(touch owned)", "`id`", `x"$(id)"`, "with/path"} {
		script := stagingShellScript(safeTestKey(unsafe, "sha256-0000000000000000000000000000000000000000000000000000000000000000"))
		if strings.ContainsAny(unsafe, "$/`\\\"") && strings.Contains(script, unsafe) {
			t.Fatalf("unsafe name %q leaked into shell fragment:\n%s", unsafe, script)
		}
		if !strings.HasPrefix(script, "exit 1") {
			t.Fatalf("unsafe name %q must force the script to refuse:\n%s", unsafe, script)
		}
	}
}

func TestBuildOverStagingRejectsUnsafeRepositoryName(t *testing.T) {
	input := source.SourceInput{Root: t.TempDir(), ProjectName: "$(touch owned)", Files: []string{"README.md"}}
	key := safeTestKey(input.ProjectName, "sha256-0000000000000000000000000000000000000000000000000000000000000000")
	_, remote, _, err := buildOverStaging(t.Context(), "unused-host", input, t.TempDir(), key)
	if !remote || err == nil {
		t.Fatalf("unsafe repository name must fail before tar or SSH: remote=%v err=%v", remote, err)
	}
}

// TestStagingShellScriptTrapsEveryExit asserts that the trap covers
// normal exit, error, interrupt, and termination so a dropped SSH
// connection or interrupt does not leak the staging directory.
func TestStagingShellScriptTrapsEveryExit(t *testing.T) {
	script := stagingShellScript(safeTestKey("demo", "sha256-0000000000000000000000000000000000000000000000000000000000000000"))
	if !strings.Contains(script, "trap '") || !strings.Contains(script, "rm -rf") {
		t.Fatalf("script must trap and remove staging; got:\n%s", script)
	}
	for _, signal := range []string{"EXIT", "INT", "TERM"} {
		if !strings.Contains(script, signal) {
			t.Fatalf("script must trap %s; got:\n%s", signal, script)
		}
	}
}

// TestStagingShellScriptDoesNotRecursiveChmod asserts that the trap
// only removes the staging tree and never runs a recursive chmod.
// A recursive chmod walks the tree and follows symlinks; a hostile
// source tree could include a symlink whose target lives outside
// the staging directory. The simple `rm -rf` on the staging root
// only removes the staging directory itself.
func TestStagingShellScriptDoesNotRecursiveChmod(t *testing.T) {
	script := stagingShellScript(safeTestKey("demo", "sha256-0000000000000000000000000000000000000000000000000000000000000000"))
	if strings.Contains(script, "chmod -R") {
		t.Fatalf("staging cleanup must not run recursive chmod (follows symlinks); got:\n%s", script)
	}
}

// TestStagingShellScriptDoesNotExpandClientSourceText asserts the
// fragment never interpolates client-controlled source paths as shell
// syntax; only literal-quoted values can appear.
func TestStagingShellScriptDoesNotExpandClientSourceText(t *testing.T) {
	script := stagingShellScript(safeTestKey("demo", "sha256-0000000000000000000000000000000000000000000000000000000000000000"))
	for _, banned := range []string{"$source", "$repo", "$path"} {
		if strings.Contains(script, banned) {
			t.Fatalf("script must not interpolate client source field %q; got:\n%s", banned, script)
		}
	}
}

// Symlink safety tests.
//
// A hostile source tree may contain symlinks whose targets live outside the
// copied tree. Remote staging cleanup uses a flat `rm -rf` over the staging
// root only, with no recursive chmod, so it must leave external targets
// untouched. These tests use a controlled SSH session to prove that fact.

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
