package cli

// Plan 16 Phase 0/1 CLI surface for `vci artifacts ls|get`. The `get`
// path is the only non-JSON stdout stream; `ls` keeps the standard
// JSON envelope. Client roots proxy both over a fake `ssh` stub in
// PATH whose recorded argv proves the artifacts command was forwarded,
// not executed locally.
//
// Plan 17 Phase 0/1 CLI surface for `vci logs`. Phase 0 freezes the
// `vci check` `stdout_path`/`stderr_path` contract and the negative
// stubs (no run-id, bad stream, bad --tail). Phase 1 pins the raw
// byte stream, the bounded tail, and the client proxy over a fake
// `ssh` stub in PATH whose recorded argv proves the logs command was
// forwarded, not executed locally.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/app"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/store"
)

// writeCLIPathStub writes an executable shell stub into dir and
// prepends dir to PATH for the duration of the test.
func writeCLIPathStub(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+previous)
}

// setupArtifactsRunCLI initializes a coordinator root under VCI_ROOT
// with one persisted run and one collected artifact
// (build/out.bin = "cli-artifact\n").
func setupArtifactsRunCLI(t *testing.T) (model.Layout, model.RunID) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	l := model.Layout{Root: root}
	if err := app.Initialize(l); err != nil {
		t.Fatal(err)
	}
	runStore := store.Store{Layout: l}
	record, err := store.NewRun("demo", "mac-local", []string{"true"}, "source", map[string]any{}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := runStore.Save(record); err != nil {
		t.Fatal(err)
	}
	runDir, err := l.RunDir(string(record.ID))
	if err != nil {
		t.Fatal(err)
	}
	artDir := filepath.Join(runDir, "artifacts", "build")
	if err := os.MkdirAll(artDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artDir, "out.bin"), []byte("cli-artifact\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return l, record.ID
}

// setupLogsRunCLI initializes a coordinator root under VCI_ROOT with
// one persisted run and durable stdout.log/stderr.log files (the
// shape the worker produces).
func setupLogsRunCLI(t *testing.T, stdoutContent, stderrContent string) (model.Layout, model.RunID) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	l := model.Layout{Root: root}
	if err := app.Initialize(l); err != nil {
		t.Fatal(err)
	}
	runStore := store.Store{Layout: l}
	record, err := store.NewRun("demo", "mac-local", []string{"true"}, "source", map[string]any{}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := runStore.Save(record); err != nil {
		t.Fatal(err)
	}
	runDir, err := l.RunDir(string(record.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "stdout.log"), []byte(stdoutContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "stderr.log"), []byte(stderrContent), 0o600); err != nil {
		t.Fatal(err)
	}
	return l, record.ID
}

// TestArtifactsLsEnvelope pins the `artifacts ls` JSON envelope: files
// (sorted relative paths) and the truncated flag.
func TestArtifactsLsEnvelope(t *testing.T) {
	_, id := setupArtifactsRunCLI(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"artifacts", "ls", string(id)}, &out, &errOut); code != 0 {
		t.Fatalf("artifacts ls: %d %s", code, out.String())
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("not OK: %+v", resp.Error)
	}
	data, _ := resp.Data.(map[string]any)
	files, _ := data["files"].([]any)
	if len(files) != 1 || files[0] != "build/out.bin" {
		t.Errorf("files: %v", data["files"])
	}
	if data["truncated"] != false {
		t.Errorf("truncated: %v", data["truncated"])
	}
}

// TestArtifactsLsMissingRunID pins the Phase 0 negative stub:
// `artifacts ls` without a run-id is invalid_arguments, and the bare
// `artifacts` command with no operation is invalid_arguments too.
func TestArtifactsLsMissingRunID(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	if err := app.Initialize(model.Layout{Root: root}); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"artifacts", "ls"}, &out, &errOut); code != 2 {
		t.Fatalf("ls without run-id: exit %d", code)
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Error == nil || resp.Error.Code != "invalid_arguments" {
		t.Fatalf("ls without run-id: %+v", resp)
	}
	out.Reset()
	if code := Run([]string{"artifacts"}, &out, &errOut); code != 2 {
		t.Fatalf("artifacts without op: exit %d", code)
	}
	_ = json.Unmarshal(out.Bytes(), &resp)
	if resp.OK || resp.Error == nil || resp.Error.Code != "invalid_arguments" {
		t.Fatalf("artifacts without op: %+v", resp)
	}
}

