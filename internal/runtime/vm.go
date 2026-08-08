package runtime

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hypernewbie/vci/internal/executor"
	"github.com/hypernewbie/vci/internal/process"
)

// VM is the VM-runtime executor. It implements the same shape as
// executor.Local and runtime.Docker: a single ExecuteSupervised call
// that returns an executor.Result plus an error. The runner shells
// out to the system VM binary (`tart` on macOS, `lima` as fallback)
// without a Go SDK; the VM hypervisor is the host's responsibility,
// and Vci never builds, pulls, or pins VM images from inside Vci.
// The snapshot reference is verbatim (validated by
// config.ValidateMachineRuntime), so the substring is safe to forward
// as a positional argument.
//
// The workspace is shared read-write with the guest via virtiofs at
// `/vci/work`. Vci never mounts `~/.vci`, `state/`, or `~/.ssh`.
// Only the per-run workspace is exposed. No shell, no template
// interpolation: every argument is a literal string from the
// validated source. The guest runs as the host UID/GID.
type VM struct {
	Runner    process.Runner
	Snapshot  string
	Resources Resources
	// Binary is the VM runner executable name; the default is
	// "tart" on macOS. Tests can override the lookup.
	Binary string
}

// NewVM returns a VM runner for the documented snapshot. The
// binary defaults to `tart`; pass `Binary` to override (the
// controlled test fixture supplies its own stub).
func NewVM(snapshot string) VM {
	return VM{Snapshot: snapshot, Binary: "tart"}
}

// CommandArgv returns the documented VM invocation as an exact arg
// slice. The function is exported so tests can pin the shape without
// re-implementing the runner.
//
// The exact arg slice (subject to `Binary` substitution) is:
//
//	tart run --no-gui --dir <absWorkspace>:/vci/work \
//	    --workdir /vci/work --cpus 2 --memory 4g \
//	    <snapshot> -- <command...>
//
// The workspace is shared read-write with the guest through the
// documented tart directory mount (`--dir`); the guest's
// `/vci/work` is the workspace. The runner never opens any host
// path other than the workspace.
func (v VM) CommandArgv(workspace string, argv []string) ([]string, error) {
	return v.commandArgv(workspace, true, argv)
}

// CommandArgvRemote is the remote-host mirror of CommandArgv: the
// exact same arg shape, but the workspace path is used verbatim
// instead of being resolved with filepath.Abs, because the path names
// a directory on the remote host (reached via ssh), not the
// coordinator. The remote shell expands the unquoted `~` before the
// VM client sees the `--dir` mount source.
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

// ExecuteSupervised runs the project's command inside the VM. The
// interface matches executor.Local so the build path can select the
// runner without knowing the shape.
func (v VM) ExecuteSupervised(ctx context.Context, request executor.Request, onStart func(process.Running) error) (executor.Result, error) {
	if request.Workspace == "" {
		return executor.Result{}, fmt.Errorf("workspace is required")
	}
	argv := append([]string{request.Executable}, request.Args...)
	args, err := v.CommandArgv(request.Workspace, argv)
	if err != nil {
		return executor.Result{}, err
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
	result := executor.Result{
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
			// VM runners report a missing/invalid image via a
			// non-zero exit, not via the OS-exec level. Treat as
			// a job failure (the build path classifies by
			// ExitCode); the snapshot not-found case is mapped
			// by the caller via substring classification until
			// a stable code is observed.
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
