package cli

// Project environment key validation at the CLI boundary. The
// coordinator config must reject environment keys that are not POSIX
// identifiers (`^[A-Za-z_][A-Za-z0-9_]*$`) at config-load time, so
// the failure surfaces as a configuration-class error before any setup
// mutation or run can read the value, and must accept valid
// underscore keys so existing configs keep working.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hypernewbie/vci/internal/model"
)

// writeCoordinatorConfigWithEnv writes a minimal coordinator root
// whose `demo` project carries the given TOML environment table.
func writeCoordinatorConfigWithEnv(t *testing.T, root, envTable string) {
	t.Helper()
	cfg := "schema_version = 1\norchestrator = \"self\"\n\n[log_limits]\nstdout_bytes = 4096\nstderr_bytes = 4096\n\n[retention]\nmax_bytes = 1048576\n\n[machines.mac-local]\n\n[projects.demo]\nmachines = [\"mac-local\"]\ncommand = [\"true\"]\n\n" + envTable
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCliRejectsInvalidEnvironmentKeyAsConfigurationError pins that a
// config whose project environment key is not a POSIX identifier is
// rejected as a configuration-class error when the CLI loads the root
// (the load every setup mutation and run performs first).
func TestCliRejectsInvalidEnvironmentKeyAsConfigurationError(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	writeCoordinatorConfigWithEnv(t, root, "[projects.demo.environment]\n\"FOO-BAR\" = \"x\"\n")
	var out, errOut bytes.Buffer
	if code := Run([]string{"projects"}, &out, &errOut); code == 0 {
		t.Fatalf("config with invalid environment key accepted: %s", out.String())
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil {
		t.Fatalf("expected error envelope, got: %+v", resp)
	}
	if resp.Error.Class != model.FailureConfiguration {
		t.Fatalf("expected configuration class, got %q: %s", resp.Error.Class, resp.Error.Message)
	}
	if resp.Error.Retryable {
		t.Fatalf("invalid environment key must not be retryable: %+v", resp.Error)
	}
}

// TestCliAcceptsValidUnderscoreEnvironmentKeys pins that valid POSIX
// identifier keys (including leading and trailing underscores) load
// and round-trip through the `projects` inventory envelope.
func TestCliAcceptsValidUnderscoreEnvironmentKeys(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	writeCoordinatorConfigWithEnv(t, root, "[projects.demo.environment]\nFOO_BAR = \"1\"\n_LEADING = \"2\"\nTrailing_9 = \"3\"\n")
	var out, errOut bytes.Buffer
	if code := Run([]string{"projects"}, &out, &errOut); code != 0 {
		t.Fatalf("projects: %d %s", code, out.String())
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("projects failed: %+v", resp.Error)
	}
	projects, ok := resp.Data.([]any)
	if !ok || len(projects) != 1 {
		t.Fatalf("projects data: %+v", resp.Data)
	}
	entry, ok := projects[0].(map[string]any)
	if !ok {
		t.Fatalf("project entry: %+v", projects[0])
	}
	projectData, ok := entry["project"].(map[string]any)
	if !ok {
		t.Fatalf("project field: %+v", entry)
	}
	env, ok := projectData["environment"].(map[string]any)
	if !ok {
		t.Fatalf("environment missing from envelope: %+v", projectData)
	}
	if env["FOO_BAR"] != "1" || env["_LEADING"] != "2" || env["Trailing_9"] != "3" {
		t.Fatalf("environment round-trip: %+v", env)
	}
}