// TestArtifactsLsMissingRunNotFound pins the not_found envelope for a
// nonexistent run: configuration class, not retryable.
func TestArtifactsLsMissingRunNotFound(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	if err := app.Initialize(model.Layout{Root: root}); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"artifacts", "ls", "run_missing"}, &out, &errOut); code != 2 {
		t.Fatalf("ls missing run: exit %d", code)
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Error == nil || resp.Error.Code != "not_found" || resp.Error.Class != "configuration" || resp.Error.Retryable {
		t.Fatalf("ls missing run: %+v", resp.Error)
	}
}

// TestArtifactsGetStreamsRawBytes pins that `artifacts get` writes the
// artifact's exact bytes to stdout with no JSON wrapper and exits 0.
func TestArtifactsGetStreamsRawBytes(t *testing.T) {
	_, id := setupArtifactsRunCLI(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"artifacts", "get", string(id), "build/out.bin"}, &out, &errOut); code != 0 {
		t.Fatalf("artifacts get: %d %s", code, out.String())
	}
	if got := out.String(); got != "cli-artifact\n" {
		t.Errorf("raw bytes: %q", got)
	}
}

// TestArtifactsGetRejectsTraversal pins the Phase 0 negative stub:
// traversal, absolute, excluded, and whitespace rel paths are all
// invalid_arguments envelopes, never filesystem access.
func TestArtifactsGetRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	if err := app.Initialize(model.Layout{Root: root}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"../etc/passwd", "build/../../etc/passwd", "/etc/passwd", ".git/config", ".vci/run.json", "a b", "-flag", "a//b"} {
		var out, errOut bytes.Buffer
		if code := Run([]string{"artifacts", "get", "run_1", rel}, &out, &errOut); code != 2 {
			t.Fatalf("get %q: exit %d", rel, code)
		}
		var resp Response
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatalf("get %q: %v", rel, err)
		}
		if resp.OK || resp.Error == nil || resp.Error.Code != "invalid_arguments" {
			t.Fatalf("get %q: %+v", rel, resp)
		}
	}
}

// TestArtifactsGetNotFound pins the not_found envelope for a missing
// artifact and a missing run on the raw-byte path.
func TestArtifactsGetNotFound(t *testing.T) {
	_, id := setupArtifactsRunCLI(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"artifacts", "get", string(id), "build/nope.bin"}, &out, &errOut); code != 2 {
		t.Fatalf("get missing artifact: exit %d", code)
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Error == nil || resp.Error.Code != "not_found" {
		t.Fatalf("get missing artifact: %+v", resp.Error)
	}
	out.Reset()
	if code := Run([]string{"artifacts", "get", "run_missing", "x"}, &out, &errOut); code != 2 {
		t.Fatalf("get missing run: exit %d", code)
	}
	_ = json.Unmarshal(out.Bytes(), &resp)
	if resp.OK || resp.Error == nil || resp.Error.Code != "not_found" {
		t.Fatalf("get missing run: %+v", resp.Error)
	}
}

// TestArtifactsClientProxyViaFakeSSH pins that a client root forwards
// `artifacts ls` (JSON envelope relayed) and `artifacts get` (raw
// bytes streamed) over ordinary ssh; the fake ssh argv records the
// artifacts command so nothing ran locally.
func TestArtifactsClientProxyViaFakeSSH(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	envelope := `{"schema_version":1,"command":"artifacts","ok":true,"data":{"files":["build/out.txt"],"truncated":false},"error":null}`
	body := "#!/bin/sh\n"
	body += "echo \"$*\" >> " + logPath + "\n"
	body += "cat >/dev/null 2>&1\n"
	body += "case \"$*\" in\n"
	body += "  *\"'artifacts' 'get'\"*) printf 'raw-bytes-from-remote' ;;\n"
	body += "  *) printf '%s' '" + envelope + "' ;;\n"
	body += "esac\n"
	body += "exit 0\n"
	writeCLIPathStub(t, dir, "ssh", body)

	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("schema_version = 1\norchestrator = \"coordinator\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := Run([]string{"artifacts", "ls", "run_1"}, &out, &errOut); code != 0 {
		t.Fatalf("proxied ls: %d %s", code, out.String())
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("proxied ls not OK: %+v", resp.Error)
	}

	out.Reset()
	if code := Run([]string{"artifacts", "get", "run_1", "build/out.txt"}, &out, &errOut); code != 0 {
		t.Fatalf("proxied get: %d %s", code, out.String())
	}
	if got := out.String(); got != "raw-bytes-from-remote" {
		t.Errorf("proxied get bytes: %q", got)
	}

	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ssh stub log missing: %v", err)
	}
	s := string(log)
	for _, want := range []string{"vci 'artifacts' 'ls' 'run_1'", "vci 'artifacts' 'get' 'run_1' 'build/out.txt'"} {
		if !strings.Contains(s, want) {
			t.Errorf("ssh log missing %q: %s", want, s)
		}
	}
	// No run record was created on the client root.
	entries, _ := os.ReadDir(filepath.Join(root, "state", "runs"))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "run_") {
			t.Errorf("client created run record %s", entry.Name())
		}
	}
}

