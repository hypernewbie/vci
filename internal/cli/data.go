package cli

// Artifacts commands:
//   vci artifacts ls <run-id>
//   vci artifacts get <run-id> <rel>
// `ls` returns JSON; `get` returns raw bytes.
// Client roots proxy to coordinator over ssh.
import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/hypernewbie/vci/internal/app"
	"github.com/hypernewbie/vci/internal/model"
	"io"
	"strconv"
)

// runArtifacts handles artifacts subcommands.
func runArtifacts(args []string) (any, *model.VciError) {
	if len(args) == 0 {
		return nil, model.NewError("invalid_arguments", model.FailureUsage, "Usage: artifacts ls <run-id>.", false)
	}
	switch args[0] {
	case "ls":
		return runArtifactsList(args[1:])
	case "get":
		// Dispatching `get` here is unsupported; public Run streams raw bytes.
		return nil, model.NewError("invalid_arguments", model.FailureUsage, "Usage: artifacts get <run-id> <rel>.", false)
	default:
		return nil, model.NewError("invalid_arguments", model.FailureUsage, fmt.Sprintf("Unknown artifacts operation %q.", args[0]), false)
	}
}

// extractMachine pulls the optional --machine <name> flag from args.
func extractMachine(args []string) (string, []string, *model.VciError) {
	out := make([]string, 0, len(args))
	var machine string
	for i := 0; i < len(args); i++ {
		if args[i] == "--machine" {
			if i+1 >= len(args) {
				return "", nil, model.NewError("invalid_arguments", model.FailureUsage, "--machine requires a value.", false)
			}
			machine = args[i+1]
			i++
			continue
		}
		out = append(out, args[i])
	}
	return machine, out, nil
}

// runArtifactsList runs artifacts ls.
// Returns files and truncated flag.
// Remote responses pass through unchanged.
func runArtifactsList(args []string) (any, *model.VciError) {
	machine, args, verr := extractMachine(args)
	if verr != nil {
		return nil, verr
	}
	if len(args) != 1 || !model.ValidRunID(model.RunID(args[0])) {
		return nil, model.NewError("invalid_arguments", model.FailureUsage, "Usage: artifacts ls <run-id>.", false)
	}
	l, err := resolveLayout()
	if err != nil {
		return nil, artifactsError(err)
	}
	remoteConfigured, remoteErr := app.RemoteConfigured(l)
	if remoteErr != nil {
		return nil, artifactsError(remoteErr)
	}
	if remoteConfigured {
		cmdArgs := []string{"ls", args[0]}
		if machine != "" {
			cmdArgs = append(cmdArgs, "--machine", machine)
		}
		raw, _, remoteErr := app.RemoteCommand(context.Background(), l, "artifacts", cmdArgs...)
		if remoteErr != nil {
			return nil, artifactsError(remoteErr)
		}
		var resp Response
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, model.NewError("remote_invalid_response", model.FailureInfrastructure, err.Error(), true)
		}
		if resp.SchemaVersion != model.EnvelopeSchemaVersion || resp.Command != "artifacts" || !resp.OK || resp.Error != nil {
			return nil, model.NewError("remote_invalid_response", model.FailureInfrastructure, "remote artifacts ls response has an invalid envelope", true)
		}
		return resp.Data, nil
	}
	target, terr := app.ResolveTarget(l, model.RunID(args[0]), machine)
	if terr != nil {
		return nil, artifactsError(terr)
	}
	files, truncated, err := app.ListArtifacts(l, target)
	if err != nil {
		return nil, artifactsError(err)
	}
	return map[string]any{"files": files, "truncated": truncated}, nil
}

