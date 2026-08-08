package model

import (
	"errors"
	"fmt"
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
