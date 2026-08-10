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
	"time"

	"github.com/hypernewbie/vci/internal/config"
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
// the local workspace into `ssh <host> vci internal-stage <workDir>`,
// the worker-side replacement for the shell rm/mkdir/tar composition.
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
	for _, want := range []string{"builder", "vci", "internal-stage", "~/.vci/state/work/run_abc"} {
		if !strings.Contains(s, want) {
			t.Errorf("stage command missing %q: %s", want, s)
		}
	}
	// No shell fragment may cross the wire: the worker-side subcommand
	// owns clearing, creating, and extracting.
	for _, banned := range []string{"rm -rf", "mkdir -p", "tar -xpf"} {
		if strings.Contains(s, banned) {
			t.Errorf("stage command must not embed shell fragments (%q): %s", banned, s)
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

// stdoutRunner is a scriptRunner that also writes scripted output to
// the command's stdout writer before replaying the recorded result.
type stdoutRunner struct {
	scriptRunner
	stdout string
}

func (r *stdoutRunner) Run(ctx context.Context, command process.Command) (process.Result, error) {
	if command.Stdout != nil {
		io.WriteString(command.Stdout, r.stdout)
	}
	return r.scriptRunner.Run(ctx, command)
}

// TestProbeSeedHeadReturnsHead pins that ProbeSeedHead runs
// `ssh <host> git -C <seed> rev-parse HEAD` through the runner, with a
// `~/` seed rendered as `$HOME` plus its suffix, and returns the trimmed
// stdout as the seed head. A `~/` seed whose suffix holds a space renders
// as `$HOME` plus a single-quoted suffix.
func TestProbeSeedHeadReturnsHead(t *testing.T) {
	runner := &stdoutRunner{stdout: "deadbeef\n  "}
	head, err := (Client{Runner: runner}).ProbeSeedHead(context.Background(), "charon", "~/code/vidl")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if head != "deadbeef" {
		t.Fatalf("head: %q", head)
	}
	if len(runner.commands) != 1 {
		t.Fatalf("runner invoked %d times", len(runner.commands))
	}
	cmd := runner.commands[0]
	if cmd.Executable != "ssh" {
		t.Errorf("executable: %q", cmd.Executable)
	}
	want := []string{"charon", "git", "-C", "$HOME/code/vidl", "rev-parse", "HEAD"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("args: %q", cmd.Args)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Errorf("arg %d: %q want %q", i, cmd.Args[i], want[i])
		}
	}

	// A `~/` seed with a space in its suffix renders as $HOME plus a
	// single-quoted suffix, never with the tilde inside the quotes.
	spaceRunner := &stdoutRunner{stdout: "c0ffee\n"}
	if _, err := (Client{Runner: spaceRunner}).ProbeSeedHead(context.Background(), "charon", "~/my code/vidl"); err != nil {
		t.Fatalf("probe space seed: %v", err)
	}
	if len(spaceRunner.commands) != 1 {
		t.Fatalf("runner invoked %d times for space seed", len(spaceRunner.commands))
	}
	if got := spaceRunner.commands[0].Args[3]; got != "$HOME'/my code/vidl'" {
		t.Errorf("space seed -C arg: %q want %q", got, "$HOME'/my code/vidl'")
	}
}

// TestProbeSeedHeadNonzeroExit pins that a nonzero remote exit (the
// seed is not a Git checkout) is an empty head, not an error.
func TestProbeSeedHeadNonzeroExit(t *testing.T) {
	runner := &scriptRunner{result: process.Result{ExitCode: 128}, err: errors.New("exit status 128")}
	head, err := (Client{Runner: runner}).ProbeSeedHead(context.Background(), "charon", "~/code/vidl")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if head != "" {
		t.Fatalf("head: %q", head)
	}
}

// TestProbeSeedHeadNonzeroExitWithoutError pins that a runner
// reporting a nonzero exit without an error still yields an empty
// head rather than a transport error.
func TestProbeSeedHeadNonzeroExitWithoutError(t *testing.T) {
	runner := &scriptRunner{result: process.Result{ExitCode: 3}}
	head, err := (Client{Runner: runner}).ProbeSeedHead(context.Background(), "charon", "~/code/vidl")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if head != "" {
		t.Fatalf("head: %q", head)
	}
}

// TestProbeSeedHeadTransportError pins that an ssh-level failure with
// no remote exit is a wrapped transport error.
func TestProbeSeedHeadTransportError(t *testing.T) {
	runner := &scriptRunner{err: errors.New("connection refused")}
	head, err := (Client{Runner: runner}).ProbeSeedHead(context.Background(), "charon", "~/code/vidl")
	if err == nil {
		t.Fatal("transport error not reported")
	}
	if head != "" {
		t.Fatalf("head: %q", head)
	}
	if !strings.Contains(err.Error(), "ssh charon") || !errors.Is(err, runner.err) {
		t.Errorf("error: %v", err)
	}
}

// TestProbeSeedHeadRejectsBadInput pins that invalid hosts and seeds
// and a nil runner fail before any subprocess starts.
func TestProbeSeedHeadRejectsBadInput(t *testing.T) {
	runner := &scriptRunner{}
	cases := []struct {
		name   string
		client Client
		host   string
		seed   string
	}{
		{"empty host", Client{Runner: runner}, "", "~/code/vidl"},
		{"empty seed", Client{Runner: runner}, "charon", ""},
		{"dash seed", Client{Runner: runner}, "charon", "-seed"},
		{"control seed", Client{Runner: runner}, "charon", "a\nb"},
		{"nil runner", Client{}, "charon", "~/code/vidl"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.client.ProbeSeedHead(context.Background(), tc.host, tc.seed); err == nil {
				t.Errorf("accepted host %q seed %q", tc.host, tc.seed)
			}
		})
	}
	if len(runner.commands) != 0 {
		t.Errorf("runner invoked %d times for rejected input", len(runner.commands))
	}
}

