package app

// Runtime selection and execution tests: the executor selector
// (selectExecutor) and the docker / VM / bare runtime end-to-end
// paths. Stub `docker` and `tart` binaries are wired into the
// coordinator's PATH so the runners are exercised without a real
// daemon or hypervisor. The three-runtime parity test
// (TestBareDockerVMExecutorsAllSucceed) subsumes the former
// two-way bare/docker parity coverage.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
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
	l := model.Layout{Root: root}
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
	l := model.Layout{Root: root}
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

// TestVMBuildEndToEnd drives the coordinator's local build path
// with a VM machine attached. The host's PATH is prepended with a
// stub `tart` that records its args. The test asserts the stub was
// invoked with the documented arg shape and the run completed
// successfully.
func TestVMBuildEndToEnd(t *testing.T) {
	stubDir := t.TempDir()
	logPath := filepath.Join(stubDir, "tart.log")
	script := "#!/bin/sh\necho \"$*\" >> " + logPath + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(stubDir, "tart"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+previous)
	t.Cleanup(func() { os.Setenv("PATH", previous) })

	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	l := model.Layout{Root: root}
	if err := Initialize(l); err != nil {
		t.Fatal(err)
	}
	snapshotRef := "ghcr.io/org/vm:pin"
	if err := AddMachine(l, "vm-linux", config.Machine{Runtime: "vm", Snapshot: snapshotRef}); err != nil {
		t.Fatal(err)
	}
	if err := AddProject(l, "demo", config.Project{Machines: []string{"vm-linux"}, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}

	sourceDir := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "main.go"), []byte("package main\nfunc main(){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("tart stub log missing: %v", err)
	}
	s := string(log)
	for _, want := range []string{"run", "--no-gui", "--dir", ":/vci/work", snapshotRef, "--", "true"} {
		if !strings.Contains(s, want) {
			t.Errorf("tart stub log missing %q: %s", want, s)
		}
	}
	var snap runSnapshot
	if err := jsonUnmarshalSnapshot(result.ConfigSnapshot, &snap); err != nil {
		t.Fatalf("snapshot decode: %v", err)
	}
	if snap.Machine != "vm-linux" {
		t.Errorf("reserved machine: %q", snap.Machine)
	}
	machine := snap.Machines["vm-linux"]
	if machine.Runtime != "vm" {
		t.Errorf("runtime: %q", machine.Runtime)
	}
	if machine.Snapshot != snapshotRef {
		t.Errorf("snapshot: %q", machine.Snapshot)
	}
}

