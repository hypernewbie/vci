package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/hypernewbie/vci/internal/app"
	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/model"
)

func runSetup(args []string) (any, *model.VciError) {
	if len(args) == 0 {
		return nil, model.NewError("setup_command_required", model.FailureUsage, "A setup operation is required.", false)
	}
	l, err := resolveLayout()
	if err != nil {
		return nil, model.NewError("setup_failed", model.FailureConfiguration, err.Error(), false)
	}
	switch args[0] {
	case "reap":
		if errMsg := requireCoordinatorRole(l); errMsg != nil {
			return nil, errMsg
		}
		if len(args) != 1 {
			return nil, model.NewError("invalid_arguments", model.FailureUsage, "setup reap takes no arguments.", false)
		}
		report, err := app.Maintain(l)
		if err != nil {
			return nil, model.NewError("maintenance_failed", model.FailureState, err.Error(), true)
		}
		return report, nil
	case "init":
		if len(args) != 1 {
			return nil, model.NewError("invalid_arguments", model.FailureUsage, "setup init takes no arguments.", false)
		}
		if err := app.Initialize(l); err != nil {
			return nil, model.NewError("setup_failed", model.FailureConfiguration, err.Error(), false)
		}
		return map[string]any{"initialized": true}, nil
	case "machine":
		if errMsg := requireCoordinatorRole(l); errMsg != nil {
			return nil, errMsg
		}
		if len(args) < 3 {
			return nil, model.NewError("invalid_arguments", model.FailureUsage, "Usage: setup machine add|remove <name>.", false)
		}
		switch args[1] {
		case "add":
			if len(args) != 3 {
				return nil, model.NewError("invalid_arguments", model.FailureUsage, "Usage: setup machine add <name>.", false)
			}
			if err := app.AddMachine(l, args[2], config.Machine{}); err != nil {
				return nil, model.NewError("machine_update_failed", model.FailureConfiguration, err.Error(), false)
			}
		case "remove":
			if len(args) != 3 {
				return nil, model.NewError("invalid_arguments", model.FailureUsage, "Usage: setup machine remove <name>.", false)
			}
			if err := app.RemoveMachine(l, args[2]); err != nil {
				return nil, model.NewError("machine_update_failed", model.FailureConfiguration, err.Error(), false)
			}
		default:
			return nil, model.NewError("invalid_arguments", model.FailureUsage, "Machine operation must be add or remove.", false)
		}
		return map[string]any{"machine": args[2], "updated": true}, nil
	case "project":
		if errMsg := requireCoordinatorRole(l); errMsg != nil {
			return nil, errMsg
		}
		if len(args) < 3 || args[1] != "add" {
			return nil, model.NewError("invalid_arguments", model.FailureUsage, "Usage: setup project add <name> --machine <name> --command <exe> [--arg <arg>].", false)
		}
		machine, executable := "", ""
		commandArgs := []string{}
		for i := 3; i < len(args); i++ {
			switch args[i] {
			case "--machine":
				if i+1 >= len(args) {
					return nil, model.NewError("invalid_arguments", model.FailureUsage, "--machine requires a name.", false)
				}
				machine = args[i+1]
				i++
			case "--command":
				if i+1 >= len(args) {
					return nil, model.NewError("invalid_arguments", model.FailureUsage, "--command requires a name.", false)
				}
				executable = args[i+1]
				i++
			case "--arg":
				if i+1 >= len(args) {
					return nil, model.NewError("invalid_arguments", model.FailureUsage, "--arg requires a value.", false)
				}
				commandArgs = append(commandArgs, args[i+1])
				i++
			default:
				return nil, model.NewError("invalid_arguments", model.FailureUsage, fmt.Sprintf("Unknown project option %q.", args[i]), false)
			}
		}
		if machine == "" || executable == "" {
			return nil, model.NewError("invalid_arguments", model.FailureUsage, "Project requires --machine and --command.", false)
		}
		if err := app.AddProject(l, args[2], config.Project{Machines: []string{machine}, Command: append([]string{executable}, commandArgs...)}); err != nil {
			return nil, model.NewError("project_update_failed", model.FailureConfiguration, err.Error(), false)
		}
		return map[string]any{"project": args[2], "command": append([]string{executable}, commandArgs...), "updated": true}, nil
	default:
		return nil, model.NewError("invalid_arguments", model.FailureUsage, fmt.Sprintf("Unknown setup operation %q.", args[0]), false)
	}
}

func validRunID(value string) bool { return strings.HasPrefix(value, "run_") }

// requireCoordinatorRole returns an error when this root does not declare
// orchestrator = "self", which is the only role permitted to mutate
// coordinator state through setup.
func requireCoordinatorRole(l layout.Layout) *model.VciError {
	if err := app.RequireCoordinatorRole(l); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return model.NewError("setup_not_initialized", model.FailureConfiguration,
				"Run \"vci setup init\" first.", false)
		}
		return model.NewError("setup_not_coordinator", model.FailureConfiguration, err.Error(), false)
	}
	return nil
}
