package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/hypernewbie/vci/internal/app"
	"github.com/hypernewbie/vci/internal/model"
)

func Run(args []string, stdout, stderr io.Writer) int {
	command := ""
	if len(args) > 0 {
		command = args[0]
	}
	parsed, parseErr := Parse(args)
	if parseErr != nil {
		if command == "" {
			command = ""
		}
		if err := Write(stdout, Failure(command, parseErr)); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 2
	}
	response, exitCode := dispatch(parsed.Name, parsed.Args)
	if err := Write(stdout, response); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return exitCode
}

func dispatch(command string, args []string) (Response, int) {
	switch command {
	case "":
		return Failure(command, model.NewError("command_required", model.FailureUsage, "A command is required.", false)), 2
	case "setup":
		data, setupErr := runSetup(args)
		if setupErr != nil {
			return Failure(command, setupErr), 2
		}
		return Success(command, data), 0
	case "machines", "projects":
		l, err := resolveLayout()
		if err != nil {
			return appFailure(command, err), 2
		}
		inventory, err := app.ReadInventory(l)
		if err != nil {
			return appFailure(command, err), 2
		}
		if command == "machines" {
			return Success(command, inventory.Machines), 0
		}
		return Success(command, inventory.Projects), 0
	case "build":
		if len(args) != 1 {
			return Failure(command, model.NewError("invalid_arguments", model.FailureUsage, "Usage: build <path>.", false)), 2
		}
		l, err := resolveLayout()
		if err != nil {
			return appFailure(command, err), 2
		}
		prepared, err := app.Prepare(context.Background(), l, args[0])
		if err != nil {
			return appFailure(command, err), 2
		}
		if err := spawnRun(prepared.Record.ID); err != nil {
			return appFailure(command, err), 2
		}
		return Success(command, map[string]any{"run_id": prepared.Record.ID, "state": prepared.Record.State}), 0
	case "check":
		if len(args) != 1 || !validRunID(args[0]) {
			return Failure(command, model.NewError("invalid_arguments", model.FailureUsage, "Usage: check <run-id>.", false)), 2
		}
		l, err := resolveLayout()
		if err != nil {
			return appFailure(command, err), 2
		}
		result, err := app.Check(l, model.RunID(args[0]))
		if err != nil {
			return appFailure(command, err), 2
		}
		return Success(command, result), 0
	case "abort":
		if len(args) != 1 || !validRunID(args[0]) {
			return Failure(command, model.NewError("invalid_arguments", model.FailureUsage, "Usage: abort <run-id>.", false)), 2
		}
		l, err := resolveLayout()
		if err != nil {
			return appFailure(command, err), 2
		}
		result, err := app.Abort(l, model.RunID(args[0]))
		if err != nil {
			return appFailure(command, err), 2
		}
		return Success(command, result), 0
	case "internal-run":
		if len(args) != 1 || !validRunID(args[0]) {
			return Failure(command, model.NewError("invalid_arguments", model.FailureUsage, "Usage: internal-run <run-id>.", false)), 2
		}
		l, err := resolveLayout()
		if err != nil {
			return appFailure(command, err), 2
		}
		_, err = app.ExecutePrepared(context.Background(), l, model.RunID(args[0]))
		if err != nil {
			return appFailure(command, err), 2
		}
		return Success(command, map[string]any{"run_id": args[0], "completed": true}), 0
	default:
		return Failure(command, model.NewError("unknown_command", model.FailureUsage, fmt.Sprintf("Command %q is not recognized.", command), false)), 2
	}
}

func Main() {
	os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
}
