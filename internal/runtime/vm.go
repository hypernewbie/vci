package runtime

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hypernewbie/vci/internal/process"
)

// VM runs the VM binary (default tart) in a supervised call.
// Workspace is mounted read-write at /vci/work; snapshot and host args are validated.
type VM struct {
	Runner    process.Runner
	Snapshot  string
	Resources Resources
	// Binary is the VM runner binary; default is "tart" on macOS.
	Binary string
}

// CommandArgv builds VM arguments.
// Format is fixed:
//
//	tart run --no-gui --dir <workspace>:/vci/work --workdir /vci/work
//	--cpus <n> --memory <size> <snapshot> -- <command...>
func (v VM) CommandArgv(workspace string, argv []string) ([]string, error) {
	return v.commandArgv(workspace, true, argv)
}

// CommandArgvRemote mirrors CommandArgv for remote hosts.
// Workspace is used verbatim (no filepath.Abs), since it targets a
// remote directory reached via SSH. The remote shell expands `~` before
// the VM mount is processed.
func (v VM) CommandArgvRemote(workspace string, argv []string) ([]string, error) {
	return v.commandArgv(workspace, false, argv)
}

func (v VM) commandArgv(workspace string, abs bool, argv []string) ([]string, error) {
	if v.Snapshot == "" {
		return nil, fmt.Errorf("%w: snapshot is empty", ErrRuntimeUnavailable)
	}
	if workspace == "" {
		return nil, fmt.Errorf("%w: workspace is empty", ErrRuntimeUnavailable)
	}
	ws := workspace
	if abs {
		resolved, err := filepath.Abs(workspace)
		if err != nil {
			return nil, fmt.Errorf("%w: resolve workspace: %v", ErrRuntimeUnavailable, err)
		}
		ws = resolved
	}
	binary := v.Binary
	if binary == "" {
		binary = "tart"
	}
	res := v.resourcesOrDefault()
	args := []string{
		"run", "--no-gui",
		"--dir", ws + ":/vci/work",
		"--workdir", "/vci/work",
		"--cpus", fmt.Sprintf("%d", res.CPUs),
		"--memory", res.Memory,
	}
	args = append(args, v.Snapshot, "--")
	args = append(args, argv...)
	return prependBinary(binary, args), nil
}

func prependBinary(binary string, args []string) []string {
	out := make([]string, 0, len(args)+1)
	out = append(out, binary)
	out = append(out, args...)
	return out
}

// ExecuteSupervised runs the project command in the VM.
// It matches runtime.Local's interface so runtimes can be selected uniformly.
func (v VM) ExecuteSupervised(ctx context.Context, request Request, onStart func(process.Running) error) (Result, error) {
	if request.Workspace == "" {
		return Result{}, fmt.Errorf("workspace is required")
	}
	argv := append([]string{request.Executable}, request.Args...)
	args, err := v.CommandArgv(request.Workspace, argv)
	if err != nil {
		return Result{}, err
	}
	binary := v.Binary
	if binary == "" {
		binary = "tart"
	}
	runner := v.Runner
	if runner == nil {
		runner = process.Native{}
	}
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=/tmp",
		"TMPDIR=/tmp",
	}
	for key, value := range request.Environment {
		env = append(env, key+"="+value)
	}
	started := time.Now().UTC()
	command := process.Command{
		Executable: binary,
		Args:       args[1:],
		Dir:        request.Workspace,
		Env:        env,
		Stdout:     request.Stdout,
		Stderr:     request.Stderr,
	}
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
	result := Result{
		ExitCode:           processResult.ExitCode,
		Signaled:           processResult.Signaled,
		Signal:             processResult.Signal,
		ResolvedExecutable: binary,
		StartedAt:          started,
		FinishedAt:         finished,
		Duration:           finished.Sub(started),
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			// VM runners surface missing/invalid images as non-zero exits.
			// Treat as job failures (classified by exit code), while the
			// caller maps snapshot-not-found by message until stable codes
			// are available.
			return result, nil
		}
		if strings.Contains(runErr.Error(), "executable file not found") {
			return result, fmt.Errorf("%w: %s binary not found in PATH", ErrRuntimeUnavailable, binary)
		}
		return result, fmt.Errorf("%w: %v", ErrRuntimeUnavailable, runErr)
	}
	return result, nil
}

func (v VM) resourcesOrDefault() Resources {
	res := v.Resources
	if res.CPUs <= 0 {
		res.CPUs = DefaultResources().CPUs
	}
	if res.Memory == "" {
		res.Memory = DefaultResources().Memory
	}
	return res
}