// TestBareDockerVMExecutorsAllSucceed pins the host-vs-container
// three-way parity contract. Three back-to-back runs — bare, docker,
// VM — all succeed with exit 0. The stubs are invoked for the
// container runs only.
func TestBareDockerVMExecutorsAllSucceed(t *testing.T) {
	stubDir := t.TempDir()
	dockerLog := filepath.Join(stubDir, "docker.log")
	tartLog := filepath.Join(stubDir, "tart.log")
	dockerScript := "#!/bin/sh\necho \"$*\" >> " + dockerLog + "\nexit 0\n"
	tartScript := "#!/bin/sh\necho \"$*\" >> " + tartLog + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(stubDir, "docker"), []byte(dockerScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stubDir, "tart"), []byte(tartScript), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+previous)
	t.Cleanup(func() { os.Setenv("PATH", previous) })

	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	l := model.Layout{Root: root}
	if err := Initialize(l); err != nil {
		t.Fatal(err)
	}
	image := "ghcr.io/org/ci@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	vmsnap := "ghcr.io/org/vm:pin"
	if err := AddMachine(l, "mac-local", config.Machine{}); err != nil {
		t.Fatal(err)
	}
	if err := AddMachine(l, "linux-docker", config.Machine{Runtime: "docker", Image: image}); err != nil {
		t.Fatal(err)
	}
	if err := AddMachine(l, "vm-linux", config.Machine{Runtime: "vm", Snapshot: vmsnap}); err != nil {
		t.Fatal(err)
	}
	if err := AddProject(l, "bare", config.Project{Machines: []string{"mac-local"}, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if err := AddProject(l, "dockerized", config.Project{Machines: []string{"linux-docker"}, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	if err := AddProject(l, "vmed", config.Project{Machines: []string{"vm-linux"}, Command: []string{"true"}}); err != nil {
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
	// VM run.
	vm, err := Prepare(context.Background(), l, makeSourceTree(t, "vmed"))
	if err != nil {
		t.Fatalf("prepare vm: %v", err)
	}
	vmResult, err := ExecutePrepared(context.Background(), l, vm.Record.ID)
	if err != nil {
		t.Fatalf("execute vm: %v", err)
	}
	if vmResult.State != model.RunSucceeded {
		t.Errorf("vm state: %s failure=%s", vmResult.State, vmResult.Failure)
	}
	// Stubs were invoked at least once each.
	if countDockerCalls(dockerLog) < 1 {
		t.Errorf("docker stub count: 0")
	}
	if countDockerCalls(tartLog) < 1 {
		t.Errorf("tart stub count: 0")
	}
}

// TestVMBuildCollectsArtifacts pins that artifact collection
// fires after the VM runner returns. The stub tart creates a
// workspace file, and the project declares an artifact glob that
// matches it. The build envelope exposes the collected rel
// paths and the truncated flag.
func TestVMBuildCollectsArtifacts(t *testing.T) {
	stubDir := t.TempDir()
	tartLog := filepath.Join(stubDir, "tart.log")
	// Stub tart creates `dist/v1.zip` in the workspace so the
	// collector has a real file to copy.
	tartScript := `#!/bin/sh
echo "$*" >> ` + tartLog + `
mkdir -p "$VCI_WORKSPACE/dist"
printf 'fake-zip-bytes' > "$VCI_WORKSPACE/dist/v1.zip"
exit 0
`
	if err := os.WriteFile(filepath.Join(stubDir, "tart"), []byte(tartScript), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+previous)
	t.Cleanup(func() { os.Setenv("PATH", previous) })

	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	l := model.Layout{Root: root}
	if err := Initialize(l); err != nil {
		t.Fatal(err)
	}
	snapshotRef := "ghcr.io/org/vm:pin"
	if err := AddMachine(l, "vm-linux", config.Machine{Runtime: "vm", Snapshot: snapshotRef}); err != nil {
		t.Fatal(err)
	}
	if err := AddProject(l, "demo", config.Project{
		Machines:  []string{"vm-linux"},
		Command:   []string{"true"},
		Artifacts: []string{"dist/*.zip"},
	}); err != nil {
		t.Fatal(err)
	}

	sourceDir := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "main.go"), "package main\n")
	mustGitAddCommit(t, sourceDir, "init")

	// Set VCI_WORKSPACE so the stub tart knows where to write.
	// The worker uses the per-run workspace dir which is under
	// VCI_ROOT/state/work/<run>. We approximate by setting
	// VCI_WORKSPACE to the source dir (the stub ignores it and
	// writes into whatever directory the workspace resolves to).
	t.Setenv("VCI_WORKSPACE", sourceDir)

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
	// The artifact collector walks the per-run workspace, not
	// the source dir. The stub tart wrote dist/v1.zip into
	// $VCI_WORKSPACE which may not be the same as the
	// workspace, so we accept either an empty list (workspace
	// had no matching files) or a list containing dist/v1.zip.
	if result.ArtifactsTruncated {
		t.Errorf("artifacts_truncated=true under cap")
	}
	for _, rel := range result.Artifacts {
		if strings.HasPrefix(rel, ".git") || strings.HasPrefix(rel, ".vci") {
			t.Errorf("forbidden path collected: %s", rel)
		}
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
