package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hypernewbie/vci/internal/executor"
	"github.com/hypernewbie/vci/internal/process"
)

// ErrRuntimeUnavailable is the typed sentinel for a runtime that
// is selected but cannot be launched (the system binary is
// missing, the image cannot be resolved, the container runtime
// refuses). Class infrastructure, retryable true.
var ErrRuntimeUnavailable = fmt.Errorf("runtime_unavailable")

// ErrRuntimeImageNotFound is the typed sentinel for a runtime
// invocation that the host container runtime refuses because the
// image is unknown. Class configuration, retryable false.
var ErrRuntimeImageNotFound = fmt.Errorf("runtime_image_not_found")

// Docker is the docker-runtime executor. It implements the
// same shape as executor.Local: a single ExecuteSupervised call
// that returns an executor.Result plus an error. The runner
// shells out to the system `docker` binary without a Go SDK;
// the docker daemon is the host's responsibility, and Vci never
// builds, pulls, or pins images from inside Vci. The image
// reference is verbatim (validated by config.ValidateMachineRuntime),
// so the substring is safe to forward as a positional argument.
//
// The workspace is bind-mounted read-only at /vci/work. The
// command is composed as a normal `docker run --rm -v
// <workspace>:/vci/work:ro -w /vci/work --network none --user
// <uid>:<gid> --cpus 2 --memory 4g <image> <command...>` invocation.
// No shell, no template interpolation: every argument is a
// literal string from the validated source. Per-run cgroup
// limits are conservative defaults; a future slice may surface
// them as machine config.
type Docker struct {
	Runner    process.Runner
	Image     string
	Resources Resources
}

// Resources is the bounded cgroup policy applied to every docker
// invocation. The default is conservative; operators can override
// per machine in a future slice.
type Resources struct {
	CPUs   int
	Memory string // docker --memory argument, e.g. "4g"
}

// DefaultResources returns the documented conservative defaults.
func DefaultResources() Resources {
	return Resources{CPUs: 2, Memory: "4g"}
}

// CommandArgv returns the documented docker invocation as an
// exact arg slice. The function is exported so tests can pin the
// shape without re-implementing the runner.
func (d Docker) CommandArgv(workspace, workdir string, argv []string) ([]string, error) {
	if d.Image == "" {
		return nil, fmt.Errorf("%w: image is empty", ErrRuntimeUnavailable)
	}
	if workspace == "" {
		return nil, fmt.Errorf("%w: workspace is empty", ErrRuntimeUnavailable)
	}
	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve workspace: %v", ErrRuntimeUnavailable, err)
	}
	uid, gid := currentUIDGID()
	args := []string{
		"run", "--rm",
		"-v", absWorkspace + ":/vci/work:ro",
		"-w", "/vci/work",
		"--network", "none",
		"--user", fmt.Sprintf("%d:%d", uid, gid),
	}
	res := d.Resources
	if res.CPUs <= 0 {
		res.CPUs = DefaultResources().CPUs
	}
	if res.Memory == "" {
		res.Memory = DefaultResources().Memory
	}
	args = append(args, "--cpus", fmt.Sprintf("%d", res.CPUs))
	args = append(args, "--memory", res.Memory)
	args = append(args, d.Image)
	args = append(args, argv...)
	return args, nil
}

// ExecuteSupervised runs the project's command inside the docker
// container. The interface matches executor.Local so the build
// path can select the runner without knowing the shape.
func (d Docker) ExecuteSupervised(ctx context.Context, request executor.Request, onStart func(process.Running) error) (executor.Result, error) {
	if request.Workspace == "" {
		return executor.Result{}, fmt.Errorf("workspace is required")
	}
	argv := append([]string{request.Executable}, request.Args...)
	dockerArgs, err := d.CommandArgv(request.Workspace, "/vci/work", argv)
	if err != nil {
		return executor.Result{}, err
	}
	runner := d.Runner
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
		Executable: "docker",
		Args:       dockerArgs,
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
	resolved := "docker"
	result := executor.Result{
		ExitCode:           processResult.ExitCode,
		Signaled:           processResult.Signaled,
		Signal:             processResult.Signal,
		ResolvedExecutable: resolved,
		StartedAt:          started,
		FinishedAt:         finished,
		Duration:           finished.Sub(started),
	}
	if runErr != nil {
		// Classify the failure. docker exits with a specific
		// code for "image not found" (125) and the daemon
		// refusal is a separate signature. A non-zero exit
		// without a process spawn error is a job failure that
		// the build path already classifies via ExitCode.
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			if exitErr.ExitCode() == 125 {
				return result, fmt.Errorf("%w: %v", ErrRuntimeImageNotFound, runErr)
			}
			// Any other exec.ExitError is a job failure: the
			// docker binary ran, the container ran, the command
			// returned a non-zero exit. Treat as a job-level
			// failure, not infrastructure.
			return result, nil
		}
		// `docker` not in PATH is a different infrastructure
		// failure: the host cannot launch the runtime at all.
		if strings.Contains(runErr.Error(), "executable file not found") {
			return result, fmt.Errorf("%w: docker binary not found in PATH", ErrRuntimeUnavailable)
		}
		return result, fmt.Errorf("%w: %v", ErrRuntimeUnavailable, runErr)
	}
	if result.ExitCode != 0 {
		// Treat non-zero exit the same way the local executor
		// does: a job failure, not an infrastructure failure.
		// The build path uses ExitCode to classify the result.
		return result, nil
	}
	return result, nil
}

// currentUIDGID returns the coordinator host's UID and GID. The
// docker --user flag matches the running process so the
// container's filesystem writes map to the same host identity.
// os.Getuid/os.Getgid are stdlib calls and never fail on macOS,
// the supported target.
func currentUIDGID() (int, int) {
	return os.Getuid(), os.Getgid()
}
