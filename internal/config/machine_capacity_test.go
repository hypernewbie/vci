package config

import (
	"strings"
	"testing"
)

// TestEffectiveCapacityOneByDefault pins the compatibility default: an
// empty Machine struct (zero or omitted max_concurrent) has effective
// capacity 1, so the existing single-machine coordinator config still
// behaves as before.
func TestEffectiveCapacityOneByDefault(t *testing.T) {
	if got := EffectiveCapacity(Machine{}); got != 1 {
		t.Fatalf("empty Machine effective capacity = %d, want 1", got)
	}
	if got := EffectiveCapacity(Machine{MaxConcurrent: 0}); got != 1 {
		t.Fatalf("explicit zero capacity = %d, want 1", got)
	}
}

// TestEffectiveCapacityExplicit pins the rule that an explicit positive
// value passes through and is the local-slot capacity.
func TestEffectiveCapacityExplicit(t *testing.T) {
	if got := EffectiveCapacity(Machine{MaxConcurrent: 4}); got != 4 {
		t.Fatalf("capacity 4 = %d, want 4", got)
	}
}

// TestDecodeCoordinatorMaxConcurrentAccepted pins the decode-side
// acceptance of the new optional machine field. A coordinator config
// that uses max_concurrent must decode and EffectiveCapacity must
// return the configured value.
func TestDecodeCoordinatorMaxConcurrentAccepted(t *testing.T) {
	const data = `schema_version = 1
orchestrator = "self"

[log_limits]
stdout_bytes = 4096
stderr_bytes = 4096

[retention]
max_bytes = 1048576

[machines.mac-local]
max_concurrent = 4

[projects.demo]
machines = ["mac-local"]
command = ["true"]
`
	cfg, err := Decode([]byte(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := cfg.Machines["mac-local"].MaxConcurrent; got != 4 {
		t.Fatalf("decoded MaxConcurrent = %d, want 4", got)
	}
	if got := EffectiveCapacity(cfg.Machines["mac-local"]); got != 4 {
		t.Fatalf("EffectiveCapacity = %d, want 4", got)
	}
}

// TestDecodeCoordinatorRejectsNegativeMaxConcurrent pins the
// validation rule: a coordinator config must not declare a negative
// local-slot capacity.
func TestDecodeCoordinatorRejectsNegativeMaxConcurrent(t *testing.T) {
	const data = `schema_version = 1
orchestrator = "self"

[log_limits]
stdout_bytes = 4096
stderr_bytes = 4096

[retention]
max_bytes = 1048576

[machines.mac-local]
max_concurrent = -1

[projects.demo]
machines = ["mac-local"]
command = ["true"]
`
	_, err := Decode([]byte(data))
	if err == nil || !strings.Contains(err.Error(), "max_concurrent") {
		t.Fatalf("expected max_concurrent rejection, got %v", err)
	}
}

// TestDecodeClientRootRejectsMaxConcurrent pins the invariant that
// the new field is forbidden on a client root, alongside the other
// coordinator-owned tables. The existing strict client validation
// rejects any [machines.*] table regardless of field; this test
// asserts the message names machines.
func TestDecodeClientRootRejectsMaxConcurrent(t *testing.T) {
	const data = `schema_version = 1
orchestrator = "builder"

[machines.mac-local]
max_concurrent = 4
`
	_, err := Decode([]byte(data))
	if err == nil || !strings.Contains(err.Error(), "machines") {
		t.Fatalf("expected client rejection naming machines, got %v", err)
	}
}

// TestDecodeCoordinatorProjectMultipleMachinesAccepted pins that a
// single project may now attach more than one machine. The strict
// per-project rules (valid names, no duplicates, every attached
// machine exists in the inventory) still apply.
func TestDecodeCoordinatorProjectMultipleMachinesAccepted(t *testing.T) {
	const data = `schema_version = 1
orchestrator = "self"

[log_limits]
stdout_bytes = 4096
stderr_bytes = 4096

[retention]
max_bytes = 1048576

[machines.alpha]
max_concurrent = 2

[machines.beta]
max_concurrent = 1

[projects.demo]
machines = ["alpha", "beta"]
command = ["true"]
`
	cfg, err := Decode([]byte(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	project := cfg.Projects["demo"]
	if len(project.Machines) != 2 {
		t.Fatalf("project machines: %v", project.Machines)
	}
	if project.Machines[0] != "alpha" || project.Machines[1] != "beta" {
		t.Fatalf("project machines order: %v", project.Machines)
	}
}

// TestDecodeCoordinatorProjectRejectsDuplicateMachines pins the
// duplicate-machine rejection in the multi-machine case.
func TestDecodeCoordinatorProjectRejectsDuplicateMachines(t *testing.T) {
	const data = `schema_version = 1
orchestrator = "self"

[log_limits]
stdout_bytes = 4096
stderr_bytes = 4096

[retention]
max_bytes = 1048576

[machines.mac-local]

[projects.demo]
machines = ["mac-local", "mac-local"]
command = ["true"]
`
	_, err := Decode([]byte(data))
	if err == nil || !strings.Contains(err.Error(), "repeats machine") {
		t.Fatalf("expected duplicate-machine rejection, got %v", err)
	}
}
