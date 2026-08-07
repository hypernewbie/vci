package reaper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/lease"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/scheduler"
	"github.com/hypernewbie/vci/internal/store"
)

func TestExpiredOwnedRunBecomesLostAndPartialIsReaped(t *testing.T) {
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
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
	if err := lease.Claim(l, run.ID, "worker-expired", now.Add(-2*time.Hour), time.Minute); err != nil {
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
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
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
	if err := lease.Claim(l, run.ID, "worker-active", now, time.Hour); err != nil {
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
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
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
func writeReservation(t *testing.T, l layout.Layout, machine, runID string, createdAt time.Time) {
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
func stagingRunRecord(t *testing.T, l layout.Layout, machine, runID string, queuedAt time.Time) {
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
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
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
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
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
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
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
	if err := lease.Claim(l, "run_lease", "worker-active", now, time.Hour); err != nil {
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
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
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
