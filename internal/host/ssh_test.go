package host

// Stub-based tests for the remote runner. A fake `ssh`/`scp` in PATH
// records its argv; no real SSH server, docker, or tart is required.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/hypernewbie/vci/internal/process"
)

// writeStub writes an executable shell stub into dir and prepends dir
// to PATH for the test.
func writeStub(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+previous)
}

const recordStub = "#!/bin/sh\necho \"$*\" >> %s\ncat >/dev/null 2>&1\nexit %d\n"

// TestRunRemoteRecordsShell pins that RunRemote invokes
// `ssh <host> <shell>` where the shell is one `sh -c` string that
// cd's into the workspace, isolates HOME/TMPDIR inside it, exports
// the project environment, and execs the runtime argv.
func TestRunRemoteRecordsShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix ssh")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	writeStub(t, dir, "ssh", "#!/bin/sh\necho \"$*\" >> "+logPath+"\nexit 0\n")

	workDir := "~/.vci/state/work/run_abc"
	argv := []string{"sh", "-c", "true"}
	env := map[string]string{"CI": "1", "TOKEN": "semi;colon"}
	var stdout, stderr bytes.Buffer
	code, err := RunRemote(context.Background(), "builder", workDir, argv, env, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit: %d", code)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ssh stub log missing: %v", err)
	}
	s := string(log)
	// The host alias is the first argv element.
	if !strings.HasPrefix(s, "builder ") {
		t.Errorf("host alias missing: %s", s)
	}
	// The workspace appears (cd + HOME/TMPDIR + mkdir).
	if strings.Count(s, workDir) < 3 {
		t.Errorf("workspace under-referenced: %s", s)
	}
	// The runtime binary and the command appear.
	for _, want := range []string{"exec ", "'sh'", "'-c'", "'true'"} {
		if !strings.Contains(s, want) {
			t.Errorf("shell missing %q: %s", want, s)
		}
	}
	// Project environment is exported, quoted.
	for _, want := range []string{"export 'CI'='1'", "export 'TOKEN'='semi;colon'"} {
		if !strings.Contains(s, want) {
			t.Errorf("shell missing env %q: %s", want, s)
		}
	}
	// No Vci state or ssh leakage beyond the workspace path itself.
	for _, banned := range []string{"VCI_ROOT", ".ssh", "id_ed25519"} {
		if strings.Contains(s, banned) {
			t.Errorf("leaked %q: %s", banned, s)
		}
	}
}

// TestRunRemoteRecordsDockerAndVM pins that the docker and vm runtime
// argv reach the remote shell with the workspace as the only mount
// source, and that the mount source's leading `~` is rewritten to the
// captured login home (`"$__vci_login_home"`) instead of being left
// for the shell to expand against the isolated runtime HOME.
func TestRunRemoteRecordsDockerAndVM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix ssh")
	}
	for _, tc := range []struct {
		name string
		argv []string
		want []string
	}{
		{
			name: "docker",
			argv: []string{"docker", "run", "--rm", "-v", "~/.vci/state/work/run_abc:/vci/work:ro", "-w", "/vci/work", "ghcr.io/org/ci:pin", "true"},
			want: []string{"exec", "'docker'", "run", "--rm", `"$__vci_login_home"/.vci/state/work/run_abc:/vci/work:ro`, "'ghcr.io/org/ci:pin'"},
		},
		{
			name: "vm",
			argv: []string{"tart", "run", "--no-gui", "--dir", "~/.vci/state/work/run_abc:/vci/work", "--workdir", "/vci/work", "ghcr.io/org/vm:pin", "--", "true"},
			want: []string{"exec", "'tart'", "run", "--no-gui", `"$__vci_login_home"/.vci/state/work/run_abc:/vci/work`, "'ghcr.io/org/vm:pin'", "--", "'true'"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			logPath := filepath.Join(dir, "ssh.log")
			writeStub(t, dir, "ssh", "#!/bin/sh\necho \"$*\" >> "+logPath+"\nexit 0\n")
			var stdout, stderr bytes.Buffer
			code, err := RunRemote(context.Background(), "builder", "~/.vci/state/work/run_abc", tc.argv, nil, &stdout, &stderr)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if code != 0 {
				t.Fatalf("exit: %d", code)
			}
			log, _ := os.ReadFile(logPath)
			s := string(log)
			for _, want := range tc.want {
				if !strings.Contains(s, want) {
					t.Errorf("shell missing %q: %s", want, s)
				}
			}
			// The login home is captured before the isolation re-export,
			// and no mount source resolves through the isolated HOME.
			if !strings.Contains(s, "__vci_login_home=$HOME && export HOME=") {
				t.Errorf("login home not captured before isolation: %s", s)
			}
			if strings.Contains(s, ".home/.vci") {
				t.Errorf("mount resolved through isolated HOME: %s", s)
			}
			// Exactly one `-v`/`--dir` mount and it is the workspace.
			if strings.Count(s, "-v") != 1 && !strings.Contains(s, "--dir") {
				t.Errorf("mount shape unexpected: %s", s)
			}
			if strings.Contains(s, ".ssh") || strings.Contains(s, "VCI_ROOT") {
				t.Errorf("leak: %s", s)
			}
		})
	}
}

