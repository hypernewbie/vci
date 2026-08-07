package app

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// initCoordinatorMultiMachineRoot writes a coordinator config that
// declares two coordinator-local machines and one project attached
// to both. The command argument is used verbatim so tests can pass a
// blocking command like `sleep 30`.
func initCoordinatorMultiMachineRoot(t *testing.T, fixture *SSHFixture, command string) {
	t.Helper()
	cfg := filepath.Join(fixture.coordinatorRoot, "config.toml")
	body := "schema_version = 1\norchestrator = \"self\"\n\n[log_limits]\nstdout_bytes = 4194304\nstderr_bytes = 4194304\n\n[retention]\nmax_bytes = 536870912\n\n[machines.alpha]\nmax_concurrent = 1\n\n[machines.beta]\nmax_concurrent = 1\n\n[projects.demo]\nmachines = [\"alpha\", \"beta\"]\ncommand = [" + tomlSlice(strings.Split(command, " ")) + "]\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatalf("multi-machine: write coordinator config: %v", err)
	}
}

// TestSchedulerTwoSlotsBothRunDistinctMachines pins the multi-machine
// path: two local slots, two blocking jobs, distinct selected machine
// names; a third submission is rejected with `machine_unavailable`.
// After releasing one, a fourth submission is accepted.
func TestSchedulerTwoSlotsBothRunDistinctMachines(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}
	initCoordinatorMultiMachineRoot(t, fixture, "sleep 30")
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	sourceDir := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "# demo\n")
	mustGitAddCommit(t, sourceDir, "init")

	env1 := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env1.OK {
		t.Fatalf("build 1 not ok: %s", pretty(env1))
	}
	var d1 struct {
		RunID   string `json:"run_id"`
		State   string `json:"state"`
		Machine string `json:"machine"`
	}
	if err := json.Unmarshal(env1.Data, &d1); err != nil {
		t.Fatal(err)
	}
	if !isMultiMachineMember(d1.Machine) {
		t.Fatalf("public machine must be alpha or beta; got %q in %s", d1.Machine, pretty(env1))
	}
	waitState(t, fixture, d1.RunID, "running", 10*time.Second)

	env2 := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env2.OK {
		t.Fatalf("build 2 not ok: %s", pretty(env2))
	}
	var d2 struct {
		RunID   string `json:"run_id"`
		State   string `json:"state"`
		Machine string `json:"machine"`
	}
	if err := json.Unmarshal(env2.Data, &d2); err != nil {
		t.Fatal(err)
	}
	if !isMultiMachineMember(d2.Machine) {
		t.Fatalf("public machine must be alpha or beta; got %q in %s", d2.Machine, pretty(env2))
	}
	waitState(t, fixture, d2.RunID, "running", 10*time.Second)

	m1 := readMachine(t, fixture, d1.RunID)
	m2 := readMachine(t, fixture, d2.RunID)
	if m1 != d1.Machine {
		t.Fatalf("public machine %q must match private run.json machine %q", d1.Machine, m1)
	}
	if m2 != d2.Machine {
		t.Fatalf("public machine %q must match private run.json machine %q", d2.Machine, m2)
	}
	if m1 == m2 {
		t.Fatalf("two slots must run on distinct machines, got both on %q", m1)
	}
	if !isMultiMachineMember(m1) || !isMultiMachineMember(m2) {
		t.Fatalf("machines must be alpha or beta; got %q and %q", m1, m2)
	}

	// 3. Third submission is rejected with machine_unavailable, retryable true.
	env3 := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if env3.OK {
		t.Fatalf("third submission must be rejected: %s", pretty(env3))
	}
	if env3.Error == nil {
		t.Fatalf("third submission must carry an error envelope: %s", pretty(env3))
	}
	if env3.Error.Code != "machine_unavailable" || !env3.Error.Retryable || env3.Error.Class != "state" {
		t.Fatalf("unexpected error: %+v", env3.Error)
	}
	// No third run record or claim is created.
	runsDir := filepath.Join(fixture.coordinatorRoot, "state", "runs")
	if entries, err := os.ReadDir(runsDir); err == nil {
		for _, e := range entries {
			if e.Name() != d1.RunID && e.Name() != d2.RunID {
				t.Fatalf("third submission must not create a run record, found %s", e.Name())
			}
		}
	}

	// 4. Release one job; a fourth submission is accepted.
	abortEnv := runClientBinary(t, fixture, clientRoot, "abort", d1.RunID)
	if !abortEnv.OK {
		t.Fatalf("abort not ok: %s", pretty(abortEnv))
	}
	waitState(t, fixture, d1.RunID, "aborted", 10*time.Second)
	env4 := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env4.OK {
		t.Fatalf("build 4 not ok: %s", pretty(env4))
	}
	var d4 struct {
		RunID   string `json:"run_id"`
		State   string `json:"state"`
		Machine string `json:"machine"`
	}
	if err := json.Unmarshal(env4.Data, &d4); err != nil {
		t.Fatal(err)
	}
	if !isMultiMachineMember(d4.Machine) {
		t.Fatalf("public machine must be alpha or beta; got %q in %s", d4.Machine, pretty(env4))
	}
	waitState(t, fixture, d4.RunID, "running", 10*time.Second)
}

