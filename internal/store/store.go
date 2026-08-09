// Package store persists a coordinator's durable per-run state.
// A run's directory holds its record, lease, and result; Store
// serializes every write behind an advisory lock so concurrent
// workers and reapers cannot corrupt a run. The package also
// provides the inter-process lock primitive (Acquire) and the
// worker-lease helpers (Claim, Renew, Release, Read) that guard
// a run against concurrent workers.
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
	"runtime"
	"time"

	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/process"
)

type RunRecord struct {
	SchemaVersion           int             `json:"schema_version"`
	ID                      model.RunID     `json:"run_id"`
	Project                 string          `json:"project"`
	Machine                 string          `json:"machine"`
	State                   model.RunState  `json:"state"`
	SourceDigest            string          `json:"source_digest,omitempty"`
	ConfigDigest            string          `json:"config_digest,omitempty"`
	ConfigSnapshot          json.RawMessage `json:"config_snapshot,omitempty"`
	Command                 []string        `json:"command"`
	QueuedAt                time.Time       `json:"queued_at"`
	UpdatedAt               time.Time       `json:"updated_at"`
	CancellationRequestedAt *time.Time      `json:"cancellation_requested_at,omitempty"`
}

type Store struct{ Layout model.Layout }

func NewRun(project, machine string, command []string, sourceDigest string, snapshot any, now time.Time) (RunRecord, error) {
	id, err := newID(now)
	if err != nil {
		return RunRecord{}, err
	}
	return NewRunFromID(model.RunID(id), project, machine, command, sourceDigest, snapshot, now)
}

// NewRunFromID builds a RunRecord under a caller-supplied run id.
// The initial state is `queued`; production build paths must
// transition to `staging` inside the same scheduler transaction so a
// reservation never exists without a record.
func NewRunFromID(id model.RunID, project, machine string, command []string, sourceDigest string, snapshot any, now time.Time) (RunRecord, error) {
	return newRun(id, project, machine, command, sourceDigest, snapshot, model.RunQueued, now)
}

// NewStagedRunFromID builds a RunRecord already in `staging` state
// under a caller-supplied run id. It is the production build path's
// constructor: the scheduler transaction publishes this record, so a
// reservation never exists without a durable staging record.
func NewStagedRunFromID(id model.RunID, project, machine string, command []string, sourceDigest string, snapshot any, now time.Time) (RunRecord, error) {
	return newRun(id, project, machine, command, sourceDigest, snapshot, model.RunStaging, now)
}

func newRun(id model.RunID, project, machine string, command []string, sourceDigest string, snapshot any, state model.RunState, now time.Time) (RunRecord, error) {
	if !model.ValidRunID(id) {
		return RunRecord{}, fmt.Errorf("invalid run id %q", id)
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return RunRecord{}, err
	}
	sum := sha256.Sum256(data)
	return RunRecord{SchemaVersion: model.SchemaVersion, ID: id, Project: project, Machine: machine, State: state, SourceDigest: sourceDigest, ConfigDigest: hex.EncodeToString(sum[:]), ConfigSnapshot: data, Command: append([]string(nil), command...), QueuedAt: now.UTC(), UpdatedAt: now.UTC()}, nil
}

func newID(now time.Time) (string, error) {
	random := make([]byte, 6)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return fmt.Sprintf("run_%x_%s", now.UnixNano(), hex.EncodeToString(random)), nil
}

func (s Store) runDir(id model.RunID) (string, error) { return s.Layout.RunDir(string(id)) }

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
	unlock, err := Acquire(filepath.Join(dir, "run.lock"))
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
	unlock, err := Acquire(filepath.Join(dir, "run.lock"))
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
	if model.IsTerminal(before.State) && !reflect.DeepEqual(before, record) {
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
	if err := dir.Sync(); err != nil && runtime.GOOS != "windows" {
		return err
	}
	return nil
}

func (s Store) PublishResult(id model.RunID, value any) error {
	dir, err := s.Layout.RunDir(string(id))
	if err != nil {
		return err
	}
	unlock, err := Acquire(filepath.Join(dir, "run.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	record, err := s.loadPath(filepath.Join(dir, "run.json"), id)
	if err != nil {
		return err
	}
	if model.IsTerminal(record.State) {
		return fmt.Errorf("run %s is terminal", id)
	}
	path := filepath.Join(dir, "result.json")
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("run %s already has a result", id)
	} else if !os.IsNotExist(err) {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicJSON(path, data)
}

func (s Store) ReadResult(id model.RunID) (json.RawMessage, error) {
	dir, err := s.Layout.RunDir(string(id))
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(dir, "result.json"))
}

type Lease struct {
	RunID     model.RunID `json:"run_id"`
	Owner     string      `json:"owner"`
	ExpiresAt time.Time   `json:"expires_at"`
}

func Claim(l model.Layout, id model.RunID, owner string, now time.Time, ttl time.Duration) error {
	dir, err := l.RunDir(string(id))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	unlock, err := Acquire(filepath.Join(dir, "run.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	path := filepath.Join(dir, "lease.json")
	if existing, err := read(path); err == nil && existing.ExpiresAt.After(now) {
		return fmt.Errorf("run %s is leased by %s", id, existing.Owner)
	}
	data, err := json.Marshal(Lease{RunID: id, Owner: owner, ExpiresAt: now.Add(ttl).UTC()})
	if err != nil {
		return err
	}
	return atomicJSON(path, data)
}

func Renew(l model.Layout, id model.RunID, owner string, now time.Time, ttl time.Duration) error {
	path, err := leasePath(l, id)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	unlock, err := Acquire(filepath.Join(dir, "run.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	current, err := read(path)
	if err != nil {
		return err
	}
	if current.Owner != owner || !current.ExpiresAt.After(now) {
		return fmt.Errorf("lease for %s is not owned by %s", id, owner)
	}
	current.ExpiresAt = now.Add(ttl).UTC()
	data, err := json.Marshal(current)
	if err != nil {
		return err
	}
	return atomicJSON(path, data)
}

func Release(l model.Layout, id model.RunID, owner string) error {
	path, err := leasePath(l, id)
	if err != nil {
		return err
	}
	unlock, err := Acquire(filepath.Join(filepath.Dir(path), "run.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	current, err := read(path)
	if err != nil {
		return err
	}
	if current.Owner != owner {
		return fmt.Errorf("lease for %s is not owned by %s", id, owner)
	}
	return os.Remove(path)
}

func Read(l model.Layout, id model.RunID) (Lease, error) {
	path, err := leasePath(l, id)
	if err != nil {
		return Lease{}, err
	}
	return read(path)
}

// ReadHasNoLease reports whether the named run has no worker lease
// (the lease file is absent). A corrupt lease file is treated as
// "has a lease" so the caller does not misclassify it as missing.
func ReadHasNoLease(l model.Layout, id model.RunID) bool {
	path, err := leasePath(l, id)
	if err != nil {
		return false
	}
	if _, err := os.Stat(path); err != nil {
		return os.IsNotExist(err)
	}
	return false
}

func leasePath(l model.Layout, id model.RunID) (string, error) {
	dir, err := l.RunDir(string(id))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "lease.json"), nil
}
func read(path string) (Lease, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Lease{}, err
	}
	var out Lease
	if err := json.Unmarshal(data, &out); err != nil {
		return Lease{}, err
	}
	return out, nil
}
