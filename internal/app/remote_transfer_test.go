package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypernewbie/vci/internal/source"
)

func TestBuildOverStagingRejectsTarFailureAfterRemoteResponse(t *testing.T) {
	bin := t.TempDir()
	writeExecutable(t, filepath.Join(bin, "tar"), "#!/bin/sh\nexit 7\n")
	writeExecutable(t, filepath.Join(bin, "ssh"), "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"schema_version\":1,\"command\":\"build\",\"ok\":true,\"data\":{}}'\n")
	t.Setenv("PATH", bin)

	input := source.SourceInput{Root: t.TempDir(), ProjectName: "demo", Files: []string{"README.md"}}
	key := safeTestKey("demo", "sha256-0000000000000000000000000000000000000000000000000000000000000000")
	raw, remote, _, err := buildOverStaging(context.Background(), "coordinator", input, t.TempDir(), key)
	if !remote || err == nil {
		t.Fatalf("tar failure must reject the transfer: remote=%v err=%v", remote, err)
	}
	if raw != nil || !strings.Contains(err.Error(), "archive source") {
		t.Fatalf("tar failure must not return a remote response: raw=%q err=%v", raw, err)
	}
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write executable %s: %v", path, err)
	}
}
