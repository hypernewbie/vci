// Package process runs supervised external commands.
// Each command gets its own process group, supports context
// cancellation (TERM then bounded KILL), limits captured output,
// and persists execution metadata for cancellation tracking.
package process

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hypernewbie/vci/internal/model"
)

type Result struct {
	ExitCode int
	Signaled bool
	Signal   string
}
type Command struct {
	Executable string
	Args       []string
	Dir        string
	Env        []string
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
}
type Running struct {
	PID       int
	PGID      int
	StartedAt time.Time
}
type Runner interface {
	Run(context.Context, Command) (Result, error)
}
type SupervisedRunner interface {
	RunSupervised(context.Context, Command, func(Running) error) (Result, error)
}
type Native struct{}

func (Native) Run(ctx context.Context, command Command) (Result, error) {
	return (Native{}).RunSupervised(ctx, command, nil)
}

func (Native) RunSupervised(ctx context.Context, command Command, onStart func(Running) error) (Result, error) {
	cmd := exec.Command(command.Executable, command.Args...)
	cmd.Dir, cmd.Env = command.Dir, command.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = command.Stdin, command.Stdout, command.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	running := Running{PID: cmd.Process.Pid, PGID: cmd.Process.Pid, StartedAt: time.Now().UTC()}
	if onStart != nil {
		if err := onStart(running); err != nil {
			terminate(cmd.Process.Pid)
			_, _ = cmd.Process.Wait()
			return Result{}, err
		}
	}
	cancelDone := make(chan struct{})
	var cancelErr error
	go func() {
		select {
		case <-ctx.Done():
			terminate(cmd.Process.Pid)
		case <-cancelDone:
		}
	}()
	err := cmd.Wait()
	close(cancelDone)
	if ctx.Err() != nil {
		cancelErr = ctx.Err()
	}
	result := Result{ExitCode: 0}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
		if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			result.Signaled = true
			result.Signal = status.Signal().String()
		}
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return result, err
	}
	if cancelErr != nil {
		return result, cancelErr
	}
	return result, nil
}

func terminate(pid int) {
	// Worker owns the process group and sends TERM then bounded KILL on cancellation.
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	timer := time.NewTimer(500 * time.Millisecond)
	<-timer.C
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

type LimitWriter struct {
	w         io.Writer
	max       int64
	n         int64
	truncated bool
}

func New(w io.Writer, max int64) *LimitWriter { return &LimitWriter{w: w, max: max} }
func (l *LimitWriter) Write(p []byte) (int, error) {
	if l.max >= 0 && l.n >= l.max {
		l.truncated = true
		return len(p), nil
	}
	allowed := p
	if l.max >= 0 && int64(len(allowed)) > l.max-l.n {
		allowed = allowed[:l.max-l.n]
		l.truncated = true
	}
	n, err := l.w.Write(allowed)
	l.n += int64(n)
	if len(allowed) < len(p) {
		l.truncated = true
		return len(p), err
	}
	return len(p), err
}
func (l *LimitWriter) Truncated() bool { return l.truncated }

type Pair struct {
	Stdout *LimitWriter
	Stderr *LimitWriter
}

func NewPair(stdout, stderr io.Writer, stdoutMax, stderrMax int64) Pair {
	return Pair{Stdout: New(stdout, stdoutMax), Stderr: New(stderr, stderrMax)}
}

type CancellationPhase string

const (
	CancellationNone        CancellationPhase = ""
	CancellationRequested   CancellationPhase = "requested"
	CancellationTerminating CancellationPhase = "terminating"
	CancellationReaped      CancellationPhase = "reaped"
	// CancellationKilled is legacy VCI (`006ae53`) metadata for forcibly-killed
	// runs, accepted only when loading old execution records.
	// Workers never emit this phase; active runs stay requested→terminating→reaped.
	CancellationKilled CancellationPhase = "killed"
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
	case CancellationNone, CancellationRequested, CancellationTerminating, CancellationReaped, CancellationKilled:
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
