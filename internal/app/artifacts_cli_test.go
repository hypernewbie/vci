package app

// Plan 16 Phase 1 tests for the read-side artifact surface
// (ListArtifacts / GetArtifact) and the client ssh proxy. The proxy
// tests use a fake `ssh` stub in PATH that records argv and emits a
// canned remote envelope / raw bytes, so no server and no real remote
// are required.

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

// setupArtifactsRun initializes a coordinator root with one persisted
// (queued) run record and returns the layout and run id.
func setupArtifactsRun(t *testing.T) (layout.Layout, model.RunID) {
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
	return l, record.ID
}

// TestArtifactsLsListsCollected pins that ListArtifacts returns the
// same relative paths the collector produced (sorted, slash-separated)
// and surfaces the durable result.json truncated flag.
func TestArtifactsLsListsCollected(t *testing.T) {
	l, id := setupArtifactsRun(t)
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "build", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(workspace, "build", "out.bin"), "ok\n")
	mustWriteFile(t, filepath.Join(workspace, "build", "sub", "file.txt"), "nested\n")

	runDir, err := l.RunDir(string(id))
	if err != nil {
		t.Fatal(err)
	}
	collected, truncated, err := CollectArtifacts(workspace, runDir, []string{"build/*"})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if truncated {
		t.Error("truncated=true under cap")
	}
	if err := (store.Store{Layout: l}).PublishResult(id, BuildResult{RunID: id, Artifacts: collected, ArtifactsTruncated: true}); err != nil {
		t.Fatalf("publish result: %v", err)
	}

	files, truncated, err := ListArtifacts(l, id)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !truncated {
		t.Error("truncated flag not surfaced from result.json")
	}
	if len(files) != len(collected) {
		t.Fatalf("files %v vs collected %v", files, collected)
	}
	for _, want := range collected {
		if !containsRel(files, want) {
			t.Errorf("missing %q: %v", want, files)
		}
	}
	for i := 1; i < len(files); i++ {
		if files[i-1] > files[i] {
			t.Errorf("files not sorted: %v", files)
		}
	}
}

// TestArtifactsLsMissingRun pins that a nonexistent run record maps to
// model.ErrRunNotFound and a run with no artifacts lists empty without
// error.
func TestArtifactsLsMissingRun(t *testing.T) {
	l, id := setupArtifactsRun(t)
	if _, _, err := ListArtifacts(l, "run_missing"); !errors.Is(err, model.ErrRunNotFound) {
		t.Errorf("missing run: %v", err)
	}
	files, truncated, err := ListArtifacts(l, id)
	if err != nil {
		t.Fatalf("list empty run: %v", err)
	}
	if len(files) != 0 || truncated {
		t.Errorf("empty run: %v truncated=%v", files, truncated)
	}
}

// TestArtifactsGetStreamsBytes pins that GetArtifact returns the exact
// bytes of one collected artifact (binary safe) and the correct size.
func TestArtifactsGetStreamsBytes(t *testing.T) {
	l, id := setupArtifactsRun(t)
	runDir, err := l.RunDir(string(id))
	if err != nil {
		t.Fatal(err)
	}
	artDir := filepath.Join(runDir, "artifacts", "build")
	if err := os.MkdirAll(artDir, 0o700); err != nil {
		t.Fatal(err)
	}
	payload := []byte("binary\x00\x01\x02\xffpayload\n")
	if err := os.WriteFile(filepath.Join(artDir, "out.bin"), payload, 0o600); err != nil {
		t.Fatal(err)
	}

	reader, size, err := GetArtifact(l, id, "build/out.bin")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer reader.Close()
	if size != int64(len(payload)) {
		t.Errorf("size %d want %d", size, len(payload))
	}
	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("bytes differ: %q", got)
	}
}

// TestArtifactsGetRejectsTraversal pins that traversal, absolute,
// excluded, control/whitespace, and option-like relative paths are all
// rejected with ErrInvalidArtifactPath before any filesystem access,
// and that missing runs/artifacts map to their sentinels.
func TestArtifactsGetRejectsTraversal(t *testing.T) {
	l, id := setupArtifactsRun(t)
	for _, rel := range []string{
		"../etc/passwd",
		"build/../../etc/passwd",
		"/etc/passwd",
		"\\etc\\passwd",
		".git/config",
		".vci/state/run.json",
		"build/.git/config",
		"a b",
		"a\tb",
		"a\nb",
		"-flag",
		"",
		"a//b",
		".",
		"..",
	} {
		if _, _, err := GetArtifact(l, id, rel); err == nil {
			t.Errorf("GetArtifact(%q): want error", rel)
		} else if !errors.Is(err, ErrInvalidArtifactPath) {
			t.Errorf("GetArtifact(%q): %v not wrapped in ErrInvalidArtifactPath", rel, err)
		}
	}
	if _, _, err := GetArtifact(l, "run_missing", "x"); !errors.Is(err, model.ErrRunNotFound) {
		t.Errorf("missing run: %v", err)
	}
	if _, _, err := GetArtifact(l, id, "build/nope.bin"); !errors.Is(err, ErrArtifactNotFound) {
		t.Errorf("missing artifact: %v", err)
	}
}

// clientRootWithFakeSSH writes a client config (orchestrator selector)
// and installs a fake `ssh` stub in PATH that records argv and answers
// with a canned artifacts ls envelope or raw get bytes. Returns the
// layout and the ssh log path.
func clientRootWithFakeSSH(t *testing.T) (layout.Layout, string) {
	t.Helper()
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

// TestClientArtifactsProxy pins that a client root forwards
// `artifacts ls` and `artifacts get` over ordinary ssh: the recorded
// ssh argv carries the artifacts command (never executed locally), ls
// returns the remote JSON envelope unchanged, and get returns the raw
// remote bytes.
func TestClientArtifactsProxy(t *testing.T) {
	l, logPath := clientRootWithFakeSSH(t)

	raw, remote, err := RemoteCommand(context.Background(), l, "artifacts", "ls", "run_1")
	if err != nil || !remote {
		t.Fatalf("remote ls: remote=%v err=%v", remote, err)
	}
	var resp struct {
		SchemaVersion int  `json:"schema_version"`
		OK            bool `json:"ok"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil || !resp.OK {
		t.Fatalf("remote ls envelope: %v %s", err, raw)
	}

	raw, remote, err = RemoteGetArtifact(context.Background(), l, "run_1", "build/out.txt")
	if err != nil || !remote {
		t.Fatalf("remote get: remote=%v err=%v", remote, err)
	}
	if string(raw) != "raw-bytes-from-remote" {
		t.Errorf("get bytes: %q", raw)
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
	// No run state was created on the client root.
	entries, _ := os.ReadDir(filepath.Join(l.Root, "state", "runs"))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "run_") {
			t.Errorf("client created run record %s", entry.Name())
		}
	}
}

// TestClientArtifactsProxyPreservesRemoteError pins that a remote
// failure (missing run) is preserved as a Vci error envelope rather
// than relabeled as raw artifact bytes or ssh failure.
func TestClientArtifactsProxyPreservesRemoteError(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	envelope := `{"schema_version":1,"command":"artifacts","ok":false,"data":{},"error":{"code":"not_found","class":"configuration","message":"run not found","retryable":false}}`
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

	raw, remote, err := RemoteGetArtifact(context.Background(), l, "run_1", "build/out.txt")
	if err != nil || !remote {
		t.Fatalf("remote get error path: remote=%v err=%v", remote, err)
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
}
