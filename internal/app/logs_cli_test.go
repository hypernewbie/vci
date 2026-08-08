package app

// Plan 17 Phase 1 tests for the read-side log surface (ReadLog) and
// the client ssh proxy (RemoteLog). The proxy tests use a fake `ssh`
// stub in PATH that records argv and emits canned remote bytes, so no
// server and no real remote are required.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/store"
)

// setupLogsRun initializes a coordinator root with one persisted run
// record plus durable stdout.log/stderr.log files (the shape the
// worker produces) and returns the layout and run id.
func setupLogsRun(t *testing.T) (layout.Layout, model.RunID) {
	t.Helper()
	l := layout.Layout{Root: t.TempDir()}
	if err := l.Ensure(); err != nil {
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
	mustWriteFile(t, filepath.Join(runDir, "stdout.log"), "out line 1\nout line 2\n")
	mustWriteFile(t, filepath.Join(runDir, "stderr.log"), "err line 1\nerr line 2\n")
	return l, record.ID
}

// TestLogsReadsStdout pins that ReadLog streams the durable
// stdout.log bytes exactly, with the correct size.
func TestLogsReadsStdout(t *testing.T) {
	l, id := setupLogsRun(t)
	reader, size, err := ReadLog(l, id, "stdout")
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	defer reader.Close()
	if size != int64(len("out line 1\nout line 2\n")) {
		t.Errorf("size %d", size)
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "out line 1\nout line 2\n" {
		t.Errorf("stdout bytes: %q", got)
	}
}

// TestLogsReadsStderr pins that ReadLog with stream "stderr" selects
// the durable stderr.log.
func TestLogsReadsStderr(t *testing.T) {
	l, id := setupLogsRun(t)
	reader, _, err := ReadLog(l, id, "stderr")
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "err line 1\nerr line 2\n" {
		t.Errorf("stderr bytes: %q", got)
	}
}

// TestLogsMissingRunNotFound pins that a nonexistent run record maps
// to model.ErrRunNotFound.
func TestLogsMissingRunNotFound(t *testing.T) {
	l, _ := setupLogsRun(t)
	if _, _, err := ReadLog(l, "run_missing", "stdout"); !errors.Is(err, model.ErrRunNotFound) {
		t.Errorf("missing run: %v", err)
	}
}

// TestLogsMissingFileNotFound pins that a run without the requested
// log file maps to ErrLogNotFound.
func TestLogsMissingFileNotFound(t *testing.T) {
	l, id := setupLogsRun(t)
	if err := os.Remove(filepath.Join(l.Root, "state", "runs", string(id), "stdout.log")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadLog(l, id, "stdout"); !errors.Is(err, ErrLogNotFound) {
		t.Errorf("missing file: %v", err)
	}
}

// TestLogsRejectsBadStream pins that any stream other than "stdout"
// or "stderr" is rejected with ErrInvalidLogStream before any
// filesystem access, and that a swapped-in symlink is rejected via
// Lstat (ErrLogNotFound) rather than followed.
func TestLogsRejectsBadStream(t *testing.T) {
	l, id := setupLogsRun(t)
	for _, stream := range []string{"", "foo", "STDOUT", "stdout ", "stdout.log"} {
		if _, _, err := ReadLog(l, id, stream); err == nil {
			t.Errorf("ReadLog(%q): want error", stream)
		} else if !errors.Is(err, ErrInvalidLogStream) {
			t.Errorf("ReadLog(%q): %v not wrapped in ErrInvalidLogStream", stream, err)
		}
	}
	runDir, err := l.RunDir(string(id))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside.log")
	mustWriteFile(t, target, "outside\n")
	if err := os.Remove(filepath.Join(runDir, "stdout.log")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(runDir, "stdout.log")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadLog(l, id, "stdout"); !errors.Is(err, ErrLogNotFound) {
		t.Errorf("symlink: %v not ErrLogNotFound", err)
	}
}

// clientRootWithFakeSSHLogs writes a client config (orchestrator
// selector) and installs a fake `ssh` stub in PATH that records argv
// and answers with canned raw log bytes. Returns the layout and the
// ssh log path.
func clientRootWithFakeSSHLogs(t *testing.T) (layout.Layout, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	body := "#!/bin/sh\n"
	body += "echo \"$*\" >> " + logPath + "\n"
	body += "cat >/dev/null 2>&1\n"
	body += "printf 'remote-log-bytes'\n"
	body += "exit 0\n"
	writePathStub(t, dir, "ssh", body)

	root := t.TempDir()
	l := layout.Layout{Root: root}
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "schema_version = 1\norchestrator = \"coordinator\"\n"
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return l, logPath
}

// TestClientLogsProxy pins that a client root forwards `vci logs`
// over ordinary ssh: the recorded ssh argv carries the logs command
// (never executed locally) and the raw remote bytes are returned
// verbatim, with --stderr/--tail forwarded as flags.
func TestClientLogsProxy(t *testing.T) {
	l, logPath := clientRootWithFakeSSHLogs(t)

	raw, remote, err := RemoteLog(context.Background(), l, "run_1", "stdout", 0)
	if err != nil || !remote {
		t.Fatalf("remote logs: remote=%v err=%v", remote, err)
	}
	if string(raw) != "remote-log-bytes" {
		t.Errorf("log bytes: %q", raw)
	}

	raw, remote, err = RemoteLog(context.Background(), l, "run_1", "stderr", 5)
	if err != nil || !remote {
		t.Fatalf("remote logs stderr+tail: remote=%v err=%v", remote, err)
	}
	if string(raw) != "remote-log-bytes" {
		t.Errorf("log bytes (stderr): %q", raw)
	}

	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ssh stub log missing: %v", err)
	}
	s := string(log)
	for _, want := range []string{"vci 'logs' 'run_1'", "vci 'logs' 'run_1' '--stderr' '--tail' '5'"} {
		if !strings.Contains(s, want) {
			t.Errorf("ssh log missing %q: %s", want, s)
		}
	}
	// No run state was created on the client root.
	entries, _ := os.ReadDir(filepath.Join(l.Root, "state", "runs"))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "run_") {
			t.Errorf("client created run record %s", entry.Name())
		}
	}
}

// TestClientLogsProxyPreservesRemoteError pins that a remote failure
// (missing run) is preserved as a Vci error envelope rather than
// relabeled as raw log bytes or ssh failure.
func TestClientLogsProxyPreservesRemoteError(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	envelope := `{"schema_version":1,"command":"logs","ok":false,"data":{},"error":{"code":"not_found","class":"configuration","message":"run not found","retryable":false}}`
	body := "#!/bin/sh\n"
	body += "echo \"$*\" >> " + logPath + "\n"
	body += "cat >/dev/null 2>&1\n"
	body += "printf '%s' '" + envelope + "'\n"
	body += "exit 2\n"
	writePathStub(t, dir, "ssh", body)

	root := t.TempDir()
	l := layout.Layout{Root: root}
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("schema_version = 1\norchestrator = \"coordinator\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	raw, remote, err := RemoteLog(context.Background(), l, "run_missing", "stdout", 0)
	if err != nil || !remote {
		t.Fatalf("remote logs error path: remote=%v err=%v", remote, err)
	}
	var resp struct {
		OK    bool `json:"ok"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || resp.OK || resp.Error == nil || resp.Error.Code != "not_found" {
		t.Fatalf("remote error envelope not preserved: %v %s", err, raw)
	}
	if got := string(raw); !bytes.Contains([]byte(got), []byte("run not found")) {
		t.Errorf("error message not preserved: %s", got)
	}
}