// TestRemoteRuntimeMountUsesLoginHome pins the mount-source fix end to
// end: the composed remote shell is executed against a real `sh` with
// stub docker/tart binaries in PATH, and the recorded mount source is
// the workspace under the original login HOME — not the isolated
// runtime HOME (`.home`) tree. It also pins that the runtime still
// receives the isolated HOME/TMPDIR.
func TestRemoteRuntimeMountUsesLoginHome(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix ssh")
	}
	loginHome := filepath.Join(t.TempDir(), "home")
	workDir := "~/.vci/state/work/run_abc"
	absWork := filepath.Join(loginHome, ".vci", "state", "work", "run_abc")
	for _, dir := range []string{loginHome, absWork} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Stub docker/tart record their environment and argv; the shell
	// `exec`s them, so HOME/TMPDIR here are the runtime's isolated
	// values and `$*` carries the resolved mount source.
	dir := t.TempDir()
	dockerLog := filepath.Join(dir, "docker.log")
	writeStub(t, dir, "docker", "#!/bin/sh\necho \"HOME=$HOME TMPDIR=$TMPDIR $*\" >> "+dockerLog+"\nexit 0\n")
	tartLog := filepath.Join(dir, "tart.log")
	writeStub(t, dir, "tart", "#!/bin/sh\necho \"HOME=$HOME TMPDIR=$TMPDIR $*\" >> "+tartLog+"\nexit 0\n")
	t.Setenv("HOME", loginHome)

	dockerShell, err := composeShell(workDir, []string{"docker", "run", "--rm", "-v", workDir + ":/vci/work:ro", "-w", "/vci/work", "ghcr.io/org/ci:pin", "true"}, nil)
	if err != nil {
		t.Fatalf("compose docker shell: %v", err)
	}
	if err := exec.Command("sh", "-c", dockerShell).Run(); err != nil {
		t.Fatalf("run docker shell: %v\nshell: %s", err, dockerShell)
	}
	tartShell, err := composeShell(workDir, []string{"tart", "run", "--no-gui", "--dir", workDir + ":/vci/work", "--workdir", "/vci/work", "ghcr.io/org/vm:pin", "--", "true"}, nil)
	if err != nil {
		t.Fatalf("compose tart shell: %v", err)
	}
	if err := exec.Command("sh", "-c", tartShell).Run(); err != nil {
		t.Fatalf("run tart shell: %v\nshell: %s", err, tartShell)
	}

	dockerData, err := os.ReadFile(dockerLog)
	if err != nil {
		t.Fatalf("docker stub log missing: %v", err)
	}
	d := string(dockerData)
	if want := absWork + ":/vci/work:ro"; !strings.Contains(d, want) {
		t.Errorf("docker -v mount %q missing: %s", want, d)
	}
	if strings.Contains(d, ".home/.vci") {
		t.Errorf("docker -v resolved through isolated HOME: %s", d)
	}
	for _, want := range []string{"HOME=" + filepath.Join(absWork, ".home"), "TMPDIR=" + filepath.Join(absWork, ".tmp")} {
		if !strings.Contains(d, want) {
			t.Errorf("docker runtime isolation missing %q: %s", want, d)
		}
	}

	tartData, err := os.ReadFile(tartLog)
	if err != nil {
		t.Fatalf("tart stub log missing: %v", err)
	}
	vm := string(tartData)
	if want := absWork + ":/vci/work"; !strings.Contains(vm, want) {
		t.Errorf("tart --dir mount %q missing: %s", want, vm)
	}
	if strings.Contains(vm, ".home/.vci") {
		t.Errorf("tart --dir resolved through isolated HOME: %s", vm)
	}
	for _, want := range []string{"HOME=" + filepath.Join(absWork, ".home"), "TMPDIR=" + filepath.Join(absWork, ".tmp")} {
		if !strings.Contains(vm, want) {
			t.Errorf("tart runtime isolation missing %q: %s", want, vm)
		}
	}
}

