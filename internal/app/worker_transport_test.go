package app

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/process"
)

// scriptedRunner is a process.Runner that records every Command and replays
// a scripted result. Tests use it to observe whether the seeded-
// reconstruction client path (ProbeSeedHead / StreamReconstruct) was touched.
type scriptedRunner struct {
	commands []process.Command
	result   process.Result
	err      error
}

func (r *scriptedRunner) Run(_ context.Context, command process.Command) (process.Result, error) {
	r.commands = append(r.commands, command)
	return r.result, r.err
}

// writeStub writes an executable shell stub into dir and prepends dir to
// PATH for the duration of the test. Stub argv is appended to a log file so
// tests can assert the exact commands a real host operation would run.
func writeStub(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+previous)
}

// TestStageOrReconstructIneligibleFallsBack pins that a machine ineligible
// for seeded reconstruction skips the probe/stream client entirely and
// stages the full workspace exactly as executeRemote does: `tar -cf - -C
// <workspace> .` piped into `ssh <host> "mkdir -p <workDir> && cd <workDir>
// && tar -xpf -"`.
func TestStageOrReconstructIneligibleFallsBack(t *testing.T) {
	ctx := context.Background()

	// Record every ssh/tar invocation the real StageRemote performs.
	dir := t.TempDir()
	sshLog := filepath.Join(dir, "ssh.log")
	writeStub(t, dir, "ssh", "#!/bin/sh\necho \"$*\" >> "+sshLog+"\ncat >/dev/null 2>&1\nexit 0\n")
	writeStub(t, dir, "tar", "#!/bin/sh\necho \"$*\" >> "+filepath.Join(dir, "tar.log")+"\nexit 0\n")

	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lcPath := filepath.Join(t.TempDir(), "submission-lc.tar")
	if err := os.WriteFile(lcPath, []byte("lc\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Ineligible: the machine has no source path seeded for the project and
	// the project declares a hard workspace exclusion, so both
	// seededReconstructionEligible and noSeedCacheEligible are false even
	// though the LC archive exists.
	runner := &scriptedRunner{result: process.Result{ExitCode: 0}}
	machine := config.Machine{Host: "builder", SourcePaths: map[string]string{"Other": "/src/other"}}
	project := config.Project{ExcludedPaths: []string{"vendor/"}}
	projectName := "Vci"

	_, err := stageOrReconstruct(ctx, runner, machine, project, projectName, lcPath, workspace, workspace, "cafebabe", "~/.vci/state/work/run_abc")
	if err != nil {
		t.Fatalf("stageOrReconstruct: %v", err)
	}

	// The seeded-reconstruction client was never invoked on the fallback path.
	if len(runner.commands) != 0 {
		t.Fatalf("scripted runner invoked %d times, want 0 on the ineligible fallback", len(runner.commands))
	}

	// StageRemote behavior: ssh runs the mkdir/cd/tar-extract shell and tar
	// archives the local workspace.
	sshOut, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatalf("ssh stub log: %v", err)
	}
	for _, want := range []string{"builder", "mkdir -p ~/.vci/state/work/run_abc", "cd ~/.vci/state/work/run_abc", "tar -xpf -"} {
		if !strings.Contains(string(sshOut), want) {
			t.Errorf("stage command missing %q: %s", want, sshOut)
		}
	}
	tarOut, err := os.ReadFile(filepath.Join(dir, "tar.log"))
	if err != nil {
		t.Fatalf("tar stub log: %v", err)
	}
	for _, want := range []string{"-cf", "-", "-C", workspace, "."} {
		if !strings.Contains(string(tarOut), want) {
			t.Errorf("tar invocation missing %q: %s", want, tarOut)
		}
	}
}

// TestStageOrReconstructProbeErrorFallsBack pins that a probe failure on an
// otherwise eligible machine falls back to full workspace staging: the
// scripted runner is invoked exactly once for ProbeSeedHead, the error sends
// stageOrReconstruct to StageRemote, and the helper returns nil.
func TestStageOrReconstructProbeErrorFallsBack(t *testing.T) {
	ctx := context.Background()

	// Record every ssh/tar invocation the real StageRemote performs.
	dir := t.TempDir()
	sshLog := filepath.Join(dir, "ssh.log")
	writeStub(t, dir, "ssh", "#!/bin/sh\necho \"$*\" >> "+sshLog+"\ncat >/dev/null 2>&1\nexit 0\n")
	writeStub(t, dir, "tar", "#!/bin/sh\necho \"$*\" >> "+filepath.Join(dir, "tar.log")+"\nexit 0\n")

	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lcPath := filepath.Join(t.TempDir(), "submission-lc.tar")
	if err := os.WriteFile(lcPath, []byte("lc\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Eligible inputs: the machine seeds this project and the LC archive is a
	// regular file, so stageOrReconstruct takes the probe/stream client path.
	// The scripted runner fails that first ProbeSeedHead invocation.
	runner := &scriptedRunner{result: process.Result{ExitCode: 0}, err: fmt.Errorf("probe failed")}
	machine := config.Machine{Host: "builder", SourcePaths: map[string]string{"Vci": "~/vci-seed"}}
	project := config.Project{}
	projectName := "Vci"

	_, err := stageOrReconstruct(ctx, runner, machine, project, projectName, lcPath, workspace, workspace, "cafebabe", "~/.vci/state/work/run_abc")
	if err != nil {
		t.Fatalf("stageOrReconstruct: %v", err)
	}

	// The probe failed, so the runner saw exactly one invocation and the
	// stream client path was never reached.
	if len(runner.commands) != 1 {
		t.Fatalf("runner invoked %d times, want exactly 1 (the failed probe)", len(runner.commands))
	}
	probe := runner.commands[0]
	if probe.Executable != "ssh" || len(probe.Args) < 6 || probe.Args[0] != "builder" || probe.Args[len(probe.Args)-2] != "rev-parse" || probe.Args[len(probe.Args)-1] != "HEAD" {
		t.Errorf("unexpected probe command: %+v", probe)
	}

	// The failed probe fell back to StageRemote: ssh runs the
	// mkdir/cd/tar-extract shell and tar archives the local workspace.
	sshOut, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatalf("ssh stub log: %v", err)
	}
	for _, want := range []string{"builder", "mkdir -p ~/.vci/state/work/run_abc", "cd ~/.vci/state/work/run_abc", "tar -xpf -"} {
		if !strings.Contains(string(sshOut), want) {
			t.Errorf("stage command missing %q: %s", want, sshOut)
		}
	}
	tarOut, err := os.ReadFile(filepath.Join(dir, "tar.log"))
	if err != nil {
		t.Fatalf("tar stub log: %v", err)
	}
	for _, want := range []string{"-cf", "-", "-C", workspace, "."} {
		if !strings.Contains(string(tarOut), want) {
			t.Errorf("tar invocation missing %q: %s", want, tarOut)
		}
	}
}

// seededRunner scripts the seeded-reconstruction ssh transport. The first
// invocation (ProbeSeedHead) writes seedHead to stdout; the second
// (StreamReconstruct) reads its stdin — the worker payload tar — and records
// the parsed members so the test can assert exactly what was streamed. Every
// command is recorded so the test can verify the fallback staging path never
// ran. A non-nil streamErr makes the stream invocation fail so the caller
// falls back to full workspace staging.
type seededRunner struct {
	commands  []process.Command
	seedHead  string
	streamed  []byte
	payload   []struct{ Name, Data string }
	parseErr  error
	err       error
	streamErr error
}

func (r *seededRunner) Run(_ context.Context, command process.Command) (process.Result, error) {
	r.commands = append(r.commands, command)
	switch len(r.commands) {
	case 1: // ProbeSeedHead: `ssh <host> git -C '<seed>' rev-parse HEAD`.
		if _, err := command.Stdout.Write([]byte(r.seedHead + "\n")); err != nil {
			return process.Result{}, err
		}
	case 2: // StreamReconstruct: `ssh <host> '<reconstruct shell>'` with the payload on stdin.
		data, err := io.ReadAll(command.Stdin)
		if err != nil {
			return process.Result{}, err
		}
		r.streamed = data
		r.payload, r.parseErr = parsePayloadTar(data)
		if r.streamErr != nil {
			return process.Result{ExitCode: 0}, r.streamErr
		}
	default:
		return process.Result{}, fmt.Errorf("unexpected %dth ssh invocation: %+v", len(r.commands), command)
	}
	return process.Result{ExitCode: 0}, r.err
}

// parsePayloadTar reads a worker payload tar into ordered members: head, an
// optional bundle, and lc.tar, exactly as workerPayload produces them.
func parsePayloadTar(data []byte) ([]struct{ Name, Data string }, error) {
	var members []struct{ Name, Data string }
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return members, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read payload tar: %w", err)
		}
		content, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("read payload member %q: %w", h.Name, err)
		}
		members = append(members, struct{ Name, Data string }{h.Name, string(content)})
	}
}

