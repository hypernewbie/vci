package app

// Plan 15 Phase 2/3 remote-execution tests. Two layers: fake
// `ssh`/`scp` stubs in PATH (no server, always green) and one
// end-to-end remote-bare build through the controlled loopback sshd
// fixture (skips when sshd is unavailable). No real docker daemon,
// tart VM, or remote machine is required.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/model"
)

// writePathStub writes an executable shell stub into dir and prepends
// dir to PATH for the test.
func writePathStub(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+previous)
}

// setupRemoteRoot initializes a coordinator root with one remote
// machine (host=builder) and one project attached to it.
func setupRemoteRoot(t *testing.T, machine config.Machine, project config.Project) layout.Layout {
	t.Helper()
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	l := layout.Layout{Root: root}
	if err := Initialize(l); err != nil {
		t.Fatal(err)
	}
	if err := AddMachine(l, "mac-remote", machine); err != nil {
		t.Fatal(err)
	}
	if err := AddProject(l, "demo", project); err != nil {
		t.Fatal(err)
	}
	return l
}

// TestRemoteBareBuildViaFakeSSH pins the remote-bare happy path: the
// coordinator materializes the workspace, stages it through the fake
// ssh, runs `true` remotely, and publishes a succeeded run whose
// machine is mac-remote. The recorded ssh argv carries the host
// alias, the workspace, and the runtime binary.
func TestRemoteBareBuildViaFakeSSH(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	writePathStub(t, dir, "ssh", "#!/bin/sh\necho \"$*\" >> "+logPath+"\ncat >/dev/null 2>&1\nexit 0\n")

	l := setupRemoteRoot(t, config.Machine{Host: "builder"}, config.Project{Machines: []string{"mac-remote"}, Command: []string{"true"}})
	prep, err := Prepare(context.Background(), l, makeSourceTree(t, "demo"))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	result, err := ExecutePrepared(context.Background(), l, prep.Record.ID)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.State != model.RunSucceeded {
		t.Fatalf("state: %s failure=%s", result.State, result.Failure)
	}
	if result.Machine != "mac-remote" {
		t.Errorf("machine: %q", result.Machine)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ssh stub log missing: %v", err)
	}
	s := string(log)
	for _, want := range []string{
		"builder",
		"~/.vci/state/work/" + string(prep.Record.ID),
		"'true'",
		"rm -rf -- ~/.vci/state/work/" + string(prep.Record.ID),
	} {
		if !strings.Contains(s, want) {
			t.Errorf("ssh log missing %q: %s", want, s)
		}
	}
	// No Vci state or ssh leakage beyond the workspace path.
	for _, banned := range []string{"VCI_ROOT", ".ssh"} {
		if strings.Contains(s, banned) {
			t.Errorf("leaked %q: %s", banned, s)
		}
	}
}

// TestRemoteDockerBuildViaFakeSSH pins that a remote docker machine
// composes the documented `docker run` argv into the remote shell
// with the remote workspace as the only mount.
func TestRemoteDockerBuildViaFakeSSH(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	writePathStub(t, dir, "ssh", "#!/bin/sh\necho \"$*\" >> "+logPath+"\ncat >/dev/null 2>&1\nexit 0\n")

	image := "ghcr.io/org/ci:pin"
	l := setupRemoteRoot(t, config.Machine{Host: "builder", Runtime: "docker", Image: image}, config.Project{Machines: []string{"mac-remote"}, Command: []string{"true"}})
	prep, err := Prepare(context.Background(), l, makeSourceTree(t, "demo"))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	result, err := ExecutePrepared(context.Background(), l, prep.Record.ID)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.State != model.RunSucceeded {
		t.Fatalf("state: %s failure=%s", result.State, result.Failure)
	}
	log, _ := os.ReadFile(logPath)
	s := string(log)
	ws := "~/.vci/state/work/" + string(prep.Record.ID)
	for _, want := range []string{
		"exec 'docker' 'run' '--rm'",
		ws + ":/vci/work:ro",
		"'ghcr.io/org/ci:pin'",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("ssh log missing %q: %s", want, s)
		}
	}
	// Exactly one mount, and it is the workspace.
	if strings.Count(s, "-v") != 1 {
		t.Errorf("expected exactly one -v mount: %s", s)
	}
}

