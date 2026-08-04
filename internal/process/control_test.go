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
	e := Execution{SchemaVersion: model.SchemaVersion, RunID: model.RunID("run_test_1"), Owner: "worker-abcdefgh", PID: 12, PGID: 12, StartedAt: time.Now().UTC(), CancellationPhase: CancellationNone}
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
