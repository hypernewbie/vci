package runtime

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypernewbie/vci/internal/process"
)

func TestLocalCapturesJobExit(t *testing.T) {
	var out, errOut bytes.Buffer
	result, err := (Local{}).ExecuteSupervised(context.Background(), Request{Executable: "/bin/sh", Args: []string{"-c", "printf ok; printf bad >&2; exit 7"}, Workspace: t.TempDir(), Stdout: &out, Stderr: &errOut}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 7 || out.String() != "ok" || errOut.String() != "bad" {
		t.Fatalf("result=%+v out=%q err=%q", result, out.String(), errOut.String())
	}
}

// TestDockerCommandArgvShape pins the exact run-by-run arg shape
// the docker executor shells out. The order, mounts, network,
// user, cgroup limits, and image position are the contract.
func TestDockerCommandArgvShape(t *testing.T) {
	d := Docker{Image: "ghcr.io/org/ci:pin"}
	argv, err := d.CommandArgv("/tmp/work", "/vci/work", []string{"go", "test", "./..."})
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := []string{
		"run", "--rm",
		"-v", "/tmp/work:/vci/work:ro",
		"-w", "/vci/work",
		"--network", "none",
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--cpus", "2",
		"--memory", "4g",
		"ghcr.io/org/ci:pin",
		"go", "test", "./...",
	}
	if !equalSlice(argv, wantPrefix) {
		t.Fatalf("argv: got %v want %v", argv, wantPrefix)
	}
}

// TestDockerMissingImageRejected pins that an empty image at
// construction time is rejected before any subprocess call.
func TestDockerMissingImageRejected(t *testing.T) {
	d := Docker{}
	_, err := d.CommandArgv("/tmp/work", "/vci/work", []string{"true"})
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestDockerMissingWorkspaceRejected pins that an empty workspace
// is rejected.
func TestDockerMissingWorkspaceRejected(t *testing.T) {
	d := Docker{Image: "img:tag"}
	_, err := d.CommandArgv("", "/vci/work", []string{"true"})
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestDockerRunsViaStub pins that the docker runner reaches a
// stub `docker` in PATH and returns the recorded exit code. The
// fixture writes a script that echoes the args to a log file so
// the test can assert the exact arg shape.
func TestDockerRunsViaStub(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
echo "$*" >> ` + logPath + `
exit 0
`
	dockerPath := filepath.Join(dir, "docker")
	if err := os.WriteFile(dockerPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+previous)

	workspace := t.TempDir()
	d := Docker{Image: "ghcr.io/org/ci:pin"}
	var stdout, stderr bytes.Buffer
	res, err := d.ExecuteSupervised(context.Background(), Request{
		Executable: "go",
		Args:       []string{"test", "./..."},
		Workspace:  workspace,
		Stdout:     &stdout,
		Stderr:     &stderr,
	}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit: %d", res.ExitCode)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(log)
	for _, want := range []string{"run", "--rm", "-v", workspace + ":/vci/work:ro", "--network", "none", "ghcr.io/org/ci:pin", "go", "test"} {
		if !strings.Contains(s, want) {
			t.Errorf("stub docker log missing %q: %s", want, s)
		}
	}
	// The expected mount is the per-run workspace. The runner never
	// mounts the VCI state root, .vci, or .ssh. The exact mount
	// substring must appear; the runner must not mount any other
	// path inside the VCI root or under the user's home.
	expectedMount := workspace + ":/vci/work:ro"
	if !strings.Contains(s, expectedMount) {
		t.Errorf("expected mount %q missing in docker arg: %s", expectedMount, s)
	}
	if strings.Contains(s, ":/vci/work:ro,") || countOccurrences(s, ":/vci/work:ro") != 1 {
		t.Errorf("unexpected multiple vci/work mounts: %s", s)
	}
	// Banned: mounting the VCI state root, ~/.vci, or ~/.ssh under
	// any context. The per-run workspace (state/work/<run>) and
	// per-run TMPDIR (state/work/<run>/.tmp) are allowed because
	// they are the only paths the runner ever mounts.
	for _, banned := range []string{".vci/", ":/home/", ":/root/", ".ssh/"} {
		if strings.Contains(s, banned) {
			t.Errorf("dangerous mount leaked: %s in %s", banned, s)
		}
	}
	if mountSentinel := workspace + "/../.."; strings.Contains(s, mountSentinel) {
		t.Errorf("workspace mount escapes: %s in %s", mountSentinel, s)
	}
}

// TestDockerNonZeroExitReturnsJobFailure pins that an exit-1
// docker invocation is a job failure, not an infrastructure
// failure. The build path uses ExitCode to classify the result.
func TestDockerNonZeroExitReturnsJobFailure(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nexit 1\n"
	dockerPath := filepath.Join(dir, "docker")
	if err := os.WriteFile(dockerPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+previous)

	workspace := t.TempDir()
	d := Docker{Image: "img:tag"}
	res, err := d.ExecuteSupervised(context.Background(), Request{
		Executable: "true",
		Workspace:  workspace,
	}, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.ExitCode != 1 {
		t.Fatalf("exit: %d", res.ExitCode)
	}
}

// TestDockerMissingBinaryIsUnavailable pins that the absence of
// the `docker` binary is an ErrRuntimeUnavailable infrastructure
// failure, not a job failure.
func TestDockerMissingBinaryIsUnavailable(t *testing.T) {
	empty := t.TempDir()
	previous := os.Getenv("PATH")
	t.Setenv("PATH", empty+string(os.PathListSeparator)+previous)

	workspace := t.TempDir()
	d := Docker{Image: "img:tag"}
	_, err := d.ExecuteSupervised(context.Background(), Request{
		Executable: "true",
		Workspace:  workspace,
	}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestDockerRunnerPassesThroughOnStart pins that the runner
// passes the supervised process group's start signal to the
// caller, matching the executor.Local contract.
func TestDockerRunnerPassesThroughOnStart(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nexit 0\n"
	dockerPath := filepath.Join(dir, "docker")
	if err := os.WriteFile(dockerPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+previous)

	workspace := t.TempDir()
	d := Docker{Image: "img:tag"}
	called := false
	_, err := d.ExecuteSupervised(context.Background(), Request{
		Executable: "true",
		Workspace:  workspace,
	}, func(running process.Running) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("onStart not called")
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func countOccurrences(s, sub string) int {
	if sub == "" {
		return 0
	}
	count := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			count++
		}
	}
	return count
}

// TestDockerUIDGIDMatchesHost pins that the docker --user flag
// carries the host coordinator process UID:GID, not a hard-coded
// 0:0. The image mount is read-only and the container must write
// to the workspace as the same host identity.
func TestDockerUIDGIDMatchesHost(t *testing.T) {
	d := Docker{Image: "img:tag"}
	argv, err := d.CommandArgv("/tmp/work", "/vci/work", []string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	// argv shape: ..., "--user", "UID:GID", ...
	want := fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	found := false
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--user" && argv[i+1] == want {
			found = true
			break
		}
	}
	if !found {
		joined := strings.Join(argv, " ")
		t.Fatalf("argv missing --user %s: %s", want, joined)
	}
}

// TestDockerCommandArgvRemoteVerbatimWorkspace pins that the
// remote-host mirror of CommandArgv uses the workspace path verbatim
// (no local filepath.Abs), because the path names a directory on the
// remote host reached via ssh. Every other arg matches the local
// shape.
func TestDockerCommandArgvRemoteVerbatimWorkspace(t *testing.T) {
	d := Docker{Image: "ghcr.io/org/ci:pin"}
	argv, err := d.CommandArgvRemote("~/.vci/state/work/run_abc", []string{"go", "test"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{
		"run --rm",
		"-v ~/.vci/state/work/run_abc:/vci/work:ro",
		"-w /vci/work",
		"--network none",
		"ghcr.io/org/ci:pin",
		"go test",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv missing %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, filepath.Join(t.TempDir(), "..")) || strings.HasPrefix(joined, "/") {
		t.Errorf("remote argv was locally Abs-resolved: %s", joined)
	}
}

// TestVMCommandArgvShape pins the exact run-by-run arg shape the
// VM executor shells out. The order, workdir, cgroup limits,
// snapshot position, and `--` separator are the contract.
func TestVMCommandArgvShape(t *testing.T) {
	v := VM{Snapshot: "ghcr.io/org/vm:pin", Binary: "tart"}
	argv, err := v.CommandArgv("/tmp/work", []string{"go", "test", "./..."})
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := []string{
		"tart", "run", "--no-gui",
		"--dir", "/tmp/work:/vci/work",
		"--workdir", "/vci/work",
		"--cpus", "2",
		"--memory", "4g",
		"ghcr.io/org/vm:pin",
		"--",
		"go", "test", "./...",
	}
	if !equalSlice(argv, wantPrefix) {
		t.Fatalf("argv: got %v want %v", argv, wantPrefix)
	}
}

// TestVMMissingSnapshotRejected pins that an empty snapshot at
// construction time is rejected before any subprocess call.
func TestVMMissingSnapshotRejected(t *testing.T) {
	v := VM{Binary: "tart"}
	if _, err := v.CommandArgv("/tmp/work", []string{"true"}); err == nil {
		t.Fatal("expected error")
	}
}

// TestVMMissingWorkspaceRejected pins that an empty workspace is
// rejected.
func TestVMMissingWorkspaceRejected(t *testing.T) {
	v := VM{Snapshot: "snap:pin", Binary: "tart"}
	if _, err := v.CommandArgv("", []string{"true"}); err == nil {
		t.Fatal("expected error")
	}
}

// TestVMRunsViaStub pins that the VM runner reaches a stub `tart`
// binary in PATH and returns the recorded exit code. The fixture
// writes a script that echoes the args to a log file so the test
// can assert the exact arg shape.
func TestVMRunsViaStub(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
echo "$*" >> ` + logPath + `
exit 0
`
	tartPath := filepath.Join(dir, "tart")
	if err := os.WriteFile(tartPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+previous)

	workspace := t.TempDir()
	v := VM{Snapshot: "ghcr.io/org/vm:pin", Binary: "tart"}
	var stdout, stderr bytes.Buffer
	res, err := v.ExecuteSupervised(context.Background(), Request{
		Executable: "go",
		Args:       []string{"test", "./..."},
		Workspace:  workspace,
		Stdout:     &stdout,
		Stderr:     &stderr,
	}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit: %d", res.ExitCode)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(log)
	for _, want := range []string{"run", "--no-gui", "--workdir", "/vci/work", "ghcr.io/org/vm:pin", "--", "go", "test"} {
		if !strings.Contains(s, want) {
			t.Errorf("stub log missing %q: %s", want, s)
		}
	}
	// The runner never mounts ~/.vci or ~/.ssh. Snapshot is a
	// positional argument; the only workspace path that appears
	// is the one the runner forwards (--workdir /vci/work).
	for _, banned := range []string{".vci/", ".ssh/"} {
		if strings.Contains(s, banned) {
			t.Errorf("dangerous path leaked: %s in %s", banned, s)
		}
	}
}

// TestVMNonZeroExitReturnsJobFailure pins that an exit-1 VM
// invocation is a job failure, not an infrastructure failure. The
// build path uses ExitCode to classify the result.
func TestVMNonZeroExitReturnsJobFailure(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nexit 1\n"
	tartPath := filepath.Join(dir, "tart")
	if err := os.WriteFile(tartPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+previous)

	workspace := t.TempDir()
	v := VM{Snapshot: "snap:pin", Binary: "tart"}
	res, err := v.ExecuteSupervised(context.Background(), Request{
		Executable: "true",
		Workspace:  workspace,
	}, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if res.ExitCode != 1 {
		t.Fatalf("exit: %d", res.ExitCode)
	}
}

// TestVMMissingBinaryIsUnavailable pins that the absence of the
// configured VM binary is an ErrRuntimeUnavailable infrastructure
// failure, not a job failure.
func TestVMMissingBinaryIsUnavailable(t *testing.T) {
	empty := t.TempDir()
	previous := os.Getenv("PATH")
	t.Setenv("PATH", empty+string(os.PathListSeparator)+previous)

	workspace := t.TempDir()
	v := VM{Snapshot: "snap:pin", Binary: "tart"}
	_, err := v.ExecuteSupervised(context.Background(), Request{
		Executable: "true",
		Workspace:  workspace,
	}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestVMRunnerPassesThroughOnStart pins that the runner passes
// the supervised process group's start signal to the caller,
// matching the executor.Local contract.
func TestVMRunnerPassesThroughOnStart(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nexit 0\n"
	tartPath := filepath.Join(dir, "tart")
	if err := os.WriteFile(tartPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+previous)

	workspace := t.TempDir()
	v := VM{Snapshot: "snap:pin", Binary: "tart"}
	called := false
	_, err := v.ExecuteSupervised(context.Background(), Request{
		Executable: "true",
		Workspace:  workspace,
	}, func(running process.Running) error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("onStart not called")
	}
}

// TestVMWorkdirPin pins that the VM runner always exposes the
// workspace at `/vci/work`. The host working directory is set to
// the workspace, so the guest sees the run's selected source.
func TestVMWorkdirPin(t *testing.T) {
	v := VM{Snapshot: "snap:pin", Binary: "tart"}
	argv, err := v.CommandArgv("/tmp/work", []string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--workdir" && argv[i+1] != "/vci/work" {
			t.Fatalf("workdir: %q", argv[i+1])
		}
	}
}

// TestVMResourcesDefault pins the documented conservative cgroup
// defaults: 2 CPUs, 4g memory. Custom Resources override them.
func TestVMResourcesDefault(t *testing.T) {
	v := VM{Snapshot: "snap:pin", Binary: "tart"}
	argv, err := v.CommandArgv("/tmp/work", []string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--cpus 2") {
		t.Errorf("missing default cpus: %s", joined)
	}
	if !strings.Contains(joined, "--memory 4g") {
		t.Errorf("missing default memory: %s", joined)
	}

	v2 := VM{Snapshot: "snap:pin", Binary: "tart", Resources: Resources{CPUs: 8, Memory: "16g"}}
	argv2, err := v2.CommandArgv("/tmp/work", []string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	joined2 := strings.Join(argv2, " ")
	if !strings.Contains(joined2, "--cpus 8") {
		t.Errorf("missing custom cpus: %s", joined2)
	}
	if !strings.Contains(joined2, "--memory 16g") {
		t.Errorf("missing custom memory: %s", joined2)
	}
}

// TestVMResolvedExecutable pins that the executor.Result records
// the configured binary as the resolved executable so the build
// envelope reflects which runner fired.
func TestVMResolvedExecutable(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nexit 0\n"
	tartPath := filepath.Join(dir, "tart")
	if err := os.WriteFile(tartPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+previous)

	v := VM{Snapshot: "snap:pin", Binary: "tart"}
	res, _ := v.ExecuteSupervised(context.Background(), Request{
		Executable: "true",
		Workspace:  t.TempDir(),
	}, nil)
	if res.ResolvedExecutable != "tart" {
		t.Errorf("resolved: %q", res.ResolvedExecutable)
	}
}

// TestVMCommandArgvIncludesWorkspaceMount pins that the VM arg
// slice forwards the host workspace verbatim through the
// documented tart directory-share flag (`--dir
// <absWorkspace>:/vci/work`) and that the command after the `--`
// separator is preserved. The pre-fix shape dropped the workspace
// source entirely, so the guest saw an empty `/vci/work`.
func TestVMCommandArgvIncludesWorkspaceMount(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	v := VM{Snapshot: "ghcr.io/org/vm:pin", Binary: "tart"}
	argv, err := v.CommandArgv(workspace, []string{"go", "test", "./..."})
	if err != nil {
		t.Fatal(err)
	}
	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		t.Fatal(err)
	}
	sep := -1
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "--dir" && argv[i+1] == absWorkspace+":/vci/work" {
			sep = i
		}
	}
	if sep < 0 {
		t.Fatalf("argv missing --dir %s:/vci/work: %v", absWorkspace, argv)
	}
	sepIdx := -1
	for i := sep + 2; i < len(argv); i++ {
		if argv[i] == "--" {
			sepIdx = i
			break
		}
	}
	if sepIdx < 0 {
		t.Fatalf("missing `--` separator after the mount: %v", argv)
	}
	rest := argv[sepIdx+1:]
	if !equalSlice(rest, []string{"go", "test", "./..."}) {
		t.Fatalf("command after `--`: got %v", rest)
	}
}

// TestVMExecuteSupervisedMountsWorkspace pins that a real
// ExecuteSupervised invocation forwards the workspace as a tart
// directory share exactly once and never leaks `~/.vci`,
// `state/`, or `~/.ssh`. The stub `tart` records its argv so the
// test can assert the mount source appears verbatim.
func TestVMExecuteSupervisedMountsWorkspace(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
echo "$*" >> ` + logPath + `
exit 0
`
	tartPath := filepath.Join(dir, "tart")
	if err := os.WriteFile(tartPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+previous)

	workspace := t.TempDir()
	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		t.Fatal(err)
	}
	v := VM{Snapshot: "ghcr.io/org/vm:pin", Binary: "tart"}
	var stdout, stderr bytes.Buffer
	res, err := v.ExecuteSupervised(context.Background(), Request{
		Executable: "go",
		Args:       []string{"test", "./..."},
		Workspace:  workspace,
		Stdout:     &stdout,
		Stderr:     &stderr,
	}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit: %d", res.ExitCode)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(log)
	mount := "--dir " + absWorkspace + ":/vci/work"
	if !strings.Contains(s, mount) {
		t.Fatalf("stub log missing workspace mount %q: %s", mount, s)
	}
	if n := strings.Count(s, absWorkspace); n != 1 {
		t.Errorf("workspace source appears %d times (want exactly once): %s", n, s)
	}
	if n := strings.Count(s, "--dir"); n != 1 {
		t.Errorf("--dir appears %d times (want exactly one share): %s", n, s)
	}
	// The runner must never mount VCI control paths. Strip the one
	// legitimate workspace source first: in Vci self-builds the
	// per-run workspace lives under <VCI_ROOT>/state/work/<run>,
	// so `state/` is part of the workspace's own path. The
	// assertion targets any *additional* mount source, which is
	// exactly what the exactly-once checks above already pin.
	rest := strings.ReplaceAll(s, absWorkspace, "")
	for _, banned := range []string{".vci/", "state/", ".ssh/"} {
		if strings.Contains(rest, banned) {
			t.Errorf("dangerous path leaked: %s in %s", banned, s)
		}
	}
}

// TestVMCommandArgvRemoteVerbatimWorkspace pins that the remote-host
// mirror of CommandArgv uses the workspace path verbatim (no local
// filepath.Abs), because the path names a directory on the remote
// host reached via ssh. Every other arg matches the local shape.
func TestVMCommandArgvRemoteVerbatimWorkspace(t *testing.T) {
	v := VM{Snapshot: "ghcr.io/org/vm:pin", Binary: "tart"}
	argv, err := v.CommandArgvRemote("~/.vci/state/work/run_abc", []string{"go", "test", "./..."})
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := []string{
		"tart", "run", "--no-gui",
		"--dir", "~/.vci/state/work/run_abc:/vci/work",
		"--workdir", "/vci/work",
		"--cpus", "2",
		"--memory", "4g",
		"ghcr.io/org/vm:pin",
		"--",
		"go", "test", "./...",
	}
	if !equalSlice(argv, wantPrefix) {
		t.Fatalf("argv: got %v want %v", argv, wantPrefix)
	}
}
