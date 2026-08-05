package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypernewbie/vci/internal/layout"
)

// Role-awareness tests. The orchestrator selector splits Vci roots
// into a coordinator (orchestrator = "self") that owns machines, projects,
// retention, and log limits, and a client (orchestrator = "<host>") that
// holds only the selector. The role is enforced at decode time and at
// every configuration mutation.

const coordinatorValid = `schema_version = 1
orchestrator = "self"

[log_limits]
stdout_bytes = 100
stderr_bytes = 100

[retention]
max_bytes = 1000

[machines.mac-local]

[projects.Vci]
machines = ["mac-local"]
command = ["go", "test", "./..."]
`

const clientValid = `schema_version = 1
orchestrator = "builder"
`

func TestDecodeCoordinatorSelf(t *testing.T) {
	cfg, err := Decode([]byte(coordinatorValid))
	if err != nil {
		t.Fatalf("coordinator config rejected: %v", err)
	}
	if cfg.Orchestrator != "self" {
		t.Fatalf("orchestrator: %q", cfg.Orchestrator)
	}
	if len(cfg.Machines) != 1 || len(cfg.Projects) != 1 {
		t.Fatalf("coordinator state stripped: %#v", cfg)
	}
}

func TestDecodeLegacyLocalDefaultsToSelf(t *testing.T) {
	const legacy = `schema_version = 1

[log_limits]
stdout_bytes = 100
stderr_bytes = 100

[retention]
max_bytes = 1000

[machines.mac-local]

[projects.Vci]
machines = ["mac-local"]
command = ["go", "test", "./..."]
`
	cfg, err := Decode([]byte(legacy))
	if err != nil {
		t.Fatalf("legacy decode failed: %v", err)
	}
	if cfg.Orchestrator != "self" {
		t.Fatalf("legacy orchestrator default: %q", cfg.Orchestrator)
	}
}

func TestDecodeClientRootAccepted(t *testing.T) {
	cfg, err := Decode([]byte(clientValid))
	if err != nil {
		t.Fatalf("client config rejected: %v", err)
	}
	if cfg.Orchestrator != "builder" {
		t.Fatalf("orchestrator: %q", cfg.Orchestrator)
	}
	if len(cfg.Machines) != 0 || len(cfg.Projects) != 0 {
		t.Fatalf("client root carried coordinator state: %#v", cfg)
	}
}

func TestDecodeClientRootRejectsMachines(t *testing.T) {
	data := clientValid + `
[machines.builder]
`
	if _, err := Decode([]byte(data)); err == nil || !strings.Contains(err.Error(), "machines") {
		t.Fatalf("expected rejection naming machines, got %v", err)
	}
}

func TestDecodeClientRootRejectsProjects(t *testing.T) {
	data := clientValid + `
[projects.demo]
machines = ["x"]
command = ["true"]
`
	if _, err := Decode([]byte(data)); err == nil || !strings.Contains(err.Error(), "projects") {
		t.Fatalf("expected rejection naming projects, got %v", err)
	}
}

func TestDecodeClientRootRejectsRetention(t *testing.T) {
	data := clientValid + `
[retention]
max_bytes = 100
`
	if _, err := Decode([]byte(data)); err == nil || !strings.Contains(err.Error(), "retention") {
		t.Fatalf("expected rejection naming retention, got %v", err)
	}
}

func TestDecodeClientRootRejectsLogLimits(t *testing.T) {
	data := clientValid + `
[log_limits]
stdout_bytes = 100
stderr_bytes = 100
`
	if _, err := Decode([]byte(data)); err == nil || !strings.Contains(err.Error(), "log") {
		t.Fatalf("expected rejection naming log limits, got %v", err)
	}
}

func TestDecodeRejectsEmptyOrchestrator(t *testing.T) {
	data := `schema_version = 1
orchestrator = ""
`
	if _, err := Decode([]byte(data)); err == nil || !strings.Contains(err.Error(), "orchestrator") {
		t.Fatalf("empty orchestrator accepted: %v", err)
	}
}

func TestDecodeRejectsOptionLikeOrchestrator(t *testing.T) {
	for _, value := range []string{"-builder", "--option", "-"} {
		data := `schema_version = 1
orchestrator = "` + value + `"
`
		if _, err := Decode([]byte(data)); err == nil {
			t.Fatalf("option-like orchestrator %q accepted", value)
		}
	}
}

func TestDecodeRejectsWhitespaceOrchestrator(t *testing.T) {
	for _, value := range []string{"builder host", "builder\thost", "builder\nhost", "builder\rhost"} {
		data := "schema_version = 1\norchestrator = \"" + value + "\"\n"
		if _, err := Decode([]byte(data)); err == nil {
			t.Fatalf("whitespace-bearing orchestrator %q accepted", value)
		}
	}
}

func TestDecodeRejectsMachinesHostField(t *testing.T) {
	data := coordinatorValid + "\n[machines.mac-local]\nhost = \"builder\"\n"
	if _, err := Decode([]byte(data)); err == nil {
		t.Fatalf("machines.*.host accepted")
	}
}

func TestMutateRejectsClientRootMachineAddition(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".vci")
	l := layout.Layout{Root: dir}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := Save(l.ConfigPath(), Defaults()); err != nil {
		t.Fatal(err)
	}
	if err := Mutate(l.ConfigPath(), func(c *Config) error {
		c.Orchestrator = "builder"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	err := Mutate(l.ConfigPath(), func(c *Config) error {
		c.Machines["builder"] = Machine{}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "client root") {
		t.Fatalf("expected client root rejection, got %v", err)
	}
}

func TestValidateOrchestratorStrict(t *testing.T) {
	cases := map[string]bool{
		"self":               true,
		"builder":            true,
		"192.168.1.10":       true,
		"":                   false,
		"-x":                 false,
		"with space":         false,
		"with\ttab":          false,
		"with\nnewline":      false,
		string([]byte{0x7f}): false,
	}
	for value, want := range cases {
		err := ValidateOrchestrator(value)
		got := err == nil
		if got != want {
			t.Errorf("ValidateOrchestrator(%q) = %v want %v", value, got, want)
		}
	}
}
