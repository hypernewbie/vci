package cli

import (
	"fmt"

	"github.com/hypernewbie/vci/internal/model"
)

type Command struct {
	Name string
	Args []string
}

func Parse(args []string) (Command, *model.VciError) {
	if len(args) == 0 {
		return Command{}, model.NewError("command_required", model.FailureUsage, "A command is required.", false)
	}
	name := args[0]
	switch name {
	case "build", "check", "abort", "projects", "machines", "setup", "internal-run", "artifacts", "logs":
		return Command{Name: name, Args: append([]string(nil), args[1:]...)}, nil
	default:
		return Command{}, model.NewError("unknown_command", model.FailureUsage, fmt.Sprintf("Command %q is not recognized.", name), false)
	}
}
