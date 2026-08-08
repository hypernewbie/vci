package app

import (
	"context"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/executor"
	"github.com/hypernewbie/vci/internal/process"
	"github.com/hypernewbie/vci/internal/runtime"
)

// Executor is the build-path runtime interface. The default
// coordinator `executor.Local` satisfies it; the docker runtime
// satisfies it through `runtime.Docker`. The interface is the
// seam the durable snapshot selects from; result remains
// explainable after config changes.
type Executor interface {
	ExecuteSupervised(ctx context.Context, request executor.Request, onStart func(process.Running) error) (executor.Result, error)
}

// selectExecutor returns the runtime that matches the durable
// snapshot's machine runtime. The selection is read from the
// snapshot's reserved machine, not from live config, so a run
// whose machine config changes after submission still publishes
// the runtime it was actually launched with.
//
// Empty runtime ⇒ bare host (`executor.Local`). docker runtime
// ⇒ `runtime.Docker` with the snapshot's image. vm runtime ⇒
// `runtime.VM` with the snapshot's snapshot reference. Anything
// else was rejected at config load time, so the default branch
// is unreachable in practice. The fallback to
// ProjectConfig.Machines[0] exists for legacy records that
// predate the explicit machine field; current writers always
// populate `machine`.
func selectExecutor(snapshot runSnapshot) Executor {
	machine := resolvedMachine(snapshot)
	switch machine.Runtime {
	case "docker":
		return runtime.Docker{Image: machine.Image}
	case "vm":
		return runtime.VM{Snapshot: machine.Snapshot, Binary: "tart"}
	}
	return executor.Local{}
}

// resolvedMachine returns the machine the durable snapshot reserved,
// falling back to ProjectConfig.Machines[0] for legacy records that
// predate the explicit machine field. It is the single name
// resolution shared by the local executor selection and the remote
// host branch.
func resolvedMachine(snapshot runSnapshot) config.Machine {
	name := snapshot.Machine
	if name == "" && len(snapshot.ProjectConfig.Machines) > 0 {
		name = snapshot.ProjectConfig.Machines[0]
	}
	return lookupMachine(snapshot, name)
}

// remoteArgv composes the argv the remote host executes for a machine
// with a non-empty Host. The runtime selection mirrors selectExecutor:
// docker and vm reuse the runtime packages' CommandArgvRemote (the
// exact documented arg shape with the workspace used verbatim — it
// names a path on the remote host, so no local filepath.Abs), and the
// bare host runs the project command directly. The docker argv is
// prefixed with the `docker` binary name so the remote shell can exec
// it; VM's argv already carries its binary.
func remoteArgv(machine config.Machine, remoteWorkDir string, command []string) ([]string, error) {
	switch machine.Runtime {
	case "docker":
		args, err := (runtime.Docker{Image: machine.Image}).CommandArgvRemote(remoteWorkDir, command)
		if err != nil {
			return nil, err
		}
		return append([]string{"docker"}, args...), nil
	case "vm":
		return (runtime.VM{Snapshot: machine.Snapshot, Binary: "tart"}).CommandArgvRemote(remoteWorkDir, command)
	}
	return command, nil
}

// lookupMachine returns the resolved machineconfig struct for the
// named machine, or the zero value if the snapshot does not
// carry it. The snapshot is the durable view; live config changes
// do not retroactively rewrite historical runs.
func lookupMachine(snapshot runSnapshot, machineName string) config.Machine {
	if machineName == "" {
		return config.Machine{}
	}
	if machine, ok := snapshot.Machines[machineName]; ok {
		return machine
	}
	return config.Machine{}
}
