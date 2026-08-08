package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/runtime"
	"github.com/hypernewbie/vci/internal/scheduler"
	"github.com/hypernewbie/vci/internal/source"
)

func resolveLayout() (layout.Layout, error) {
	if root := os.Getenv("VCI_ROOT"); root != "" {
		return layout.Layout{Root: filepath.Clean(root)}, nil
	}
	return layout.Default()
}

func appFailure(command string, err error) Response {
	message := fmt.Sprintf("%v", err)
	code, class, retryable := "operation_failed", model.FailureConfiguration, false
	if errors.Is(err, scheduler.ErrNoCapacity) {
		return Failure(command, model.NewError("machine_unavailable", model.FailureState, message, true))
	}
	if errors.Is(err, source.ErrSubmoduleUnavailable) {
		return Failure(command, model.NewError("submodule_unavailable", model.FailureConfiguration, message, false))
	}
	if errors.Is(err, source.ErrLFSContentUnavailable) {
		return Failure(command, model.NewError("lfs_content_unavailable", model.FailureConfiguration, message, false))
	}
	// Hosted build typed sentinels. These must come BEFORE the
	// substring-based "ssh" / "tar:" branches so a wrapped upstream
	// message does not get reclassified as remote_unavailable.
	if errors.Is(err, config.ErrHostedFallbackNotConfigured) {
		return Failure(command, model.NewError("hosted_fallback_not_configured", model.FailureConfiguration, message, false))
	}
	if errors.Is(err, config.ErrHostedFallbackInvalid) {
		return Failure(command, model.NewError("hosted_fallback_invalid", model.FailureConfiguration, message, false))
	}
	if errors.Is(err, config.ErrHostedSourceUnavailable) {
		return Failure(command, model.NewError("hosted_source_unavailable", model.FailureInfrastructure, message, true))
	}
	if errors.Is(err, config.ErrHostedSourceIntegrityFailed) {
		return Failure(command, model.NewError("hosted_source_integrity_failed", model.FailureInfrastructure, message, false))
	}
	if errors.Is(err, runtime.ErrRuntimeUnavailable) {
		return Failure(command, model.NewError("runtime_unavailable", model.FailureInfrastructure, message, true))
	}
	if errors.Is(err, runtime.ErrRuntimeImageNotFound) {
		return Failure(command, model.NewError("runtime_image_not_found", model.FailureConfiguration, message, false))
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
	return Failure(command, model.NewError(code, class, message, retryable))
}
