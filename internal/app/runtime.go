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
// ⇒ `runtime.Docker` with the snapshot's image. Anything else
// was rejected at config load time, so the default branch is
// unreachable in practice. The fallback to ProjectConfig.Machines[0]
// exists for legacy records that predate the explicit machine
// field; current writers always populate `machine`.
func selectExecutor(snapshot runSnapshot) Executor {
	name := snapshot.Machine
	if name == "" && len(snapshot.ProjectConfig.Machines) > 0 {
		name = snapshot.ProjectConfig.Machines[0]
	}
	machine := lookupMachine(snapshot, name)
	if machine.Runtime == "docker" {
		return runtime.Docker{Image: machine.Image}
	}
	return executor.Local{}
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
