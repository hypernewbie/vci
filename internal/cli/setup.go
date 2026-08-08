package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/hypernewbie/vci/internal/app"
	"github.com/hypernewbie/vci/internal/config"
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
			return nil, model.NewError("invalid_arguments", model.FailureUsage, "Usage: setup machine add|remove <name> [--capacity <positive-int>] [--host <ssh-destination>] [--runtime <docker|vm>] [--image <ref>] [--snapshot <ref>].", false)
		}
		switch args[1] {
		case "add":
			name := ""
			machine := config.Machine{}
			i := 2
			if i >= len(args) {
				return nil, model.NewError("invalid_arguments", model.FailureUsage, "Usage: setup machine add <name> [--capacity <positive-int>] [--host <ssh-destination>] [--runtime <docker|vm>] [--image <ref>] [--snapshot <ref>].", false)
			}
			name = args[i]
			i++
			for ; i < len(args); i++ {
				switch args[i] {
				case "--capacity":
					if i+1 >= len(args) {
						return nil, model.NewError("invalid_arguments", model.FailureUsage, "--capacity requires a positive integer.", false)
					}
					n, parseErr := strconv.Atoi(args[i+1])
					if parseErr != nil || n <= 0 {
						return nil, model.NewError("invalid_arguments", model.FailureUsage, "--capacity must be a positive integer.", false)
					}
					machine.MaxConcurrent = n
					i++
				case "--host":
					if i+1 >= len(args) {
						return nil, model.NewError("invalid_arguments", model.FailureUsage, "--host requires an ssh destination.", false)
					}
					machine.Host = args[i+1]
					if err := config.ValidateMachineHost(machine.Host); err != nil {
						return nil, model.NewError("invalid_arguments", model.FailureUsage, err.Error(), false)
					}
					i++
				case "--runtime":
					if i+1 >= len(args) {
						return nil, model.NewError("invalid_arguments", model.FailureUsage, "--runtime requires a value (docker, or empty for bare).", false)
					}
					machine.Runtime = args[i+1]
					i++
				case "--image":
					if i+1 >= len(args) {
						return nil, model.NewError("invalid_arguments", model.FailureUsage, "--image requires an image reference.", false)
					}
					machine.Image = args[i+1]
					i++
				case "--snapshot":
					if i+1 >= len(args) {
						return nil, model.NewError("invalid_arguments", model.FailureUsage, "--snapshot requires a snapshot reference.", false)
					}
					machine.Snapshot = args[i+1]
					i++
				default:
					return nil, model.NewError("invalid_arguments", model.FailureUsage, fmt.Sprintf("Unknown machine option %q.", args[i]), false)
				}
			}
			if err := app.AddMachine(l, name, machine); err != nil {
				return nil, model.NewError("machine_update_failed", model.FailureConfiguration, err.Error(), false)
			}
			result := map[string]any{"machine": name, "updated": true}
			if machine.MaxConcurrent > 0 {
				result["max_concurrent"] = machine.MaxConcurrent
			}
			if machine.Host != "" {
				result["host"] = machine.Host
			}
			if machine.Runtime != "" {
				result["runtime"] = machine.Runtime
			}
			if machine.Image != "" {
				result["image"] = machine.Image
			}
			if machine.Snapshot != "" {
				result["snapshot"] = machine.Snapshot
			}
			return result, nil
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
		if len(args) < 3 || (args[1] != "add" && args[1] != "hosted") {
			return nil, model.NewError("invalid_arguments", model.FailureUsage, "Usage: setup project add <name> --machine <name> [--machine <name>...] --command <exe> [--arg <arg>] [--artifact <glob>].\n       setup project hosted set <name> --url <url> --commit <object-id>\n       setup project hosted clear <name>", false)
		}
		if args[1] == "hosted" {
			return runSetupProjectHosted(l, args[2:])
		}
		var machines []string
		executable := ""
		commandArgs := []string{}
		var artifacts []string
		for i := 3; i < len(args); i++ {
			switch args[i] {
			case "--machine":
				if i+1 >= len(args) {
					return nil, model.NewError("invalid_arguments", model.FailureUsage, "--machine requires a name.", false)
				}
				machines = append(machines, args[i+1])
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
			case "--artifact":
				if i+1 >= len(args) {
					return nil, model.NewError("invalid_arguments", model.FailureUsage, "--artifact requires a glob.", false)
				}
				artifacts = append(artifacts, args[i+1])
				i++
			default:
				return nil, model.NewError("invalid_arguments", model.FailureUsage, fmt.Sprintf("Unknown project option %q.", args[i]), false)
			}
		}
		if len(machines) == 0 || executable == "" {
			return nil, model.NewError("invalid_arguments", model.FailureUsage, "Project requires --machine and --command.", false)
		}
		if err := app.AddProject(l, args[2], config.Project{Machines: machines, Command: append([]string{executable}, commandArgs...), Artifacts: artifacts}); err != nil {
			return nil, model.NewError("project_update_failed", model.FailureConfiguration, err.Error(), false)
		}
		out := map[string]any{"project": args[2], "machines": machines, "command": append([]string{executable}, commandArgs...), "updated": true}
		if len(artifacts) > 0 {
			out["artifacts"] = artifacts
		}
		return out, nil
	default:
		return nil, model.NewError("invalid_arguments", model.FailureUsage, fmt.Sprintf("Unknown setup operation %q.", args[0]), false)
	}
}

