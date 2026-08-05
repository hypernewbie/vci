package app

import (
	"strings"
	"testing"
)

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
	script := stagingShellScript("demo")
	mustContain := []string{"set -eu", "mkdir -m 700", "tar -C \"$PROJECT\" -xf -", "cd \"$PROJECT\"", "vci build .", "PROJECT=\"$STAGING/demo\""}
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
	script := stagingShellScript("demo")
	// The extract line must be a literal `tar -xf -` with no
	// excludes, targeting the project subdirectory.
	if !strings.Contains(script, `tar -C "$PROJECT" -xf -`) {
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
	body := stagingShellScript("demo")
	if strings.Contains(body, "sleep") || strings.Contains(body, "timeout") {
		t.Fatalf("staging shell must not embed a timeout; got:\n%s", body)
	}
}
