package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hypernewbie/vci/internal/app"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/store"
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
	// Internal worker commands (`internal-stage`, `internal-probe-cache`,
	// `internal-acquire-claim`, `internal-release-claim`, `internal-reap-cache`,
	// `internal-reconstruct`) speak plain stdout/stderr and an exit code so
	// the SSH layer can map a non-zero exit to a remote-exit error without
	// parsing a JSON envelope. The detached worker's `internal-run` stays on
	// the JSON envelope path above.
	if strings.HasPrefix(parsed.Name, "internal-") && parsed.Name != "internal-run" {
		return runInternalCommand(parsed.Name, parsed.Args, os.Stdin, stdout, stderr)
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
	case "version":
		return runVersion(args)
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
		// tar read on stdin and stages the run; the client polls and aborts it
		// like any other build.
		if submissionShape {
			prepared, err := app.PrepareFromSubmission(context.Background(), l, args[1], os.Stdin)
			if err != nil {
				return appFailure(command, err), 2
			}
			return buildResponse(command, l, prepared.Record.ID)
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
			return buildResponse(command, l, prepared.Record.ID)
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
		return buildResponse(command, l, prepared.Record.ID)
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
	case "wait-ready":
		return runWaitReady(args)
	default:
		return Failure(command, model.NewError("unknown_command", model.FailureUsage, fmt.Sprintf("Command %q is not recognized.", command), false)), 2
	}
}

// runWaitReady blocks until the coordinator has no live build. It is the
// way callers line up behind single-flight admission so that `vci build`
// on a busy coordinator does not need to poll on its own.
func runWaitReady(args []string) (Response, int) {
	const command = "wait-ready"
	interval := 1 * time.Second
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--interval":
			if i+1 >= len(args) {
				return Failure(command, model.NewError("invalid_arguments", model.FailureUsage, "--interval requires a value.", false)), 2
			}
			secs, err := strconv.Atoi(args[i+1])
			if err != nil || secs < 1 || secs > 3600 {
				return Failure(command, model.NewError("invalid_arguments", model.FailureUsage, "--interval must be an integer between 1 and 3600 seconds.", false)), 2
			}
			interval = time.Duration(secs) * time.Second
			i++
		default:
			return Failure(command, model.NewError("invalid_arguments", model.FailureUsage, fmt.Sprintf("Unknown wait-ready flag %q.", args[i]), false)), 2
		}
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
		raw, _, remoteErr := app.RemoteCommand(context.Background(), l, command, args...)
		if remoteErr != nil {
			return appFailure(command, remoteErr), 2
		}
		return decodeRemoteResponse(command, raw)
	}
	for {
		if _, busy := (store.Store{Layout: l}).HasLiveBuild(); !busy {
			return Success(command, map[string]any{"ready": true, "interval_seconds": int(interval.Seconds())}), 0
		}
		time.Sleep(interval)
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
	if response.SchemaVersion != model.EnvelopeSchemaVersion {
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
