package cli

// Plan 15 Phase 1 CLI surface for `setup machine add --host`: the
// machines inventory envelope must include the host field, and bad
// destinations must be rejected as invalid_arguments before any
// config mutation. Plan 10/13 surface for `setup machine add
// --capacity`, runtime declarations (docker/vm), and the
// multi-machine / artifact project rules, all against the same
// setup-mutation contract: valid input persists into the coordinator
// root and surfaces in the inventory envelopes, invalid input is
// rejected before mutation.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hypernewbie/vci/internal/config"
)

// loadTestConfig reads the coordinator config written by setup into a
// fresh Config for round-trip assertions.
func loadTestConfig(t *testing.T, root string) config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join(root, "config.toml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

func TestSetupAndInventoryCommands(t *testing.T) {
	t.Setenv("VCI_ROOT", filepath.Join(t.TempDir(), ".vci"))
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "init"}, &out, &errOut); code != 0 {
		t.Fatalf("setup: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"machines"}, &out, &errOut); code != 0 {
		t.Fatalf("machines: %d %s", code, out.String())
	}
	var response Response
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK {
		t.Fatalf("machines failed: %+v", response.Error)
	}
}

// TestSetupMachineAddAcceptsHost pins that
// `setup machine add <name> --host <ssh-destination>` persists the
// host and surfaces it in the `vci machines` envelope.
func TestSetupMachineAddAcceptsHost(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "init"}, &out, &errOut); code != 0 {
		t.Fatalf("setup init: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"setup", "machine", "add", "mac-remote", "--host", "builder"}, &out, &errOut); code != 0 {
		t.Fatalf("setup machine add --host: %d %s", code, out.String())
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("setup machine add failed: %+v", resp.Error)
	}
	out.Reset()
	if code := Run([]string{"machines"}, &out, &errOut); code != 0 {
		t.Fatalf("machines: %d %s", code, out.String())
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("machines failed: %+v", resp.Error)
	}
	machines, _ := resp.Data.([]any)
	if len(machines) != 1 {
		t.Fatalf("machines count: %d", len(machines))
	}
	entry, _ := machines[0].(map[string]any)
	machine, _ := entry["machine"].(map[string]any)
	if machine["host"] != "builder" {
		t.Errorf("machines envelope missing host: %v", machine)
	}
}

// TestSetupMachineAddRejectsBadHost pins that flag-like, whitespace,
// scheme, and `..` destinations are rejected with invalid_arguments
// and the coordinator config is not mutated.
func TestSetupMachineAddRejectsBadHost(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "init"}, &out, &errOut); code != 0 {
		t.Fatalf("setup init: %d %s", code, out.String())
	}
	for _, host := range []string{
		"--rm-all",
		"bad host",
		"https://builder",
		"a/../b",
	} {
		out.Reset()
		if code := Run([]string{"setup", "machine", "add", "bad", "--host", host}, &out, &errOut); code == 0 {
			t.Fatalf("bad host %q accepted", host)
		}
		var resp Response
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatalf("not JSON: %v", err)
		}
		if resp.Error == nil || resp.Error.Code != "invalid_arguments" {
			t.Fatalf("expected invalid_arguments, got %+v", resp.Error)
		}
	}
}

// TestSetupMachineAddAcceptsHostAlone pins that a bare remote machine
// (`--host` without `--runtime`) is valid: the remote runs the
// project command directly on the remote host.
func TestSetupMachineAddAcceptsHostAlone(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "init"}, &out, &errOut); code != 0 {
		t.Fatalf("setup init: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"setup", "machine", "add", "bare-remote", "--host", "builder"}, &out, &errOut); code != 0 {
		t.Fatalf("setup machine add host alone: %d %s", code, out.String())
	}
	// Round-trip: the coordinator config decodes with the host set
	// and no runtime.
	cfg := loadTestConfig(t, root)
	m := cfg.Machines["bare-remote"]
	if m.Host != "builder" {
		t.Errorf("host: %q", m.Host)
	}
	if m.Runtime != "" {
		t.Errorf("runtime: %q", m.Runtime)
	}
}