// TestStageOrReconstructStreamsSeeded pins the seeded-reconstruction success
// path end to end: for an eligible machine the probe returns the seed
// checkout's head and the reconstruction payload is streamed over the ssh
// runner's stdin, with no fallback tar/ssh staging. The streamed payload must
// carry head equal to the workspace commit and the durable lc.tar byte for
// byte; the bundle member may be present or absent (absent here because the
// seed already has head, so CreateBundle reports an empty range).
func TestStageOrReconstructStreamsSeeded(t *testing.T) {
	ctx := context.Background()

	// Sentinel ssh/tar stubs on PATH: the real StageRemote fallback execs
	// `tar` and `ssh` through PATH lookup, so any fallback invocation would
	// append to these logs.
	dir := t.TempDir()
	sshLog := filepath.Join(dir, "ssh.log")
	tarLog := filepath.Join(dir, "tar.log")
	writeStub(t, dir, "ssh", "#!/bin/sh\necho \"$*\" >> "+sshLog+"\ncat >/dev/null 2>&1\nexit 0\n")
	writeStub(t, dir, "tar", "#!/bin/sh\necho \"$*\" >> "+tarLog+"\nexit 0\n")

	// A real Git workspace with a committed HEAD; the machine's seed checkout
	// already has this commit, so the worker needs no bundle.
	workspace := t.TempDir()
	gitRun(t, workspace, "init", "-q")
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, workspace, "add", "main.go")
	gitRun(t, workspace, "commit", "-q", "-m", "seed")
	head := gitRun(t, workspace, "rev-parse", "HEAD")

	lcBytes := []byte("durable local-change archive\n")
	lcPath := filepath.Join(t.TempDir(), "submission-lc.tar")
	if err := os.WriteFile(lcPath, lcBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	// Eligible: the machine seeds this project and the LC archive is a
	// regular file, so stageOrReconstruct takes the probe/stream path.
	runner := &seededRunner{seedHead: head}
	machine := config.Machine{Host: "builder", SourcePaths: map[string]string{"Vci": "~/vci-seed"}}
	project := config.Project{}
	projectName := "Vci"

	_, err := stageOrReconstruct(ctx, runner, machine, project, projectName, lcPath, workspace, workspace, head, "~/.vci/state/work/run_abc")
	if err != nil {
		t.Fatalf("stageOrReconstruct: %v", err)
	}

	// The scripted runner saw exactly the probe then the stream, never the
	// fallback staging path (which bypasses the runner and would land in the
	// PATH stubs below).
	if len(runner.commands) != 2 {
		t.Fatalf("runner invoked %d times, want 2 (probe + stream)", len(runner.commands))
	}
	probe := runner.commands[0]
	if probe.Executable != "ssh" || len(probe.Args) < 6 || probe.Args[0] != "builder" || probe.Args[1] != "git" || probe.Args[len(probe.Args)-2] != "rev-parse" || probe.Args[len(probe.Args)-1] != "HEAD" {
		t.Errorf("unexpected probe command: %+v", probe)
	}
	stream := runner.commands[1]
	if stream.Executable != "ssh" || len(stream.Args) != 2 || stream.Args[0] != "builder" {
		t.Errorf("unexpected stream command: %+v", stream)
	}
	if len(runner.streamed) == 0 {
		t.Fatal("StreamReconstruct stdin was empty; expected the worker payload tar")
	}
	if runner.parseErr != nil {
		t.Fatalf("streamed stdin is not a valid payload tar: %v", runner.parseErr)
	}

	// The streamed payload must carry head equal to the workspace commit and
	// lc.tar byte for byte; the bundle member may be present or absent.
	var sawHead, sawLC bool
	for _, m := range runner.payload {
		switch m.Name {
		case "head":
			sawHead = true
			if m.Data != head+"\n" {
				t.Errorf("payload head = %q, want workspace commit %q", m.Data, head+"\n")
			}
		case "lc.tar":
			sawLC = true
			if m.Data != string(lcBytes) {
				t.Errorf("payload lc.tar differs from the durable archive byte for byte")
			}
		case "bundle":
			if m.Data == "" {
				t.Errorf("payload bundle member is empty")
			}
		default:
			t.Errorf("unexpected payload member %q", m.Name)
		}
	}
	if !sawHead {
		t.Error("payload is missing the head member")
	}
	if !sawLC {
		t.Error("payload is missing the lc.tar member")
	}

	// No fallback `ssh`/`tar` staging command ran on the host PATH.
	for _, log := range []string{sshLog, tarLog} {
		data, err := os.ReadFile(log)
		switch {
		case err == nil && len(data) != 0:
			t.Errorf("fallback %s stub was invoked: %q", filepath.Base(log), data)
		case err != nil && !os.IsNotExist(err):
			t.Fatalf("read stub log %s: %v", log, err)
		}
	}
}

