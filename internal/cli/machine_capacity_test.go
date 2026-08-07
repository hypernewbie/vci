package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

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
