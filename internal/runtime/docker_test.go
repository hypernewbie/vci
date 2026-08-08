package runtime

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypernewbie/vci/internal/executor"
	"github.com/hypernewbie/vci/internal/process"
)

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
	res, err := d.ExecuteSupervised(context.Background(), executor.Request{
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
	res, err := d.ExecuteSupervised(context.Background(), executor.Request{
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
	_, err := d.ExecuteSupervised(context.Background(), executor.Request{
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
	_, err := d.ExecuteSupervised(context.Background(), executor.Request{
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
