package config

import "testing"

const valid = `schema_version = 1

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

func TestDecodeValidConfig(t *testing.T) {
	cfg, err := Decode([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Projects["Vci"].Command[0] != "go" {
		t.Fatalf("command: %#v", cfg.Projects["Vci"].Command)
	}
}

func TestDecodeRejectsUnknownFields(t *testing.T) {
	if _, err := Decode([]byte(valid + "\nunknown = true\n")); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestDecodeRejectsRemovedFields(t *testing.T) {
	for _, field := range []string{"total_bytes = 200", "max_runs = 10", "ttl_days = 2", "transport = \"local\"", "runtime = \"local\"", "host = \"builder\"", "os = \"darwin\"", "arch = \"arm64\"", "image = \"image\"", "snapshot = \"snapshot\""} {
		data := valid + "\n" + field + "\n"
		if _, err := Decode([]byte(data)); err == nil {
			t.Fatalf("removed field accepted: %s", field)
		}
	}
}

func TestValidateRejectsBrokenReferences(t *testing.T) {
	cfg, err := Decode([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Projects["Vci"] = Project{Machines: []string{"missing"}, Command: []string{"go"}}
	if err := Validate(cfg); err == nil {
		t.Fatal("missing machine accepted")
	}
}

// validEnvConfig is a coordinator root whose project declares a
// POSIX-identifier environment table with underscore keys.
const validEnvConfig = `schema_version = 1
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

[projects.Vci.environment]
FOO_BAR = "1"
_LEADING = "2"
Trailing_9 = "3"
`

// TestDecodeAcceptsValidEnvironmentKeys pins that underscore-bearing
// POSIX identifier keys decode and round-trip through validation.
func TestDecodeAcceptsValidEnvironmentKeys(t *testing.T) {
	cfg, err := Decode([]byte(validEnvConfig))
	if err != nil {
		t.Fatalf("valid environment keys rejected: %v", err)
	}
	env := cfg.Projects["Vci"].Environment
	if env["FOO_BAR"] != "1" || env["_LEADING"] != "2" || env["Trailing_9"] != "3" {
		t.Fatalf("environment round-trip: %#v", env)
	}
}

// TestDecodeRejectsInvalidEnvironmentKeys pins that any key outside
// `^[A-Za-z_][A-Za-z0-9_]*$` fails Decode (the validation entry
// point used before setup and run) as a configuration error. Quoted
// TOML keys keep punctuation and spaces as a single literal key so
// the rejection is produced by the identifier rule, not by TOML
// syntax.
func TestDecodeRejectsInvalidEnvironmentKeys(t *testing.T) {
	for _, key := range []string{"FOO-BAR", "1FOO", "FOO BAR", "FOO.BAR", "FOO:BAR", ""} {
		data := valid + "\n[projects.Vci.environment]\n\"" + key + "\" = \"x\"\n"
		if _, err := Decode([]byte(data)); err == nil {
			t.Fatalf("invalid environment key %q accepted", key)
		}
	}
}

// TestValidateProjectEnvironment exercises the identifier rule
// directly: valid underscore keys pass, invalid shapes are rejected.
func TestValidateProjectEnvironment(t *testing.T) {
	valid := map[string]string{"FOO": "1", "foo_bar": "2", "_LEAD": "3", "A": "4", "_": "5", "z9_": "6"}
	if err := ValidateProjectEnvironment("Vci", valid); err != nil {
		t.Fatalf("valid keys rejected: %v", err)
	}
	for _, key := range []string{"", "1FOO", "FOO-BAR", "FOO BAR", "FOO.BAR", "FOO:BAR"} {
		if err := ValidateProjectEnvironment("Vci", map[string]string{key: "x"}); err == nil {
			t.Fatalf("invalid key %q accepted", key)
		}
	}
}