// TestClientStreamReconstructPinsCommand pins that StreamReconstruct hands
// `ssh <host> vci internal-reconstruct <workDir> <seed>` to the runner with
// the payload wired to stdin. The worker-side subcommand owns payload
// extraction, the seed copy, bundle import, checkout, local-change
// application, and payload removal.
func TestClientStreamReconstructPinsCommand(t *testing.T) {
	runner := &scriptRunner{result: process.Result{ExitCode: 0}}
	payload := bytes.NewReader([]byte("payload-tar"))
	err := (Client{Runner: runner}).StreamReconstruct(context.Background(), "charon", "~/code/vidl", "~/.vci/state/work/run_abc", payload)
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	if len(runner.commands) != 1 {
		t.Fatalf("runner invoked %d times", len(runner.commands))
	}
	cmd := runner.commands[0]
	if cmd.Executable != "ssh" {
		t.Errorf("executable: %q", cmd.Executable)
	}
	want := []string{"charon", "vci", "internal-reconstruct", "~/.vci/state/work/run_abc", "$HOME/code/vidl"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("args: %q", cmd.Args)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Errorf("arg %d: %q want %q", i, cmd.Args[i], want[i])
		}
	}
	if cmd.Stdin != io.Reader(payload) {
		t.Errorf("payload not wired to ssh stdin")
	}
}

