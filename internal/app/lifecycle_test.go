package app

// Focused lifecycle tests for the app-level release contracts:
// app.Abandon (the spawn-failure cleanup path) and the app.Abort
// RunLost branch. Both must free the scheduler slot they hold without
// disturbing anything else; these tests prove the claim tree and the
// run record state.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/scheduler"
	"github.com/hypernewbie/vci/internal/store"
)

// reserveStaged is a test-side ReserveAndPublish callback that
// persists a real staging record under the reserved run id — the same
// contract the production build path satisfies, so these tests
// exercise the scheduler's real claim tree.
func reserveStaged(l model.Layout, runID model.RunID, machine string) error {
	runStore := store.Store{Layout: l}
	cfg := config.Config{Machines: map[string]config.Machine{machine: config.Machine{MaxConcurrent: 1}}}
	return scheduler.ReserveAndPublish(l, &runStore, cfg, runID, []string{machine}, time.Now().UTC(), func(selected string) error {
		record, err := store.NewStagedRunFromID(runID, "p", selected, []string{"true"}, "source", map[string]any{}, time.Now().UTC())
		if err != nil {
			return err
		}
		return runStore.Save(record)
	})
}

// TestAbandonTerminalizesStagingRunAndFreesSlot pins app.Abandon —
// the spawn-failure cleanup path. A staging run with a live scheduler
// reservation is terminalized as aborted and its claim is released,
// so the slot is free for the next submission.
func TestAbandonTerminalizesStagingRunAndFreesSlot(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	runID := model.RunID("run_abandon")
	if err := reserveStaged(l, runID, "alpha"); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if _, err := os.Stat(l.MachineClaimPath("alpha", runID)); err != nil {
		t.Fatalf("claim must exist before abandon: %v", err)
	}
	if err := Abandon(l, runID); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	record, err := (store.Store{Layout: l}).Load(runID)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != model.RunAborted {
		t.Fatalf("state: %s, want aborted", record.State)
	}
	if _, err := os.Stat(l.MachineClaimPath("alpha", runID)); !os.IsNotExist(err) {
		t.Fatalf("claim must be released by abandon: %v", err)
	}
	// The freed slot admits the next submission.
	if err := reserveStaged(l, model.RunID("run_abandon_next"), "alpha"); err != nil {
		t.Fatalf("slot not freed by abandon: %v", err)
	}
}

// TestAbortLostRunReleasesClaim pins the app.Abort RunLost branch: a
// lost run's scheduler claim is released and the record is left
// untouched (lost is already terminal and immutable).
func TestAbortLostRunReleasesClaim(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	runID := model.RunID("run_lost_abort")
	if err := reserveStaged(l, runID, "alpha"); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	runStore := store.Store{Layout: l}
	now := time.Now().UTC()
	if _, err := runStore.Transition(runID, model.RunRunning, now); err != nil {
		t.Fatal(err)
	}
	if _, err := runStore.Transition(runID, model.RunLost, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.MachineClaimPath("alpha", runID)); err != nil {
		t.Fatalf("claim must exist before abort: %v", err)
	}
	if _, err := Abort(l, runID); err != nil {
		t.Fatalf("abort lost run: %v", err)
	}
	if _, err := os.Stat(l.MachineClaimPath("alpha", runID)); !os.IsNotExist(err) {
		t.Fatalf("claim must be released: %v", err)
	}
	record, err := runStore.Load(runID)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != model.RunLost {
		t.Fatalf("lost run mutated by abort: %s", record.State)
	}
}
