package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/hypernewbie/vci/internal/app"
	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/runtime"
	"github.com/hypernewbie/vci/internal/scheduler"
	"github.com/hypernewbie/vci/internal/source"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	case "version", "build", "check", "watch", "abort", "projects", "machines", "setup", "internal-run", "artifacts", "logs", "probe-seed", "wait-ready", "internal-stage", "internal-probe-cache", "internal-acquire-claim", "internal-release-claim", "internal-reap-cache", "internal-reconstruct":
		return Command{Name: name, Args: append([]string(nil), args[1:]...)}, nil
	default:
		return Command{}, model.NewError("unknown_command", model.FailureUsage, fmt.Sprintf("Command %q is not recognized.", name), false)
	}
}

type Response struct {
	SchemaVersion int             `json:"schema_version"`
	Command       string          `json:"command"`
	OK            bool            `json:"ok"`
	Data          any             `json:"data"`
	Error         *model.VciError `json:"error"`
}

func Success(command string, data any) Response {
	if data == nil {
		data = map[string]any{}
	}
	return Response{SchemaVersion: model.EnvelopeSchemaVersion, Command: command, OK: true, Data: data}
}

func Failure(command string, err *model.VciError) Response {
	return Response{SchemaVersion: model.EnvelopeSchemaVersion, Command: command, OK: false, Data: map[string]any{}, Error: err}
}

func Write(w io.Writer, response Response) error {
	return json.NewEncoder(w).Encode(response)
}

func resolveLayout() (model.Layout, error) {
	if root := os.Getenv("VCI_ROOT"); root != "" {
		return model.Layout{Root: filepath.Clean(root)}, nil
	}
	return model.Default()
}

// buildResponse dispatches a freshly prepared build and returns its aggregate
// summary as the build command's response.
func buildResponse(command string, l model.Layout, parentID model.RunID) (Response, int) {
	app.DispatchPending(l)
	summary, err := app.BuildSummaryView(l, parentID)
	if err != nil {
		return appFailure(command, err), 2
	}
	return Success(command, summary), 0
}

func appFailure(command string, err error) Response {
	code, class, message, retryable := classify(err)
	return Failure(command, model.NewError(code, class, message, retryable))
}

// classify maps an app-side error to the public (code, class, message,
// retryable) tuple shared by every JSON envelope producer. The typed
// sentinels are matched with errors.Is first; the substring branches
// are the fallback for wrapped transport and state errors. The message
// is the error's own text so operators see the underlying cause.
func classify(err error) (string, model.FailureClass, string, bool) {
	message := fmt.Sprintf("%v", err)
	code, class, retryable := "operation_failed", model.FailureConfiguration, false
	if errors.Is(err, scheduler.ErrNoCapacity) {
		return "machine_unavailable", model.FailureState, message, true
	}
	var busy app.ErrBuildBusy
	if errors.As(err, &busy) {
		return "coordinator_busy", model.FailureState, busy.Error(), true
	}
	if errors.Is(err, source.ErrSubmoduleUnavailable) {
		return "submodule_unavailable", model.FailureConfiguration, message, false
	}
	if errors.Is(err, source.ErrLFSContentUnavailable) {
		return "lfs_content_unavailable", model.FailureConfiguration, message, false
	}
	// Hosted build typed sentinels. These must come BEFORE the
	// substring-based "ssh" / "tar:" branches so a wrapped upstream
	// message does not get reclassified as remote_unavailable.
	if errors.Is(err, config.ErrHostedFallbackNotConfigured) {
		return "hosted_fallback_not_configured", model.FailureConfiguration, message, false
	}
	if errors.Is(err, config.ErrHostedFallbackInvalid) {
		return "hosted_fallback_invalid", model.FailureConfiguration, message, false
	}
	if errors.Is(err, config.ErrHostedSourceUnavailable) {
		return "hosted_source_unavailable", model.FailureInfrastructure, message, true
	}
	if errors.Is(err, config.ErrHostedSourceIntegrityFailed) {
		return "hosted_source_integrity_failed", model.FailureInfrastructure, message, false
	}
	if errors.Is(err, runtime.ErrRuntimeUnavailable) {
		return "runtime_unavailable", model.FailureInfrastructure, message, true
	}
	if errors.Is(err, runtime.ErrRuntimeImageNotFound) {
		return "runtime_image_not_found", model.FailureConfiguration, message, false
	}
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "cannot be aborted") || strings.Contains(lower, "is terminal"):
		code, class = "terminal_run", model.FailureState
	case strings.Contains(lower, "ownership") || strings.Contains(lower, "leased") || strings.Contains(lower, "lease"):
		code, class, retryable = "stale_ownership", model.FailureState, true
	case strings.Contains(lower, "already has a result") || strings.Contains(lower, "publish"):
		code, class = "publication_failed", model.FailureState
	case strings.Contains(lower, "ssh") || strings.Contains(lower, "tar:") || strings.Contains(lower, "unreachable") || strings.Contains(lower, "connection") || strings.Contains(lower, "invalid json") || strings.Contains(lower, "invalid response") || strings.Contains(lower, "select build input") || strings.Contains(lower, "unsupported source") || strings.Contains(lower, "source cache"):
		code, class, retryable = "remote_unavailable", model.FailureInfrastructure, true
	case strings.Contains(lower, "lock"):
		code, class, retryable = "lock_failed", model.FailureState, true
	}
	return code, class, message, retryable
}