// TestClientStreamReconstructQuotesSeed pins that a seed path needing shell
// quoting renders as `$HOME` plus a single-quoted suffix, so the tilde is
// never quoted and a suffix holding spaces stays one intact argument after
// the remote login shell unquotes it.
func TestClientStreamReconstructQuotesSeed(t *testing.T) {
	runner := &scriptRunner{result: process.Result{ExitCode: 0}}
	err := (Client{Runner: runner}).StreamReconstruct(context.Background(), "charon", "~/my code/vidl", "~/.vci/state/work/run_abc", bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	cmd := runner.commands[0]
	if len(cmd.Args) != 5 || cmd.Args[4] != "$HOME'/my code/vidl'" {
		t.Errorf("seed not rendered as $HOME plus quoted suffix: %q", cmd.Args)
	}
}

// TestClientStreamReconstructNonzeroExit pins that a non-zero remote exit is a
// reconstruction failure, not a transport error.
func TestClientStreamReconstructNonzeroExit(t *testing.T) {
	runner := &scriptRunner{result: process.Result{ExitCode: 9}, err: errors.New("exit status 9")}
	err := (Client{Runner: runner}).StreamReconstruct(context.Background(), "charon", "~/code/vidl", "~/.vci/state/work/run_abc", bytes.NewReader(nil))
	if err == nil {
		t.Fatal("nonzero remote exit not reported")
	}
	if !strings.Contains(err.Error(), "reconstruct charon: remote exit 9") {
		t.Errorf("error: %v", err)
	}
}

// TestClientStreamReconstructTransportError pins that a runner failure without
// a remote exit is a wrapped ssh transport error.
func TestClientStreamReconstructTransportError(t *testing.T) {
	runner := &scriptRunner{err: errors.New("connection refused")}
	err := (Client{Runner: runner}).StreamReconstruct(context.Background(), "charon", "~/code/vidl", "~/.vci/state/work/run_abc", bytes.NewReader(nil))
	if err == nil {
		t.Fatal("transport error not reported")
	}
	if !strings.Contains(err.Error(), "ssh charon") || !errors.Is(err, runner.err) {
		t.Errorf("error: %v", err)
	}
}

// TestClientStreamReconstructRejectsBadInput pins that invalid hosts, seeds,
// work dirs, a nil payload, and a nil runner fail before any subprocess runs.
func TestClientStreamReconstructRejectsBadInput(t *testing.T) {
	runner := &scriptRunner{}
	tc := []struct {
		name    string
		client  Client
		host    string
		seed    string
		workDir string
		payload io.Reader
	}{
		{"empty host", Client{Runner: runner}, "", "~/code/vidl", "~/.vci/state/work/run_abc", bytes.NewReader(nil)},
		{"empty seed", Client{Runner: runner}, "charon", "", "~/.vci/state/work/run_abc", bytes.NewReader(nil)},
		{"dash seed", Client{Runner: runner}, "charon", "-seed", "~/.vci/state/work/run_abc", bytes.NewReader(nil)},
		{"control seed", Client{Runner: runner}, "charon", "a\nb", "~/.vci/state/work/run_abc", bytes.NewReader(nil)},
		{"bad work dir", Client{Runner: runner}, "charon", "~/code/vidl", "/tmp/other", bytes.NewReader(nil)},
		{"nil payload", Client{Runner: runner}, "charon", "~/code/vidl", "~/.vci/state/work/run_abc", nil},
		{"nil runner", Client{}, "charon", "~/code/vidl", "~/.vci/state/work/run_abc", bytes.NewReader(nil)},
	}
	for _, tc := range tc {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.client.StreamReconstruct(context.Background(), tc.host, tc.seed, tc.workDir, tc.payload); err == nil {
				t.Errorf("accepted host %q seed %q workDir %q", tc.host, tc.seed, tc.workDir)
			}
		})
	}
	if len(runner.commands) != 0 {
		t.Errorf("runner invoked %d times for rejected input", len(runner.commands))
	}
}

// TestProbeBundleCacheHitAndMiss pins that ProbeBundleCache runs
// `ssh <host> vci internal-probe-cache <entry>` and maps a zero remote exit
// to a hit and any nonzero remote exit to a miss, with or without a runner
// error.
func TestProbeBundleCacheHitAndMiss(t *testing.T) {
	runner := &scriptRunner{result: process.Result{ExitCode: 0}}
	hit, err := (Client{Runner: runner}).ProbeBundleCache(context.Background(), "charon", "~/.vci/state/bundle-cache", "Vci", "abc123")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !hit {
		t.Fatal("zero remote exit must be a hit")
	}
	if len(runner.commands) != 1 {
		t.Fatalf("runner invoked %d times", len(runner.commands))
	}
	cmd := runner.commands[0]
	if cmd.Executable != "ssh" {
		t.Errorf("executable: %q", cmd.Executable)
	}
	want := []string{"charon", "vci", "internal-probe-cache", "~/.vci/state/bundle-cache/v1/Vci/abc123"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("args: %q", cmd.Args)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Errorf("arg %d: %q want %q", i, cmd.Args[i], want[i])
		}
	}

	for name, tc := range map[string]struct {
		result process.Result
		err    error
	}{
		"nonzero exit with error":    {process.Result{ExitCode: 1}, errors.New("exit status 1")},
		"nonzero exit without error": {process.Result{ExitCode: 3}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			r := &scriptRunner{result: tc.result, err: tc.err}
			hit, err := (Client{Runner: r}).ProbeBundleCache(context.Background(), "charon", "~/.vci/state/bundle-cache", "Vci", "abc123")
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if hit {
				t.Fatal("nonzero remote exit must be a miss")
			}
		})
	}
}

