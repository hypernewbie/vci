package app

// Client/coordinator integration tests.
//
// Each test starts a loopback sshd, configures client and
// coordinator roots, then runs the client binary end-to-end over
// system SSH and asserts the single Vci JSON envelope on stdout.

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// phase1Envelope is the minimal Vci response shape used by every integration assertion.
type phase1Envelope struct {
	SchemaVersion int             `json:"schema_version"`
	Command       string          `json:"command"`
	OK            bool            `json:"ok"`
	Data          json.RawMessage `json:"data"`
	Error         *struct {
		Code      string `json:"code"`
		Class     string `json:"class"`
		Message   string `json:"message"`
		Retryable bool   `json:"retryable"`
	} `json:"error"`
}

const modelSchemaVersion = 1

func runClientBinary(t *testing.T, fixture *SSHFixture, clientRoot string, args ...string) phase1Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, fixture.binary, args...)
	cmd.Env = append(os.Environ(),
		"HOME="+fixture.homeDir,
		"VCI_ROOT="+clientRoot,
	)
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err != nil && stdout.Len() == 0 {
		t.Logf("client run stderr:\n%s", stderr)
		t.Fatalf("client binary %v exited with %v", args, err)
	}
	var env phase1Envelope
	if jerr := json.Unmarshal([]byte(stdout.String()), &env); jerr != nil {
		t.Logf("client run stderr:\n%s", stderr)
		t.Fatalf("client did not return one Vci JSON document for %v: %v\nstdout:\n%s", args, jerr, stdout)
	}
	return env
}

func initCoordinatorRoot(t *testing.T, fixture *SSHFixture, command string, extraArgs ...string) {
	t.Helper()
	initCoordinatorRootWithCapacity(t, fixture, 0, command, extraArgs...)
}

// initCoordinatorRootWithCapacity writes a coordinator config with
// the requested max_concurrent; non-positive omits it (default one slot).
func initCoordinatorRootWithCapacity(t *testing.T, fixture *SSHFixture, capacity int, command string, extraArgs ...string) {
	t.Helper()
	cfg := filepath.Join(fixture.coordinatorRoot, "config.toml")
	allArgs := append([]string{command}, extraArgs...)
	capLine := ""
	if capacity > 0 {
		capLine = "max_concurrent = " + strconv.Itoa(capacity) + "\n"
	}
	body := "schema_version = 1\norchestrator = \"self\"\n\n[log_limits]\nstdout_bytes = 4194304\nstderr_bytes = 4194304\n\n[retention]\nmax_bytes = 536870912\n\n[machines.mac-local]\n" + capLine + "\n[projects.demo]\nmachines = [\"mac-local\"]\ncommand = [" + tomlSlice(allArgs) + "]\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatalf("phase1: write coordinator config: %v", err)
	}
}

// tomlSlice returns the TOML inline-array literal for a command and argument list.
func tomlSlice(args []string) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, tomlQuote(a))
	}
	return strings.Join(parts, ", ")
}

func tomlQuote(s string) string {
	escape := strings.NewReplacer(`\`, `\\`, `"`, `\"`).
		Replace(s)
	return `"` + escape + `"`
}

func initClientRoot(t *testing.T, fixture *SSHFixture, alias string) string {
	t.Helper()
	base := fixture.t.TempDir()
	root := filepath.Join(base, "vci")
	if err := os.MkdirAll(filepath.Join(root, "state", "tmp"), 0o700); err != nil {
		t.Fatalf("phase1: mkdir client state: %v", err)
	}
	cfg := filepath.Join(root, "config.toml")
	body := "schema_version = 1\norchestrator = \"" + alias + "\"\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatalf("phase1: write client config: %v", err)
	}
	return root
}

// TestClientMachinesProxiesToCoordinator asserts `vci machines` returns the coordinator's inventory.
func TestClientMachinesProxiesToCoordinator(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("phase1 ssh roundtrip: %v", err)
	}
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	env := runClientBinary(t, fixture, clientRoot, "machines")
	if !env.OK {
		t.Fatalf("machines not ok: %s", pretty(env))
	}
	var machines []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(env.Data, &machines); err != nil {
		t.Fatalf("machines data decode: %v", err)
	}
	if len(machines) != 1 || machines[0].Name != "mac-local" {
		t.Fatalf("expected [mac-local] from coordinator; got %s", pretty(env))
	}
}

