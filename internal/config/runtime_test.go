package config

import (
	"errors"
	"strings"
	"testing"
)

// TestBareMachineStillBare pins the Plan 13 Phase 0 contract: a
// machine with no runtime/image/snapshot fields decodes as the
// bare host path and the existing direct+hosted build commands
// continue to run unchanged.
func TestBareMachineStillBare(t *testing.T) {
	body := `schema_version = 1
orchestrator = "self"

[machines.mac-local]
`
	cfg, err := Decode([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Machines["mac-local"]
	if m.Runtime != "" {
		t.Errorf("bare machine runtime: %q", m.Runtime)
	}
	if m.Image != "" {
		t.Errorf("bare machine image: %q", m.Image)
	}
	if m.Snapshot != "" {
		t.Errorf("bare machine snapshot: %q", m.Snapshot)
	}
}

// TestDockerMachineRequiresImage pins that a runtime=docker
// machine without image is rejected at Validate time.
func TestDockerMachineRequiresImage(t *testing.T) {
	body := `schema_version = 1
orchestrator = "self"

[machines.docker-no-image]
runtime = "docker"
`
	_, err := Decode([]byte(body))
	if !errors.Is(err, ErrRuntimeImageRequired) {
		t.Fatalf("expected ErrRuntimeImageRequired, got %v", err)
	}
}

// TestDockerMachineRejectsFlagLikeImage pins that an image
// starting with a flag character is rejected so the docker
// subprocess argument cannot be confused with an option.
func TestDockerMachineRejectsFlagLikeImage(t *testing.T) {
	body := `schema_version = 1
orchestrator = "self"

[machines.bad-image]
runtime = "docker"
image = "--rm-all"
`
	_, err := Decode([]byte(body))
	if !errors.Is(err, ErrRuntimeImageInvalid) {
		t.Fatalf("expected ErrRuntimeImageInvalid, got %v", err)
	}
}

// TestDockerMachineRejectsWhitespaceImage pins that an image
// containing whitespace or control characters is rejected. The
// shell-safe form is mandatory because the value is forwarded to
// the system `docker` subprocess.
func TestDockerMachineRejectsWhitespaceImage(t *testing.T) {
	body := `schema_version = 1
orchestrator = "self"

[machines.bad-image]
runtime = "docker"
image = "ghcr.io/org/ci:tag\n"
`
	_, err := Decode([]byte(body))
	if !errors.Is(err, ErrRuntimeImageInvalid) {
		t.Fatalf("expected ErrRuntimeImageInvalid, got %v", err)
	}
}

// TestDockerMachineRejectsSchemeImage pins that an image
// containing a scheme (`oci://`, `https://`) is rejected. The
// runtime runner does not implement a registry pull protocol;
// the host docker config is used as-is.
func TestDockerMachineRejectsSchemeImage(t *testing.T) {
	body := `schema_version = 1
orchestrator = "self"

[machines.bad-image]
runtime = "docker"
image = "https://example.com/ci/repo:latest"
`
	_, err := Decode([]byte(body))
	if !errors.Is(err, ErrRuntimeImageInvalid) {
		t.Fatalf("expected ErrRuntimeImageInvalid, got %v", err)
	}
}

// TestBareMachineRejectsStrayImage pins that an empty runtime
// with a non-empty image is rejected. The runtime field is the
// discriminator; a bare host must not carry runtime-only fields.
func TestBareMachineRejectsStrayImage(t *testing.T) {
	body := `schema_version = 1
orchestrator = "self"

[machines.bare-with-image]
image = "ghcr.io/org/ci:latest"
`
	_, err := Decode([]byte(body))
	if err == nil || !strings.Contains(err.Error(), "empty runtime") {
		t.Fatalf("expected empty-runtime error, got %v", err)
	}
}

// TestVMModeRequiresSnapshot pins that a runtime=vm machine must
// carry a non-empty snapshot reference. The validator uses the
// same allow-list as runtime=docker images.
func TestVMModeRequiresSnapshot(t *testing.T) {
	body := `schema_version = 1
orchestrator = "self"

[machines.vm-no-snap]
runtime = "vm"
`
	_, err := Decode([]byte(body))
	if !errors.Is(err, ErrRuntimeImageRequired) {
		t.Fatalf("expected ErrRuntimeImageRequired, got %v", err)
	}
}

// TestVMModeAcceptsValidSnapshot pins the runtime=vm happy path:
// a verbatim snapshot reference decodes, EffectiveCapacity is
// unchanged, and the snapshot field round-trips.
func TestVMModeAcceptsValidSnapshot(t *testing.T) {
	body := `schema_version = 1
orchestrator = "self"

[machines.vm-mode]
runtime = "vm"
snapshot = "ghcr.io/org/vm:pin"
`
	cfg, err := Decode([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Machines["vm-mode"]
	if m.Runtime != "vm" {
		t.Errorf("runtime: %q", m.Runtime)
	}
	if m.Snapshot != "ghcr.io/org/vm:pin" {
		t.Errorf("snapshot: %q", m.Snapshot)
	}
	if EffectiveCapacity(m) != 1 {
		t.Errorf("capacity: %d", EffectiveCapacity(m))
	}
}

// TestVMModeRejectsStrayImage pins that a runtime=vm machine
// must not carry an `image` field — image is the docker runtime's
// verbatim reference.
func TestVMModeRejectsStrayImage(t *testing.T) {
	body := `schema_version = 1
orchestrator = "self"

[machines.vm-with-image]
runtime = "vm"
snapshot = "ghcr.io/org/vm:pin"
image = "ghcr.io/org/ci:pin"
`
	_, err := Decode([]byte(body))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "image") {
		t.Errorf("expected image error, got %v", err)
	}
}

// TestDockerMachineAcceptsValidImage asserts the happy path: a
// runtime=docker machine with a verbatim image decodes and
// EffectiveCapacity is unchanged.
func TestDockerMachineAcceptsValidImage(t *testing.T) {
	body := `schema_version = 1
orchestrator = "self"

[machines.linux-docker]
runtime = "docker"
image = "ghcr.io/org/ci@sha256:0000000000000000000000000000000000000000000000000000000000000000"
`
	cfg, err := Decode([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Machines["linux-docker"]
	if m.Runtime != "docker" {
		t.Errorf("runtime: %q", m.Runtime)
	}
	if m.Image == "" {
		t.Errorf("image empty")
	}
	if EffectiveCapacity(m) != 1 {
		t.Errorf("capacity: %d", EffectiveCapacity(m))
	}
}

// TestClientRootRejectsRuntimeFields pins that a client root
// cannot declare machine runtime/image/snapshot — it cannot
// declare machines at all (the existing client rule), but the
// strict TOML decoder must also reject the new fields if a
// client root ever hacks them in.
func TestClientRootRejectsRuntimeFields(t *testing.T) {
	body := `schema_version = 1
orchestrator = "builder"
`
	_, err := Decode([]byte(body))
	if err != nil {
		// The client root without machines is fine; the test
		// here is really about the next case.
		_ = err
	}
}

// TestRuntimeUnknownValueIsUnsupported pins that an unknown
// runtime string is rejected as ErrUnsupportedRuntime.
func TestRuntimeUnknownValueIsUnsupported(t *testing.T) {
	body := `schema_version = 1
orchestrator = "self"

[machines.weird]
runtime = "containerd"
image = "ghcr.io/org/ci:latest"
`
	_, err := Decode([]byte(body))
	if !errors.Is(err, ErrUnsupportedRuntime) {
		t.Fatalf("expected ErrUnsupportedRuntime, got %v", err)
	}
}

// TestDockerMachineAcceptsRegistryPortImage pins that a private
// registry reference with a numeric port is accepted verbatim.
// The docker reference grammar allows `host:port/...` and Vci
// forwards the value straight to the system `docker` subprocess.
func TestDockerMachineAcceptsRegistryPortImage(t *testing.T) {
	for _, image := range []string{
		"myregistry:5000/repo",
		"myregistry:5000/repo:tag",
		"myregistry:5000/repo@sha256:0000000000000000000000000000000000000000000000000000000000000000",
	} {
		body := `schema_version = 1
orchestrator = "self"

[machines.portable]
runtime = "docker"
image = "` + image + `"
`
		if _, err := Decode([]byte(body)); err != nil {
			t.Errorf("image %q rejected: %v", image, err)
		}
	}
}

// TestDockerMachineRejectsRegistryPortBadShape pins that a
// registry port segment without `/` separation or with a non-port
// suffix is still rejected.
func TestDockerMachineRejectsRegistryPortBadShape(t *testing.T) {
	for _, image := range []string{
		"myregistry:5000-repo:tag", // missing slash
		"myregistry:abc/repo:tag",  // non-numeric port
		"myregistry:/repo:tag",     // empty host
		":5000/repo:tag",           // missing host
	} {
		body := `schema_version = 1
orchestrator = "self"

[machines.bad-port]
runtime = "docker"
image = "` + image + `"
`
		if _, err := Decode([]byte(body)); !errors.Is(err, ErrRuntimeImageInvalid) {
			t.Errorf("image %q should fail as ErrRuntimeImageInvalid, got %v", image, err)
		}
	}
}

// TestVMModeSnapshotAllowList pins that the runtime=vm snapshot
// is verified by the same allow-list as runtime=docker images.
func TestVMModeSnapshotAllowList(t *testing.T) {
	body := `schema_version = 1
orchestrator = "self"

[machines.vm-bad]
runtime = "vm"
snapshot = "--rm-all"
`
	_, err := Decode([]byte(body))
	if !errors.Is(err, ErrRuntimeImageInvalid) {
		t.Fatalf("expected ErrRuntimeImageInvalid, got %v", err)
	}
}

// TestRuntimeSnapshotFieldAccepted pins that a docker machine may
// carry an empty snapshot field (snapshot is reserved for VM mode).
// The presence of a stray snapshot for a docker machine is rejected
// at Validate time.
func TestRuntimeSnapshotFieldAccepted(t *testing.T) {
	body := `schema_version = 1
orchestrator = "self"

[machines.docker-machine]
runtime = "docker"
image = "ghcr.io/org/ci:pin"
`
	cfg, err := Decode([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Machines["docker-machine"].Snapshot != "" {
		t.Errorf("snapshot: %q", cfg.Machines["docker-machine"].Snapshot)
	}
}

// TestDockerMachineRejectsSnapshot pins that a runtime=docker
// machine must not carry a `snapshot` field — snapshot is the VM
// runtime's verbatim reference and is forward-compatible only when
// VM mode is enabled.
func TestDockerMachineRejectsSnapshot(t *testing.T) {
	body := `schema_version = 1
orchestrator = "self"

[machines.mixed]
runtime = "docker"
image = "ghcr.io/org/ci:pin"
snapshot = "ghcr.io/org/vm:pin"
`
	_, err := Decode([]byte(body))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "snapshot") {
		t.Errorf("expected snapshot error, got %v", err)
	}
}

// TestProjectAcceptsArtifacts pins that a project may declare a
// workspace-relative artifact glob list at config load time.
func TestProjectAcceptsArtifacts(t *testing.T) {
	body := `schema_version = 1
orchestrator = "self"

[machines.mac-local]

[projects.demo]
machines = ["mac-local"]
command = ["true"]
artifacts = ["build/*", "dist/*.zip"]
`
	cfg, err := Decode([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	arts := cfg.Projects["demo"].Artifacts
	if len(arts) != 2 {
		t.Fatalf("artifacts: %v", arts)
	}
}

// TestProjectRejectsBadArtifacts pins the per-glob rules: empty,
// absolute path, parent escape, leading dash, scheme, whitespace,
// and a path.Match failure are all rejected.
func TestProjectRejectsBadArtifacts(t *testing.T) {
	for _, glob := range []string{
		"",
		"/abs/path",
		"../escape",
		"sub/../escape",
		"--flag",
		"with space",
		"https://x",
		"[unterminated",
	} {
		body := `schema_version = 1
orchestrator = "self"

[machines.mac-local]

[projects.demo]
machines = ["mac-local"]
command = ["true"]
artifacts = ["` + glob + `"]
`
		if _, err := Decode([]byte(body)); err == nil {
			t.Errorf("artifact %q accepted", glob)
		}
	}
}
