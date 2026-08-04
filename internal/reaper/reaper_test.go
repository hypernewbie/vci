package reaper

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/lease"
	"github.com/hypernewbie/vci/internal/model"
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