// TestClientProjectsProxiesToCoordinator asserts `vci projects` returns the coordinator's project list.
func TestClientProjectsProxiesToCoordinator(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("phase1 ssh roundtrip: %v", err)
	}
	initCoordinatorRoot(t, fixture, "echo")
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	env := runClientBinary(t, fixture, clientRoot, "projects")
	if !env.OK {
		t.Fatalf("projects not ok: %s", pretty(env))
	}
	var projects []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(env.Data, &projects); err != nil {
		t.Fatalf("projects data decode: %v", err)
	}
	if len(projects) != 1 || projects[0].Name != "demo" {
		t.Fatalf("expected [demo] from coordinator; got %s", pretty(env))
	}
}

// TestClientBuildRunsRemoteCommand verifies a client build triggers the remote worker.
func TestClientBuildRunsRemoteCommand(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("phase1 ssh roundtrip: %v", err)
	}

	// Coordinator command: write a stamp file in the workspace and exit 0.
	initCoordinatorRoot(t, fixture, "sh", "-c", "echo hello-stamp > stamp.txt; exit 0")
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	// Materialize a source tree named `demo` to match the coordinator's project.
	sourceParent := fixture.t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source dir: %v", err)
	}
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "# demo\n")
	// Make the source a git repo for the remote's git-rev-parse.
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "# demo\n")
	mustGitAddCommit(t, sourceDir, "demo commit")

	env := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env.OK {
		t.Fatalf("build not ok: %s", pretty(env))
	}
	var data struct {
		RunID string `json:"run_id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("build data decode: %v", err)
	}
	if !strings.HasPrefix(data.RunID, "run_") {
		t.Fatalf("client returned non-run id %q in %s", data.RunID, pretty(env))
	}

	// Wait for the remote worker to publish its run record.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("client got a run id %q but no remote run record appeared in 20s", data.RunID)
		}
		remoteState := remoteCheckState(t, fixture, data.RunID)
		if remoteState == "succeeded" {
			break
		}
		if remoteState == "failed" {
			t.Fatalf("remote run failed: %s", data.RunID)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Confirm the client never stored a local run record.
	if _, err := os.Stat(filepath.Join(clientRoot, "state", "runs")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(clientRoot, "state", "runs"))
		if len(entries) > 0 {
			t.Fatalf("client must not carry remote run records; found %d entries", len(entries))
		}
	}
}

// TestClientJobFailureStaysJobFailure asserts a non-zero command stays a job failure, not infrastructure.
func TestClientJobFailureStaysJobFailure(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("phase1 ssh roundtrip: %v", err)
	}
	initCoordinatorRoot(t, fixture, "sh", "-c", "exit 7")
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	sourceParent := fixture.t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source dir: %v", err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "x")
	mustGitAddCommit(t, sourceDir, "x")

	env := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env.OK {
		t.Fatalf("phase1 build must register run; got %s", pretty(env))
	}
	var data struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("build data decode: %v", err)
	}
	// Wait for the worker to publish a final result.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("worker did not publish run record in 20s")
		}
		state := remoteCheckState(t, fixture, data.RunID)
		if state == "failed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// The client `check` must report the coordinator's failed run, not an infrastructure error.
	check := runClientBinary(t, fixture, clientRoot, "check", data.RunID)
	if !check.OK {
		t.Fatalf("check must return the failed remote run: %s", pretty(check))
	}
	var checked struct {
		State   string `json:"state"`
		Failure string `json:"failure"`
	}
	if err := json.Unmarshal(check.Data, &checked); err != nil {
		t.Fatalf("decode check data: %v", err)
	}
	if checked.State != "failed" || checked.Failure != "job" {
		t.Fatalf("expected remote job failure, got %s", pretty(check))
	}
}

// TestClientUnreachableAliasPropagatesAsInfrastructure asserts an unreachable alias is classified as infrastructure.
func TestClientUnreachableAliasPropagatesAsInfrastructure(t *testing.T) {
	fixture := NewSSHFixture(t)
	clientRoot := initClientRoot(t, fixture, "unreachable-alias")

	env := runClientBinary(t, fixture, clientRoot, "projects")
	if env.OK {
		t.Fatalf("expected failure for unreachable alias, got ok: %s", pretty(env))
	}
	if env.Error == nil || env.Error.Class != "infrastructure" {
		t.Fatalf("expected infrastructure error class, got: %s", pretty(env))
	}
}

func firstJSON(buf []byte) int {
	for i, b := range buf {
		if b == '{' {
			return i
		}
	}
	return len(buf)
}

// env is the minimal Vci envelope shape used by helpers.
type env struct {
	SchemaVersion int    `json:"schema_version"`
	Command       string `json:"command"`
	OK            bool   `json:"ok"`
}

// envOK returns true if the parsed envelope reports OK.
func envOK(data []byte) bool {
	var e env
	_ = json.Unmarshal(data, &e)
	return e.OK
}

// envClass returns the envelope's failure class, or "ok" when healthy.
func envClass(data []byte) string {
	var e phase1Envelope
	if json.Unmarshal(data, &e) != nil {
		return ""
	}
	if e.OK {
		return "ok"
	}
	if e.Error == nil {
		return ""
	}
	return e.Error.Class
}

// TestClientMalformedResponseClassifiedAsInfrastructure asserts a malformed coordinator response is classified as infrastructure.
func TestClientMalformedResponseClassifiedAsInfrastructure(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("phase1 ssh roundtrip: %v", err)
	}
	initCoordinatorRoot(t, fixture, "true")
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	remoteVciPath := filepath.Join(fixture.homeDir, "bin", "vci")
	originalBinary, err := os.ReadFile(remoteVciPath)
	if err != nil {
		t.Fatalf("read original vci binary: %v", err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(remoteVciPath, originalBinary, 0o755)
	})

	for _, response := range []string{
		"NOT VALID JSON",
		`{"schema_version":1,"ok":true}`,
		`{"schema_version":1,"command":"build","ok":true,"data":{}}`,
	} {
		t.Run(response, func(t *testing.T) {
			fakeScript := "#!/bin/sh\nprintf '%s\\n' " + shellQuote(response) + "\nexit 0\n"
			if err := os.WriteFile(remoteVciPath, []byte(fakeScript), 0o755); err != nil {
				t.Fatalf("write fake vci script: %v", err)
			}
			env := runClientBinary(t, fixture, clientRoot, "projects")
			if env.OK {
				t.Fatalf("expected failure for malformed response, got ok: %s", pretty(env))
			}
			if env.Error == nil || env.Error.Class != "infrastructure" {
				t.Fatalf("expected infrastructure error class, got: %s", pretty(env))
			}
		})
	}
}

// TestClientAbortPropagatesRequest asserts a client abort transitions the coordinator run to aborted.
func TestClientAbortPropagatesRequest(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("phase1 ssh roundtrip: %v", err)
	}
	initCoordinatorRoot(t, fixture, "sleep", "30")
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	sourceParent := fixture.t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source dir: %v", err)
	}
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "# demo\n")
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "# demo\n")
	mustGitAddCommit(t, sourceDir, "demo commit")

	env := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env.OK {
		t.Fatalf("build not ok: %s", pretty(env))
	}
	var data struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("build data decode: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not reach running/staging state in 10s", data.RunID)
		}
		st := remoteCheckState(t, fixture, data.RunID)
		if st == "running" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	abortEnv := runClientBinary(t, fixture, clientRoot, "abort", data.RunID)
	if !abortEnv.OK {
		t.Fatalf("abort not ok: %s", pretty(abortEnv))
	}

	abortDeadline := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(abortDeadline) {
			t.Fatalf("run %s state was not aborted in 10s (got %q)", data.RunID, remoteCheckState(t, fixture, data.RunID))
		}
		st := remoteCheckState(t, fixture, data.RunID)
		if st == "aborted" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// remoteCheckState returns the coordinator run state for runID, or "" if absent.
func remoteCheckState(t *testing.T, fixture *SSHFixture, runID string) string {
	t.Helper()
	dir := filepath.Join(fixture.coordinatorRoot, "state", "runs", runID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	if len(entries) == 0 {
		return ""
	}
	recordPath := filepath.Join(dir, "run.json")
	data, err := os.ReadFile(recordPath)
	if err != nil {
		return ""
	}
	var rec struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return ""
	}
	return rec.State
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustGitInit(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "vci-fixture@example.com"},
		{"config", "user.name", "vci-fixture"},
		{"config", "commit.gpgsign", "false"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("phase1 git %v: %v\n%s", args, err, out)
		}
	}
}

func mustGitAddCommit(t *testing.T, dir, message string) {
	t.Helper()
	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "-q", "-m", message},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("phase1 git %v: %v\n%s", args, err, out)
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("phase1 read %s: %v", path, err)
	}
	return data
}

func pretty(env phase1Envelope) string {
	out, _ := json.Marshal(env)
	return string(out)
}