// TestProbeBundleCacheTransportError pins that a runner failure without a
// remote exit is a wrapped transport error, never a miss.
func TestProbeBundleCacheTransportError(t *testing.T) {
	runner := &scriptRunner{err: errors.New("connection refused")}
	hit, err := (Client{Runner: runner}).ProbeBundleCache(context.Background(), "charon", "~/.vci/state/bundle-cache", "Vci", "abc123")
	if err == nil {
		t.Fatal("transport error not reported")
	}
	if hit {
		t.Fatal("hit must be false on transport error")
	}
	if !strings.Contains(err.Error(), "ssh charon") || !errors.Is(err, runner.err) {
		t.Errorf("error: %v", err)
	}
}

// TestProbeBundleCacheRejectsBadInput pins that invalid hosts, roots,
// project/base segments, and a nil runner fail before any subprocess starts.
func TestProbeBundleCacheRejectsBadInput(t *testing.T) {
	runner := &scriptRunner{}
	cases := []struct {
		name      string
		client    Client
		host      string
		cacheRoot string
		project   string
		base      string
	}{
		{"empty host", Client{Runner: runner}, "", "~/.vci/state/bundle-cache", "Vci", "abc123"},
		{"empty root", Client{Runner: runner}, "charon", "", "Vci", "abc123"},
		{"slash project", Client{Runner: runner}, "charon", "~/.vci/state/bundle-cache", "a/b", "abc123"},
		{"space base", Client{Runner: runner}, "charon", "~/.vci/state/bundle-cache", "Vci", "a b"},
		{"dotdot base", Client{Runner: runner}, "charon", "~/.vci/state/bundle-cache", "Vci", ".."},
		{"nil runner", Client{}, "charon", "~/.vci/state/bundle-cache", "Vci", "abc123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.client.ProbeBundleCache(context.Background(), tc.host, tc.cacheRoot, tc.project, tc.base); err == nil {
				t.Errorf("accepted host %q root %q project %q base %q", tc.host, tc.cacheRoot, tc.project, tc.base)
			}
		})
	}
	if len(runner.commands) != 0 {
		t.Errorf("runner invoked %d times for rejected input", len(runner.commands))
	}
}

