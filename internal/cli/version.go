package cli

import (
	"context"

	"github.com/hypernewbie/vci/internal/app"
	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/scheduler"
	appversion "github.com/hypernewbie/vci/internal/version"
)

// versionData is deliberately separate from the public response envelope.
// Application identity is diagnostic data; it is not a protocol compatibility
// gate and is not copied into every command response.
type versionData struct {
	appversion.Details
	Schemas versionSchemas `json:"schemas"`
}

type versionSchemas struct {
	Envelope        int   `json:"envelope"`
	ConfigCurrent   int   `json:"config_current"`
	ConfigSupported []int `json:"config_supported"`
	Run             int   `json:"run"`
	Execution       int   `json:"execution"`
	Claim           int   `json:"claim"`
}

func currentVersionData() versionData {
	return versionData{
		Details: appversion.Current(),
		Schemas: versionSchemas{
			Envelope:        model.EnvelopeSchemaVersion,
			ConfigCurrent:   config.SchemaVersion,
			ConfigSupported: config.SupportedSchemaVersions(),
			Run:             model.RunSchemaVersion,
			Execution:       model.ExecutionSchemaVersion,
			Claim:           scheduler.ClaimSchemaVersion,
		},
	}
}

// runVersion reports this binary's identity. With --coordinator it queries the
// configured coordinator, while a self coordinator simply reports locally.
// The no-argument form never reads configuration or state.
func runVersion(args []string) (Response, int) {
	const command = "version"
	if len(args) == 0 {
		return Success(command, currentVersionData()), 0
	}
	if len(args) != 1 || args[0] != "--coordinator" {
		return Failure(command, model.NewError("invalid_arguments", model.FailureUsage, "Usage: version [--coordinator].", false)), 2
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
		raw, _, remoteErr := app.RemoteCommand(context.Background(), l, command)
		if remoteErr != nil {
			return appFailure(command, remoteErr), 2
		}
		return decodeRemoteResponse(command, raw)
	}
	return Success(command, currentVersionData()), 0
}
