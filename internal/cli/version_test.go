package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypernewbie/vci/internal/model"
	appversion "github.com/hypernewbie/vci/internal/version"
)

func TestVersionDoesNotReadConfiguration(t *testing.T) {
	t.Setenv("VCI_ROOT", filepath.Join(t.TempDir(), "missing"))
	var out, errOut bytes.Buffer
	if code := Run([]string{"version"}, &out, &errOut); code != 0 {
		t.Fatalf("version: code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	var response Response
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !response.OK || response.Command != "version" {
		t.Fatalf("unexpected response: %+v", response)
	}
	var data versionData
	raw, err := json.Marshal(response.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("decode version data: %v", err)
	}
	if data.Version != appversion.Current().Version {
		t.Fatalf("version = %q, want %q", data.Version, appversion.Current().Version)
	}
	if data.Schemas.Envelope != model.EnvelopeSchemaVersion || data.Schemas.ConfigCurrent != 2 || data.Schemas.Run != model.RunSchemaVersion || data.Schemas.Execution != model.ExecutionSchemaVersion {
		t.Fatalf("schema data = %+v", data.Schemas)
	}
	if errOut.Len() != 0 {
		t.Fatalf("diagnostics contaminated stderr: %q", errOut.String())
	}
}

func TestVersionRejectsUnexpectedArgumentsWithoutConfiguration(t *testing.T) {
	t.Setenv("VCI_ROOT", filepath.Join(t.TempDir(), "missing"))
	var out, errOut bytes.Buffer
	if code := Run([]string{"version", "--unexpected"}, &out, &errOut); code != 2 {
		t.Fatalf("version invalid args code = %d, want 2", code)
	}
	var response Response
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || response.Error.Code != "invalid_arguments" {
		t.Fatalf("unexpected response: %+v", response)
	}
}

func TestVersionCoordinatorSelfUsesLocalIdentity(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("schema_version = 2\norchestrator = \"self\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"version", "--coordinator"}, &out, &errOut); code != 0 {
		t.Fatalf("version --coordinator: code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	var response Response
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || response.Command != "version" {
		t.Fatalf("unexpected response: %+v", response)
	}
}

func TestVersionCoordinatorProxiesWithoutForwardingFlag(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("schema_version = 2\norchestrator = \"builder\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stubDir := t.TempDir()
	argsPath := filepath.Join(t.TempDir(), "ssh-args")
	responsePath := filepath.Join(t.TempDir(), "response.json")
	remoteData := map[string]any{
		"version":    "0.1.0",
		"commit":     "coordinator-commit",
		"build_date": "2026-01-02T03:04:05Z",
		"go_version": "go1.26.0",
		"os":         "linux",
		"arch":       "amd64",
		"schemas": map[string]any{
			"envelope":         1,
			"config_current":   2,
			"config_supported": []int{1, 2},
			"run":              1,
			"execution":        1,
			"claim":            1,
		},
	}
	remote := Success("version", remoteData)
	encoded, err := json.Marshal(remote)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(responsePath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	writeCLIPathStub(t, stubDir, "ssh", "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$VCI_TEST_SSH_ARGS\"\ncat \"$VCI_TEST_SSH_RESPONSE\"\n")
	t.Setenv("VCI_TEST_SSH_ARGS", argsPath)
	t.Setenv("VCI_TEST_SSH_RESPONSE", responsePath)

	var out, errOut bytes.Buffer
	if code := Run([]string{"version", "--coordinator"}, &out, &errOut); code != 0 {
		t.Fatalf("version --coordinator: code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	var response Response
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK {
		t.Fatalf("remote response failed: %+v", response.Error)
	}
	var got versionData
	raw, err := json.Marshal(response.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Commit != "coordinator-commit" {
		t.Fatalf("coordinator data = %+v", got)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	command := string(args)
	if !strings.Contains(command, "builder") || !strings.Contains(command, "version") {
		t.Fatalf("ssh args = %q", command)
	}
	if strings.Contains(command, "coordinator") {
		t.Fatalf("--coordinator was forwarded to the remote command: %q", command)
	}
}

func TestVersionCoordinatorPreservesUnknownCommand(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("schema_version = 2\norchestrator = \"builder\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stubDir := t.TempDir()
	responsePath := filepath.Join(t.TempDir(), "response.json")
	remote := Failure("version", model.NewError("unknown_command", model.FailureUsage, "Command \"version\" is not recognized.", false))
	encoded, err := json.Marshal(remote)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(responsePath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	writeCLIPathStub(t, stubDir, "ssh", "#!/bin/sh\ncat \"$VCI_TEST_SSH_RESPONSE\"\nexit 2\n")
	t.Setenv("VCI_TEST_SSH_RESPONSE", responsePath)

	var out, errOut bytes.Buffer
	if code := Run([]string{"version", "--coordinator"}, &out, &errOut); code != 2 {
		t.Fatalf("version --coordinator code = %d, want 2", code)
	}
	var response Response
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error == nil || response.Error.Code != "unknown_command" {
		t.Fatalf("remote error was not preserved: %+v", response)
	}
}