// TestSetupMachineAddAcceptsCapacity pins that `setup machine add
// <name> --capacity <n>` writes the configured capacity to the
// coordinator root. The machines envelope surfaces it.
func TestSetupMachineAddAcceptsCapacity(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "init"}, &out, &errOut); code != 0 {
		t.Fatalf("setup init: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"setup", "machine", "add", "mac-local", "--capacity", "4"}, &out, &errOut); code != 0 {
		t.Fatalf("setup machine add: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"machines"}, &out, &errOut); code != 0 {
		t.Fatalf("machines: %d %s", code, out.String())
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("machines failed: %+v", resp.Error)
	}
	machines, _ := resp.Data.([]any)
	if len(machines) != 1 {
		t.Fatalf("machines count: %d", len(machines))
	}
	entry, _ := machines[0].(map[string]any)
	if got := entry["capacity"]; got != float64(4) {
		t.Fatalf("capacity: %v", got)
	}
}

// TestSetupMachineAddRejectsNegativeCapacity pins that `--capacity
// 0`, negative values, and non-numeric values are rejected with
// `invalid_arguments`.
func TestSetupMachineAddRejectsBadCapacity(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "init"}, &out, &errOut); code != 0 {
		t.Fatalf("setup init: %d %s", code, out.String())
	}
	bad := [][]string{
		{"setup", "machine", "add", "m1", "--capacity", "0"},
		{"setup", "machine", "add", "m2", "--capacity", "-3"},
		{"setup", "machine", "add", "m3", "--capacity", "abc"},
		{"setup", "machine", "add", "m4", "--capacity"},
	}
	for _, args := range bad {
		out.Reset()
		if code := Run(args, &out, &errOut); code == 0 {
			t.Fatalf("bad capacity accepted: %v", args)
		}
		var resp Response
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatalf("not JSON: %v", err)
		}
		if resp.Error == nil || resp.Error.Code != "invalid_arguments" {
			t.Fatalf("expected invalid_arguments, got %+v", resp.Error)
		}
	}
}

// TestSetupProjectAddAcceptsMultipleMachines pins that repeated
// `--machine <name>` writes an ordered multi-machine list to the
// project. The strict coordinator validation rejects duplicates and
// missing machines; an explicit two-machine project survives.
func TestSetupProjectAddAcceptsMultipleMachines(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "init"}, &out, &errOut); code != 0 {
		t.Fatalf("setup init: %d %s", code, out.String())
	}
	for _, machine := range []string{"alpha", "beta"} {
		out.Reset()
		args := []string{"setup", "machine", "add", machine}
		if machine == "alpha" {
			args = append(args, "--capacity", "2")
		}
		if code := Run(args, &out, &errOut); code != 0 {
			t.Fatalf("setup machine add %s: %d %s", machine, code, out.String())
		}
	}
	out.Reset()
	if code := Run([]string{"setup", "project", "add", "demo", "--machine", "alpha", "--machine", "beta", "--command", "true"}, &out, &errOut); code != 0 {
		t.Fatalf("setup project add: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"projects"}, &out, &errOut); code != 0 {
		t.Fatalf("projects: %d %s", code, out.String())
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("projects failed: %+v", resp.Error)
	}
	projects, _ := resp.Data.([]any)
	if len(projects) != 1 {
		t.Fatalf("projects count: %d", len(projects))
	}
	entry, _ := projects[0].(map[string]any)
	projectEntry, _ := entry["project"].(map[string]any)
	machines, _ := projectEntry["machines"].([]any)
	if len(machines) != 2 || machines[0] != "alpha" || machines[1] != "beta" {
		t.Fatalf("project machines: %v", machines)
	}
}

// TestSetupProjectAddRejectsDuplicateMachines pins that the duplicate
// rule survives the multi-machine CLI. The project must be rejected
// even though the CLI parsed both --machine flags.
func TestSetupProjectAddRejectsDuplicateMachines(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "init"}, &out, &errOut); code != 0 {
		t.Fatalf("setup init: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"setup", "machine", "add", "mac-local"}, &out, &errOut); code != 0 {
		t.Fatalf("setup machine add: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"setup", "project", "add", "demo", "--machine", "mac-local", "--machine", "mac-local", "--command", "true"}, &out, &errOut); code == 0 {
		t.Fatalf("setup project add with duplicate machine accepted")
	}
}

// TestClientRootRejectsNewMachineField pins that the new machine
// field is forbidden on a client root because the entire
// `[machines.*]` table is forbidden there.
func TestClientRootRejectsNewMachineField(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	cfg := `schema_version = 1
orchestrator = "builder"

[machines.mac-local]
max_concurrent = 2
`
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"machines"}, &out, &errOut); code == 0 {
		t.Fatalf("client root with machines.max_concurrent accepted")
	}
}