// TestStageOrReconstructStreamErrorFallsBack pins that a reconstruction
// stream failure on an otherwise eligible machine falls back to full
// workspace staging: the scripted runner is invoked exactly twice — the
// probe returns the seed HEAD, then StreamReconstruct fails — and the helper
// returns nil after StageRemote stages the whole workspace.
func TestStageOrReconstructStreamErrorFallsBack(t *testing.T) {
	ctx := context.Background()

	// Sentinel ssh/tar stubs on PATH: the real StageRemote fallback execs
	// `tar` and `ssh` through PATH lookup, so the fallback appends to these
	// logs.
	dir := t.TempDir()
	sshLog := filepath.Join(dir, "ssh.log")
	tarLog := filepath.Join(dir, "tar.log")
	writeStub(t, dir, "ssh", "#!/bin/sh\necho \"$*\" >> "+sshLog+"\ncat >/dev/null 2>&1\nexit 0\n")
	writeStub(t, dir, "tar", "#!/bin/sh\necho \"$*\" >> "+tarLog+"\nexit 0\n")

	// A real Git workspace with a committed HEAD; the machine's seed checkout
	// already has this commit, so the worker needs no bundle.
	workspace := t.TempDir()
	gitRun(t, workspace, "init", "-q")
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, workspace, "add", "main.go")
	gitRun(t, workspace, "commit", "-q", "-m", "seed")
	head := gitRun(t, workspace, "rev-parse", "HEAD")

	lcBytes := []byte("durable local-change archive\n")
	lcPath := filepath.Join(t.TempDir(), "submission-lc.tar")
	if err := os.WriteFile(lcPath, lcBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	// Eligible: the machine seeds this project and the LC archive is a
	// regular file, so stageOrReconstruct takes the probe/stream path. The
	// probe returns the seed HEAD; the stream then fails.
	runner := &seededRunner{seedHead: head, streamErr: fmt.Errorf("stream failed")}
	machine := config.Machine{Host: "builder", SourcePaths: map[string]string{"Vci": "~/vci-seed"}}
	project := config.Project{}
	projectName := "Vci"

	_, err := stageOrReconstruct(ctx, runner, machine, project, projectName, lcPath, workspace, workspace, head, "~/.vci/state/work/run_abc")
	if err != nil {
		t.Fatalf("stageOrReconstruct: %v", err)
	}

	// The scripted runner saw exactly the probe then the failed stream.
	if len(runner.commands) != 2 {
		t.Fatalf("runner invoked %d times, want 2 (probe + failed stream)", len(runner.commands))
	}
	probe := runner.commands[0]
	if probe.Executable != "ssh" || len(probe.Args) < 6 || probe.Args[0] != "builder" || probe.Args[1] != "git" || probe.Args[len(probe.Args)-2] != "rev-parse" || probe.Args[len(probe.Args)-1] != "HEAD" {
		t.Errorf("unexpected probe command: %+v", probe)
	}
	stream := runner.commands[1]
	if stream.Executable != "ssh" || len(stream.Args) != 2 || stream.Args[0] != "builder" {
		t.Errorf("unexpected stream command: %+v", stream)
	}
	if len(runner.streamed) == 0 {
		t.Fatal("StreamReconstruct stdin was empty; expected the worker payload tar")
	}
	if runner.parseErr != nil {
		t.Fatalf("streamed stdin is not a valid payload tar: %v", runner.parseErr)
	}

	// The failed stream fell back to StageRemote: ssh runs the
	// mkdir/cd/tar-extract shell and tar archives the local workspace.
	sshOut, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatalf("ssh stub log: %v", err)
	}
	for _, want := range []string{"builder", "mkdir -p ~/.vci/state/work/run_abc", "cd ~/.vci/state/work/run_abc", "tar -xpf -"} {
		if !strings.Contains(string(sshOut), want) {
			t.Errorf("stage command missing %q: %s", want, sshOut)
		}
	}
	tarOut, err := os.ReadFile(tarLog)
	if err != nil {
		t.Fatalf("tar stub log: %v", err)
	}
	for _, want := range []string{"-cf", "-", "-C", workspace, "."} {
		if !strings.Contains(string(tarOut), want) {
			t.Errorf("tar invocation missing %q: %s", want, tarOut)
		}
	}
}

