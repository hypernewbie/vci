package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/model"
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
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "cannot be aborted") || strings.Contains(lower, "is terminal"):
		code, class = "terminal_run", model.FailureState
	case strings.Contains(lower, "ownership") || strings.Contains(lower, "leased") || strings.Contains(lower, "lease"):
		code, class, retryable = "stale_ownership", model.FailureState, true
	case strings.Contains(lower, "already has a result") || strings.Contains(lower, "publish"):
		code, class = "publication_failed", model.FailureState
	case strings.Contains(lower, "ssh") || strings.Contains(lower, "tar:") || strings.Contains(lower, "unreachable") || strings.Contains(lower, "connection") || strings.Contains(lower, "invalid json") || strings.Contains(lower, "invalid response"):
		code, class, retryable = "remote_unavailable", model.FailureInfrastructure, true
	case strings.Contains(lower, "lock"):
		code, class, retryable = "lock_failed", model.FailureState, true
	}
	return Failure(command, model.NewError(code, class, message, retryable))
}
