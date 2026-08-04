package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/model"
)

func TestTerminalSelfTransitionIsRejected(t *testing.T) {
	s := Store{Layout: layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}}
	now := time.Now().UTC()
	record, err := NewRun("p", "m", []string{"true"}, "s", map[string]any{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(record); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(record.ID, model.RunStaging, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(record.ID, model.RunRunning, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(record.ID, model.RunCommitting, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(record.ID, model.RunSucceeded, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(record.ID, model.RunSucceeded, now.Add(time.Second)); err == nil {
		t.Fatal("terminal self-transition accepted")
	}
}

func TestPublishResultIsSingleAndCheckableOnlyAfterTerminal(t *testing.T) {
	s := Store{Layout: layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}}
	now := time.Now().UTC()
	record, err := NewRun("p", "m", []string{"true"}, "s", map[string]any{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(record); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(record.ID, model.RunStaging, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(record.ID, model.RunRunning, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(record.ID, model.RunCommitting, now); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishResult(record.ID, map[string]any{"state": "succeeded"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PublishResult(record.ID, map[string]any{"state": "succeeded"}); err == nil {
		t.Fatal("result overwrite accepted")
	}
	data, err := s.ReadResult(record.ID)
	if err != nil || len(data) == 0 {
		t.Fatalf("result: %v", err)
	}
}

func TestRunRecordPersistsSnapshotAndTransitions(t *testing.T) {
	s := Store{Layout: layout.Layout{Root: filepath.Join(t.TempDir(), ".vci")}}
	now := time.Unix(100, 0)
	record, err := NewRun("Vci", "mac-local", []string{"go", "test"}, "source", map[string]any{"machine": "mac-local"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(record); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(record.ID, model.RunStaging, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ConfigDigest == "" || len(loaded.ConfigSnapshot) == 0 || loaded.State != model.RunStaging {
		t.Fatalf("loaded: %+v", loaded)
	}
}
