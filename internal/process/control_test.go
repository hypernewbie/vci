package process

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/model"
)

func TestExecutionAtomicRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "execution.json")
	e := Execution{SchemaVersion: model.ExecutionSchemaVersion, RunID: model.RunID("run_test_1"), Owner: "worker-abcdefgh", PID: 12, PGID: 12, StartedAt: time.Now().UTC(), CancellationPhase: CancellationNone}
	if err := WriteExecution(path, e); err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadExecution(path, e.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Owner != e.Owner {
		t.Fatalf("loaded: %+v", loaded)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("empty execution")
	}
	if _, err := ReadExecution(path, model.RunID("run_other")); err == nil {
		t.Fatal("mismatched execution accepted")
	}
}

// TestExecutionAcceptsLegacyCancellationPhase pins read-compatibility
// with historical Vci versions (`006ae53`) that wrote
// `cancellation_phase: "killed"`. The phase is accepted by Validate on
// load only; current workers must not write it.
func TestExecutionAcceptsLegacyCancellationPhase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "execution.json")
	legacy := `{
  "schema_version": 1,
  "run_id": "run_test_legacy",
  "owner": "worker-abcdefgh",
  "pid": 12,
  "pgid": 12,
  "started_at": "2020-01-01T00:00:00Z",
  "cancellation_phase": "killed"
}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadExecution(path, model.RunID("run_test_legacy"))
	if err != nil {
		t.Fatalf("legacy execution.json with killed phase must decode: %v", err)
	}
	if string(loaded.CancellationPhase) != "killed" {
		t.Fatalf("loaded phase: %q", loaded.CancellationPhase)
	}
}

// TestExecutionRejectsUnknownCancellationPhase pins the negative case:
// values that are neither the live phases nor the legacy value must be
// rejected by Validate.
func TestExecutionRejectsUnknownCancellationPhase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "execution.json")
	bad := `{
  "schema_version": 1,
  "run_id": "run_test_bad",
  "owner": "worker-abcdefgh",
  "pid": 12,
  "pgid": 12,
  "started_at": "2020-01-01T00:00:00Z",
  "cancellation_phase": "exploded"
}`
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadExecution(path, model.RunID("run_test_bad")); err == nil {
		t.Fatal("unknown cancellation phase must be rejected")
	}
}
