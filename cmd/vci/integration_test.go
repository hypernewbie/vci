package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

type cliEnvelope struct {
	SchemaVersion int  `json:"schema_version"`
	OK            bool `json:"ok"`
	Data          any  `json:"data"`
}

func buildBinary(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	bin := filepath.Join(t.TempDir(), "vci")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/vci")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	return bin
}
func fixture(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture/"+name+"\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "init", "-q", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	return dir
}
func invokeRaw(bin, root string, args ...string) (cliEnvelope, error, string) {
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "VCI_ROOT="+root)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	runErr := cmd.Run()
	var response cliEnvelope
	decodeErr := json.Unmarshal(out.Bytes(), &response)
	if decodeErr != nil {
		return response, fmt.Errorf("decode response: %w", decodeErr), errOut.String()
	}
	return response, runErr, errOut.String()
}

func objectData(t *testing.T, response cliEnvelope) map[string]any {
	t.Helper()
	data, ok := response.Data.(map[string]any)
	if !ok {
		t.Fatalf("object data: %#v", response.Data)
	}
	return data
}

func invoke(t *testing.T, bin, root string, args ...string) cliEnvelope {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "VCI_ROOT="+root)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %v stdout=%s stderr=%s", args, err, out.String(), errOut.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("stderr for %v: %q", args, errOut.String())
	}
	var response cliEnvelope
	dec := json.NewDecoder(&out)
	if err := dec.Decode(&response); err != nil {
		t.Fatalf("response %v: %v (%q)", args, err, out.String())
	}
	var extra any
	if err := dec.Decode(&extra); err == nil {
		t.Fatalf("multiple responses: %q", out.String())
	}
	if response.SchemaVersion != 1 || !response.OK {
		t.Fatalf("response %v: %+v", args, response)
	}
	return response
}

func TestConcurrentCompiledConfigMutations(t *testing.T) {
	bin := buildBinary(t)
	root := filepath.Join(t.TempDir(), ".vci")
	invoke(t, bin, root, "setup", "init")
	results := make(chan cliEnvelope, 2)
	for _, name := range []string{"machine-a", "machine-b"} {
		go func(name string) {
			response, runErr, stderr := invokeRaw(bin, root, "setup", "machine", "add", name)
			if runErr != nil || stderr != "" {
				t.Errorf("machine %s: %v stderr=%q response=%+v", name, runErr, stderr, response)
			}
			results <- response
		}(name)
	}
	for i := 0; i < 2; i++ {
		response := <-results
		if !response.OK {
			t.Fatalf("machine response: %+v", response)
		}
	}
	for _, item := range []struct{ name, machine string }{{"project-a", "machine-a"}, {"project-b", "machine-b"}} {
		go func(item struct{ name, machine string }) {
			response, runErr, stderr := invokeRaw(bin, root, "setup", "project", "add", item.name, "--machine", item.machine, "--command", "true")
			if runErr != nil || stderr != "" {
				t.Errorf("project %s: %v stderr=%q response=%+v", item.name, runErr, stderr, response)
			}
			results <- response
		}(item)
	}
	for i := 0; i < 2; i++ {
		response := <-results
		if !response.OK {
			t.Fatalf("project response: %+v", response)
		}
	}
	machines := invoke(t, bin, root, "machines")
	if list, ok := machines.Data.([]any); !ok || len(list) == 0 {
		t.Fatalf("machines: %+v", machines)
	}
	projects := invoke(t, bin, root, "projects")
	if list, ok := projects.Data.([]any); !ok || len(list) == 0 {
		t.Fatalf("projects: %+v", projects)
	}
}

func TestCompiledBinaryContractsAndAbort(t *testing.T) {
	bin := buildBinary(t)
	root := filepath.Join(t.TempDir(), ".vci")
	repo := fixture(t, "cli-fixture")
	invoke(t, bin, root, "setup", "init")
	invoke(t, bin, root, "setup", "machine", "add", "local")
	invoke(t, bin, root, "setup", "project", "add", "cli-fixture", "--machine", "local", "--command", "go", "--arg", "test", "--arg", "./...")
	buildResponse := invoke(t, bin, root, "build", repo)
	id, ok := objectData(t, buildResponse)["run_id"].(string)
	if !ok || id == "" {
		t.Fatalf("build response: %+v", buildResponse)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		response := invoke(t, bin, root, "check", id)
		if state, _ := objectData(t, response)["state"].(string); state == "succeeded" {
			waitWorkerSettled(t, root, id)
			goto abortCase
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("build did not succeed")
abortCase:
	abortRepo := fixture(t, "abort-fixture")
	invoke(t, bin, root, "setup", "project", "add", "abort-fixture", "--machine", "local", "--command", "sh", "--arg", "-c", "--arg", "sleep 30")
	buildResponse = invoke(t, bin, root, "build", abortRepo)
	id, ok = objectData(t, buildResponse)["run_id"].(string)
	if !ok {
		t.Fatal("abort build id")
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		response := invoke(t, bin, root, "check", id)
		if state, _ := objectData(t, response)["state"].(string); state == "running" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	invoke(t, bin, root, "abort", id)
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		response := invoke(t, bin, root, "check", id)
		if state, _ := objectData(t, response)["state"].(string); state == "aborted" {
			waitWorkerSettled(t, root, id)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(fmt.Sprintf("abort did not converge for %s", id))
}

// waitWorkerSettled waits (bounded) for the detached worker of a
// terminal run to release its owned state. The CLI publishes the
// terminal record before the worker finishes removing its workspace,
// releasing its lease, and releasing its scheduler claim; a test that
// returns at the terminal state would race the framework's TempDir
// cleanup against a still-writing worker and leave a
// `.../state: Directory not empty` turd under VCI_ROOT. Each predicate
// is a path the worker removes itself, so once all three hold no worker
// write can still land under state/. (state/locks/scheduler.lock and
// state/runs/<run>/run.lock are persistent by design and are not
// predicates; the scheduler claim removal is the worker's last write.)
func waitWorkerSettled(t *testing.T, root, id string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		workGone := !pathExists(filepath.Join(root, "state", "work", id))
		leaseGone := !pathExists(filepath.Join(root, "state", "runs", id, "lease.json"))
		claimGone := !runClaimExists(root, id)
		if workGone && leaseGone && claimGone {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("worker state did not settle for %s", id)
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// runClaimExists reports whether any scheduler claim file for the run
// id remains under state/machine-claims/<machine>/.
func runClaimExists(root, id string) bool {
	claims := filepath.Join(root, "state", "machine-claims")
	entries, err := os.ReadDir(claims)
	if err != nil {
		return false
	}
	for _, machineDir := range entries {
		if !machineDir.IsDir() {
			continue
		}
		if pathExists(filepath.Join(claims, machineDir.Name(), id+".json")) {
			return true
		}
	}
	return false
}

func TestCompiledBinaryUnknownCommandIsJSON(t *testing.T) {
	bin := buildBinary(t)
	root := filepath.Join(t.TempDir(), ".vci")
	cmd := exec.Command(bin, "unknown")
	cmd.Env = append(os.Environ(), "VCI_ROOT="+root)
	var out bytes.Buffer
	cmd.Stdout = &out
	_ = cmd.Run()
	var response map[string]any
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatalf("not JSON: %q", out.String())
	}
	if response["schema_version"] != float64(1) {
		t.Fatalf("response: %v", response)
	}
}