// TestAcquireAndReleaseBundleClaimPinCommands pins the claim commands:
// acquire invokes `vci internal-acquire-claim <entry> <claimID>` and release
// invokes `vci internal-release-claim <entry> <claimID>`. The worker-side
// subcommands own the complete-marker guard, claims dir creation, marker
// write, and rm -f semantics.
func TestAcquireAndReleaseBundleClaimPinCommands(t *testing.T) {
	runner := &scriptRunner{result: process.Result{ExitCode: 0}}
	if err := (Client{Runner: runner}).AcquireBundleClaim(context.Background(), "charon", "~/.vci/state/bundle-cache", "Vci", "abc123", "run_1"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := (Client{Runner: runner}).ReleaseBundleClaim(context.Background(), "charon", "~/.vci/state/bundle-cache", "Vci", "abc123", "run_1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if len(runner.commands) != 2 {
		t.Fatalf("runner invoked %d times, want 2", len(runner.commands))
	}
	entry := "~/.vci/state/bundle-cache/v1/Vci/abc123"
	acquire := runner.commands[0]
	if acquire.Executable != "ssh" {
		t.Fatalf("acquire command: %+v", acquire)
	}
	wantAcquire := []string{"charon", "vci", "internal-acquire-claim", entry, "run_1"}
	if len(acquire.Args) != len(wantAcquire) {
		t.Fatalf("acquire args: %q", acquire.Args)
	}
	for i := range wantAcquire {
		if acquire.Args[i] != wantAcquire[i] {
			t.Errorf("acquire arg %d: %q want %q", i, acquire.Args[i], wantAcquire[i])
		}
	}
	release := runner.commands[1]
	wantRelease := []string{"charon", "vci", "internal-release-claim", entry, "run_1"}
	if len(release.Args) != len(wantRelease) {
		t.Fatalf("release args: %q", release.Args)
	}
	for i := range wantRelease {
		if release.Args[i] != wantRelease[i] {
			t.Errorf("release arg %d: %q want %q", i, release.Args[i], wantRelease[i])
		}
	}
}

// TestAcquireAndReleaseBundleClaimNonzeroExit pins that a nonzero remote
// exit on acquire or release is reported as a remote-exit error.
func TestAcquireAndReleaseBundleClaimNonzeroExit(t *testing.T) {
	runner := &scriptRunner{result: process.Result{ExitCode: 7}, err: errors.New("exit status 7")}
	err := (Client{Runner: runner}).AcquireBundleClaim(context.Background(), "charon", "~/.vci/state/bundle-cache", "Vci", "abc123", "run_1")
	if err == nil {
		t.Fatal("acquire: nonzero remote exit not reported")
	}
	if !strings.Contains(err.Error(), "acquire claim charon: remote exit 7") {
		t.Errorf("error: %v", err)
	}

	runner = &scriptRunner{result: process.Result{ExitCode: 7}, err: errors.New("exit status 7")}
	err = (Client{Runner: runner}).ReleaseBundleClaim(context.Background(), "charon", "~/.vci/state/bundle-cache", "Vci", "abc123", "run_1")
	if err == nil {
		t.Fatal("release: nonzero remote exit not reported")
	}
	if !strings.Contains(err.Error(), "release claim charon: remote exit 7") {
		t.Errorf("error: %v", err)
	}
}

// TestAcquireAndReleaseBundleClaimRejectBadInput pins that invalid hosts and
// segments fail before any subprocess starts.
func TestAcquireAndReleaseBundleClaimRejectBadInput(t *testing.T) {
	runner := &scriptRunner{}
	if err := (Client{Runner: runner}).AcquireBundleClaim(context.Background(), "charon", "~/.vci/state/bundle-cache", "Vci", "abc123", "a/b"); err == nil {
		t.Error("acquire accepted invalid claimID")
	}
	if err := (Client{Runner: runner}).ReleaseBundleClaim(context.Background(), "charon", "~/.vci/state/bundle-cache", "Vci", "abc123", ""); err == nil {
		t.Error("release accepted empty claimID")
	}
	if err := (Client{Runner: runner}).AcquireBundleClaim(context.Background(), "", "~/.vci/state/bundle-cache", "Vci", "abc123", "run_1"); err == nil {
		t.Error("acquire accepted empty host")
	}
	if err := (Client{Runner: runner}).ReleaseBundleClaim(context.Background(), "charon", "~/.vci/state/bundle-cache", "Vci", "..", "run_1"); err == nil {
		t.Error("release accepted dotdot base")
	}
	if err := (Client{Runner: runner}).AcquireBundleClaim(context.Background(), "charon", "", "Vci", "abc123", "run_1"); err == nil {
		t.Error("acquire accepted empty cache root")
	}
	if len(runner.commands) != 0 {
		t.Errorf("runner invoked %d times for rejected input", len(runner.commands))
	}
}

// TestClientStreamNoSeedReconstructPinsCommand pins the one-shot no-seed
// reconstruction invocation: `vci internal-reconstruct <workDir> --no-seed`
// with no cache flags at all. The worker-side subcommand owns repository
// initialization, bundle import, checkout, local-change application, and
// payload removal.
func TestClientStreamNoSeedReconstructPinsCommand(t *testing.T) {
	runner := &scriptRunner{result: process.Result{ExitCode: 0}}
	payload := bytes.NewReader([]byte("payload-tar"))
	err := (Client{Runner: runner}).StreamNoSeedReconstruct(context.Background(), "charon", "~/.vci/state/work/run_abc", NoSeedCacheSpec{}, payload)
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	if len(runner.commands) != 1 {
		t.Fatalf("runner invoked %d times", len(runner.commands))
	}
	cmd := runner.commands[0]
	if cmd.Executable != "ssh" {
		t.Fatalf("command: %+v", cmd)
	}
	want := []string{"charon", "vci", "internal-reconstruct", "~/.vci/state/work/run_abc", "--no-seed"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("args: %q", cmd.Args)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Errorf("arg %d: %q want %q", i, cmd.Args[i], want[i])
		}
	}
	if cmd.Stdin != io.Reader(payload) {
		t.Errorf("payload not wired to ssh stdin")
	}
	for _, banned := range []string{"state/bundle-cache", "complete", "meta.json"} {
		if strings.Contains(strings.Join(cmd.Args, " "), banned) {
			t.Errorf("one-shot command must not touch the cache (%q): %q", banned, cmd.Args)
		}
	}
}

