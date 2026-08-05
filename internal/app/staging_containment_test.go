package app

import (
	"strings"
	"testing"
)

// Staging containment tests.
//
// The remote staging directory must live under the remote Vci root's
// TempDir with private permissions, must use trap-based cleanup that
// covers every exit path, and the reaper must sweep stale transfer
// directories without touching unrelated content. No production Go file
// changes for these tests; they assert on the staged shell fragment.

// TestStagingShellScriptPlacesUnderRemoteVciRoot asserts the remote
// shell fragment derives the staging path from the remote's own Vci
// root (or its default) and never hard-codes an absolute /tmp staging.
func TestStagingShellScriptPlacesUnderRemoteVciRoot(t *testing.T) {
	script := stagingShellScript("demo")
	if !strings.Contains(script, `${VCI_ROOT:-$HOME/.vci}`) {
		t.Fatalf("script must derive remote Vci root; got:\n%s", script)
	}
	if !strings.Contains(script, "demo") {
		t.Fatalf("script must use the project basename as staging prefix; got:\n%s", script)
	}
	if strings.Contains(script, "/tmp/vci-build-") {
		t.Fatalf("script must not use the old hard-coded /tmp/vci-build staging path; got:\n%s", script)
	}
	// The remote staging contains a PROJECT subdirectory whose name
	// matches repoName, so the remote `vci build .` finds the
	// project by git-root basename lookup.
	if !strings.Contains(script, `PROJECT="$STAGING/demo"`) {
		t.Fatalf("script must nest project under randomized staging root; got:\n%s", script)
	}
}

// TestStagingShellScriptDoesNotNestUnderSource asserts the staging
// layout no longer uses the old `$STAGING/source` directory.
func TestStagingShellScriptDoesNotNestUnderSource(t *testing.T) {
	script := stagingShellScript("demo")
	if strings.Contains(script, "$STAGING/source") {
		t.Fatalf("script must not use the obsolete $STAGING/source layout; got:\n%s", script)
	}
}

// TestStagingShellScriptUsesMktempForUniqueness asserts that the
// staging directory is created via `mktemp -d` so concurrent or
// rapid re-entry stages cannot collide on the same remote.
func TestStagingShellScriptUsesMktempForUniqueness(t *testing.T) {
	script := stagingShellScript("demo")
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
	for _, unsafe := range []string{"", ".", "..", ".hidden", "with/path", "$(touch owned)", "`id`", `x"$(id)"`} {
		script := stagingShellScript(unsafe)
		if strings.ContainsAny(unsafe, "$/`\\\"") && strings.Contains(script, unsafe) {
			t.Fatalf("unsafe name %q leaked into shell fragment:\n%s", unsafe, script)
		}
		if !strings.Contains(script, `PROJECT="$STAGING/source"`) {
			t.Fatalf("unsafe name %q must use the literal fallback:\n%s", unsafe, script)
		}
	}
}

func TestBuildOverStagingRejectsUnsafeRepositoryName(t *testing.T) {
	_, remote, err := buildOverStaging(t.Context(), "unused-host", t.TempDir(), "$(touch owned)")
	if !remote || err == nil {
		t.Fatalf("unsafe repository name must fail before tar or SSH: remote=%v err=%v", remote, err)
	}
}

// TestStagingShellScriptTrapsEveryExit asserts that the trap covers
// normal exit, error, interrupt, and termination so a dropped SSH
// connection or interrupt does not leak the staging directory.
func TestStagingShellScriptTrapsEveryExit(t *testing.T) {
	script := stagingShellScript("demo")
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
	script := stagingShellScript("demo")
	if strings.Contains(script, "chmod -R") {
		t.Fatalf("staging cleanup must not run recursive chmod (follows symlinks); got:\n%s", script)
	}
}

// TestStagingShellScriptDoesNotExpandClientSourceText asserts the
// fragment never interpolates client-controlled source paths as shell
// syntax; only literal-quoted values can appear.
func TestStagingShellScriptDoesNotExpandClientSourceText(t *testing.T) {
	script := stagingShellScript("demo")
	for _, banned := range []string{"$source", "$repo", "$path"} {
		if strings.Contains(script, banned) {
			t.Fatalf("script must not interpolate client source field %q; got:\n%s", banned, script)
		}
	}
}