// TestRemoteVMBuildViaFakeSSH pins that a remote vm machine composes
// the documented `tart run` argv into the remote shell.
func TestRemoteVMBuildViaFakeSSH(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	writePathStub(t, dir, "ssh", "#!/bin/sh\necho \"$*\" >> "+logPath+"\ncat >/dev/null 2>&1\nexit 0\n")

	snapshot := "ghcr.io/org/vm:pin"
	l := setupRemoteRoot(t, config.Machine{Host: "builder", Runtime: "vm", Snapshot: snapshot}, config.Project{Machines: []string{"mac-remote"}, Command: []string{"true"}})
	prep, err := Prepare(context.Background(), l, makeSourceTree(t, "demo"))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	result, err := ExecutePrepared(context.Background(), l, prep.Record.ID)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.State != model.RunSucceeded {
		t.Fatalf("state: %s failure=%s", result.State, result.Failure)
	}
	log, _ := os.ReadFile(logPath)
	s := string(log)
	ws := "~/.vci/state/work/" + string(prep.Record.ID)
	for _, want := range []string{"exec 'tart' 'run' '--no-gui'", ws + ":/vci/work", "'ghcr.io/org/vm:pin'", "--", "'true'"} {
		if !strings.Contains(s, want) {
			t.Errorf("ssh log missing %q: %s", want, s)
		}
	}
}

// TestRemoteBuildCollectsArtifactsViaFakeSCP pins the remote artifact
// contract: after the remote command finishes, the workspace is
// fetched back with `scp` and the local collector publishes the
// matches. The fake scp stub materializes the fetched tree so
// CollectArtifacts has real bytes to copy.
func TestRemoteBuildCollectsArtifactsViaFakeSCP(t *testing.T) {
	dir := t.TempDir()
	sshLog := filepath.Join(dir, "ssh.log")
	scpLog := filepath.Join(dir, "scp.log")
	writePathStub(t, dir, "ssh", "#!/bin/sh\necho \"$*\" >> "+sshLog+"\ncat >/dev/null 2>&1\nexit 0\n")
	// Fake scp: parse the remote source (the arg containing `:`),
	// the local destination (the last arg), and lay down the fetched
	// tree at <dest>/<run>/build/out.bin exactly as `scp -r` would.
	scpStub := `#!/bin/sh
echo "$*" >> ` + scpLog + `
remote=""
for a in "$@"; do
  case "$a" in
    *:*) remote="$a" ;;
  esac
done
last=""
for a in "$@"; do last="$a"; done
run="${remote##*/}"
mkdir -p "$last/$run/build"
printf 'fake-zip-bytes' > "$last/$run/build/out.bin"
exit 0
`
	writePathStub(t, dir, "scp", scpStub)

	l := setupRemoteRoot(t, config.Machine{Host: "builder"}, config.Project{Machines: []string{"mac-remote"}, Command: []string{"true"}, Artifacts: []string{"build/*"}})
	prep, err := Prepare(context.Background(), l, makeSourceTree(t, "demo"))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	result, err := ExecutePrepared(context.Background(), l, prep.Record.ID)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.State != model.RunSucceeded {
		t.Fatalf("state: %s failure=%s", result.State, result.Failure)
	}
	scpLogData, _ := os.ReadFile(scpLog)
	s := string(scpLogData)
	ws := "~/.vci/state/work/" + string(prep.Record.ID)
	if !strings.Contains(s, "builder:"+ws) {
		t.Errorf("scp log missing remote source %q: %s", ws, s)
	}
	if len(result.Artifacts) != 1 || result.Artifacts[0] != "build/out.bin" {
		t.Fatalf("artifacts: %v", result.Artifacts)
	}
	if result.ArtifactsTruncated {
		t.Errorf("artifacts_truncated=true under cap")
	}
	// The collected artifact is durable on the coordinator.
	runDir, err := l.RunDir(string(prep.Record.ID))
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(runDir, "artifacts", "build", "out.bin")
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("durable artifact missing: %v", err)
	}
}

// TestRemoteBareBuildViaSSHDFixture drives the real loopback sshd:
// the coordinator stages the workspace, runs `true` on the remote
// session, and publishes a succeeded run. It skips when ssh/sshd are
// unavailable.
func TestRemoteBareBuildViaSSHDFixture(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}
	l := setupRemoteRoot(t, config.Machine{Host: fixture.SSHAlias()}, config.Project{Machines: []string{"mac-remote"}, Command: []string{"true"}})
	prep, err := Prepare(context.Background(), l, makeSourceTree(t, "demo"))
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	result, err := ExecutePrepared(context.Background(), l, prep.Record.ID)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.State != model.RunSucceeded {
		t.Fatalf("state: %s failure=%s", result.State, result.Failure)
	}
	if result.Machine != "mac-remote" {
		t.Errorf("machine: %q", result.Machine)
	}
	// The remote tree was cleaned up: the fixture home's
	// state/work/<run> must not exist.
	remoteWork := filepath.Join(fixture.homeDir, ".vci", "state", "work", string(prep.Record.ID))
	if _, err := os.Stat(remoteWork); !os.IsNotExist(err) {
		t.Errorf("remote workspace turd remains: %v", err)
	}
}