// TestClientStreamNoSeedReconstructHitCommand pins the cache-hit invocation:
// `vci internal-reconstruct <workDir> --no-seed --cache <entry> --use-cached`.
// The worker-side subcommand seeds the repository from the cached entry
// bundle and treats the payload bundle as an optional delta; no admission or
// eviction flags are passed.
func TestClientStreamNoSeedReconstructHitCommand(t *testing.T) {
	runner := &scriptRunner{result: process.Result{ExitCode: 0}}
	cache := NoSeedCacheSpec{Root: "~/.vci/state/bundle-cache", Project: "Vci", Base: "abc123", UseCached: true}
	err := (Client{Runner: runner}).StreamNoSeedReconstruct(context.Background(), "charon", "~/.vci/state/work/run_abc", cache, bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	cmd := runner.commands[0]
	want := []string{"charon", "vci", "internal-reconstruct", "~/.vci/state/work/run_abc", "--no-seed", "--cache", "~/.vci/state/bundle-cache/v1/Vci/abc123", "--use-cached"}
	if len(cmd.Args) != len(want) {
		t.Fatalf("args: %q", cmd.Args)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Errorf("arg %d: %q want %q", i, cmd.Args[i], want[i])
		}
	}
	// The hit path never asks the worker to admit or evict.
	joined := strings.Join(cmd.Args, " ")
	for _, banned := range []string{"--admit", "--evict"} {
		if strings.Contains(joined, banned) {
			t.Errorf("hit command must not admit or evict (%q): %q", banned, cmd.Args)
		}
	}
}

// TestClientStreamNoSeedReconstructAdmitCommand pins the cache-miss
// invocation: the streamed bundle is admitted (`--admit <bytes> <rfc3339>`)
// and the LRU eviction loop runs after admission with the policy limits
// embedded (`--evict <maxEntries> <maxBytes>`).
func TestClientStreamNoSeedReconstructAdmitCommand(t *testing.T) {
	runner := &scriptRunner{result: process.Result{ExitCode: 0}}
	now := time.Date(2026, 8, 9, 12, 34, 56, 0, time.UTC)
	cache := NoSeedCacheSpec{
		Root: "~/.vci/state/bundle-cache", Project: "Vci", Base: "abc123",
		Admit: true, Evict: true, MaxEntries: 5, MaxBytes: 5368709120, BundleBytes: 12345, Now: now,
	}
	err := (Client{Runner: runner}).StreamNoSeedReconstruct(context.Background(), "charon", "~/.vci/state/work/run_abc", cache, bytes.NewReader(nil))
	if err != nil {
		t.Fatalf("reconstruct: %v", err)
	}
	cmd := runner.commands[0]
	want := []string{
		"charon", "vci", "internal-reconstruct", "~/.vci/state/work/run_abc", "--no-seed",
		"--cache", "~/.vci/state/bundle-cache/v1/Vci/abc123",
		"--admit", "12345", "2026-08-09T12:34:56Z",
		"--evict", "5", "5368709120",
	}
	if len(cmd.Args) != len(want) {
		t.Fatalf("args: %q", cmd.Args)
	}
	for i := range want {
		if cmd.Args[i] != want[i] {
			t.Errorf("arg %d: %q want %q", i, cmd.Args[i], want[i])
		}
	}
}

// TestClientStreamNoSeedReconstructNonzeroExit pins that a non-zero remote
// exit is a reconstruction failure, not a transport error.
func TestClientStreamNoSeedReconstructNonzeroExit(t *testing.T) {
	runner := &scriptRunner{result: process.Result{ExitCode: 9}, err: errors.New("exit status 9")}
	err := (Client{Runner: runner}).StreamNoSeedReconstruct(context.Background(), "charon", "~/.vci/state/work/run_abc", NoSeedCacheSpec{}, bytes.NewReader(nil))
	if err == nil {
		t.Fatal("nonzero remote exit not reported")
	}
	if !strings.Contains(err.Error(), "reconstruct charon: remote exit 9") {
		t.Errorf("error: %v", err)
	}
}