func isMultiMachineMember(name string) bool {
	return name == "alpha" || name == "beta"
}

func waitState(t *testing.T, fixture *SSHFixture, runID, want string, max time.Duration) {
	t.Helper()
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if got := remoteCheckState(t, fixture, runID); got == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("run %s state did not reach %q within %s (last=%q)", runID, want, max, remoteCheckState(t, fixture, runID))
}

func readMachine(t *testing.T, fixture *SSHFixture, runID string) string {
	t.Helper()
	dir := filepath.Join(fixture.coordinatorRoot, "state", "runs", runID)
	data, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		t.Fatalf("read run record: %v", err)
	}
	var rec struct {
		Machine string `json:"machine"`
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("decode run record: %v", err)
	}
	return rec.Machine
}

// TestSchedulerReapPreservesActiveClaims pins that `setup reap`
// during active jobs preserves their claims. The reaper counts
// active bytes and only releases terminal claims.
func TestSchedulerReapPreservesActiveClaims(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}
	initCoordinatorMultiMachineRoot(t, fixture, "sleep 30")
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	sourceDir := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "# demo\n")
	mustGitAddCommit(t, sourceDir, "init")

	env1 := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env1.OK {
		t.Fatalf("build 1 not ok: %s", pretty(env1))
	}
	var d1 struct {
		RunID string `json:"run_id"`
	}
	json.Unmarshal(env1.Data, &d1)
	waitState(t, fixture, d1.RunID, "running", 10*time.Second)

	env2 := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env2.OK {
		t.Fatalf("build 2 not ok: %s", pretty(env2))
	}
	var d2 struct {
		RunID string `json:"run_id"`
	}
	json.Unmarshal(env2.Data, &d2)
	waitState(t, fixture, d2.RunID, "running", 10*time.Second)

	// setup reap on the coordinator. The client root rejects setup
	// mutations by role, so the coordinator's own binary is invoked
	// against the coordinator root.
	reapCmd := exec.Command(fixture.binary, "setup", "reap")
	reapCmd.Env = append(os.Environ(), "VCI_ROOT="+fixture.coordinatorRoot)
	reapOut, reapErr := reapCmd.CombinedOutput()
	if reapErr != nil {
		t.Fatalf("setup reap failed: %v: %s", reapErr, string(reapOut))
	}

	// Both claims must still exist (active runs).
	claimsDir := filepath.Join(fixture.coordinatorRoot, "state", "machine-claims")
	for _, runID := range []string{d1.RunID, d2.RunID} {
		found := false
		if machines, err := os.ReadDir(claimsDir); err == nil {
			for _, m := range machines {
				if _, err := os.Stat(filepath.Join(claimsDir, m.Name(), runID+".json")); err == nil {
					found = true
					break
				}
			}
		}
		if !found {
			t.Fatalf("reap removed live claim for %s", runID)
		}
	}
}
