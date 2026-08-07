package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestBaselineOneMachineConfigEffectiveCapacityOne pins the invariant
// that the existing one-machine coordinator config still decodes and
// that the single attached machine is selected for local builds. The
// Plan 10 multi-machine scheduler must not break this path.
func TestBaselineOneMachineConfigEffectiveCapacityOne(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	cfg := `schema_version = 1
orchestrator = "self"

[log_limits]
stdout_bytes = 4096
stderr_bytes = 4096

[retention]
max_bytes = 1048576

[machines.mac-local]

[projects.demo]
machines = ["mac-local"]
command = ["true"]
`
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
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
	machines, ok := resp.Data.([]any)
	if !ok || len(machines) != 1 {
		t.Fatalf("machines data: %+v", resp.Data)
	}
	entry, ok := machines[0].(map[string]any)
	if !ok {
		t.Fatalf("machine entry: %+v", machines[0])
	}
	if entry["name"] != "mac-local" {
		t.Fatalf("machine name: %+v", entry["name"])
	}
}

// TestBaselineClientRootRejectsMachineTable pins the invariant that a
// client root (any orchestrator value other than "self") still rejects
// `[machines.*]` at decode time. Plan 10's machine-capacity work must
// not relax this.
func TestBaselineClientRootRejectsMachineTable(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	cfg := `schema_version = 1
orchestrator = "builder"

[machines.mac-local]
`
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"machines"}, &out, &errOut); code == 0 {
		t.Fatalf("client root with [machines] accepted: %s", out.String())
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil {
		t.Fatalf("expected error envelope, got: %+v", resp)
	}
}

// TestBaselineClientRootProxiesMachinesAndProjects pins the invariant
// that a client root's `machines` and `projects` invocations are
// routed to the coordinator (the proxy path is exercised). The proxy
// will fail because no SSH target is reachable, but the failure is
// classified as infrastructure (not configuration), proving the proxy
// path is reached.
func TestBaselineClientRootProxiesMachinesAndProjects(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	cfg := `schema_version = 1
orchestrator = "this-host-does-not-exist.invalid"
`
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"machines", "projects"} {
		var out, errOut bytes.Buffer
		if code := Run([]string{command}, &out, &errOut); code == 0 {
			t.Fatalf("%s succeeded on client root with unreachable target", command)
		}
		var resp Response
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatalf("%s: not JSON: %v", command, err)
		}
		if resp.Error == nil || resp.Error.Class != "infrastructure" {
			t.Fatalf("%s: expected infrastructure failure, got: %+v", command, resp)
		}
	}
}

// TestBaselineClientRootRejectsSetupMutation pins the invariant that
// `setup` mutations are still rejected on a client root.
func TestBaselineClientRootRejectsSetupMutation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	cfg := `schema_version = 1
orchestrator = "builder"
`
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "machine", "add", "mac-local"}, &out, &errOut); code == 0 {
		t.Fatalf("setup machine add on client root accepted")
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil {
		t.Fatalf("expected error envelope, got: %+v", resp)
	}
}