// TestClientStreamNoSeedReconstructRejectsBadInput pins that invalid hosts,
// work dirs, cache specs, a nil payload, and a nil runner fail before any
// subprocess starts.
func TestClientStreamNoSeedReconstructRejectsBadInput(t *testing.T) {
	runner := &scriptRunner{}
	cases := []struct {
		name    string
		client  Client
		host    string
		workDir string
		cache   NoSeedCacheSpec
		payload io.Reader
	}{
		{"empty host", Client{Runner: runner}, "", "~/.vci/state/work/run_abc", NoSeedCacheSpec{}, bytes.NewReader(nil)},
		{"nil payload", Client{Runner: runner}, "charon", "~/.vci/state/work/run_abc", NoSeedCacheSpec{}, nil},
		{"nil runner", Client{}, "charon", "~/.vci/state/work/run_abc", NoSeedCacheSpec{}, bytes.NewReader(nil)},
		{"bad workdir", Client{Runner: runner}, "charon", "/etc/passwd", NoSeedCacheSpec{}, bytes.NewReader(nil)},
		{"bad project", Client{Runner: runner}, "charon", "~/.vci/state/work/run_abc", NoSeedCacheSpec{Root: "~/.vci/state/bundle-cache", Project: "a/b", Base: "abc123", UseCached: true}, bytes.NewReader(nil)},
		{"use-cached without entry", Client{Runner: runner}, "charon", "~/.vci/state/work/run_abc", NoSeedCacheSpec{UseCached: true}, bytes.NewReader(nil)},
		{"admit without entry", Client{Runner: runner}, "charon", "~/.vci/state/work/run_abc", NoSeedCacheSpec{Admit: true}, bytes.NewReader(nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.client.StreamNoSeedReconstruct(context.Background(), tc.host, tc.workDir, tc.cache, tc.payload); err == nil {
				t.Errorf("accepted host %q workdir %q cache %+v", tc.host, tc.workDir, tc.cache)
			}
		})
	}
	if len(runner.commands) != 0 {
		t.Errorf("runner invoked %d times for rejected input", len(runner.commands))
	}
}

