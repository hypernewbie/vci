// Package runtime defines executors for local, docker, and vm runs.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"github.com/hypernewbie/vci/internal/process"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrRuntimeUnavailable indicates runtime startup failure (binary or launch issues).
var ErrRuntimeUnavailable = fmt.Errorf("runtime_unavailable")

// ErrRuntimeImageNotFound means the docker image was not found.
var ErrRuntimeImageNotFound = fmt.Errorf("runtime_image_not_found")

// Docker runs commands in a container via the docker binary.
// Uses exact argv without shell expansion; mounts workspace read-only at /vci/work.
type Docker struct {
	Runner    process.Runner
	Image     string
	Resources Resources
}

// Resources configures docker limits for one run.
type Resources struct {
	CPUs   int
	Memory string // docker --memory argument, e.g. "4g"
}

// DefaultResources returns built-in docker limits.
func DefaultResources() Resources {
	return Resources{CPUs: 2, Memory: "4g"}
}

// CommandArgv returns the docker argument vector for local execution.
func (d Docker) CommandArgv(workspace, workdir string, argv []string) ([]string, error) {
	return d.commandArgv(workspace, true, argv)
}

// CommandArgvRemote mirrors CommandArgv for remote hosts, using workspace as provided.
func (d Docker) CommandArgvRemote(workspace string, argv []string) ([]string, error) {
	return d.commandArgv(workspace, false, argv)
}

func (d Docker) commandArgv(workspace string, abs bool, argv []string) ([]string, error) {
	if d.Image == "" {
		return nil, fmt.Errorf("%w: image is empty", ErrRuntimeUnavailable)
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
	uid, gid := currentUIDGID()
	args := []string{
		"run", "--rm",
		"-v", ws + ":/vci/work:ro",
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

// ExecuteSupervised runs the command in docker; signature matches Local runtime.
func (d Docker) ExecuteSupervised(ctx context.Context, request Request, onStart func(process.Running) error) (Result, error) {
	if request.Workspace == "" {
		return Result{}, fmt.Errorf("workspace is required")
	}
	argv := append([]string{request.Executable}, request.Args...)
	dockerArgs, err := d.CommandArgv(request.Workspace, "/vci/work", argv)
	if err != nil {
		return Result{}, err
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
	}
	// Capture bounded stderr tail for exit-125 classification while still
	// streaming full stderr to caller.
	var stderrTail *stderrRecorder
	if request.Stderr != nil {
		stderrTail = &stderrRecorder{w: request.Stderr, max: maxClassifyStderr}
		command.Stderr = stderrTail
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
	result := Result{
		ExitCode:           processResult.ExitCode,
		Signaled:           processResult.Signaled,
		Signal:             processResult.Signal,
		ResolvedExecutable: resolved,
		StartedAt:          started,
		FinishedAt:         finished,
		Duration:           finished.Sub(started),
	}
	if runErr != nil {
		// Classify failures: exit 125 indicates runtime launch failure;
		// non-125 exit errors are treated as container exit status.
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			if exitErr.ExitCode() == 125 {
				if imageNotFoundStderr(stderrTail) {
					return result, fmt.Errorf("%w: %v", ErrRuntimeImageNotFound, runErr)
				}
				return result, fmt.Errorf("%w: %v", ErrRuntimeUnavailable, runErr)
			}
			// Non-125 exec.ExitError means container command failure.
			// Treat as job-level, not infrastructure.
			return result, nil
		}
		// Missing docker executable in PATH is infrastructure failure.
		if strings.Contains(runErr.Error(), "executable file not found") {
			return result, fmt.Errorf("%w: docker binary not found in PATH", ErrRuntimeUnavailable)
		}
		return result, fmt.Errorf("%w: %v", ErrRuntimeUnavailable, runErr)
	}
	if result.ExitCode != 0 {
		// Non-zero exit is a job failure, consistent with local execution.
		return result, nil
	}
	return result, nil
}

// maxClassifyStderr limits captured stderr for exit-125 daemon-failure detection.
const maxClassifyStderr = 64 * 1024

// stderrRecorder forwards stderr and keeps a bounded tail for
// classifying docker exit-125 daemon errors.
type stderrRecorder struct {
	w   io.Writer
	buf []byte
	max int
}

func (r *stderrRecorder) Write(p []byte) (int, error) {
	n, err := r.w.Write(p)
	if n > 0 {
		r.record(p[:n])
	}
	return n, err
}

func (r *stderrRecorder) record(p []byte) {
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		r.buf = append([]byte(nil), r.buf[len(r.buf)-r.max:]...)
	}
}

func (r *stderrRecorder) String() string { return string(r.buf) }

// imageNotFoundSignatures lists daemon phrases indicating missing images.
var imageNotFoundSignatures = []string{
	"no such image",
	"manifest unknown",
	"pull access denied",
	"repository does not exist",
}

// imageNotFoundStderr checks captured stderr for image-not-found signatures.
func imageNotFoundStderr(recorder *stderrRecorder) bool {
	if recorder == nil || len(recorder.buf) == 0 {
		return false
	}
	lower := strings.ToLower(recorder.String())
	for _, sig := range imageNotFoundSignatures {
		if strings.Contains(lower, sig) {
			return true
		}
	}
	return false
}

// currentUIDGID returns host UID/GID for docker --user mapping.
func currentUIDGID() (int, int) {
	return os.Getuid(), os.Getgid()
}

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
