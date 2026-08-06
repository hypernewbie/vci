package app

import (
	"strings"
	"testing"

	"github.com/hypernewbie/vci/internal/source"
)

// safeTestKey returns a digest + project pair that already passes
// validateCacheKey so existing shape tests still build the staging
// fragment without depending on a real digest computation.
func safeTestKey(project, digest string) safeCacheKey {
	return safeCacheKey{Digest: digest, Project: project}
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
