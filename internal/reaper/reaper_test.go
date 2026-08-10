package reaper

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/scheduler"
	"github.com/hypernewbie/vci/internal/store"
)

func TestExpiredOwnedRunBecomesLostAndPartialIsReaped(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run, err := store.NewRun("p", "m", []string{"true"}, "source", map[string]any{}, now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	s := store.Store{Layout: l}
	if err := s.Save(run); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(run.ID, model.RunStaging, now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.Claim(l, run.ID, "worker-expired", now.Add(-2*time.Hour), time.Minute); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(l.WorkDir(), string(run.ID)+".partial")
	if err := os.MkdirAll(partial, 0o700); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-2 * time.Hour)
	_ = os.Chtimes(partial, old, old)
	report, err := Run(l, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.MarkedLost != 1 || report.Removed != 1 {
		t.Fatalf("report: %+v", report)
	}
	loaded, _ := s.Load(run.ID)
	if loaded.State != model.RunLost {
		t.Fatalf("state: %s", loaded.State)
	}
}

func TestActiveLeaseProtectsOldPartialWorkspace(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run, err := store.NewRun("p", "m", []string{"true"}, "source", map[string]any{}, now)
	if err != nil {
		t.Fatal(err)
	}
	s := store.Store{Layout: l}
	if err := s.Save(run); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(run.ID, model.RunStaging, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Claim(l, run.ID, "worker-active", now, time.Hour); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(l.WorkDir(), string(run.ID)+".partial")
	if err := os.MkdirAll(partial, 0o700); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-2 * time.Hour)
	_ = os.Chtimes(partial, old, old)
	report, err := Run(l, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Removed != 0 {
		t.Fatalf("report: %+v", report)
	}
	if _, err := os.Stat(partial); err != nil {
		t.Fatalf("partial removed: %v", err)
	}
}

func TestReapsOldPartialWorkspace(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(l.WorkDir(), "run_old.partial")
	if err := os.MkdirAll(partial, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(partial, old, old); err != nil {
		t.Fatal(err)
	}
	report, err := Run(l, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if report.Removed != 1 {
		t.Fatalf("report: %+v", report)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf("partial remains: %v", err)
	}
}

// writeReservation is the test-only helper that writes a valid
// scheduler claim file directly. The reservation mirrors what the
// production Reserve call writes: schema 1, a machine name, a run id,
// and a UTC created_at. The reaper must treat the claim as valid
// without going through the production Reserve path.
func writeReservation(t *testing.T, l model.Layout, machine, runID string, createdAt time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(l.MachineClaimsDir(), machine), 0o700); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"schema_version": scheduler.ClaimSchemaVersion,
		"machine":        machine,
		"run_id":         runID,
		"created_at":     createdAt.UTC().Format(time.RFC3339Nano),
	})
	target := l.MachineClaimPath(machine, model.RunID(runID))
	if err := os.WriteFile(target, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// stagingRunRecord creates a staging RunRecord with a reservation but
// no lease. It returns the run id. The reaper must consult the
// reservation's created_at against the documented pre-start grace and
// either retain or terminalize the record accordingly.
func stagingRunRecord(t *testing.T, l model.Layout, machine, runID string, queuedAt time.Time) {
	t.Helper()
	record, err := store.NewRun("p", machine, []string{"true"}, "source", map[string]any{}, queuedAt)
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite the run id so the test owns the choice.
	record.ID = model.RunID(runID)
	runStore := store.Store{Layout: l}
	if err := runStore.Save(record); err != nil {
		t.Fatal(err)
	}
	if _, err := runStore.Transition(record.ID, model.RunStaging, queuedAt); err != nil {
		t.Fatal(err)
	}
}

// TestReaperRetainsStagingWithinPreStartGrace pins that a staging run
// with a valid reservation younger than the pre-start grace is
// retained. The detached worker may still be racing to publish its
// lease; the reaper must not steal the slot.
func TestReaperRetainsStagingWithinPreStartGrace(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	writeReservation(t, l, "alpha", "run_young", now.Add(-30*time.Second))
	stagingRunRecord(t, l, "alpha", "run_young", now.Add(-30*time.Second))

	report, err := Run(l, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.MarkedLost != 0 {
		t.Fatalf("within-grace staging must not be lost: %+v", report)
	}
	runStore := store.Store{Layout: l}
	loaded, _ := runStore.Load("run_young")
	if loaded.State != model.RunStaging {
		t.Fatalf("state went to %s during grace", loaded.State)
	}
	if _, err := os.Stat(l.MachineClaimPath("alpha", model.RunID("run_young"))); err != nil {
		t.Fatalf("valid reservation must remain: %v", err)
	}
}

// TestReaperMarksStagingLostAfterPreStartGrace pins that a staging
// run with a valid reservation older than the pre-start grace is
// terminalized as lost and its claim is released.
func TestReaperMarksStagingLostAfterPreStartGrace(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// Use an "old" creation time well past the pre-start grace.
	old := now.Add(-2 * time.Hour)
	writeReservation(t, l, "alpha", "run_stale", old)
	stagingRunRecord(t, l, "alpha", "run_stale", old)

	report, err := Run(l, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.MarkedLost != 1 {
		t.Fatalf("post-grace staging must be lost: %+v", report)
	}
	if report.SchedulerClaimsReleased < 1 {
		t.Fatalf("post-grace claim must be reaped: %+v", report)
	}
	runStore := store.Store{Layout: l}
	loaded, _ := runStore.Load("run_stale")
	if loaded.State != model.RunLost {
		t.Fatalf("state must be lost; got %s", loaded.State)
	}
	if _, err := os.Stat(l.MachineClaimPath("alpha", model.RunID("run_stale"))); !os.IsNotExist(err) {
		t.Fatalf("post-grace claim must be reaped: %v", err)
	}
}

// TestReaperStagingWithActiveLeaseNotGovernedByClaimAge pins that an
// active lease overrides claim age. The reaper never uses claim age
// to kill a run that has acquired its worker lease.
func TestReaperStagingWithActiveLeaseNotGovernedByClaimAge(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// Claim reservation is old (well past pre-start grace), but the
	// worker's lease was taken with a fresh `now` and a long TTL, so
	// it is still well inside the renewal window.
	oldReservation := now.Add(-3 * time.Hour)
	writeReservation(t, l, "alpha", "run_lease", oldReservation)
	stagingRunRecord(t, l, "alpha", "run_lease", oldReservation)
	if err := store.Claim(l, "run_lease", "worker-active", now, time.Hour); err != nil {
		t.Fatal(err)
	}
	report, err := Run(l, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.MarkedLost != 0 {
		t.Fatalf("active lease must override claim age: %+v", report)
	}
	runStore := store.Store{Layout: l}
	loaded, _ := runStore.Load("run_lease")
	if loaded.State != model.RunStaging {
		t.Fatalf("active-lease staging must remain; got %s", loaded.State)
	}
	if _, err := os.Stat(l.MachineClaimPath("alpha", model.RunID("run_lease"))); err != nil {
		t.Fatalf("active-lease claim must remain: %v", err)
	}
}

// TestReaperLegacyQueuedNoLeaseBecomesAborted pins that an old queued
// record with no lease is terminalized as aborted during maintenance,
// counted separately from worker losses. Legacy queued records cannot
// survive indefinitely because the current public path only spawns
// after staging.
func TestReaperLegacyQueuedNoLeaseBecomesAborted(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run, err := store.NewRun("p", "alpha", []string{"true"}, "source", map[string]any{}, now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	runStore := store.Store{Layout: l}
	if err := runStore.Save(run); err != nil {
		t.Fatal(err)
	}
	// No Transition: state stays queued.
	report, err := Run(l, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.MarkedLost != 0 {
		t.Fatalf("queued must not be classified as a worker loss: %+v", report)
	}
	if report.QueuedAborted != 1 {
		t.Fatalf("queued must be aborted: %+v", report)
	}
	loaded, _ := runStore.Load(run.ID)
	if loaded.State != model.RunAborted {
		t.Fatalf("state must be aborted; got %s", loaded.State)
	}
}

// Plan 16 Phase 2 reaper-owned artifact retention tests. ReapArtifacts
// removes state/runs/<run>/artifacts of lost/aborted runs older than
// transferStaleAge and keeps succeeded/failed (and recent terminal)
// artifacts until the run itself is reaped by the existing retention.

// writeArtifactsRun persists a run record in the given state with a
// controlled UpdatedAt and writes one artifact (build/out.bin) into
// its artifacts dir with a matching mtime.
func writeArtifactsRun(t *testing.T, l model.Layout, id string, state model.RunState, at time.Time) {
	t.Helper()
	record, err := store.NewRun("p", "m", []string{"true"}, "source", map[string]any{}, at)
	if err != nil {
		t.Fatal(err)
	}
	record.ID = model.RunID(id)
	record.State = state
	record.UpdatedAt = at.UTC()
	if err := (store.Store{Layout: l}).Save(record); err != nil {
		t.Fatal(err)
	}
	artRoot := filepath.Join(l.RunsDir(), id, "artifacts")
	artDir := filepath.Join(artRoot, "build")
	if err := os.MkdirAll(artDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artDir, "out.bin"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(artRoot, at, at)
}

// TestReapArtifactsRemovesStaleLostRunArtifacts pins that artifacts of
// a lost run older than transferStaleAge are removed wholesale while
// the run record itself is untouched.
func TestReapArtifactsRemovesStaleLostRunArtifacts(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	old := now.Add(-2 * time.Hour)
	writeArtifactsRun(t, l, "run_lost", model.RunLost, old)

	reaped, err := ReapArtifacts(l, now)
	if err != nil {
		t.Fatal(err)
	}
	if reaped != 1 {
		t.Fatalf("reaped %d want 1", reaped)
	}
	if _, err := os.Stat(filepath.Join(l.RunsDir(), "run_lost", "artifacts")); !os.IsNotExist(err) {
		t.Fatalf("artifacts dir remains: %v", err)
	}
	if _, err := (store.Store{Layout: l}).Load("run_lost"); err != nil {
		t.Fatalf("run record removed: %v", err)
	}
}

// TestReapArtifactsKeepsRecentSucceeded pins that a recent succeeded
// run keeps its artifacts: only stale lost/aborted runs are reaped.
func TestReapArtifactsKeepsRecentSucceeded(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	writeArtifactsRun(t, l, "run_ok", model.RunSucceeded, now.Add(-time.Minute))

	reaped, err := ReapArtifacts(l, now)
	if err != nil {
		t.Fatal(err)
	}
	if reaped != 0 {
		t.Fatalf("reaped %d want 0", reaped)
	}
	if _, err := os.Stat(filepath.Join(l.RunsDir(), "run_ok", "artifacts", "build", "out.bin")); err != nil {
		t.Fatalf("succeeded artifacts removed: %v", err)
	}
}

// TestReapArtifactsKeepsRecentLostAndReapsStaleAborted pins both sides
// of the age gate: a recently terminalized lost run is retained (a
// terminalizing worker can still be mid-cleanup), while a stale
// aborted run is reaped.
func TestReapArtifactsKeepsRecentLostAndReapsStaleAborted(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	writeArtifactsRun(t, l, "run_lost_recent", model.RunLost, now.Add(-time.Minute))
	writeArtifactsRun(t, l, "run_aborted_old", model.RunAborted, now.Add(-2*time.Hour))

	reaped, err := ReapArtifacts(l, now)
	if err != nil {
		t.Fatal(err)
	}
	if reaped != 1 {
		t.Fatalf("reaped %d want 1", reaped)
	}
	if _, err := os.Stat(filepath.Join(l.RunsDir(), "run_lost_recent", "artifacts")); err != nil {
		t.Fatalf("recent lost artifacts removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.RunsDir(), "run_aborted_old", "artifacts")); !os.IsNotExist(err) {
		t.Fatalf("stale aborted artifacts remain: %v", err)
	}
}

// committingRunWithReservation writes a run record in `committing`
// state under the given machine with a valid scheduler claim and
// either a stale worker lease or no lease at all. The shape is a
// worker that died between publish and the terminal transition: the
// lease (if any) expired long ago while the claim still reserves the
// slot.
func committingRunWithReservation(t *testing.T, l model.Layout, machine, runID string, at time.Time, staleLease bool) {
	t.Helper()
	record, err := store.NewRun("p", machine, []string{"true"}, "source", map[string]any{}, at)
	if err != nil {
		t.Fatal(err)
	}
	record.ID = model.RunID(runID)
	runStore := store.Store{Layout: l}
	if err := runStore.Save(record); err != nil {
		t.Fatal(err)
	}
	if _, err := runStore.Transition(record.ID, model.RunStaging, at); err != nil {
		t.Fatal(err)
	}
	if _, err := runStore.Transition(record.ID, model.RunRunning, at); err != nil {
		t.Fatal(err)
	}
	if _, err := runStore.Transition(record.ID, model.RunCommitting, at); err != nil {
		t.Fatal(err)
	}
	writeReservation(t, l, machine, runID, at)
	if staleLease {
		if err := store.Claim(l, record.ID, "worker-dead", at, time.Minute); err != nil {
			t.Fatal(err)
		}
	}
}

// TestReaperRecoversCommittingRunWithoutLiveLease pins the committing
// recovery: a committing run whose worker lease is stale or absent is
// recovered as lost so the scheduler claim is released on the same
// sweep. The regression proof is the slot itself — after maintenance
// a fresh reservation on the same capacity-one machine must succeed,
// so a dead committing worker can never leak the slot.
func TestReaperRecoversCommittingRunWithoutLiveLease(t *testing.T) {
	for _, tc := range []struct {
		name       string
		staleLease bool
	}{
		{"stale lease", true},
		{"absent lease", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
			if err := l.Ensure(); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			old := now.Add(-2 * time.Hour)
			committingRunWithReservation(t, l, "alpha", "run_commit", old, tc.staleLease)

			report, err := Run(l, now)
			if err != nil {
				t.Fatal(err)
			}
			if report.MarkedLost != 1 {
				t.Fatalf("committing run must be lost: %+v", report)
			}
			if report.SchedulerClaimsReleased < 1 {
				t.Fatalf("committing claim must be reaped: %+v", report)
			}
			runStore := store.Store{Layout: l}
			loaded, err := runStore.Load("run_commit")
			if err != nil {
				t.Fatal(err)
			}
			if loaded.State != model.RunLost {
				t.Fatalf("state must be lost; got %s", loaded.State)
			}
			if _, err := os.Stat(l.MachineClaimPath("alpha", model.RunID("run_commit"))); !os.IsNotExist(err) {
				t.Fatalf("claim must be released: %v", err)
			}
			// No slot leak: the same capacity-one machine admits a
			// fresh reservation after the sweep.
			cfg := config.Config{Machines: map[string]config.Machine{"alpha": {MaxConcurrent: 1}}}
			fresh := model.RunID("run_commit_fresh")
			err = scheduler.ReserveAndPublish(l, &runStore, cfg, fresh, []string{"alpha"}, now, func(machine string) error {
				record, rerr := store.NewStagedRunFromID(fresh, "p", machine, []string{"true"}, "source", map[string]any{}, now)
				if rerr != nil {
					return rerr
				}
				return runStore.Save(record)
			})
			if err != nil {
				t.Fatalf("slot leaked by committing recovery: %v", err)
			}
		})
	}
}

// TestReapRunSurfacesArtifactsReaped pins that the maintenance sweep
// (reaper.Run, invoked by app.Maintain via `setup reap`) reaps stale
// lost artifacts, keeps succeeded artifacts, and surfaces the count as
// artifacts_reaped in the report.
func TestReapRunSurfacesArtifactsReaped(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	old := now.Add(-2 * time.Hour)
	writeArtifactsRun(t, l, "run_lost", model.RunLost, old)
	writeArtifactsRun(t, l, "run_ok", model.RunSucceeded, now.Add(-time.Minute))

	report, err := Run(l, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.ArtifactsReaped != 1 {
		t.Fatalf("artifacts_reaped %d want 1: %+v", report.ArtifactsReaped, report)
	}
	if _, err := os.Stat(filepath.Join(l.RunsDir(), "run_lost", "artifacts")); !os.IsNotExist(err) {
		t.Fatalf("stale lost artifacts remain after Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.RunsDir(), "run_ok", "artifacts", "build", "out.bin")); err != nil {
		t.Fatalf("succeeded artifacts removed by Run: %v", err)
	}
}

// Plan 15 Phase 3 reaper-ownership tests: per-run state/work/<run>
// temp roots (.tmp/.home), whole-workspace removal for terminal runs
// whose lease is gone, and remote turd cleanup via a fake ssh stub.

// terminalRunRecord writes a terminal (lost) run record with no lease
// and returns its id. The record's workspace dir is created with the
// given contents.
func terminalRunRecord(t *testing.T, l model.Layout, runID string) model.RunID {
	t.Helper()
	now := time.Now().UTC()
	record, err := store.NewRun("p", "m", []string{"true"}, "source", map[string]any{}, now)
	if err != nil {
		t.Fatal(err)
	}
	record.ID = model.RunID(runID)
	runStore := store.Store{Layout: l}
	if err := runStore.Save(record); err != nil {
		t.Fatal(err)
	}
	if _, err := runStore.Transition(record.ID, model.RunStaging, now); err != nil {
		t.Fatal(err)
	}
	if _, err := runStore.Transition(record.ID, model.RunLost, now); err != nil {
		t.Fatal(err)
	}
	return record.ID
}

// TestReapWorkDirsRemovesTerminalWorkspace pins that a terminal run
// whose worker lease is gone has its whole state/work/<run> tree
// removed by the reaper.
func TestReapWorkDirsRemovesTerminalWorkspace(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	id := terminalRunRecord(t, l, "run_terminal")
	workDir := filepath.Join(l.WorkDir(), string(id))
	if err := os.MkdirAll(filepath.Join(workDir, ".tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(l, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if report.WorkspacesRemoved != 1 {
		t.Fatalf("report: %+v", report)
	}
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Fatalf("terminal workspace remains: %v", err)
	}
}

// TestReapWorkDirsKeepsActiveLeaseWorkspace pins that a running run
// with a live worker lease keeps its workspace and its temp roots,
// even when .tmp/.home look old: the live worker owns them.
func TestReapWorkDirsKeepsActiveLeaseWorkspace(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	record, err := store.NewRun("p", "m", []string{"true"}, "source", map[string]any{}, now)
	if err != nil {
		t.Fatal(err)
	}
	runStore := store.Store{Layout: l}
	if err := runStore.Save(record); err != nil {
		t.Fatal(err)
	}
	if _, err := runStore.Transition(record.ID, model.RunStaging, now); err != nil {
		t.Fatal(err)
	}
	if _, err := runStore.Transition(record.ID, model.RunRunning, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Claim(l, record.ID, "worker-live", now, time.Hour); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(l.WorkDir(), string(record.ID))
	if err := os.MkdirAll(filepath.Join(workDir, ".tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	past := now.Add(-2 * transferStaleAge)
	_ = os.Chtimes(filepath.Join(workDir, ".tmp"), past, past)
	report, err := Run(l, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.WorkspacesRemoved != 0 || report.PerRunTmpRemoved != 0 {
		t.Fatalf("live run was touched: %+v", report)
	}
	if _, err := os.Stat(workDir); err != nil {
		t.Fatalf("live workspace removed: %v", err)
	}
}

// TestReapWorkDirsSweepsStaleTmpHome pins the per-run temp-root
// sweep: a workspace whose run record no longer exists (or is not
// terminal) and whose .tmp/.home are older than the transfer-stale
// age has those roots removed while the workspace itself is kept.
func TestReapWorkDirsSweepsStaleTmpHome(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(l.WorkDir(), "run_orphan")
	for _, sub := range []string{".tmp", ".home"} {
		if err := os.MkdirAll(filepath.Join(workDir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(workDir, sub, "junk"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * transferStaleAge)
	_ = os.Chtimes(filepath.Join(workDir, ".tmp"), past, past)
	_ = os.Chtimes(filepath.Join(workDir, ".home"), past, past)
	report, err := Run(l, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if report.PerRunTmpRemoved != 2 {
		t.Fatalf("report: %+v", report)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".tmp")); !os.IsNotExist(err) {
		t.Fatalf(".tmp remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".home")); !os.IsNotExist(err) {
		t.Fatalf(".home remains: %v", err)
	}
	if _, err := os.Stat(workDir); err != nil {
		t.Fatalf("workspace was removed: %v", err)
	}
}

// TestReapWorkDirsKeepsFreshTmpHome pins that a fresh .tmp/.home is
// never removed: it may belong to a worker that is still setting up.
func TestReapWorkDirsKeepsFreshTmpHome(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(l.WorkDir(), "run_fresh")
	if err := os.MkdirAll(filepath.Join(workDir, ".tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	report, err := Run(l, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if report.PerRunTmpRemoved != 0 {
		t.Fatalf("fresh .tmp removed: %+v", report)
	}
	if _, err := os.Stat(filepath.Join(workDir, ".tmp")); err != nil {
		t.Fatalf("fresh .tmp removed: %v", err)
	}
}

// TestCleanupRemoteInvokesSSHRm pins that CleanupRemote runs
// `ssh <host> rm -rf -- <path>` with a fake ssh stub and validates
// both arguments first.
func TestCleanupRemoteInvokesSSHRm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-specific")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	script := "#!/bin/sh\necho \"$*\" >> " + logPath + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+previous)

	if err := CleanupRemote(context.Background(), "builder", "~/.vci/state/work/run_abc"); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(log)
	for _, want := range []string{"builder", "rm -rf --", "~/.vci/state/work/run_abc"} {
		if !strings.Contains(s, want) {
			t.Errorf("ssh log missing %q: %s", want, s)
		}
	}
}

// TestCleanupRemoteRejectsUnsafe pins that CleanupRemote refuses
// unsafe hosts and paths without invoking ssh.
func TestCleanupRemoteRejectsUnsafe(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+previous)
	for _, tc := range []struct {
		host, path string
	}{
		{"", "~/.vci/state/work/run_abc"},
		{"builder", ""},
		{"builder", "/tmp/x"},
		{"-flag", "~/.vci/state/work/run_abc"},
	} {
		if err := CleanupRemote(context.Background(), tc.host, tc.path); err == nil {
			t.Errorf("host %q path %q accepted", tc.host, tc.path)
		}
	}
}

// TestReapRemoteTurdsCleansTerminalHostRuns pins that a terminal run
// whose reserved machine declared a remote host gets its mirrored
// remote workspace swept via ssh, and that the report counts it.
func TestReapRemoteTurdsCleansTerminalHostRuns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-specific")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	script := "#!/bin/sh\necho \"$*\" >> " + logPath + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+previous)

	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	snapshot := map[string]any{
		"machine": "mac-remote",
		"machines": map[string]config.Machine{
			"mac-remote": {Host: "builder"},
		},
	}
	record, err := store.NewRun("p", "mac-remote", []string{"true"}, "source", snapshot, now)
	if err != nil {
		t.Fatal(err)
	}
	runStore := store.Store{Layout: l}
	if err := runStore.Save(record); err != nil {
		t.Fatal(err)
	}
	if _, err := runStore.Transition(record.ID, model.RunStaging, now); err != nil {
		t.Fatal(err)
	}
	if _, err := runStore.Transition(record.ID, model.RunRunning, now); err != nil {
		t.Fatal(err)
	}
	if _, err := runStore.Transition(record.ID, model.RunCommitting, now); err != nil {
		t.Fatal(err)
	}
	if _, err := runStore.Transition(record.ID, model.RunSucceeded, now); err != nil {
		t.Fatal(err)
	}
	report, err := Run(l, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.RemoteCleaned != 1 {
		t.Fatalf("report: %+v", report)
	}
	log, _ := os.ReadFile(logPath)
	s := string(log)
	if !strings.Contains(s, "builder rm -rf -- ~/.vci/state/work/"+string(record.ID)) {
		t.Errorf("remote sweep missing: %s", s)
	}
}

// TestReapRemoteTurdsSkipsLiveLease pins that a terminal run whose
// worker lease is still live is not remotely swept: the worker may
// still be terminalizing.
func TestReapRemoteTurdsSkipsLiveLease(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+previous)

	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	snapshot := map[string]any{
		"machine": "mac-remote",
		"machines": map[string]config.Machine{
			"mac-remote": {Host: "builder"},
		},
	}
	record, err := store.NewRun("p", "mac-remote", []string{"true"}, "source", snapshot, now)
	if err != nil {
		t.Fatal(err)
	}
	runStore := store.Store{Layout: l}
	if err := runStore.Save(record); err != nil {
		t.Fatal(err)
	}
	if _, err := runStore.Transition(record.ID, model.RunStaging, now); err != nil {
		t.Fatal(err)
	}
	if _, err := runStore.Transition(record.ID, model.RunRunning, now); err != nil {
		t.Fatal(err)
	}
	if _, err := runStore.Transition(record.ID, model.RunCommitting, now); err != nil {
		t.Fatal(err)
	}
	if _, err := runStore.Transition(record.ID, model.RunSucceeded, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Claim(l, record.ID, "worker-finishing", now, time.Hour); err != nil {
		t.Fatal(err)
	}
	report, err := Run(l, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.RemoteCleaned != 0 {
		t.Fatalf("live-lease run swept: %+v", report)
	}
}

// TestReapBundleCachesReportsRemoteCounts pins the remote bundle-cache sweep:
// an eligible machine (host configured) attached to a project invokes the
// worker reap shell over ssh, the stale/evicted removal counts the shell
// reports land in the reaper report JSON, and hostless machines never invoke
// ssh.
func TestReapBundleCachesReportsRemoteCounts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix-specific")
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + logPath + "\nprintf 'stale=2 evicted=1\\n'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+previous)

	cfg := config.Config{
		Machines: map[string]config.Machine{
			"builder": {Host: "builder-host"},
			"local":   {},
		},
		Projects: map[string]config.Project{
			"app": {Machines: []string{"builder", "local"}, Command: []string{"make"}},
		},
	}
	var report Report
	ReapRemoteBundleCaches(&report, cfg, time.Now().UTC())

	if report.BundleCacheStaleRemoved != 2 || report.BundleCacheEvictedRemoved != 1 {
		t.Fatalf("report counts: %+v", report)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"bundle_cache_stale_removed":2`, `"bundle_cache_evicted_removed":1`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("report JSON missing %s: %s", want, raw)
		}
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected one ssh invocation for the remote machine, got %d: %q", len(lines), log)
	}
	for _, want := range []string{"builder-host", "bundle-cache/v1/app", ".vci-reap"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("ssh invocation missing %q: %s", want, lines[0])
		}
	}
}