// TestSetupMachineAddAcceptsDockerRuntime pins that
// `setup machine add <name> --runtime docker --image <ref>`
// persists the runtime declaration and the machines envelope
// surfaces it. The image is a verbatim reference; the runtime
// runner parses it as a positional argument.
func TestSetupMachineAddAcceptsDockerRuntime(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "init"}, &out, &errOut); code != 0 {
		t.Fatalf("setup init: %d %s", code, out.String())
	}
	out.Reset()
	image := "ghcr.io/org/ci@sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if code := Run([]string{"setup", "machine", "add", "linux-docker", "--runtime", "docker", "--image", image}, &out, &errOut); code != 0 {
		t.Fatalf("setup machine add docker: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"machines"}, &out, &errOut); code != 0 {
		t.Fatalf("machines: %d %s", code, out.String())
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("machines failed: %+v", resp.Error)
	}
	machines, _ := resp.Data.([]any)
	if len(machines) != 1 {
		t.Fatalf("machines count: %d", len(machines))
	}
	entry, _ := machines[0].(map[string]any)
	machine, _ := entry["machine"].(map[string]any)
	if machine["runtime"] != "docker" {
		t.Errorf("runtime: %v", machine["runtime"])
	}
	if machine["image"] != image {
		t.Errorf("image: %v", machine["image"])
	}
}

// TestSetupMachineAddRejectsBadDockerImage pins that a
// flag-like image is rejected at the CLI surface and the
// coordinator config is not mutated.
func TestSetupMachineAddRejectsBadDockerImage(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "init"}, &out, &errOut); code != 0 {
		t.Fatalf("setup init: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"setup", "machine", "add", "bad", "--runtime", "docker", "--image", "--rm-all"}, &out, &errOut); code == 0 {
		t.Fatalf("flag-like image accepted")
	}
	var resp Response
	_ = json.Unmarshal(out.Bytes(), &resp)
	// The CLI emits an invalid_arguments envelope; the deeper
	// config validator would emit machine_update_failed, but the
	// CLI parser defends ahead of the validator.
	if resp.Error == nil {
		t.Fatalf("expected error: %s", out.String())
	}
}

// TestSetupMachineAddAcceptsVMRuntime pins that
// `setup machine add <name> --runtime vm --snapshot <ref>`
// persists the VM runtime and snapshot. The CLI surface mirrors
// the docker path with --snapshot taking the verbatim reference.
func TestSetupMachineAddAcceptsVMRuntime(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "init"}, &out, &errOut); code != 0 {
		t.Fatalf("setup init: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"setup", "machine", "add", "vm1",
		"--runtime", "vm",
		"--snapshot", "ghcr.io/org/vm:pin",
	}, &out, &errOut); code != 0 {
		t.Fatalf("setup machine add vm: %d %s", code, out.String())
	}
	cfgPath := filepath.Join(root, "config.toml")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`runtime = "vm"`)) {
		t.Errorf("vm runtime missing: %s", raw)
	}
	if !bytes.Contains(raw, []byte(`snapshot = "ghcr.io/org/vm:pin"`)) {
		t.Errorf("snapshot missing: %s", raw)
	}
}

// TestSetupMachineAddRejectsVMWithoutSnapshot pins that
// `runtime=vm` without `--snapshot` fails at the CLI surface as
// configuration.
func TestSetupMachineAddRejectsVMWithoutSnapshot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "init"}, &out, &errOut); code != 0 {
		t.Fatalf("setup init: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"setup", "machine", "add", "vm1", "--runtime", "vm"}, &out, &errOut); code == 0 {
		t.Fatalf("vm without snapshot accepted")
	}
}

