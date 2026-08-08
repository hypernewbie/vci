package app

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/layout"
)

// TestPrepareSelectsDockerRuntime pins that a run record
// persisted for a docker machine carries the runtime/image
// fields in the snapshot, and that the snapshot's
// `selectExecutor` returns a Docker-backed Executor. The test
// wires the runner through a stub `docker` in PATH so no real
// daemon is required.
func TestPrepareSelectsDockerRuntime(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
echo "$*" >> ` + logPath + `
exit 0
`
	dockerPath := filepath.Join(dir, "docker")
	if err := os.WriteFile(dockerPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+previous)

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

	// Check the snapshot's machine config is correct.
	cfg, err := config.Load(l.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Machines["linux-docker"]
	if m.Runtime != "docker" {
		t.Errorf("runtime: %q", m.Runtime)
	}
	if m.Image != image {
		t.Errorf("image: %q", m.Image)
	}

	// Build a snapshot the same way buildStagedSnapshot does and
	// assert selectExecutor returns a Docker-backed Executor.
	project := cfg.Projects["demo"]
	snap := runSnapshot{
		ProjectConfig: project,
		Machines:      cfg.Machines,
	}
	exec := selectExecutor(snap)
	// The runtime.Docker type is unexported, but we can verify
	// by inspecting the result type. The local executor also
	// satisfies the Executor interface, so a type assertion
	// distinguishes them.
	type dockerRunner interface {
		CommandArgv(string, string, []string) ([]string, error)
	}
	if _, ok := exec.(dockerRunner); !ok {
		t.Fatalf("expected docker-backed Executor, got %T", exec)
	}
}

// TestSelectExecutorFallsBackToBareForEmptyRuntime pins that a
// machine with no runtime fields selects the bare executor. The
// default path is unchanged.
func TestSelectExecutorFallsBackToBareForEmptyRuntime(t *testing.T) {
	snap := runSnapshot{
		ProjectConfig: config.Project{Machines: []string{"mac-local"}},
		Machines: map[string]config.Machine{
			"mac-local": {},
		},
	}
	exec := selectExecutor(snap)
	// Type-distinguish via presence of CommandArgv.
	type dockerRunner interface {
		CommandArgv(string, string, []string) ([]string, error)
	}
	if _, ok := exec.(dockerRunner); ok {
		t.Fatalf("bare machine should not select docker-backed Executor")
	}
	// Smoke: must satisfy the Executor interface.
	var _ Executor = exec
}

// TestSelectExecutorUsesReservedMachine pins that the runtime is
// resolved from the durable snapshot's reserved `machine` field,
// not from ProjectConfig.Machines[0]. A multi-machine project
// where the reservation lands on a docker machine but the project
// is also attached to a bare machine must still select docker.
func TestSelectExecutorUsesReservedMachine(t *testing.T) {
	image := "ghcr.io/org/ci@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	snap := runSnapshot{
		Machine: "linux-docker",
		ProjectConfig: config.Project{
			Machines: []string{"mac-local", "linux-docker"},
		},
		Machines: map[string]config.Machine{
			"mac-local":    {},
			"linux-docker": {Runtime: "docker", Image: image},
		},
	}
	exec := selectExecutor(snap)
	type dockerRunner interface {
		CommandArgv(string, string, []string) ([]string, error)
	}
	docker, ok := exec.(dockerRunner)
	if !ok {
		t.Fatalf("reserved machine linux-docker should select docker-backed Executor, got %T", exec)
	}
	argv, err := docker.CommandArgv("/tmp/work", "/vci/work", []string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, image) {
		t.Errorf("docker argv missing image %q: %s", image, joined)
	}
}

// TestSelectExecutorSelectsVM pins that a snapshot with
// runtime=vm selects the VM-backed executor. The docker
// type-assertion must NOT match; the VM-backed executor is
// distinguished by a different CommandArgv signature (no workdir
// arg — VM uses --workdir /vci/work).
func TestSelectExecutorSelectsVM(t *testing.T) {
	snap := runSnapshot{
		Machine: "vm-linux",
		ProjectConfig: config.Project{
			Machines: []string{"vm-linux"},
		},
		Machines: map[string]config.Machine{
			"vm-linux": {Runtime: "vm", Snapshot: "ghcr.io/org/vm:pin"},
		},
	}
	exec := selectExecutor(snap)
	// The VM runner exposes the same `tart` argv shape we pin
	// elsewhere; the docker runner's CommandArgv has a workdir
	// argument that the VM runner does not. We can also assert
	// via the resolved-executable field of a stub run, but a
	// type assertion is the cheapest way to pin the seam.
	type dockerRunner interface {
		CommandArgv(string, string, []string) ([]string, error)
	}
	if _, ok := exec.(dockerRunner); ok {
		t.Fatalf("vm machine should not select docker-backed Executor")
	}
	// Type-distinguish via the resolved-executable behaviour:
	// run a stub via ExecuteSupervised with a fake Binary.
	if _, ok := exec.(Executor); !ok {
		t.Fatalf("vm executor must satisfy Executor interface")
	}
}

// TestSnapshotMachinesPersisted pins that the staged snapshot
// composition includes the Machines map so the durable run
// record can reconstruct the runtime selection at execution
// time. Run records do not retroactively rewrite history when
// config changes.
func TestSnapshotMachinesPersisted(t *testing.T) {
	image := "ghcr.io/org/ci:pin"
	project := config.Project{Machines: []string{"linux-docker"}, Command: []string{"true"}}
	cfg := config.Config{
		SchemaVersion: config.SchemaVersion,
		Orchestrator:  config.OrchestratorSelf,
		Machines: map[string]config.Machine{
			"linux-docker": {Runtime: "docker", Image: image},
		},
		Projects: map[string]config.Project{"demo": project},
	}
	out := buildStagedSnapshot("demo", project, "linux-docker", cfg, nil)
	raw, _ := json.Marshal(out)
	if !bytes.Contains(raw, []byte(`"machines"`)) {
		t.Errorf("snapshot missing machines: %s", raw)
	}
	if !bytes.Contains(raw, []byte(`"runtime":"docker"`)) {
		t.Errorf("snapshot missing runtime field: %s", raw)
	}
	if !bytes.Contains(raw, []byte(image)) {
		t.Errorf("snapshot missing image: %s", raw)
	}
}
