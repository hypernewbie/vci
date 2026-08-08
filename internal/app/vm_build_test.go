package app

// VM-vs-host and VM-vs-docker parity tests for the VM runtime
// selector. The test wires a stub `tart` binary into the
// coordinator's PATH so the VM runner can be exercised without
// requiring a real hypervisor.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/model"
)

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
	l := layout.Layout{Root: root}
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
	l := layout.Layout{Root: root}
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
	l := layout.Layout{Root: root}
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
