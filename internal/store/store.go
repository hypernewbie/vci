package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/lock"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/process"
)

type RunRecord struct {
	SchemaVersion           int              `json:"schema_version"`
	ID                      model.RunID      `json:"run_id"`
	Project                 string           `json:"project"`
	Machine                 string           `json:"machine"`
	State                   model.RunState   `json:"state"`
	SourceDigest            string           `json:"source_digest,omitempty"`
	ConfigDigest            string           `json:"config_digest,omitempty"`
	ConfigSnapshot          json.RawMessage  `json:"config_snapshot,omitempty"`
	Command                 []string         `json:"command"`
	QueuedAt                time.Time        `json:"queued_at"`
	UpdatedAt               time.Time        `json:"updated_at"`
	Result                  *json.RawMessage `json:"result,omitempty"`
	CancellationRequestedAt *time.Time       `json:"cancellation_requested_at,omitempty"`
	CancellationPhase       string           `json:"cancellation_phase,omitempty"`
}

type Store struct{ Layout layout.Layout }

func NewRun(project, machine string, command []string, sourceDigest string, snapshot any, now time.Time) (RunRecord, error) {
	data, err := json.Marshal(snapshot)
	if err != nil {
		return RunRecord{}, err
	}
	sum := sha256.Sum256(data)
	id, err := newID(now)
	if err != nil {
		return RunRecord{}, err
	}
	return RunRecord{SchemaVersion: model.SchemaVersion, ID: model.RunID(id), Project: project, Machine: machine, State: model.RunQueued, SourceDigest: sourceDigest, ConfigDigest: hex.EncodeToString(sum[:]), ConfigSnapshot: data, Command: append([]string(nil), command...), QueuedAt: now.UTC(), UpdatedAt: now.UTC()}, nil
}

func newID(now time.Time) (string, error) {
	random := make([]byte, 6)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return fmt.Sprintf("run_%x_%s", now.UnixNano(), hex.EncodeToString(random)), nil
}

func (s Store) runDir(id model.RunID) (string, error) { return s.Layout.RunDir(string(id)) }
func (s Store) lockPath(id model.RunID) (string, error) {
	dir, err := s.runDir(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "run.lock"), nil
}

func (s Store) Save(record RunRecord) error {
	if err := validateRecord(record); err != nil {
		return err
	}
	dir, err := s.runDir(record.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	unlock, err := lock.Acquire(filepath.Join(dir, "run.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	return s.saveUnlocked(record)
}

func (s Store) saveUnlocked(record RunRecord) error {
	if err := validateRecord(record); err != nil {
		return err
	}
	dir, err := s.runDir(record.ID)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return atomicJSON(filepath.Join(dir, "run.json"), data)
}

func validateRecord(record RunRecord) error {
	if record.SchemaVersion != model.SchemaVersion {
		return fmt.Errorf("unsupported run schema version %d", record.SchemaVersion)
	}
	if !model.ValidRunID(record.ID) || !record.State.Valid() {
		return fmt.Errorf("invalid run record %q", record.ID)
	}
	return nil
}

func (s Store) Load(id model.RunID) (RunRecord, error) {
	if !model.ValidRunID(id) {
		return RunRecord{}, fmt.Errorf("invalid run id %q", id)
	}
	dir, err := s.runDir(id)
	if err != nil {
		return RunRecord{}, err
	}
	return s.loadPath(filepath.Join(dir, "run.json"), id)
}

func (s Store) loadUnlocked(id model.RunID) (RunRecord, error) { return s.Load(id) }

func (s Store) loadPath(path string, id model.RunID) (RunRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return RunRecord{}, err
	}
	var record RunRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return RunRecord{}, fmt.Errorf("decode run: %w", err)
	}
	if record.ID != id {
		return RunRecord{}, fmt.Errorf("invalid run record %q", id)
	}
	if err := validateRecord(record); err != nil {
		return RunRecord{}, err
	}
	return record, nil
}

// Mutate serializes one load/validate/write operation. The callback must not
// perform a process wait or another run/config mutation.
func (s Store) Mutate(id model.RunID, fn func(*RunRecord) error) (RunRecord, error) {
	dir, err := s.runDir(id)
	if err != nil {
		return RunRecord{}, err
	}
	unlock, err := lock.Acquire(filepath.Join(dir, "run.lock"))
	if err != nil {
		return RunRecord{}, err
	}
	defer unlock()
	record, err := s.loadPath(filepath.Join(dir, "run.json"), id)
	if err != nil {
		return RunRecord{}, err
	}
	before := record
	if err := fn(&record); err != nil {
		return RunRecord{}, err
	}
	if isTerminal(before.State) && !reflect.DeepEqual(before, record) {
		return RunRecord{}, fmt.Errorf("terminal run %s is immutable", id)
	}
	if err := validateRecord(record); err != nil {
		return RunRecord{}, err
	}
	if err := s.saveUnlocked(record); err != nil {
		return RunRecord{}, err
	}
	return record, nil
}

func isTerminal(state model.RunState) bool {
	return state == model.RunSucceeded || state == model.RunFailed || state == model.RunLost || state == model.RunAborted
}

func (s Store) Transition(id model.RunID, to model.RunState, now time.Time) (RunRecord, error) {
	return s.Mutate(id, func(record *RunRecord) error {
		if err := model.Transition(record.State, to); err != nil {
			return err
		}
		record.State, record.UpdatedAt = to, now.UTC()
		return nil
	})
}

func (s Store) RequestCancellation(id model.RunID, now time.Time) (RunRecord, error) {
	return s.Mutate(id, func(record *RunRecord) error {
		if record.State == model.RunSucceeded || record.State == model.RunFailed || record.State == model.RunAborted {
			return fmt.Errorf("run %s cannot be cancelled from state %s", id, record.State)
		}
		if record.CancellationRequestedAt == nil {
			t := now.UTC()
			record.CancellationRequestedAt = &t
			record.CancellationPhase = "requested"
			record.UpdatedAt = t
			dir, err := s.runDir(id)
			if err != nil {
				return err
			}
			path := filepath.Join(dir, "execution.json")
			if execution, readErr := process.ReadExecution(path, id); readErr == nil {
				execution.CancellationRequestedAt = &t
				execution.CancellationPhase = process.CancellationRequested
				if err := process.WriteExecution(path, execution); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s Store) SaveResultState(id model.RunID, result any, terminal model.RunState, now time.Time) (RunRecord, error) {
	var raw json.RawMessage
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return RunRecord{}, err
	}
	raw = append(raw, data...)
	return s.Mutate(id, func(record *RunRecord) error {
		if !model.CanTransition(record.State, model.RunCommitting) && record.State != model.RunCommitting {
			return fmt.Errorf("run %s is not publishable from state %s", id, record.State)
		}
		if record.State != model.RunCommitting {
			record.State = model.RunCommitting
			record.UpdatedAt = now.UTC()
		}
		if record.Result != nil {
			return fmt.Errorf("run %s already has a result", id)
		}
		record.Result = &raw
		if err := model.Transition(record.State, terminal); err != nil {
			return err
		}
		record.State, record.UpdatedAt = terminal, now.UTC()
		return nil
	})
}

func atomicJSON(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".run-*")
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
