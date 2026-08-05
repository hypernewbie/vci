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
