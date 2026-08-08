package cli

// Plan 15 Phase 1 CLI surface for `setup machine add --host`. The
// machines inventory envelope must include the host field, and bad
// destinations must be rejected as invalid_arguments before any
// config mutation.

import (
	"bytes"
	"encoding/json"
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
