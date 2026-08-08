package config

// Machine.Host contract tests (Plan 15 Phase 0/1). A machine gains an
// optional `host` field: an SSH destination reached via the ordinary
// system `ssh` executable. Empty means the machine runs on the
// coordinator host. The rules are frozen here before the field is
// implemented, so the validation surface cannot drift.

import (
	"strings"
	"testing"
)

// TestMachineHostAcceptsValidAlias pins that an optional `host` field
// (a strict SSH destination) is accepted for bare, docker, and vm
// runtimes, and that an omitted host decodes as empty (local).
func TestMachineHostAcceptsValidAlias(t *testing.T) {
	cases := []struct {
		runtime, image, snapshot, host string
	}{
		{"", "", "", "builder"},
		{"", "", "", ""},
		{"docker", "ghcr.io/org/ci:pin", "", "builder"},
		{"vm", "", "ghcr.io/org/vm:pin", "builder@remote"},
	}
	for _, tc := range cases {
		body := `schema_version = 1
orchestrator = "self"

[machines.mac-remote]
host = "` + tc.host + `"
runtime = "` + tc.runtime + `"
image = "` + tc.image + `"
snapshot = "` + tc.snapshot + `"
`
		cfg, err := Decode([]byte(body))
		if err != nil {
			t.Fatalf("host %q runtime %q rejected: %v", tc.host, tc.runtime, err)
		}
		if got := cfg.Machines["mac-remote"].Host; got != tc.host {
			t.Errorf("host: got %q want %q", got, tc.host)
		}
	}
}

// TestMachineHostRejectsBadAlias pins the strict host rules: no
// whitespace or control characters, no leading dash, no scheme, no
// `..` segment. The value is forwarded to the system `ssh` executable
// as a destination argument.
func TestMachineHostRejectsBadAlias(t *testing.T) {
	for _, host := range []string{
		"bad host",
		"bad\nhost",
		"-flag",
		"https://builder",
		"a/../b",
		"../escape",
	} {
		body := `schema_version = 1
orchestrator = "self"

[machines.bad-host]
host = "` + host + `"
`
		if _, err := Decode([]byte(body)); err == nil {
			t.Errorf("host %q accepted", host)
		}
	}
}

// TestClientRootRejectsHost pins that a client root still rejects the
// whole `[machines.*]` table, now including the `host` field.
func TestClientRootRejectsHost(t *testing.T) {
	body := `schema_version = 1
orchestrator = "builder"

[machines.mac-remote]
host = "builder"
`
	_, err := Decode([]byte(body))
	if err == nil || !strings.Contains(err.Error(), "must not declare machines") {
		t.Fatalf("expected client root rejection, got %v", err)
	}
}

// TestHostAllowedForEveryRuntime pins that `host` is orthogonal to the
// runtime: bare remote (`runtime` omitted), docker remote, and vm
// remote all validate.
func TestHostAllowedForEveryRuntime(t *testing.T) {
	body := `schema_version = 1
orchestrator = "self"

[machines.bare-remote]
host = "builder"

[machines.docker-remote]
host = "builder"
runtime = "docker"
image = "ghcr.io/org/ci:pin"

[machines.vm-remote]
host = "builder"
runtime = "vm"
snapshot = "ghcr.io/org/vm:pin"
`
	cfg, err := Decode([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Machines["bare-remote"].Host != "builder" {
		t.Errorf("bare-remote host: %q", cfg.Machines["bare-remote"].Host)
	}
	if cfg.Machines["docker-remote"].Host != "builder" {
		t.Errorf("docker-remote host: %q", cfg.Machines["docker-remote"].Host)
	}
	if cfg.Machines["vm-remote"].Host != "builder" {
		t.Errorf("vm-remote host: %q", cfg.Machines["vm-remote"].Host)
	}
}