// runArtifactsGet runs artifacts get.
// Writes raw artifact bytes on success.
// On failure, writes a JSON error envelope.
func runArtifactsGet(args []string, stdout, stderr io.Writer) int {
	machine, args, verr := extractMachine(args)
	if verr != nil {
		return writeFailure(stdout, "artifacts", verr)
	}
	if len(args) != 3 || !model.ValidRunID(model.RunID(args[1])) {
		return writeFailure(stdout, "artifacts", model.NewError("invalid_arguments", model.FailureUsage, "Usage: artifacts get <run-id> <rel>.", false))
	}
	id := model.RunID(args[1])
	rel := args[2]
	if err := app.ValidateArtifactRel(rel); err != nil {
		return writeFailure(stdout, "artifacts", artifactsError(err))
	}
	l, err := resolveLayout()
	if err != nil {
		return writeFailure(stdout, "artifacts", artifactsError(err))
	}
	remoteConfigured, remoteErr := app.RemoteConfigured(l)
	if remoteErr != nil {
		return writeFailure(stdout, "artifacts", artifactsError(remoteErr))
	}
	if remoteConfigured {
		raw, _, remoteErr := app.RemoteGetArtifact(context.Background(), l, id, machine, rel)
		if remoteErr != nil {
			return writeFailure(stdout, "artifacts", artifactsError(remoteErr))
		}
		if verr, ok := errorEnvelope(raw); ok {
			return writeFailure(stdout, "artifacts", verr)
		}
		if _, err := stdout.Write(raw); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	target, terr := app.ResolveTarget(l, id, machine)
	if terr != nil {
		return writeFailure(stdout, "artifacts", artifactsError(terr))
	}
	reader, _, err := app.GetArtifact(l, target, rel)
	if err != nil {
		return writeFailure(stdout, "artifacts", artifactsError(err))
	}
	defer reader.Close()
	if _, err := io.Copy(stdout, reader); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// artifactsError maps artifact errors to public CLI envelopes.
func artifactsError(err error) *model.VciError {
	if errors.Is(err, model.ErrRunNotFound) || errors.Is(err, app.ErrArtifactNotFound) {
		return model.NewError("not_found", model.FailureConfiguration, err.Error(), false)
	}
	if errors.Is(err, app.ErrInvalidArtifactPath) {
		return model.NewError("invalid_arguments", model.FailureUsage, err.Error(), false)
	}
	if errors.Is(err, app.ErrTargetSelect) {
		return model.NewError("invalid_arguments", model.FailureUsage, err.Error(), false)
	}
	code, class, message, retryable := classify(err)
	return model.NewError(code, class, message, retryable)
}

// errorEnvelope checks raw bytes for a Vci error envelope.
func errorEnvelope(raw []byte) (*model.VciError, bool) {
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, false
	}
	if resp.SchemaVersion != model.EnvelopeSchemaVersion || resp.OK || resp.Error == nil {
		return nil, false
	}
	return resp.Error, true
}

// writeFailure writes a JSON failure envelope.
func writeFailure(w io.Writer, command string, verr *model.VciError) int {
	if err := Write(w, Failure(command, verr)); err != nil {
		return 1
	}
	return 2
}

// Logs commands:
//   vci logs <run-id> [--stderr] [--tail <n>]
// Streams binary log bytes and proxies via ssh.

// tailMin is the inclusive lower bound for --tail.
const tailMin = 1

// tailMax is the inclusive upper bound for --tail.
const tailMax = 100000

// runLogs streams selected logs to stdout.
// Binary output is preserved; coordinator handles tailing.
func runLogs(args []string, stdout, stderr io.Writer) int {
	id, stream, tail, machine, verr := parseLogsArgs(args)
	if verr != nil {
		return writeFailure(stdout, "logs", verr)
	}
	l, err := resolveLayout()
	if err != nil {
		return writeFailure(stdout, "logs", logsError(err))
	}
	remoteConfigured, remoteErr := app.RemoteConfigured(l)
	if remoteErr != nil {
		return writeFailure(stdout, "logs", logsError(remoteErr))
	}
	if remoteConfigured {
		raw, _, remoteErr := app.RemoteLog(context.Background(), l, id, machine, stream, tail)
		if remoteErr != nil {
			return writeFailure(stdout, "logs", logsError(remoteErr))
		}
		if verr, ok := errorEnvelope(raw); ok {
			// Return JSON envelope errors instead of raw bytes.
			return writeFailure(stdout, "logs", verr)
		}
		if _, err := stdout.Write(raw); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	target, terr := app.ResolveTarget(l, id, machine)
	if terr != nil {
		return writeFailure(stdout, "logs", logsError(terr))
	}
	reader, _, err := app.ReadLog(l, target, stream)
	if err != nil {
		return writeFailure(stdout, "logs", logsError(err))
	}
	defer reader.Close()
	if tail > 0 {
		data, err := io.ReadAll(reader)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if _, err := stdout.Write(tailLines(data, tail)); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	if _, err := io.Copy(stdout, reader); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// parseLogsArgs parses `vci logs` arguments and validates usage.
func parseLogsArgs(args []string) (model.RunID, string, int, string, *model.VciError) {
	if len(args) == 0 || !model.ValidRunID(model.RunID(args[0])) {
		return "", "", 0, "", model.NewError("invalid_arguments", model.FailureUsage, "Usage: logs <run-id> [--machine <name>] [--stderr] [--tail <n>].", false)
	}
	id := model.RunID(args[0])
	stream := "stdout"
	tail := 0
	var machine string
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--stderr":
			stream = "stderr"
		case "--machine":
			if i+1 >= len(args) {
				return "", "", 0, "", model.NewError("invalid_arguments", model.FailureUsage, "--machine requires a value.", false)
			}
			machine = args[i+1]
			i++
		case "--tail":
			if i+1 >= len(args) {
				return "", "", 0, "", model.NewError("invalid_arguments", model.FailureUsage, "--tail requires a value between 1 and 100000.", false)
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < tailMin || n > tailMax {
				return "", "", 0, "", model.NewError("invalid_arguments", model.FailureUsage, "--tail must be an integer between 1 and 100000.", false)
			}
			tail = n
			i++
		default:
			return "", "", 0, "", model.NewError("invalid_arguments", model.FailureUsage, fmt.Sprintf("Unknown logs flag %q.", args[i]), false)
		}
	}
	return id, stream, tail, machine, nil
}

// tailLines returns the last n lines, emulating tail -n.
// Input with fewer than n lines is returned unchanged; n is prevalidated.
func tailLines(data []byte, n int) []byte {
	if n <= 0 || len(data) == 0 {
		return []byte{}
	}
	lines := bytes.Count(data, []byte{'\n'})
	if data[len(data)-1] != '\n' {
		lines++
	}
	if lines <= n {
		return data
	}
	// Skip the first (lines-n) newlines and keep bytes after the last one.
	skip := lines - n
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			skip--
			if skip == 0 {
				return data[i+1:]
			}
		}
	}
	return data
}

// logsError maps log errors to public envelope codes.
func logsError(err error) *model.VciError {
	if errors.Is(err, model.ErrRunNotFound) || errors.Is(err, app.ErrLogNotFound) {
		return model.NewError("not_found", model.FailureConfiguration, err.Error(), false)
	}
	if errors.Is(err, app.ErrInvalidLogStream) {
		return model.NewError("invalid_arguments", model.FailureUsage, err.Error(), false)
	}
	if errors.Is(err, app.ErrTargetSelect) {
		return model.NewError("invalid_arguments", model.FailureUsage, err.Error(), false)
	}
	code, class, message, retryable := classify(err)
	return model.NewError(code, class, message, retryable)
}