// runSetupProjectHosted handles `setup project hosted set|clear <name>`.
// Both subcommands mutate only a coordinator root via the
// config.Mutate-backed app helpers. The --url and --commit values are
// validated through HostedFallback.Validate before any disk write.
func runSetupProjectHosted(l model.Layout, args []string) (any, *model.VciError) {
	if len(args) < 1 {
		return nil, model.NewError("invalid_arguments", model.FailureUsage, "Usage: setup project hosted set <name> --url <url> --commit <object-id>\n       setup project hosted clear <name>", false)
	}
	switch args[0] {
	case "set":
		if len(args) < 2 {
			return nil, model.NewError("invalid_arguments", model.FailureUsage, "Usage: setup project hosted set <name> --url <url> --commit <object-id>", false)
		}
		name := args[1]
		url := ""
		commit := ""
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--url":
				if i+1 >= len(args) {
					return nil, model.NewError("invalid_arguments", model.FailureUsage, "--url requires a value.", false)
				}
				url = args[i+1]
				i++
			case "--commit":
				if i+1 >= len(args) {
					return nil, model.NewError("invalid_arguments", model.FailureUsage, "--commit requires a value.", false)
				}
				commit = args[i+1]
				i++
			default:
				return nil, model.NewError("invalid_arguments", model.FailureUsage, fmt.Sprintf("Unknown hosted option %q.", args[i]), false)
			}
		}
		if url == "" || commit == "" {
			return nil, model.NewError("invalid_arguments", model.FailureUsage, "hosted set requires --url and --commit.", false)
		}
		if err := app.SetHostedFallback(l, name, url, commit); err != nil {
			if errors.Is(err, config.ErrHostedFallbackInvalid) {
				return nil, model.NewError("hosted_fallback_invalid", model.FailureConfiguration, err.Error(), false)
			}
			return nil, model.NewError("hosted_update_failed", model.FailureConfiguration, err.Error(), false)
		}
		return map[string]any{"project": name, "hosted_url": url, "hosted_commit": commit, "updated": true}, nil
	case "clear":
		if len(args) != 2 {
			return nil, model.NewError("invalid_arguments", model.FailureUsage, "Usage: setup project hosted clear <name>.", false)
		}
		if err := app.ClearHostedFallback(l, args[1]); err != nil {
			return nil, model.NewError("hosted_update_failed", model.FailureConfiguration, err.Error(), false)
		}
		return map[string]any{"project": args[1], "cleared": true}, nil
	default:
		return nil, model.NewError("invalid_arguments", model.FailureUsage, fmt.Sprintf("Unknown hosted operation %q.", args[0]), false)
	}
}

// requireCoordinatorRole returns an error when this root does not declare
// orchestrator = "self", which is the only role permitted to mutate
// coordinator state through setup.
func requireCoordinatorRole(l model.Layout) *model.VciError {
	if err := app.RequireCoordinatorRole(l); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return model.NewError("setup_not_initialized", model.FailureConfiguration,
				"Run \"vci setup init\" first.", false)
		}
		return model.NewError("setup_not_coordinator", model.FailureConfiguration, err.Error(), false)
	}
	return nil
}