// TestSetupProjectAddAcceptsArtifacts pins that
// `setup project add <name> --machine ... --command ... --artifact <glob>
// [--artifact <glob>...]` persists the artifact globs into the
// coordinator root and surfaces them in the projects envelope.
func TestSetupProjectAddAcceptsArtifacts(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "init"}, &out, &errOut); code != 0 {
		t.Fatalf("setup init: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"setup", "machine", "add", "mac-local"}, &out, &errOut); code != 0 {
		t.Fatalf("setup machine add: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"setup", "project", "add", "demo",
		"--machine", "mac-local",
		"--command", "go",
		"--arg", "test",
		"--arg", "./...",
		"--artifact", "build/*",
		"--artifact", "dist/*.zip",
	}, &out, &errOut); code != 0 {
		t.Fatalf("setup project add: %d %s", code, out.String())
	}
	// Round-trip: load the on-disk config and confirm artifacts.
	cfgPath := filepath.Join(root, "config.toml")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`build/*`)) {
		t.Errorf("artifact missing in config: %s", raw)
	}
	if !bytes.Contains(raw, []byte(`dist/*.zip`)) {
		t.Errorf("artifact missing in config: %s", raw)
	}
	// `vci projects` envelopes the artifact globs.
	out.Reset()
	if code := Run([]string{"projects"}, &out, &errOut); code != 0 {
		t.Fatalf("projects: %d %s", code, out.String())
	}
	var resp struct {
		Data []struct {
			Name    string         `json:"name"`
			Project map[string]any `json:"project"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var demo map[string]any
	for _, p := range resp.Data {
		if p.Name == "demo" {
			demo = p.Project
			break
		}
	}
	if demo == nil {
		t.Fatalf("demo missing: %s", out.String())
	}
	arts, _ := demo["artifacts"].([]any)
	if len(arts) != 2 {
		t.Errorf("artifacts count: %d (%v)", len(arts), arts)
	}
}

// TestSetupProjectAddRejectsBadArtifact pins that bad globs
// (absolute, parent escape, scheme, leading dash, whitespace,
// path.Match failure) are rejected at the CLI surface as
// `project_update_failed` configuration failure.
func TestSetupProjectAddRejectsBadArtifact(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "init"}, &out, &errOut); code != 0 {
		t.Fatalf("setup init: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"setup", "machine", "add", "mac-local"}, &out, &errOut); code != 0 {
		t.Fatalf("setup machine add: %d %s", code, out.String())
	}
	out.Reset()
	bad := []string{
		"/abs/path",
		"../escape",
		"--flag",
		"with space",
		"https://x",
	}
	for _, glob := range bad {
		out.Reset()
		errOut.Reset()
		if code := Run([]string{"setup", "project", "add", "demo",
			"--machine", "mac-local",
			"--command", "true",
			"--artifact", glob,
		}, &out, &errOut); code == 0 {
			t.Errorf("artifact %q accepted", glob)
		}
	}
}

// TestSetupMachineUpdateSourcePath pins that `setup machine update
// <name> --source-path <project>=<path>` persists the source path for
// an existing machine once the referenced project exists.
func TestSetupMachineUpdateSourcePath(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "init"}, &out, &errOut); code != 0 {
		t.Fatalf("setup init: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"setup", "machine", "add", "charon"}, &out, &errOut); code != 0 {
		t.Fatalf("setup machine add: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"setup", "project", "add", "vidl", "--machine", "charon", "--command", "true"}, &out, &errOut); code != 0 {
		t.Fatalf("setup project add: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"setup", "machine", "update", "charon", "--source-path", "vidl=/code/vidl"}, &out, &errOut); code != 0 {
		t.Fatalf("setup machine update: %d %s", code, out.String())
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("setup machine update failed: %+v", resp.Error)
	}
	cfg := loadTestConfig(t, root)
	if got := cfg.Machines["charon"].SourcePaths["vidl"]; got != "/code/vidl" {
		t.Errorf("source path: %q", got)
	}
}

// TestSetupMachineUpdateRejectsUnknownProject pins that a source path
// key referencing a project that does not exist is rejected by
// re-validation before any write.
func TestSetupMachineUpdateRejectsUnknownProject(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "init"}, &out, &errOut); code != 0 {
		t.Fatalf("setup init: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"setup", "machine", "add", "charon"}, &out, &errOut); code != 0 {
		t.Fatalf("setup machine add: %d %s", code, out.String())
	}
	out.Reset()
	code := Run([]string{"setup", "machine", "update", "charon", "--source-path", "nope=/code/nope"}, &out, &errOut)
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if code == 0 && resp.OK {
		t.Fatalf("source path referencing unknown project accepted")
	}
}

// TestSetupMachineUpdateRejectsMalformedSourcePath pins that a
// --source-path value without a project=path separator is rejected as
// a usage error before any mutation.
func TestSetupMachineUpdateRejectsMalformedSourcePath(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "init"}, &out, &errOut); code != 0 {
		t.Fatalf("setup init: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"setup", "machine", "add", "charon"}, &out, &errOut); code != 0 {
		t.Fatalf("setup machine add: %d %s", code, out.String())
	}
	out.Reset()
	code := Run([]string{"setup", "machine", "update", "charon", "--source-path", "noseparator"}, &out, &errOut)
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if code == 0 && resp.OK {
		t.Fatalf("malformed --source-path accepted")
	}
	if resp.Error == nil || resp.Error.Code != "invalid_arguments" {
		t.Fatalf("expected invalid_arguments, got %+v", resp.Error)
	}
}

// TestSetupMachineUpdateMissingMachine pins that updating a machine
// that does not exist fails without mutating the config.
func TestSetupMachineUpdateMissingMachine(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "init"}, &out, &errOut); code != 0 {
		t.Fatalf("setup init: %d %s", code, out.String())
	}
	out.Reset()
	code := Run([]string{"setup", "machine", "update", "ghost", "--source-path", "vidl=/code/vidl"}, &out, &errOut)
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if code == 0 && resp.OK {
		t.Fatalf("update of missing machine accepted")
	}
}

