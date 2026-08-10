package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/scheduler"
	"github.com/hypernewbie/vci/internal/store"
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

// TestSchedulerTwoSlotsBothRunDistinctMachines pins the fan-out dispatch
// contract on two capacity-one machines: every submission stages one child
// per attached machine, a busy machine's child stays queued while its
// sibling runs, and freeing the slot lets the next dispatch pass launch the
// queued child. No submission is rejected.
func TestSchedulerTwoSlotsBothRunDistinctMachines(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
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

	// First submission: both machines are free, so both children run on
	// distinct machines.
	env1 := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env1.OK {
		t.Fatalf("build 1 not ok: %s", pretty(env1))
	}
	p1 := decodeSubmission(t, env1)
	assertTargetsCover(t, p1, "alpha", "beta")
	waitTarget(t, fixture, p1.RunID, "alpha", "running", 15*time.Second)
	waitTarget(t, fixture, p1.RunID, "beta", "running", 15*time.Second)

	// The coordinator runs one build at a time; finish build 1 so build 2 can
	// be admitted.
	abortEnv := runClientBinary(t, fixture, clientRoot, "abort", p1.RunID)
	if !abortEnv.OK {
		t.Fatalf("abort build 1 not ok: %s", pretty(abortEnv))
	}
	waitTarget(t, fixture, p1.RunID, "alpha", "aborted", 15*time.Second)
	waitTarget(t, fixture, p1.RunID, "beta", "aborted", 15*time.Second)

	// Second submission with beta's slot occupied: the beta child must
	// stay queued while the alpha child runs.
	legacyID := holdMachineSlot(t, fixture, "beta")
	env2 := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env2.OK {
		t.Fatalf("build 2 must not be rejected for a busy target; got %s", pretty(env2))
	}
	p2 := decodeSubmission(t, env2)
	assertTargetsCover(t, p2, "alpha", "beta")
	waitTarget(t, fixture, p2.RunID, "alpha", "running", 15*time.Second)
	states := remoteTargets(t, fixture, p2.RunID)
	if states["beta"].State != "queued" {
		t.Fatalf("busy beta target must stay queued while alpha runs; got %v", states)
	}
	if states["alpha"].State != "running" {
		t.Fatalf("alpha target must run while beta is busy; got %v", states)
	}

	// Free the slot; the next dispatch pass (via setup reap) launches the
	// queued child on the freed machine.
	l := model.Layout{Root: fixture.coordinatorRoot}
	if err := scheduler.Release(l, "beta", legacyID); err != nil {
		t.Fatalf("release beta slot: %v", err)
	}
	reapCmd := exec.Command(fixture.binary, "setup", "reap")
	reapCmd.Env = append(os.Environ(), "VCI_ROOT="+fixture.coordinatorRoot)
	if out, err := reapCmd.CombinedOutput(); err != nil {
		t.Fatalf("setup reap failed: %v: %s", err, out)
	}
	waitTarget(t, fixture, p2.RunID, "beta", "running", 15*time.Second)
}

// waitTarget polls a build request's target on machine until its durable
// state reaches want.
func waitTarget(t *testing.T, fixture *SSHFixture, parentID, machine, want string, max time.Duration) {
	t.Helper()
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		if remoteTargets(t, fixture, parentID)[machine].State == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("target %s of run %s did not reach %q within %s (last=%v)", machine, parentID, want, max, remoteTargets(t, fixture, parentID))
}

// holdMachineSlot creates a legacy single-machine run on the coordinator and
// reserves a scheduler slot for it, occupying the machine's capacity so a
// fan-out build's child on that machine queues instead of running. The lease
// keeps the reaper from treating the staging run as abandoned.
func holdMachineSlot(t *testing.T, fixture *SSHFixture, machine string) model.RunID {
	t.Helper()
	l := model.Layout{Root: fixture.coordinatorRoot}
	runStore := store.Store{Layout: l}
	cfg, err := config.Load(l.ConfigPath())
	if err != nil {
		t.Fatalf("load coordinator config: %v", err)
	}
	now := time.Now().UTC()
	id, err := store.NewRunID(now)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := store.NewRunFromID(id, "demo", machine, []string{"sleep", "30"}, "", map[string]any{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := runStore.Save(legacy); err != nil {
		t.Fatalf("save legacy run: %v", err)
	}
	if _, err := runStore.Transition(id, model.RunStaging, now); err != nil {
		t.Fatalf("promote legacy run: %v", err)
	}
	if err := scheduler.Reserve(l, runStore, cfg, machine, id, now); err != nil {
		t.Fatalf("reserve %s for legacy run: %v", machine, err)
	}
	if err := store.Claim(l, id, "scheduler-integration-test", now, 30*time.Minute); err != nil {
		t.Fatalf("lease legacy run: %v", err)
	}
	return id
}

// TestSchedulerReapPreservesActiveClaims pins that `setup reap`
// during an active fan-out build preserves every target's scheduler
// claim. The reaper only releases claims whose run record is terminal.
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

	env := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env.OK {
		t.Fatalf("build not ok: %s", pretty(env))
	}
	parent := decodeSubmission(t, env)
	assertTargetsCover(t, parent, "alpha", "beta")
	waitTarget(t, fixture, parent.RunID, "alpha", "running", 15*time.Second)
	waitTarget(t, fixture, parent.RunID, "beta", "running", 15*time.Second)

	// setup reap on the coordinator. The client root rejects setup
	// mutations by role, so the coordinator's own binary is invoked
	// against the coordinator root.
	reapCmd := exec.Command(fixture.binary, "setup", "reap")
	reapCmd.Env = append(os.Environ(), "VCI_ROOT="+fixture.coordinatorRoot)
	if reapOut, reapErr := reapCmd.CombinedOutput(); reapErr != nil {
		t.Fatalf("setup reap failed: %v: %s", reapErr, string(reapOut))
	}

	// Every live target's claim must still exist after the reap cycle.
	claimsDir := filepath.Join(fixture.coordinatorRoot, "state", "machine-claims")
	for _, target := range remoteTargets(t, fixture, parent.RunID) {
		claimPath := filepath.Join(claimsDir, target.Machine, target.ID+".json")
		if _, err := os.Stat(claimPath); err != nil {
			t.Fatalf("reap removed live claim for %s (%s): %v", target.Machine, target.ID, err)
		}
	}
	// The reap cycle must not disturb the running targets.
	for _, machine := range []string{"alpha", "beta"} {
		if got := remoteTargets(t, fixture, parent.RunID)[machine].State; got != "running" {
			t.Fatalf("reap disturbed target %s (state=%s)", machine, got)
		}
	}
}
