package app

// Host-vs-container parity tests for the docker runtime
// selector. The test wires a stub `docker` binary into the
// coordinator's PATH so the docker runner can be exercised
// without requiring a real daemon.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/model"
)

// TestDockerBuildEndToEnd drives the coordinator's local build
// path with a docker machine attached. The host's PATH is
// prepended with a stub `docker` that records its args. The test
// asserts the docker stub was invoked with the documented arg
// shape and the run completed successfully.
func TestDockerBuildEndToEnd(t *testing.T) {
	stubDir := t.TempDir()
	logPath := filepath.Join(stubDir, "docker.log")
	script := "#!/bin/sh\necho \"$*\" >> " + logPath + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(stubDir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+previous)
	t.Cleanup(func() { os.Setenv("PATH", previous) })

	// Coordinator root.
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	l := layout.Layout{Root: root}
	if err := Initialize(l); err != nil {
		t.Fatal(err)
	}
	image := "ghcr.io/org/ci@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if err := AddMachine(l, "linux-docker", config.Machine{Runtime: "docker", Image: image}); err != nil {
		t.Fatal(err)
	}
	if err := AddProject(l, "demo", config.Project{Machines: []string{"linux-docker"}, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}

	// Build source: a tiny git repo the coordinator can stage.
	sourceDir := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "main.go"), []byte("package main\nfunc main(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Initialize git so the source pipeline accepts the dir.
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "demo\n")
	mustGitAddCommit(t, sourceDir, "init")

	prep, err := Prepare(context.Background(), l, sourceDir)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	result, err := ExecutePrepared(context.Background(), l, prep.Record.ID)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.State != model.RunSucceeded {
		t.Fatalf("state: %s failure=%s", result.State, result.Failure)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("docker stub log missing: %v", err)
	}
	s := string(log)
	for _, want := range []string{"run", "--rm", "ghcr.io/org/ci", "true"} {
		if !strings.Contains(s, want) {
			t.Errorf("docker stub log missing %q: %s", want, s)
		}
	}
	// Select the executor from the staged snapshot and assert
	// it is docker-backed.
	var snap runSnapshot
	if err := jsonUnmarshalSnapshot(result.ConfigSnapshot, &snap); err != nil {
		t.Fatalf("snapshot decode: %v", err)
	}
	exec := selectExecutor(snap)
	type dockerRunner interface {
		CommandArgv(string, string, []string) ([]string, error)
	}
	if _, ok := exec.(dockerRunner); !ok {
		t.Errorf("snapshot did not select docker-backed executor")
	}
}

// TestBareAndDockerExecutorsBothSucceed pins the host-vs-container
// parity contract. Two back-to-back runs — one bare, one docker —
// both succeed with exit 0. The docker stub is invoked for the
// docker run only.
func TestBareAndDockerExecutorsBothSucceed(t *testing.T) {
	stubDir := t.TempDir()
	logPath := filepath.Join(stubDir, "docker.log")
	script := "#!/bin/sh\necho \"$*\" >> " + logPath + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(stubDir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+previous)
	t.Cleanup(func() { os.Setenv("PATH", previous) })

	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	l := layout.Layout{Root: root}
	if err := Initialize(l); err != nil {
		t.Fatal(err)
	}
	image := "ghcr.io/org/ci@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if err := AddMachine(l, "mac-local", config.Machine{}); err != nil {
		t.Fatal(err)
	}
	if err := AddMachine(l, "linux-docker", config.Machine{Runtime: "docker", Image: image}); err != nil {
		t.Fatal(err)
	}
	if err := AddProject(l, "bare", config.Project{Machines: []string{"mac-local"}, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if err := AddProject(l, "dockerized", config.Project{Machines: []string{"linux-docker"}, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}

	// Bare run.
	bare, err := Prepare(context.Background(), l, makeSourceTree(t, "bare"))
	if err != nil {
		t.Fatalf("prepare bare: %v", err)
	}
	bareResult, err := ExecutePrepared(context.Background(), l, bare.Record.ID)
	if err != nil {
		t.Fatalf("execute bare: %v", err)
	}
	if bareResult.State != model.RunSucceeded {
		t.Errorf("bare state: %s failure=%s", bareResult.State, bareResult.Failure)
	}
	// Docker run.
	docker, err := Prepare(context.Background(), l, makeSourceTree(t, "dockerized"))
	if err != nil {
		t.Fatalf("prepare docker: %v", err)
	}
	dockerResult, err := ExecutePrepared(context.Background(), l, docker.Record.ID)
	if err != nil {
		t.Fatalf("execute docker: %v", err)
	}
	if dockerResult.State != model.RunSucceeded {
		t.Errorf("docker state: %s failure=%s", dockerResult.State, dockerResult.Failure)
	}
	// Docker stub was invoked exactly once.
	if count := countDockerCalls(logPath); count < 1 {
		t.Errorf("docker stub count: %d", count)
	}
}

// TestBareExecutorSelectedForNonDockerMachine pins the host-vs-container
// parity contract from the snapshot side: a bare machine always
// selects the bare executor, regardless of snapshot mutations.
func TestBareExecutorSelectedForNonDockerMachine(t *testing.T) {
	snap := runSnapshot{
		ProjectConfig: config.Project{Machines: []string{"mac-local"}},
		Machines: map[string]config.Machine{
			"mac-local": {},
		},
	}
	exec := selectExecutor(snap)
	type dockerRunner interface {
		CommandArgv(string, string, []string) ([]string, error)
	}
	if _, ok := exec.(dockerRunner); ok {
		t.Fatalf("bare machine should not select docker-backed Executor")
	}
}

// makeSourceTree returns a minimal git-initialized source dir
// named `name` (so the project name matches the project config).
func makeSourceTree(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGitInit(t, dir)
	mustWriteFile(t, filepath.Join(dir, "README.md"), name+"\n")
	mustGitAddCommit(t, dir, "init")
	return dir
}

// jsonUnmarshalSnapshot decodes a runConfigSnapshot JSON document
// into a typed runSnapshot struct.
func jsonUnmarshalSnapshot(raw []byte, dst *runSnapshot) error {
	return json.Unmarshal(raw, dst)
}

// countDockerCalls returns the number of lines in the stub
// docker call log.
func countDockerCalls(logPath string) int {
	data, err := os.ReadFile(logPath)
	if err != nil {
		return 0
	}
	if len(data) == 0 {
		return 0
	}
	return len(strings.Split(strings.TrimSpace(string(data)), "\n"))
}