// TestClientReapBundleCache pins the remote bundle-cache reaping pass through
// a scripted runner: validation happens before any subprocess, the invocation
// is `vci internal-reap-cache` with the project dir, the derived reference
// paths, the RFC3339 cutoff timestamps, and the policy limits, and the client
// parses the printed `stale=N evicted=M` line. Nonzero remote exits stay
// remote-exit failures and runner failures without a remote exit stay wrapped
// transport errors.
func TestClientReapBundleCache(t *testing.T) {
	projDir := "~/.vci/state/bundle-cache/v1/Vci"
	refDir := projDir + "/.vci-reap"
	now := time.Date(2026, 8, 9, 12, 34, 56, 0, time.UTC)
	cutoff := "2026-08-09T12:04:56Z"

	t.Run("pins command and parses counts", func(t *testing.T) {
		runner := &stdoutRunner{stdout: "stale=3 evicted=2\n"}
		got, err := (Client{Runner: runner}).ReapBundleCache(context.Background(), "charon", "~/.vci/state/bundle-cache", "Vci", config.BundleCachePolicy{MaxEntries: 5, MaxBytes: 5 << 30}, now)
		if err != nil {
			t.Fatalf("reap: %v", err)
		}
		if got != (ReapBundleCacheResult{Stale: 3, Evicted: 2}) {
			t.Fatalf("counts: %+v", got)
		}
		if len(runner.commands) != 1 {
			t.Fatalf("runner invoked %d times", len(runner.commands))
		}
		cmd := runner.commands[0]
		if cmd.Executable != "ssh" {
			t.Fatalf("command: %+v", cmd)
		}
		want := []string{
			"charon", "vci", "internal-reap-cache",
			projDir, refDir, refDir + "/partial", refDir + "/claim",
			cutoff, cutoff,
			"5", "5368709120",
		}
		if len(cmd.Args) != len(want) {
			t.Fatalf("args: %q", cmd.Args)
		}
		for i := range want {
			if cmd.Args[i] != want[i] {
				t.Errorf("arg %d: %q want %q", i, cmd.Args[i], want[i])
			}
		}
	})

	t.Run("parses zero counts", func(t *testing.T) {
		runner := &stdoutRunner{stdout: "stale=0 evicted=0\n"}
		got, err := (Client{Runner: runner}).ReapBundleCache(context.Background(), "charon", "~/.vci/state/bundle-cache", "Vci", config.BundleCachePolicy{MaxEntries: 5}, now)
		if err != nil {
			t.Fatalf("reap: %v", err)
		}
		if got != (ReapBundleCacheResult{}) {
			t.Fatalf("counts: %+v", got)
		}
	})

	t.Run("no limits keeps no-op eviction", func(t *testing.T) {
		runner := &stdoutRunner{stdout: "stale=1 evicted=0\n"}
		got, err := (Client{Runner: runner}).ReapBundleCache(context.Background(), "charon", "~/.vci/state/bundle-cache", "Vci", config.BundleCachePolicy{}, now)
		if err != nil {
			t.Fatalf("reap: %v", err)
		}
		if got.Stale != 1 || got.Evicted != 0 {
			t.Fatalf("counts: %+v", got)
		}
		// With no positive limits the worker skips the eviction loop and
		// reports zero evictions; the limits travel as zero decimal args.
		cmd := runner.commands[0]
		if got := cmd.Args[len(cmd.Args)-2:]; got[0] != "0" || got[1] != "0" {
			t.Errorf("no-limit command must carry zero limits: %q", cmd.Args)
		}
	})

	t.Run("malformed output", func(t *testing.T) {
		runner := &stdoutRunner{stdout: "boom\n"}
		if _, err := (Client{Runner: runner}).ReapBundleCache(context.Background(), "charon", "~/.vci/state/bundle-cache", "Vci", config.BundleCachePolicy{MaxEntries: 5}, now); err == nil {
			t.Fatal("malformed output accepted")
		}
	})

	t.Run("nonzero remote exit", func(t *testing.T) {
		runner := &scriptRunner{result: process.Result{ExitCode: 7}, err: errors.New("exit status 7")}
		_, err := (Client{Runner: runner}).ReapBundleCache(context.Background(), "charon", "~/.vci/state/bundle-cache", "Vci", config.BundleCachePolicy{MaxEntries: 5}, now)
		if err == nil {
			t.Fatal("nonzero remote exit not reported")
		}
		if !strings.Contains(err.Error(), "reap cache charon: remote exit 7") {
			t.Errorf("error: %v", err)
		}
	})

	t.Run("transport error", func(t *testing.T) {
		runner := &scriptRunner{err: errors.New("connection refused")}
		_, err := (Client{Runner: runner}).ReapBundleCache(context.Background(), "charon", "~/.vci/state/bundle-cache", "Vci", config.BundleCachePolicy{MaxEntries: 5}, now)
		if err == nil {
			t.Fatal("transport error not reported")
		}
		if !strings.Contains(err.Error(), "ssh charon") || !errors.Is(err, runner.err) {
			t.Errorf("error: %v", err)
		}
	})

	t.Run("rejects bad input", func(t *testing.T) {
		runner := &scriptRunner{}
		cases := []struct {
			name      string
			client    Client
			host      string
			cacheRoot string
			project   string
		}{
			{"empty host", Client{Runner: runner}, "", "~/.vci/state/bundle-cache", "Vci"},
			{"empty root", Client{Runner: runner}, "charon", "", "Vci"},
			{"slash project", Client{Runner: runner}, "charon", "~/.vci/state/bundle-cache", "a/b"},
			{"dotdot project", Client{Runner: runner}, "charon", "~/.vci/state/bundle-cache", ".."},
			{"nil runner", Client{}, "charon", "~/.vci/state/bundle-cache", "Vci"},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := tc.client.ReapBundleCache(context.Background(), tc.host, tc.cacheRoot, tc.project, config.BundleCachePolicy{MaxEntries: 5}, now); err == nil {
					t.Errorf("accepted host %q root %q project %q", tc.host, tc.cacheRoot, tc.project)
				}
			})
		}
		if len(runner.commands) != 0 {
			t.Errorf("runner invoked %d times for rejected input", len(runner.commands))
		}
	})
}

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
