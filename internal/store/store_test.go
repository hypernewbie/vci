package store

import (
	"encoding/json"
	"os"
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

// TestLegacyRunRecordFieldsAreTolerated pins read-compatibility with
// persisted run.json files written by historical Vci versions that
// carried a `result` field and a `cancellation_phase` field on the
// run record itself. Both fields are absent from the live RunRecord
// schema; plain `json.Unmarshal` must ignore them so old files still
// decode into the current record.
func TestLegacyRunRecordFieldsAreTolerated(t *testing.T) {
	dir := t.TempDir()
	l := layout.Layout{Root: dir}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	runID := model.RunID("run_legacy_1")
	runDir, err := l.RunDir(string(runID))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := `{
  "schema_version": 1,
  "run_id": "run_legacy_1",
  "project": "demo",
  "machine": "mac-local",
  "state": "queued",
  "command": ["true"],
  "queued_at": "2020-01-01T00:00:00Z",
  "updated_at": "2020-01-01T00:00:00Z",
  "cancellation_phase": "requested",
  "result": {"state": "succeeded", "exit_code": 0}
}`
	if err := os.WriteFile(filepath.Join(runDir, "run.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s := Store{Layout: l}
	loaded, err := s.Load(runID)
	if err != nil {
		t.Fatalf("legacy run.json with result+cancellation_phase must decode: %v", err)
	}
	if loaded.State != model.RunQueued || loaded.Project != "demo" {
		t.Fatalf("loaded record: %+v", loaded)
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(loaded.ConfigSnapshot, &probe); err != nil && len(loaded.ConfigSnapshot) != 0 {
		t.Fatalf("config snapshot: %v", err)
	}
}