// TestRunRemoteReturnsExitCode pins that a non-zero remote exit is a
// job failure code, not an ssh-level error.
func TestRunRemoteReturnsExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix ssh")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	writeStub(t, dir, "ssh", "#!/bin/sh\necho \"$*\" >> "+logPath+"\nexit 7\n")
	var stdout, stderr bytes.Buffer
	code, err := RunRemote(context.Background(), "builder", "~/.vci/state/work/run_abc", []string{"true"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 7 {
		t.Fatalf("exit: %d want 7", code)
	}
}

// TestRunRemoteRejectsBadInput pins that unsafe hosts and paths are
// rejected before any subprocess starts.
func TestRunRemoteRejectsBadInput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix ssh")
	}
	dir := t.TempDir()
	writeStub(t, dir, "ssh", "#!/bin/sh\nexit 0\n")
	for _, tc := range []struct {
		host, workDir string
	}{
		{"", "~/.vci/state/work/run_abc"},
		{"-flag", "~/.vci/state/work/run_abc"},
		{"bad host", "~/.vci/state/work/run_abc"},
		{"builder", ""},
		{"builder", "/tmp/other"},
		{"builder", "~/.vci/state/work/.."},
	} {
		var stdout, stderr bytes.Buffer
		if _, err := RunRemote(context.Background(), tc.host, tc.workDir, []string{"true"}, nil, &stdout, &stderr); err == nil {
			t.Errorf("host %q workDir %q accepted", tc.host, tc.workDir)
		}
	}
}

// TestStageRemoteStreamsWorkspace pins that staging pipes a tar of
// the local workspace into `ssh <host> "mkdir -p <workDir> && cd
// <workDir> && tar -xpf -"`.
func TestStageRemoteStreamsWorkspace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix ssh")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	writeStub(t, dir, "ssh", "#!/bin/sh\necho \"$*\" >> "+logPath+"\ncat >/dev/null 2>&1\nexit 0\n")
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := StageRemote(context.Background(), "builder", "~/.vci/state/work/run_abc", workspace); err != nil {
		t.Fatalf("stage: %v", err)
	}
	log, _ := os.ReadFile(logPath)
	s := string(log)
	for _, want := range []string{"builder", "mkdir -p ~/.vci/state/work/run_abc", "cd ~/.vci/state/work/run_abc", "tar -xpf -"} {
		if !strings.Contains(s, want) {
			t.Errorf("stage command missing %q: %s", want, s)
		}
	}
}

// TestStageRemoteRejectsUnsafe pins that staging validates both the
// host and the remote path before touching the workspace.
func TestStageRemoteRejectsUnsafe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix ssh")
	}
	dir := t.TempDir()
	writeStub(t, dir, "ssh", "#!/bin/sh\nexit 0\n")
	workspace := t.TempDir()
	for _, tc := range []struct {
		host, remote string
	}{
		{"", "~/.vci/state/work/run_abc"},
		{"builder", ""},
		{"builder", "~/elsewhere"},
	} {
		if err := StageRemote(context.Background(), tc.host, tc.remote, workspace); err == nil {
			t.Errorf("stage host %q remote %q accepted", tc.host, tc.remote)
		}
	}
}

// TestFetchRemoteInvokesScp pins that artifact fetch runs
// `scp -r -q <host>:<workDir> <localDest>`.
func TestFetchRemoteInvokesScp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix ssh")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "scp.log")
	writeStub(t, dir, "scp", "#!/bin/sh\necho \"$*\" >> "+logPath+"\nexit 0\n")
	dest := t.TempDir()
	if err := FetchRemote(context.Background(), "builder", "~/.vci/state/work/run_abc", dest); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	log, _ := os.ReadFile(logPath)
	s := string(log)
	for _, want := range []string{"-r", "-q", "builder:~/.vci/state/work/run_abc", dest} {
		if !strings.Contains(s, want) {
			t.Errorf("scp args missing %q: %s", want, s)
		}
	}
}

