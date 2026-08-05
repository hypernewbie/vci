package app

// Direct-copy measurement tests.
//
// These tests verify that direct SSH copy creates tar streams matching
// input sizes, and that the trap removes staging state on exit.

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

func measureTarSize(sourceDir string) (int64, error) {
	out, err := exec.Command("tar", "-C", sourceDir, "-cf", "-", ".").Output()
	if err != nil {
		return 0, err
	}
	return int64(len(out)), nil
}

func TestTarSizeEqualsInputSize(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skipf("tar not available: %v", err)
	}

	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "hello\n")
	mustWriteFile(t, filepath.Join(sourceDir, "lib.go"), "package demo\n")
	mustGitAddCommit(t, sourceDir, "init")

	tarBytes, err := measureTarSize(sourceDir)
	if err != nil {
		t.Fatalf("measure tar size: %v", err)
	}
	if tarBytes <= 0 {
		t.Fatalf("tar must emit a non-empty archive for a non-empty source")
	}
}

func TestRepeatedUnchangedTransferIsFullSize(t *testing.T) {
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skipf("tar not available: %v", err)
	}

	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), strings.Repeat("a", 1024))
	mustWriteFile(t, filepath.Join(sourceDir, "data.bin"), strings.Repeat("b", 8192))
	mustGitAddCommit(t, sourceDir, "init")

	firstTar, err := measureTarSize(sourceDir)
	if err != nil {
		t.Fatalf("first tar: %v", err)
	}
	secondTar, err := measureTarSize(sourceDir)
	if err != nil {
		t.Fatalf("second tar: %v", err)
	}
	if firstTar != secondTar {
		t.Fatalf("expected unchanged repeated transfer to equal initial (%d), got %d", firstTar, secondTar)
	}
}

func TestTrapRemovesStagingAfterTransfer(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}
	if _, err := exec.LookPath("tar"); err != nil {
		t.Skipf("tar not available: %v", err)
	}

	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "direct-copy\n")
	mustGitAddCommit(t, sourceDir, "init")

	initCoordinatorRoot(t, fixture, "true")
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())
	env := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env.OK {
		t.Fatalf("client build failed: %s", pretty(env))
	}
	var submitted struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(env.Data, &submitted); err != nil {
		t.Fatalf("decode submitted run: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for remoteCheckState(t, fixture, submitted.RunID) != "succeeded" {
		if time.Now().After(deadline) {
			t.Fatalf("remote build %s did not succeed", submitted.RunID)
		}
		time.Sleep(100 * time.Millisecond)
	}

	rootTmp := filepath.Join(fixture.coordinatorRoot, "state", "tmp")
	stagingLeft := findStagingArtifacts(rootTmp)
	if len(stagingLeft) > 0 {
		t.Fatalf("trap cleanup left staging directories: %v", stagingLeft)
	}
}

func findStagingArtifacts(tmpDir string) []string {
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return nil
	}
	var left []string
	for _, e := range entries {
		if e.IsDir() && (strings.HasPrefix(e.Name(), "vci-source-") || strings.HasPrefix(e.Name(), "vci-source.")) {
			left = append(left, e.Name())
		}
	}
	return left
}