// TestArtifactsClientProxyPreservesRemoteError pins that a remote get
// failure (missing run) surfaces as a JSON not_found envelope with
// exit 2, not as streamed bytes.
func TestArtifactsClientProxyPreservesRemoteError(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	envelope := `{"schema_version":1,"command":"artifacts","ok":false,"data":{},"error":{"code":"not_found","class":"configuration","message":"run not found","retryable":false}}`
	body := "#!/bin/sh\n"
	body += "echo \"$*\" >> " + logPath + "\n"
	body += "cat >/dev/null 2>&1\n"
	body += "printf '%s' '" + envelope + "'\n"
	body += "exit 2\n"
	writeCLIPathStub(t, dir, "ssh", body)

	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("schema_version = 1\norchestrator = \"coordinator\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := Run([]string{"artifacts", "get", "run_1", "build/out.txt"}, &out, &errOut); code != 2 {
		t.Fatalf("proxied get error: exit %d %s", code, out.String())
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Error == nil || resp.Error.Code != "not_found" {
		t.Fatalf("proxied get error envelope: %+v", resp)
	}
}

// TestLogsCheckFreeze pins the Phase 0 contract that `vci check`'s
// envelope still carries the durable `stdout_path`/`stderr_path`
// fields pointing at the run's log files.
func TestLogsCheckFreeze(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	l := model.Layout{Root: root}
	if err := app.Initialize(l); err != nil {
		t.Fatal(err)
	}
	runStore := store.Store{Layout: l}
	record, err := store.NewRun("demo", "mac-local", []string{"true"}, "source", map[string]any{}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := runStore.Save(record); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, state := range []model.RunState{model.RunStaging, model.RunRunning, model.RunCommitting} {
		if _, err := runStore.Transition(record.ID, state, now); err != nil {
			t.Fatalf("transition to %s: %v", state, err)
		}
	}
	runDir, err := l.RunDir(string(record.ID))
	if err != nil {
		t.Fatal(err)
	}
	result := app.BuildResult{
		RunID:      record.ID,
		State:      model.RunSucceeded,
		StdoutPath: filepath.Join(runDir, "stdout.log"),
		StderrPath: filepath.Join(runDir, "stderr.log"),
	}
	if err := runStore.PublishResult(record.ID, result); err != nil {
		t.Fatal(err)
	}
	if _, err := runStore.Transition(record.ID, model.RunSucceeded, now); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"check", string(record.ID)}, &out, &errOut); code != 0 {
		t.Fatalf("check: %d %s", code, out.String())
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("check failed: %+v", resp.Error)
	}
	data, ok := resp.Data.(map[string]any)
	if !ok {
		t.Fatalf("check data: %T", resp.Data)
	}
	if data["stdout_path"] != filepath.Join(runDir, "stdout.log") || data["stderr_path"] != filepath.Join(runDir, "stderr.log") {
		t.Errorf("check paths: stdout=%v stderr=%v", data["stdout_path"], data["stderr_path"])
	}
}

// TestLogsReadsStdout pins that `vci logs <run-id>` writes the
// stdout.log bytes verbatim to stdout with no JSON wrapper and exits
// 0.
func TestLogsReadsStdout(t *testing.T) {
	_, id := setupLogsRunCLI(t, "out line 1\nout line 2\n", "err line 1\n")
	var out, errOut bytes.Buffer
	if code := Run([]string{"logs", string(id)}, &out, &errOut); code != 0 {
		t.Fatalf("logs: %d %s", code, out.String())
	}
	if got := out.String(); got != "out line 1\nout line 2\n" {
		t.Errorf("stdout bytes: %q", got)
	}
	if errOut.Len() != 0 {
		t.Errorf("stderr contaminated: %q", errOut.String())
	}
}

