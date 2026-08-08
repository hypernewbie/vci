// Package model defines Vci's shared domain types and the
// coordinator's on-disk state layout. It is the foundation every
// other package builds on: run identifiers and state machine, the
// public error envelope, and the Layout helpers that name where
// state, sources, work, and machine claims live under a root.
package model

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// ErrRunNotFound is returned by read-side run operations when the
// requested run record does not exist. Public CLIs map it to the
// `not_found` envelope (configuration class, not retryable).
var ErrRunNotFound = errors.New("run not found")

const SchemaVersion = 1

type RunID string

var validRunID = regexp.MustCompile(`^run_[A-Za-z0-9._-]+$`)

func ValidRunID(id RunID) bool { return validRunID.MatchString(string(id)) }

type RunState string

const (
	RunQueued     RunState = "queued"
	RunStaging    RunState = "staging"
	RunRunning    RunState = "running"
	RunCommitting RunState = "committing"
	RunSucceeded  RunState = "succeeded"
	RunFailed     RunState = "failed"
	RunLost       RunState = "lost"
	RunAborted    RunState = "aborted"
)

func (s RunState) Valid() bool {
	switch s {
	case RunQueued, RunStaging, RunRunning, RunCommitting, RunSucceeded, RunFailed, RunLost, RunAborted:
		return true
	default:
		return false
	}
}

func CanTransition(from, to RunState) bool {
	if from == to {
		return from != RunSucceeded && from != RunFailed && from != RunLost && from != RunAborted
	}
	switch from {
	case RunQueued:
		return to == RunStaging || to == RunAborted
	case RunStaging:
		return to == RunRunning || to == RunLost || to == RunAborted
	case RunRunning:
		return to == RunCommitting || to == RunFailed || to == RunLost || to == RunAborted
	case RunCommitting:
		return to == RunSucceeded || to == RunFailed || to == RunLost || to == RunAborted
	case RunLost:
		return to == RunAborted
	default:
		return false
	}
}

func Transition(from, to RunState) error {
	if !from.Valid() || !to.Valid() {
		return fmt.Errorf("invalid run state transition %q -> %q", from, to)
	}
	if !CanTransition(from, to) {
		return fmt.Errorf("illegal run state transition %q -> %q", from, to)
	}
	return nil
}

// IsTerminal reports whether a run state is final and immutable:
// succeeded, failed, lost, or aborted.
func IsTerminal(state RunState) bool {
	return state == RunSucceeded || state == RunFailed || state == RunLost || state == RunAborted
}

type FailureClass string

const (
	FailureUsage          FailureClass = "usage"
	FailureConfiguration  FailureClass = "configuration"
	FailureInfrastructure FailureClass = "infrastructure"
	FailureState          FailureClass = "state"
)

type VciError struct {
	Code      string       `json:"code"`
	Class     FailureClass `json:"class"`
	Message   string       `json:"message"`
	Retryable bool         `json:"retryable"`
}

func (e *VciError) Error() string { return e.Message }

func NewError(code string, class FailureClass, message string, retryable bool) *VciError {
	return &VciError{Code: code, Class: class, Message: message, Retryable: retryable}
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type Layout struct{ Root string }

func Default() (Layout, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Layout{}, fmt.Errorf("resolve home directory: %w", err)
	}
	return Layout{Root: filepath.Join(home, ".vci")}, nil
}

func (l Layout) ConfigPath() string     { return filepath.Join(l.Root, "config.toml") }
func (l Layout) StateDir() string       { return filepath.Join(l.Root, "state") }
func (l Layout) RunsDir() string        { return filepath.Join(l.StateDir(), "runs") }
func (l Layout) SourcesDir() string     { return filepath.Join(l.StateDir(), "sources") }
func (l Layout) BlobsDir() string       { return filepath.Join(l.SourcesDir(), "blobs") }
func (l Layout) ManifestsDir() string   { return filepath.Join(l.SourcesDir(), "manifests") }
func (l Layout) WorkDir() string        { return filepath.Join(l.StateDir(), "work") }
func (l Layout) LocksDir() string       { return filepath.Join(l.StateDir(), "locks") }
func (l Layout) TempDir() string        { return filepath.Join(l.StateDir(), "tmp") }
func (l Layout) SourceCacheDir() string { return filepath.Join(l.StateDir(), "source-cache") }

// SchedulerLockPath returns the single Vci-owned scheduler lock used to
// serialize inspect/sweep/select/claim/write under scheduler concurrency.
// Its parent is LocksDir(), which previously had no real consumer.
func (l Layout) SchedulerLockPath() string { return filepath.Join(l.LocksDir(), "scheduler.lock") }

// MachineClaimsDir returns the root of the durable per-machine reservation
// tree. Each machine has a subdirectory; each reservation is one
// JSON file at <machine>/<run-id>.json.
func (l Layout) MachineClaimsDir() string { return filepath.Join(l.StateDir(), "machine-claims") }

// MachineClaimPath returns the absolute claim file for one
// (machine, run-id) tuple. The caller must validate both names.
func (l Layout) MachineClaimPath(machine string, runID RunID) string {
	return filepath.Join(l.MachineClaimsDir(), machine, string(runID)+".json")
}

func (l Layout) Ensure() error {
	if l.Root == "" {
		return fmt.Errorf("vci root is empty")
	}
	for _, dir := range []string{l.Root, l.StateDir(), l.RunsDir(), l.SourcesDir(), l.BlobsDir(), l.ManifestsDir(), l.WorkDir(), l.LocksDir(), l.TempDir(), l.SourceCacheDir(), l.MachineClaimsDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("protect %s: %w", dir, err)
		}
	}
	return nil
}

func (l Layout) RunDir(id string) (string, error) {
	if !ValidName(id) {
		return "", fmt.Errorf("invalid run id %q", id)
	}
	return filepath.Join(l.RunsDir(), id), nil
}

func ValidName(value string) bool { return value != "" && safeName.MatchString(value) }
