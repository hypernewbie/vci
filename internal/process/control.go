package process

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hypernewbie/vci/internal/model"
)

type CancellationPhase string

const (
	CancellationNone        CancellationPhase = ""
	CancellationRequested   CancellationPhase = "requested"
	CancellationTerminating CancellationPhase = "terminating"
	CancellationKilled      CancellationPhase = "killed"
	CancellationReaped      CancellationPhase = "reaped"
)

type Execution struct {
	SchemaVersion           int               `json:"schema_version"`
	RunID                   model.RunID       `json:"run_id"`
	Owner                   string            `json:"owner"`
	PID                     int               `json:"pid"`
	PGID                    int               `json:"pgid"`
	StartedAt               time.Time         `json:"started_at"`
	CancellationRequestedAt *time.Time        `json:"cancellation_requested_at,omitempty"`
	CancellationPhase       CancellationPhase `json:"cancellation_phase"`
}

func NewOwner() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "worker-" + hex.EncodeToString(buf), nil
}

func (e Execution) Validate(id model.RunID) error {
	if e.SchemaVersion != model.SchemaVersion {
		return fmt.Errorf("unsupported execution schema version %d", e.SchemaVersion)
	}
	if e.RunID != id || !model.ValidRunID(id) {
		return fmt.Errorf("execution run mismatch")
	}
	if len(e.Owner) < 8 {
		return fmt.Errorf("execution owner is invalid")
	}
	if e.PID <= 0 || e.PGID <= 0 {
		return fmt.Errorf("execution process identity is invalid")
	}
	if e.StartedAt.IsZero() {
		return fmt.Errorf("execution start time is missing")
	}
	switch e.CancellationPhase {
	case CancellationNone, CancellationRequested, CancellationTerminating, CancellationKilled, CancellationReaped:
	default:
		return fmt.Errorf("invalid cancellation phase %q", e.CancellationPhase)
	}
	return nil
}

func WriteExecution(path string, execution Execution) error {
	if err := execution.Validate(execution.RunID); err != nil {
		return err
	}
	data, err := json.MarshalIndent(execution, "", "  ")
	if err != nil {
		return err
	}
	return atomicPrivateJSON(path, data)
}

func ReadExecution(path string, id model.RunID) (Execution, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Execution{}, err
	}
	var execution Execution
	if err := json.Unmarshal(data, &execution); err != nil {
		return Execution{}, fmt.Errorf("decode execution: %w", err)
	}
	if err := execution.Validate(id); err != nil {
		return Execution{}, err
	}
	return execution, nil
}

func RemoveExecution(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func atomicPrivateJSON(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".execution-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