// TestLogsReadsStderr pins that `--stderr` selects the stderr.log.
func TestLogsReadsStderr(t *testing.T) {
	_, id := setupLogsRunCLI(t, "out line 1\n", "err line 1\nerr line 2\n")
	var out, errOut bytes.Buffer
	if code := Run([]string{"logs", string(id), "--stderr"}, &out, &errOut); code != 0 {
		t.Fatalf("logs --stderr: %d %s", code, out.String())
	}
	if got := out.String(); got != "err line 1\nerr line 2\n" {
		t.Errorf("stderr bytes: %q", got)
	}
}

// TestLogsTailLastN pins the bounded `--tail <n>`: the last n lines
// are printed (tail -n semantics, trailing newline preserved), a tail
// larger than the file returns everything, and the flag combines with
// --stderr.
func TestLogsTailLastN(t *testing.T) {
	_, id := setupLogsRunCLI(t, "l1\nl2\nl3\nl4\nl5\n", "e1\ne2\n")
	var out, errOut bytes.Buffer
	if code := Run([]string{"logs", string(id), "--tail", "2"}, &out, &errOut); code != 0 {
		t.Fatalf("logs --tail 2: %d %s", code, out.String())
	}
	if got := out.String(); got != "l4\nl5\n" {
		t.Errorf("tail 2: %q", got)
	}
	out.Reset()
	if code := Run([]string{"logs", string(id), "--tail", "2", "--stderr"}, &out, &errOut); code != 0 {
		t.Fatalf("logs --tail 2 --stderr: %d %s", code, out.String())
	}
	if got := out.String(); got != "e1\ne2\n" {
		t.Errorf("stderr tail 2: %q", got)
	}
	out.Reset()
	if code := Run([]string{"logs", string(id), "--tail", "100"}, &out, &errOut); code != 0 {
		t.Fatalf("logs --tail 100: %d %s", code, out.String())
	}
	if got := out.String(); got != "l1\nl2\nl3\nl4\nl5\n" {
		t.Errorf("tail 100: %q", got)
	}
}

// TestTailLinesUnit pins the tail -n edge cases directly: a trailing
// newline terminates the final line, unterminated trailing content is
// its own final line, and fewer than n lines returns everything.
func TestTailLinesUnit(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"a\nb\n", 1, "b\n"},
		{"a\nb", 1, "b"},
		{"a\nb\nc\n", 2, "b\nc\n"},
		{"a\nb\nc", 2, "b\nc"},
		{"a\nb\n", 3, "a\nb\n"},
		{"", 1, ""},
		{"\n\n\n", 2, "\n\n"},
		{"a\n", 1, "a\n"},
		{"a", 1, "a"},
	}
	for _, tc := range cases {
		if got := string(tailLines([]byte(tc.in), tc.n)); got != tc.want {
			t.Errorf("tailLines(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

// TestLogsMissingRunNotFound pins the not_found envelope for a
// nonexistent run on the raw-byte path: configuration class, not
// retryable.
func TestLogsMissingRunNotFound(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	if err := app.Initialize(model.Layout{Root: root}); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"logs", "run_missing"}, &out, &errOut); code != 2 {
		t.Fatalf("logs missing run: exit %d", code)
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Error == nil || resp.Error.Code != "not_found" || resp.Error.Class != "configuration" || resp.Error.Retryable {
		t.Fatalf("logs missing run: %+v", resp.Error)
	}
}

// TestLogsMissingFileNotFound pins the not_found envelope for a run
// whose requested log file is absent.
func TestLogsMissingFileNotFound(t *testing.T) {
	l, id := setupLogsRunCLI(t, "out line 1\n", "err line 1\n")
	if err := os.Remove(filepath.Join(l.Root, "state", "runs", string(id), "stdout.log")); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"logs", string(id)}, &out, &errOut); code != 2 {
		t.Fatalf("logs missing file: exit %d", code)
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Error == nil || resp.Error.Code != "not_found" {
		t.Fatalf("logs missing file: %+v", resp.Error)
	}
}

// TestLogsRejectsBadStream pins the Phase 0 negative stubs: `logs`
// without a run-id, an unknown flag (a bad stream selector), and a
// malformed or out-of-bounds --tail are all invalid_arguments
// envelopes.
func TestLogsRejectsBadStream(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	if err := app.Initialize(model.Layout{Root: root}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"logs"},
		{"logs", "--foo"},
		{"logs", "run_1", "--foo"},
		{"logs", "run_1", "--tail", "abc"},
		{"logs", "run_1", "--tail", "0"},
		{"logs", "run_1", "--tail", "100001"},
		{"logs", "run_1", "--tail", "-3"},
		{"logs", "run_1", "--tail"},
		{"logs", "run_1", "extra"},
	} {
		var out, errOut bytes.Buffer
		if code := Run(args, &out, &errOut); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
		var resp Response
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatalf("%v: not JSON: %v", args, err)
		}
		if resp.OK || resp.Error == nil || resp.Error.Code != "invalid_arguments" || resp.Error.Class != "usage" {
			t.Fatalf("%v: %+v", args, resp)
		}
		if resp.Command != "logs" {
			t.Errorf("%v: command echo %q", args, resp.Command)
		}
	}
}