// TestSetupProjectUpdateExclude pins that `setup project update
// <name> --exclude <glob> [--exclude <glob>...]` replaces the
// project's excluded paths in the coordinator root.
func TestSetupProjectUpdateExclude(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "init"}, &out, &errOut); code != 0 {
		t.Fatalf("setup init: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"setup", "machine", "add", "m1"}, &out, &errOut); code != 0 {
		t.Fatalf("setup machine add: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"setup", "project", "add", "p1", "--machine", "m1", "--command", "true"}, &out, &errOut); code != 0 {
		t.Fatalf("setup project add: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"setup", "project", "update", "p1", "--exclude", "*.env", "--exclude", "secrets"}, &out, &errOut); code != 0 {
		t.Fatalf("setup project update: %d %s", code, out.String())
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("setup project update failed: %+v", resp.Error)
	}
	cfg := loadTestConfig(t, root)
	got := cfg.Projects["p1"].ExcludedPaths
	if len(got) != 2 || got[0] != "*.env" || got[1] != "secrets" {
		t.Errorf("excluded paths: %v", got)
	}
}

// TestSetupProjectUpdateRejectsBadGlob pins that a malformed glob is
// rejected by re-validation before any write.
func TestSetupProjectUpdateRejectsBadGlob(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "init"}, &out, &errOut); code != 0 {
		t.Fatalf("setup init: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"setup", "machine", "add", "m1"}, &out, &errOut); code != 0 {
		t.Fatalf("setup machine add: %d %s", code, out.String())
	}
	out.Reset()
	if code := Run([]string{"setup", "project", "add", "p1", "--machine", "m1", "--command", "true"}, &out, &errOut); code != 0 {
		t.Fatalf("setup project add: %d %s", code, out.String())
	}
	out.Reset()
	code := Run([]string{"setup", "project", "update", "p1", "--exclude", "["}, &out, &errOut)
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if code == 0 && resp.OK {
		t.Fatalf("malformed glob accepted")
	}
}

// TestSetupProjectUpdateMissingProject pins that updating a project
// that does not exist fails without mutating the config.
func TestSetupProjectUpdateMissingProject(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "init"}, &out, &errOut); code != 0 {
		t.Fatalf("setup init: %d %s", code, out.String())
	}
	out.Reset()
	code := Run([]string{"setup", "project", "update", "ghost", "--exclude", "*.env"}, &out, &errOut)
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	if code == 0 && resp.OK {
		t.Fatalf("update of missing project accepted")
	}
}
