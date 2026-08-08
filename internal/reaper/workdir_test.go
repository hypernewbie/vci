package reaper

// Plan 15 Phase 3 reaper-ownership tests: per-run state/work/<run>
// temp roots (.tmp/.home), whole-workspace removal for terminal runs
// whose lease is gone, and remote turd cleanup via a fake ssh stub.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/lease"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/store"
)

// terminalRunRecord writes a terminal (lost) run record with no lease
// and returns its id. The record's workspace dir is created with the
// given contents.
func terminalRunRecord(t *testing.T, l layout.Layout, runID string) model.RunID {
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
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
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
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
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
	if err := lease.Claim(l, record.ID, "worker-live", now, time.Hour); err != nil {
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
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
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
	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
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
	dir := t.TempDir()
	logPath := filepath.Join(dir, "ssh.log")
	script := "#!/bin/sh\necho \"$*\" >> " + logPath + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := os.Getenv("PATH")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+previous)

	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
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

	l := layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
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
	if err := lease.Claim(l, record.ID, "worker-finishing", now, time.Hour); err != nil {
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