// TestLogsArtifactsRejectInvalidRunID pins that malformed run ids are
// rejected as invalid_arguments on every logs/artifacts surface
// before any store or ssh access: `logs`, `artifacts ls`, and
// `artifacts get`.
func TestLogsArtifactsRejectInvalidRunID(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	if err := app.Initialize(model.Layout{Root: root}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"logs", "not_a_run"},
		{"logs", "../escape"},
		{"artifacts", "ls", "not_a_run"},
		{"artifacts", "get", "not_a_run", "x"},
	} {
		var out, errOut bytes.Buffer
		if code := Run(args, &out, &errOut); code != 2 {
			t.Fatalf("%v: exit %d", args, code)
		}
		var resp Response
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatalf("%v: not JSON: %v", args, err)
		}
		if resp.OK || resp.Error == nil || resp.Error.Code != "invalid_arguments" || resp.Error.Class != "usage" {
			t.Fatalf("%v: %+v", args, resp)
		}
	}
}

// TestLogsClientProxyViaFakeSSH pins that a client root forwards
// `vci logs` over ordinary ssh; the fake ssh argv records the logs
// command (never executed locally) and the raw remote bytes are
// relayed verbatim.
func TestLogsClientProxyViaFakeSSH(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	body := "#!/bin/sh\n"
	body += "echo \"$*\" >> " + logPath + "\n"
	body += "cat >/dev/null 2>&1\n"
	body += "printf 'remote-log-bytes'\n"
	body += "exit 0\n"
	writeCLIPathStub(t, dir, "ssh", body)

	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("schema_version = 1\norchestrator = \"coordinator\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := Run([]string{"logs", "run_1"}, &out, &errOut); code != 0 {
		t.Fatalf("proxied logs: %d %s", code, out.String())
	}
	if got := out.String(); got != "remote-log-bytes" {
		t.Errorf("proxied logs bytes: %q", got)
	}

	out.Reset()
	if code := Run([]string{"logs", "run_1", "--stderr", "--tail", "3"}, &out, &errOut); code != 0 {
		t.Fatalf("proxied logs stderr+tail: %d %s", code, out.String())
	}
	if got := out.String(); got != "remote-log-bytes" {
		t.Errorf("proxied logs stderr+tail bytes: %q", got)
	}

	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ssh stub log missing: %v", err)
	}
	s := string(log)
	for _, want := range []string{"vci 'logs' 'run_1'", "vci 'logs' 'run_1' '--stderr' '--tail' '3'"} {
		if !strings.Contains(s, want) {
			t.Errorf("ssh log missing %q: %s", want, s)
		}
	}
	// No run record was created on the client root.
	entries, _ := os.ReadDir(filepath.Join(root, "state", "runs"))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "run_") {
			t.Errorf("client created run record %s", entry.Name())
		}
	}
}

// TestLogsClientProxyPreservesRemoteError pins that a remote logs
// failure (missing run) surfaces as a JSON not_found envelope with
// exit 2, not as streamed bytes.
func TestLogsClientProxyPreservesRemoteError(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	envelope := `{"schema_version":1,"command":"logs","ok":false,"data":{},"error":{"code":"not_found","class":"configuration","message":"run not found","retryable":false}}`
	body := "#!/bin/sh\n"
	body += "echo \"$*\" >> " + logPath + "\n"
	body += "cat >/dev/null 2>&1\n"
	body += "printf '%s' '" + envelope + "'\n"
	body += "exit 2\n"
	writeCLIPathStub(t, dir, "ssh", body)

	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("schema_version = 1\norchestrator = \"coordinator\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := Run([]string{"logs", "run_missing"}, &out, &errOut); code != 2 {
		t.Fatalf("proxied logs error: exit %d %s", code, out.String())
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Error == nil || resp.Error.Code != "not_found" {
		t.Fatalf("proxied logs error envelope: %+v", resp)
	}
}