// scriptRunner is a process.Runner that records every Command and
// replays a scripted result, exercising Client.RunRemote without an
// ssh binary or PATH stubs.
type scriptRunner struct {
	commands []process.Command
	result   process.Result
	err      error
}

func (r *scriptRunner) Run(_ context.Context, command process.Command) (process.Result, error) {
	r.commands = append(r.commands, command)
	return r.result, r.err
}

// TestClientRunRemoteInvokesRunner pins that Client.RunRemote validates
// and composes before handing `ssh <host> <shell>` and the supplied
// stdout/stderr writers to Runner.Run.
func TestClientRunRemoteInvokesRunner(t *testing.T) {
	runner := &scriptRunner{result: process.Result{ExitCode: 0}}
	workDir := "~/.vci/state/work/run_abc"
	argv := []string{"sh", "-c", "echo ok"}
	env := map[string]string{"CI": "1"}
	var stdout, stderr bytes.Buffer
	code, err := (Client{Runner: runner}).RunRemote(context.Background(), "builder", workDir, argv, env, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit: %d", code)
	}
	if len(runner.commands) != 1 {
		t.Fatalf("runner invoked %d times", len(runner.commands))
	}
	cmd := runner.commands[0]
	if cmd.Executable != "ssh" {
		t.Errorf("executable: %q", cmd.Executable)
	}
	if len(cmd.Args) != 2 || cmd.Args[0] != "builder" {
		t.Fatalf("args: %q", cmd.Args)
	}
	shell := cmd.Args[1]
	for _, want := range []string{"cd " + workDir, "export 'CI'='1'", "exec 'sh' '-c' 'echo ok'"} {
		if !strings.Contains(shell, want) {
			t.Errorf("shell missing %q: %s", want, shell)
		}
	}
	if cmd.Stdout != io.Writer(&stdout) || cmd.Stderr != io.Writer(&stderr) {
		t.Errorf("stdout/stderr writers not propagated")
	}
}

// TestClientRunRemoteNonzeroExit pins that a non-zero remote exit is
// surfaced as the exit code with no transport error.
func TestClientRunRemoteNonzeroExit(t *testing.T) {
	runner := &scriptRunner{result: process.Result{ExitCode: 9}, err: errors.New("exit status 9")}
	var stdout, stderr bytes.Buffer
	code, err := (Client{Runner: runner}).RunRemote(context.Background(), "builder", "~/.vci/state/work/run_abc", []string{"true"}, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 9 {
		t.Fatalf("exit: %d want 9", code)
	}
}

// TestClientRunRemoteTransportError pins that a runner failure without
// a non-zero exit is an ssh transport error, not an exit code.
func TestClientRunRemoteTransportError(t *testing.T) {
	runner := &scriptRunner{err: errors.New("connection refused")}
	var stdout, stderr bytes.Buffer
	code, err := (Client{Runner: runner}).RunRemote(context.Background(), "builder", "~/.vci/state/work/run_abc", []string{"true"}, nil, &stdout, &stderr)
	if err == nil {
		t.Fatal("transport error not reported")
	}
	if code != 0 {
		t.Fatalf("exit: %d want 0", code)
	}
	if !strings.Contains(err.Error(), "ssh builder") || !errors.Is(err, runner.err) {
		t.Errorf("error: %v", err)
	}
}

// TestValidateRemotePath pins the remote-tree grammar: only the fixed
// layout plus a run id.
func TestValidateRemotePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix ssh")
	}
	for _, good := range []string{
		"~/.vci/state/work/run_abc",
		"~/.vci/state/work/run_1_2",
	} {
		if err := ValidateRemotePath(good); err != nil {
			t.Errorf("valid path %q rejected: %v", good, err)
		}
	}
	for _, bad := range []string{
		"",
		"-flag",
		"/tmp/x",
		"~/.vci/state/work",
		"~/.vci/state/work/..",
		"~/.vci/state/work/run_x extra",
		"~/elsewhere",
	} {
		if err := ValidateRemotePath(bad); err == nil {
			t.Errorf("invalid path %q accepted", bad)
		}
	}
}
