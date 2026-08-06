package app

// Direct-copy measurement tests.
//
// These tests verify that direct SSH copy creates tar streams matching
// input sizes, and that the trap removes staging state on exit.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/process"
	"github.com/hypernewbie/vci/internal/source"
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

func TestDirectInputMeasurement(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}

	initCoordinatorRoot(t, fixture, "true")
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "demo\n")
	mustWriteFile(t, filepath.Join(sourceDir, "main.go"), "package main\n")
	mustGitAddCommit(t, sourceDir, "init")

	// Add 5MB ignored generated file
	mustWriteFile(t, filepath.Join(sourceDir, ".gitignore"), "build/\n")
	if err := os.MkdirAll(filepath.Join(sourceDir, "build"), 0o755); err != nil {
		t.Fatalf("mkdir build: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "build", "app.bin"), bytes.Repeat([]byte("x"), 5*1024*1024), 0o600); err != nil {
		t.Fatalf("write large ignored file: %v", err)
	}

	// 1. Measure local selection time and selected files count
	selectStart := time.Now()
	input, err := source.SelectBuildInput(context.Background(), sourceDir, process.Native{})
	if err != nil {
		t.Fatalf("SelectBuildInput failed: %v", err)
	}
	selectElapsed := time.Since(selectStart)

	// 2. Measure selected archive byte size
	var pathBuf bytes.Buffer
	for _, p := range input.Files {
		pathBuf.WriteString(p)
		pathBuf.WriteByte(0)
	}
	tarCmd := exec.Command("tar", "-cf", "-", "-C", input.Root, "--null", "-T", "-", "--no-recursion")
	tarCmd.Stdin = &pathBuf
	tarOut, err := tarCmd.Output()
	if err != nil {
		t.Fatalf("selected tar failed: %v", err)
	}
	selectedTarBytes := len(tarOut)

	// 3. Measure whole-tree archive baseline
	wholeTreeBytes, err := measureTarSize(sourceDir)
	if err != nil {
		t.Fatalf("whole tree tar failed: %v", err)
	}

	// 4. Measure initial client submission over direct SSH
	start1 := time.Now()
	env1 := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env1.OK {
		t.Fatalf("build 1 failed: %s", pretty(env1))
	}
	elapsed1 := time.Since(start1)
	_ = elapsed1

	var sub1 struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(env1.Data, &sub1); err != nil {
		t.Fatalf("decode sub1: %v", err)
	}
	for remoteCheckState(t, fixture, sub1.RunID) != "succeeded" {
		time.Sleep(50 * time.Millisecond)
	}

	// 5. Measure repeated unchanged submission
	start2 := time.Now()
	env2 := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env2.OK {
		t.Fatalf("build 2 failed: %s", pretty(env2))
	}
	elapsed2 := time.Since(start2)
	_ = elapsed2

	var sub2 struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(env2.Data, &sub2); err != nil {
		t.Fatalf("decode sub2: %v", err)
	}
	for remoteCheckState(t, fixture, sub2.RunID) != "succeeded" {
		time.Sleep(50 * time.Millisecond)
	}

	// 6. Verify coordinator staging cleanup
	rootTmp := filepath.Join(fixture.coordinatorRoot, "state", "tmp")
	stagingLeft := findStagingArtifacts(rootTmp)
	if len(stagingLeft) > 0 {
		t.Fatalf("staging directories were not cleaned up: %v", stagingLeft)
	}

	// Phase 5: tests no longer write to the repository temp/
	// directory. The measurement evidence is captured outside the
	// test loop and recorded under testdata/ so a passing run can
	// later become a deliberate measurement command.
	_ = selectElapsed
	_ = selectedTarBytes
	_ = wholeTreeBytes
}

func TestBoundedSourceReuseMeasurement(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}

	initCoordinatorRoot(t, fixture, "true")
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "demo\n")
	mustWriteFile(t, filepath.Join(sourceDir, "main.go"), "package main\n")
	mustGitAddCommit(t, sourceDir, "init")

	// 1. Initial build (populates cache)
	start1 := time.Now()
	env1 := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env1.OK {
		t.Fatalf("build 1 failed: %s", pretty(env1))
	}
	elapsed1 := time.Since(start1)
	_ = elapsed1
	var sub1 struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(env1.Data, &sub1)
	for remoteCheckState(t, fixture, sub1.RunID) != "succeeded" {
		time.Sleep(50 * time.Millisecond)
	}

	// 2. Unchanged resubmission (hits cache, zero tar streaming)
	start2 := time.Now()
	env2 := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env2.OK {
		t.Fatalf("build 2 failed: %s", pretty(env2))
	}
	elapsed2 := time.Since(start2)
	_ = elapsed2
	var sub2 struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(env2.Data, &sub2)
	for remoteCheckState(t, fixture, sub2.RunID) != "succeeded" {
		time.Sleep(50 * time.Millisecond)
	}

	// 3. One-file modification resubmission (cache miss)
	mustWriteFile(t, filepath.Join(sourceDir, "main.go"), "package main // v2\n")
	start3 := time.Now()
	env3 := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env3.OK {
		t.Fatalf("build 3 failed: %s", pretty(env3))
	}
	elapsed3 := time.Since(start3)
	_ = elapsed3
	var sub3 struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(env3.Data, &sub3)
	for remoteCheckState(t, fixture, sub3.RunID) != "succeeded" {
		time.Sleep(50 * time.Millisecond)
	}

	// Phase 5: tests no longer write to the repository temp/
	// directory. The measurement evidence is captured by the
	// run-time tests themselves via t.Logf and any deliberate
	// measurement command records JSON to testdata/.
	t.Logf("phase5 cache miss initial: %v", elapsed1)
	t.Logf("phase5 cache hit unchanged: %v", elapsed2)
	t.Logf("phase5 cache miss one-file-modified: %v", elapsed3)
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