// TestStageOrReconstructEmptySubmittedHeadFallsBack pins that an empty
// submitted head cannot seed a reconstruction payload: even though the
// machine is eligible and the probe returns the seed checkout's head,
// stageOrReconstruct never invokes StreamReconstruct and falls back to full
// workspace staging exactly as executeRemote stages it.
func TestStageOrReconstructEmptySubmittedHeadFallsBack(t *testing.T) {
	ctx := context.Background()

	// Sentinel ssh/tar stubs on PATH: the real StageRemote fallback execs
	// `tar` and `ssh` through PATH lookup, so any fallback invocation appends
	// to these logs.
	dir := t.TempDir()
	sshLog := filepath.Join(dir, "ssh.log")
	tarLog := filepath.Join(dir, "tar.log")
	writeStub(t, dir, "ssh", "#!/bin/sh\necho \"$*\" >> "+sshLog+"\ncat >/dev/null 2>&1\nexit 0\n")
	writeStub(t, dir, "tar", "#!/bin/sh\necho \"$*\" >> "+tarLog+"\nexit 0\n")

	// A real Git workspace with a committed HEAD; the machine's seed checkout
	// already has this commit, so the probe returns it as the seed head.
	workspace := t.TempDir()
	gitRun(t, workspace, "init", "-q")
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, workspace, "add", "main.go")
	gitRun(t, workspace, "commit", "-q", "-m", "seed")
	head := gitRun(t, workspace, "rev-parse", "HEAD")

	lcPath := filepath.Join(t.TempDir(), "submission-lc.tar")
	if err := os.WriteFile(lcPath, []byte("lc\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Eligible: the machine seeds this project and the LC archive is a
	// regular file, so stageOrReconstruct takes the probe/stream path. The
	// probe returns the seed HEAD; submittedHead is empty, so no payload head
	// exists and the helper must fall back to full staging without streaming.
	runner := &seededRunner{seedHead: head}
	machine := config.Machine{Host: "builder", SourcePaths: map[string]string{"Vci": "~/vci-seed"}}
	project := config.Project{}
	projectName := "Vci"

	_, err := stageOrReconstruct(ctx, runner, machine, project, projectName, lcPath, workspace, workspace, "", "~/.vci/state/work/run_abc")
	if err != nil {
		t.Fatalf("stageOrReconstruct: %v", err)
	}

	// The scripted runner saw exactly the probe; StreamReconstruct was never
	// invoked because the empty submitted head cannot seed a payload.
	if len(runner.commands) != 1 {
		t.Fatalf("runner invoked %d times, want exactly 1 (the probe; no stream)", len(runner.commands))
	}
	probe := runner.commands[0]
	if probe.Executable != "ssh" || len(probe.Args) < 6 || probe.Args[0] != "builder" || probe.Args[1] != "git" || probe.Args[len(probe.Args)-2] != "rev-parse" || probe.Args[len(probe.Args)-1] != "HEAD" {
		t.Errorf("unexpected probe command: %+v", probe)
	}
	if len(runner.streamed) != 0 {
		t.Fatal("StreamReconstruct received stdin despite the empty submitted head")
	}

	// The empty submitted head fell back to StageRemote: ssh runs the
	// mkdir/cd/tar-extract shell and tar archives the local workspace.
	sshOut, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatalf("ssh stub log: %v", err)
	}
	for _, want := range []string{"builder", "mkdir -p ~/.vci/state/work/run_abc", "cd ~/.vci/state/work/run_abc", "tar -xpf -"} {
		if !strings.Contains(string(sshOut), want) {
			t.Errorf("stage command missing %q: %s", want, sshOut)
		}
	}
	tarOut, err := os.ReadFile(tarLog)
	if err != nil {
		t.Fatalf("tar stub log: %v", err)
	}
	for _, want := range []string{"-cf", "-", "-C", workspace, "."} {
		if !strings.Contains(string(tarOut), want) {
			t.Errorf("tar invocation missing %q: %s", want, tarOut)
		}
	}
}
