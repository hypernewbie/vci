package cli

// Plan 17 Phase 1 public surface:
//
//	vci logs <run-id>             # stdout.log bytes to stdout (binary-safe)
//	vci logs <run-id> --stderr    # stderr.log bytes to stdout
//	vci logs <run-id> --tail <n>  # last n lines (1..100000)
//
// `logs` is the second non-JSON stdout path (after `artifacts get`):
// Run intercepts it before dispatch and streams the durable log bytes
// so binary/garbled output survives. Every failure still returns a
// JSON envelope. On a client root the bytes come from the coordinator
// over ordinary ssh (runSSHRaw via RemoteLog), exactly like
// `artifacts get`. No follow/tail daemon and no streaming protocol:
// `--tail` reads the whole file then prints the last n lines, with n
// bounded so a mistyped argument cannot turn `logs` into a full-file
// dump through an unbounded slice.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/hypernewbie/vci/internal/app"
	"github.com/hypernewbie/vci/internal/model"
)

// tailMin is the inclusive lower bound for --tail: every log read
// keeps at least one line.
const tailMin = 1

// tailMax is the inclusive upper bound for --tail.
const tailMax = 100000

// runLogs implements `vci logs <run-id> [--stderr] [--tail <n>]`: the
// selected stream's durable log bytes are written to stdout with no
// JSON wrapper so binary content survives. Failures (usage, not_found,
// transport) still use the JSON envelope. On a client root the bytes
// come from the coordinator over ssh and are relayed verbatim, with
// the tail applied on the coordinator.
func runLogs(args []string, stdout, stderr io.Writer) int {
	id, stream, tail, verr := parseLogsArgs(args)
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
		raw, _, remoteErr := app.RemoteLog(context.Background(), l, id, stream, tail)
		if remoteErr != nil {
			return writeFailure(stdout, "logs", logsError(remoteErr))
		}
		if verr, ok := errorEnvelope(raw); ok {
			// The remote wrote a JSON error envelope (missing run or
			// log, invalid stream or tail). Report it instead of
			// streaming it.
			return writeFailure(stdout, "logs", verr)
		}
		if _, err := stdout.Write(raw); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	reader, _, err := app.ReadLog(l, id, stream)
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

// parseLogsArgs parses `vci logs <run-id> [--stderr] [--tail <n>]`.
// The run-id must be the first argument; flags may follow in any
// order. A missing run-id, an unknown flag (which would be a bad
// stream selector), a missing --tail value, a non-integer tail, and a
// tail outside 1..tailMax are all invalid_arguments.
func parseLogsArgs(args []string) (model.RunID, string, int, *model.VciError) {
	if len(args) == 0 || !model.ValidRunID(model.RunID(args[0])) {
		return "", "", 0, model.NewError("invalid_arguments", model.FailureUsage, "Usage: logs <run-id> [--stderr] [--tail <n>].", false)
	}
	id := model.RunID(args[0])
	stream := "stdout"
	tail := 0
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--stderr":
			stream = "stderr"
		case "--tail":
			if i+1 >= len(args) {
				return "", "", 0, model.NewError("invalid_arguments", model.FailureUsage, "--tail requires a value between 1 and 100000.", false)
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < tailMin || n > tailMax {
				return "", "", 0, model.NewError("invalid_arguments", model.FailureUsage, "--tail must be an integer between 1 and 100000.", false)
			}
			tail = n
			i++
		default:
			return "", "", 0, model.NewError("invalid_arguments", model.FailureUsage, fmt.Sprintf("Unknown logs flag %q.", args[i]), false)
		}
	}
	return id, stream, tail, nil
}

// tailLines returns the last n lines of data, matching `tail -n`
// semantics: lines are separated by '\n', a trailing newline
// terminates the final line, and unterminated trailing content is its
// own final line. Fewer than n lines returns the whole input
// unchanged. n must already be validated to 1..tailMax.
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
	// Skip the first (lines-n) newlines; the kept region starts right
	// after the last skipped newline.
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

// logsError converts an app-side logs error into the public envelope:
// missing runs and log files are `not_found` (configuration, not
// retryable); invalid streams are `invalid_arguments`; everything
// else reuses the shared classify mapping so ssh and config failures
// keep their infrastructure classes.
func logsError(err error) *model.VciError {
	if errors.Is(err, model.ErrRunNotFound) || errors.Is(err, app.ErrLogNotFound) {
		return model.NewError("not_found", model.FailureConfiguration, err.Error(), false)
	}
	if errors.Is(err, app.ErrInvalidLogStream) {
		return model.NewError("invalid_arguments", model.FailureUsage, err.Error(), false)
	}
	code, class, message, retryable := classify(err)
	return model.NewError(code, class, message, retryable)
}
