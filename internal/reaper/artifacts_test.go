package reaper

// Plan 16 Phase 2 reaper-owned artifact retention tests. ReapArtifacts
// removes state/runs/<run>/artifacts of lost/aborted runs older than
// transferStaleAge and keeps succeeded/failed (and recent terminal)
// artifacts until the run itself is reaped by the existing retention.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/store"
)

// writeArtifactsRun persists a run record in the given state with a
// controlled UpdatedAt and writes one artifact (build/out.bin) into
// its artifacts dir with a matching mtime.
func writeArtifactsRun(t *testing.T, l layout.Layout, id string, state model.RunState, at time.Time) {
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
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
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
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
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
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
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

// TestReapRunSurfacesArtifactsReaped pins that the maintenance sweep
// (reaper.Run, invoked by app.Maintain via `setup reap`) reaps stale
// lost artifacts, keeps succeeded artifacts, and surfaces the count as
// artifacts_reaped in the report.
func TestReapRunSurfacesArtifactsReaped(t *testing.T) {
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
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
