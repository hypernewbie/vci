package cli

import (
	"context"
	"encoding/json"
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
		if err := Write(stdout, Failure(command, parseErr)); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 2
	}
	// `vci artifacts get <run-id> <rel>` is the first non-JSON stdout
	// path: the artifact's raw bytes stream directly so binary content
	// survives. Every other command, including `artifacts ls`, uses
	// the JSON envelope.
	if parsed.Name == "artifacts" && len(parsed.Args) >= 1 && parsed.Args[0] == "get" {
		return runArtifactsGet(parsed.Args, stdout, stderr)
	}
	// `vci logs <run-id> [--stderr] [--tail <n>]` is the second
	// non-JSON stdout path: the selected stream's durable log bytes
	// stream directly so binary or garbled output survives. Every
	// failure still returns a JSON envelope.
	if parsed.Name == "logs" {
		return runLogs(parsed.Args, stdout, stderr)
	}
	if parsed.Name == "watch" {
		return runWatch(parsed.Args, stdout, stderr)
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
		remoteConfigured, remoteErr := app.RemoteConfigured(l)
		if remoteErr != nil {
			return appFailure(command, remoteErr), 2
		}
		if remoteConfigured {
			if raw, remote, remoteErr := app.RemoteCommand(context.Background(), l, command); remoteErr != nil {
				return appFailure(command, remoteErr), 2
			} else if remote {
				return decodeRemoteResponse(command, raw)
			}
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
		// Accept exactly one of: "build <path>" (1 arg) or
		// "build --hosted <project>" (2 args, first is the flag).
		// Every other shape is a usage error.
		hostedShape := len(args) == 2 && args[0] == "--hosted"
		submissionShape := len(args) == 2 && args[0] == "--from-submission"
		if len(args) != 1 && !hostedShape && !submissionShape {
			return Failure(command, model.NewError("invalid_arguments", model.FailureUsage, "Usage: build <path> | build --hosted <project> | build --from-submission <project>.", false)), 2
		}
		if len(args) == 1 && (args[0] == "--hosted" || args[0] == "--from-submission") {
			return Failure(command, model.NewError("invalid_arguments", model.FailureUsage, "Usage: build <path> | build --hosted <project> | build --from-submission <project>.", false)), 2
		}
		l, err := resolveLayout()
		if err != nil {
			return appFailure(command, err), 2
		}
		// The --from-submission form reconstructs a workspace from a submission
		// tar read on stdin and runs the build to completion. It is the
		// coordinator-side receiver for a remote client build.
		if submissionShape {
			result, err := app.BuildFromSubmission(context.Background(), l, args[1], os.Stdin)
			if err != nil {
				return appFailure(command, err), 2
			}
			return Success(command, result), 0
		}
		// `build --hosted <project>` is a coordinator-only source
		// mode. A client root proxies the command through ordinary
		// RemoteCommand so the coordinator retains all source
		// policy authority; the client sends no tar, no URL, no
		// commit, and no configuration mutation.
		if len(args) == 2 && args[0] == "--hosted" {
			projectName := args[1]
			remoteConfigured, remoteErr := app.RemoteConfigured(l)
			if remoteErr != nil {
				return appFailure(command, remoteErr), 2
			}
			if remoteConfigured {
				raw, _, remoteErr := app.RemoteCommand(context.Background(), l, "build", "--hosted", projectName)
				if remoteErr != nil {
					return appFailure(command, remoteErr), 2
				}
				return decodeRemoteResponse(command, raw)
			}
			prepared, err := app.PrepareHosted(context.Background(), l, projectName)
			if err != nil {
				return appFailure(command, err), 2
			}
			if err := spawnRun(prepared.Record.ID); err != nil {
				_ = app.Abandon(l, prepared.Record.ID)
				return appFailure(command, err), 2
			}
			return Success(command, map[string]any{"run_id": prepared.Record.ID, "state": prepared.Record.State, "machine": prepared.Record.Machine}), 0
		}
		remoteConfigured, remoteErr := app.RemoteConfigured(l)
		if remoteErr != nil {
			return appFailure(command, remoteErr), 2
		}
		if remoteConfigured {
			if raw, remote, remoteErr := app.RemoteBuild(context.Background(), l, args[0]); remoteErr != nil {
				return appFailure(command, remoteErr), 2
			} else if remote {
				return decodeRemoteResponse(command, raw)
			}
		}
		prepared, err := app.Prepare(context.Background(), l, args[0])
		if err != nil {
			return appFailure(command, err), 2
		}
		if err := spawnRun(prepared.Record.ID); err != nil {
			// Spawn failed: terminalize the prepared run and release
			// its scheduler reservation so the slot is freed.
			_ = app.Abandon(l, prepared.Record.ID)
			return appFailure(command, err), 2
		}
		return Success(command, map[string]any{"run_id": prepared.Record.ID, "state": prepared.Record.State, "machine": prepared.Record.Machine}), 0
	case "probe-seed":
		if len(args) != 1 || !model.ValidName(args[0]) {
			return Failure(command, model.NewError("invalid_arguments", model.FailureUsage, "Usage: probe-seed <project>.", false)), 2
		}
		l, err := resolveLayout()
		if err != nil {
			return appFailure(command, err), 2
		}
		have, err := app.ProbeSeed(context.Background(), l, args[0])
		if err != nil {
			return appFailure(command, err), 2
		}
		return Success(command, map[string]any{"have": have}), 0
	case "check":
		return checkRun(context.Background(), args)
	case "abort":
		if len(args) != 1 || !model.ValidRunID(model.RunID(args[0])) {
			return Failure(command, model.NewError("invalid_arguments", model.FailureUsage, "Usage: abort <run-id>.", false)), 2
		}
		l, err := resolveLayout()
		if err != nil {
			return appFailure(command, err), 2
		}
		remoteConfigured, remoteErr := app.RemoteConfigured(l)
		if remoteErr != nil {
			return appFailure(command, remoteErr), 2
		}
		if remoteConfigured {
			if raw, remote, remoteErr := app.RemoteAbort(context.Background(), l, model.RunID(args[0])); remoteErr != nil {
				return appFailure(command, remoteErr), 2
			} else if remote {
				return decodeRemoteResponse(command, raw)
			}
		}
		result, err := app.Abort(l, model.RunID(args[0]))
		if err != nil {
			return appFailure(command, err), 2
		}
		return Success(command, result), 0
	case "internal-run":
		if len(args) != 1 || !model.ValidRunID(model.RunID(args[0])) {
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
	case "artifacts":
		data, artErr := runArtifacts(args)
		if artErr != nil {
			return Failure(command, artErr), 2
		}
		return Success(command, data), 0
	default:
		return Failure(command, model.NewError("unknown_command", model.FailureUsage, fmt.Sprintf("Command %q is not recognized.", command), false)), 2
	}
}

func checkRun(ctx context.Context, args []string) (Response, int) {
	const command = "check"
	if len(args) != 1 || !model.ValidRunID(model.RunID(args[0])) {
		return Failure(command, model.NewError("invalid_arguments", model.FailureUsage, "Usage: check <run-id>.", false)), 2
	}
	l, err := resolveLayout()
	if err != nil {
		return appFailure(command, err), 2
	}
	remoteConfigured, remoteErr := app.RemoteConfigured(l)
	if remoteErr != nil {
		return appFailure(command, remoteErr), 2
	}
	if remoteConfigured {
		if raw, remote, remoteErr := app.RemoteCheck(ctx, l, model.RunID(args[0])); remoteErr != nil {
			return appFailure(command, remoteErr), 2
		} else if remote {
			return decodeRemoteResponse(command, raw)
		}
	}
	result, err := app.Check(l, model.RunID(args[0]))
	if err != nil {
		return appFailure(command, err), 2
	}
	return Success(command, result), 0
}

func decodeRemoteResponse(command string, raw []byte) (Response, int) {
	var response Response
	if err := json.Unmarshal(raw, &response); err != nil {
		return Failure(command, model.NewError("remote_invalid_response", model.FailureInfrastructure, err.Error(), true)), 2
	}
	if response.SchemaVersion != model.SchemaVersion {
		return Failure(command, model.NewError("remote_schema_mismatch", model.FailureInfrastructure, "remote response schema is unsupported", true)), 2
	}
	if response.Command != command || response.Data == nil || (!response.OK && response.Error == nil) || (response.OK && response.Error != nil) {
		return Failure(command, model.NewError("remote_invalid_response", model.FailureInfrastructure, "remote response has an invalid envelope", true)), 2
	}
	if !response.OK {
		return response, 2
	}
	return response, 0
}

func Main() {
	os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
}
