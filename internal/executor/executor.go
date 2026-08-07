package executor

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/hypernewbie/vci/internal/process"
)

type Request struct {
	Executable  string
	Args        []string
	Workspace   string
	Home        string
	Temp        string
	Environment map[string]string
	Stdout      io.Writer
	Stderr      io.Writer
}
type Result struct {
	ExitCode           int           `json:"exit_code"`
	Signaled           bool          `json:"signaled"`
	Signal             string        `json:"signal,omitempty"`
	ResolvedExecutable string        `json:"resolved_executable"`
	StartedAt          time.Time     `json:"started_at"`
	FinishedAt         time.Time     `json:"finished_at"`
	Duration           time.Duration `json:"duration_ns"`
}
type Local struct{ Runner process.Runner }

func (l Local) ExecuteSupervised(ctx context.Context, request Request, onStart func(process.Running) error) (Result, error) {
	return l.execute(ctx, request, onStart)
}

func (l Local) execute(ctx context.Context, request Request, onStart func(process.Running) error) (Result, error) {
	if request.Workspace == "" {
		return Result{}, fmt.Errorf("workspace is required")
	}
	resolved, err := exec.LookPath(request.Executable)
	if err != nil {
		return Result{}, fmt.Errorf("resolve executable %q: %w", request.Executable, err)
	}
	if request.Home == "" {
		request.Home = filepath.Join(request.Workspace, ".home")
	}
	if request.Temp == "" {
		request.Temp = filepath.Join(request.Workspace, ".tmp")
	}
	if err := os.MkdirAll(request.Home, 0o700); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(request.Temp, 0o700); err != nil {
		return Result{}, err
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + request.Home, "TMPDIR=" + request.Temp}
	for key, value := range request.Environment {
		env = append(env, key+"="+value)
	}
	started := time.Now().UTC()
	runner := l.Runner
	if runner == nil {
		runner = process.Native{}
	}
	command := process.Command{Executable: resolved, Args: request.Args, Dir: request.Workspace, Env: env, Stdout: request.Stdout, Stderr: request.Stderr}
	var processResult process.Result
	var runErr error
	if supervised, ok := runner.(process.SupervisedRunner); ok {
		processResult, runErr = supervised.RunSupervised(ctx, command, onStart)
	} else {
		if onStart != nil {
			onStart(process.Running{StartedAt: started})
		}
		processResult, runErr = runner.Run(ctx, command)
	}
	finished := time.Now().UTC()
	result := Result{ExitCode: processResult.ExitCode, Signaled: processResult.Signaled, Signal: processResult.Signal, ResolvedExecutable: resolved, StartedAt: started, FinishedAt: finished, Duration: finished.Sub(started)}
	if runErr != nil && result.ExitCode == 0 {
		return result, runErr
	}
	return result, nil
}
