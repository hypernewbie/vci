package cli

// Plan 16 Phase 1 public surface:
//
//	vci artifacts ls <run-id>          # JSON list + truncated flag
//	vci artifacts get <run-id> <rel>   # raw bytes on stdout
//
// `ls` is a normal JSON-envelope command. `get` is the first non-JSON
// stdout path in the whole CLI: Run intercepts it before dispatch and
// streams the artifact's exact bytes so binary content survives. Every
// `get` failure still returns a JSON envelope. On a client root both
// operations proxy to the coordinator over ordinary ssh (RemoteCommand
// for `ls`, RemoteGetArtifact for the raw-byte `get`), exactly like
// `check`/`machines`/`abort`.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/hypernewbie/vci/internal/app"
	"github.com/hypernewbie/vci/internal/model"
)

// runArtifacts dispatches `vci artifacts <op> ...`. The `get`
// operation is handled earlier by Run's raw-byte path and never
// reaches dispatch; `ls` is the JSON-envelope inventory query.
func runArtifacts(args []string) (any, *model.VciError) {
	if len(args) == 0 {
		return nil, model.NewError("invalid_arguments", model.FailureUsage, "Usage: artifacts ls <run-id>.", false)
	}
	switch args[0] {
	case "ls":
		return runArtifactsList(args[1:])
	case "get":
		// Reaching dispatch with `get` means a direct misuse of
		// dispatch; the public Run path streams raw bytes instead.
		return nil, model.NewError("invalid_arguments", model.FailureUsage, "Usage: artifacts get <run-id> <rel>.", false)
	default:
		return nil, model.NewError("invalid_arguments", model.FailureUsage, fmt.Sprintf("Unknown artifacts operation %q.", args[0]), false)
	}
}

// runArtifactsList implements `vci artifacts ls <run-id>`: a JSON
// envelope with `files` (sorted relative paths) and `truncated` (the
// durable 64 MiB cap flag). On a client root the query is proxied over
// ssh and the remote envelope's data is returned unchanged.
func runArtifactsList(args []string) (any, *model.VciError) {
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
		raw, _, remoteErr := app.RemoteCommand(context.Background(), l, "artifacts", "ls", args[0])
		if remoteErr != nil {
			return nil, artifactsError(remoteErr)
		}
		var resp Response
		if err := json.Unmarshal(raw, &resp); err != nil {
			return nil, model.NewError("remote_invalid_response", model.FailureInfrastructure, err.Error(), true)
		}
		if resp.SchemaVersion != model.SchemaVersion || resp.Command != "artifacts" || !resp.OK || resp.Error != nil {
			return nil, model.NewError("remote_invalid_response", model.FailureInfrastructure, "remote artifacts ls response has an invalid envelope", true)
		}
		return resp.Data, nil
	}
	files, truncated, err := app.ListArtifacts(l, model.RunID(args[0]))
	if err != nil {
		return nil, artifactsError(err)
	}
	return map[string]any{"files": files, "truncated": truncated}, nil
}

// runArtifactsGet implements `vci artifacts get <run-id> <rel>`: the
// artifact's exact bytes are written to stdout with no JSON wrapper so
// binary content survives. Failures (usage, not_found, transport)
// still use the JSON envelope. On a client root the bytes come from
// the coordinator over ssh and are relayed verbatim.
func runArtifactsGet(args []string, stdout, stderr io.Writer) int {
	if len(args) != 3 || !model.ValidRunID(model.RunID(args[1])) {
		return writeFailure(stdout, "artifacts", model.NewError("invalid_arguments", model.FailureUsage, "Usage: artifacts get <run-id> <rel>.", false))
	}
	id := model.RunID(args[1])
	rel := args[2]
	l, err := resolveLayout()
	if err != nil {
		return writeFailure(stdout, "artifacts", artifactsError(err))
	}
	remoteConfigured, remoteErr := app.RemoteConfigured(l)
	if remoteErr != nil {
		return writeFailure(stdout, "artifacts", artifactsError(remoteErr))
	}
	if remoteConfigured {
		raw, _, remoteErr := app.RemoteGetArtifact(context.Background(), l, id, rel)
		if remoteErr != nil {
			return writeFailure(stdout, "artifacts", artifactsError(remoteErr))
		}
		if verr, ok := errorEnvelope(raw); ok {
			// The remote wrote a JSON error envelope (missing run or
			// artifact, invalid rel). Report it instead of streaming it.
			return writeFailure(stdout, "artifacts", verr)
		}
		if _, err := stdout.Write(raw); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	reader, _, err := app.GetArtifact(l, id, rel)
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

// artifactsError converts an app-side artifacts error into the public
// envelope: missing runs and artifacts are `not_found` (configuration,
// not retryable); rejected relative paths are `invalid_arguments`;
// everything else reuses the shared classify mapping so ssh and config
// failures keep their infrastructure classes.
func artifactsError(err error) *model.VciError {
	if errors.Is(err, model.ErrRunNotFound) || errors.Is(err, app.ErrArtifactNotFound) {
		return model.NewError("not_found", model.FailureConfiguration, err.Error(), false)
	}
	if errors.Is(err, app.ErrInvalidArtifactPath) {
		return model.NewError("invalid_arguments", model.FailureUsage, err.Error(), false)
	}
	code, class, message, retryable := classify(err)
	return model.NewError(code, class, message, retryable)
}

// errorEnvelope reports whether raw bytes are a valid Vci error
// envelope. The remote `artifacts get` writes raw artifact bytes on
// success, so a response that decodes as a Failure envelope must be a
// remote error (missing run/artifact, invalid rel) and is reported
// rather than streamed.
func errorEnvelope(raw []byte) (*model.VciError, bool) {
	var resp Response
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, false
	}
	if resp.SchemaVersion != model.SchemaVersion || resp.OK || resp.Error == nil {
		return nil, false
	}
	return resp.Error, true
}

// writeFailure writes a JSON failure envelope and returns the exit
// code (2 on success, 1 when the envelope itself cannot be written).
func writeFailure(w io.Writer, command string, verr *model.VciError) int {
	if err := Write(w, Failure(command, verr)); err != nil {
		return 1
	}
	return 2
}
