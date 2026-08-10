package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunWritesOneJSONResponse(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"unknown"}, &out, &errOut); code == 0 {
		t.Fatal("unknown command succeeded")
	}
	var got Response
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if got.OK || got.Error == nil || got.Error.Code != "unknown_command" {
		t.Fatalf("unexpected response: %+v", got)
	}
	if errOut.Len() != 0 {
		t.Fatalf("diagnostics contaminated stderr: %q", errOut.String())
	}
}

// TestRunRejectsMalformedRunIDsAcrossCommands proves public CLI inputs
// for run-id-bearing commands are validated by `model.ValidRunID`
// before any local/SSH operation. The path traversal payload
// `run_/../../x` (formerly accepted by the prefix-only rule) and a
// whitespace-bearing ID must both be rejected. A valid run ID is
// accepted for parsing but rejected downstream because no run record
// exists under the temporary root — that path must still emit exactly
// one JSON envelope with `invalid_arguments` for the bad inputs.
func TestRunRejectsMalformedRunIDsAcrossCommands(t *testing.T) {
	prevRoot, hadRoot := os.LookupEnv("VCI_ROOT")
	tmp := t.TempDir()
	if err := os.Setenv("VCI_ROOT", tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadRoot {
			_ = os.Setenv("VCI_ROOT", prevRoot)
		} else {
			_ = os.Unsetenv("VCI_ROOT")
		}
	})

	cases := []struct {
		name    string
		command string
		args    []string
	}{
		{"check-traversal", "check", []string{"run_/../../x"}},
		{"abort-traversal", "abort", []string{"run_/../../x"}},
		{"internal-run-traversal", "internal-run", []string{"run_/../../x"}},
		{"logs-traversal", "logs", []string{"run_/../../x"}},
		{"check-whitespace", "check", []string{"run_has space"}},
		{"abort-whitespace", "abort", []string{"run_has space"}},
		{"internal-run-whitespace", "internal-run", []string{"run_has space"}},
		{"logs-whitespace", "logs", []string{"run_has space"}},
		{"check-empty-after-prefix", "check", []string{"run_"}},
		{"abort-empty-after-prefix", "abort", []string{"run_"}},
		{"internal-run-empty-after-prefix", "internal-run", []string{"run_"}},
		{"logs-empty-after-prefix", "logs", []string{"run_"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			args := append([]string{tc.command}, tc.args...)
			code := Run(args, &out, &errOut)
			if code == 0 {
				t.Fatalf("expected non-zero exit for malformed run id")
			}
			var got Response
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatalf("stdout is not JSON: %v", err)
			}
			if got.OK || got.Error == nil {
				t.Fatalf("expected failure envelope, got: %+v", got)
			}
			if got.Error.Code != "invalid_arguments" {
				t.Fatalf("code: %q", got.Error.Code)
			}
			if got.Command != tc.command {
				t.Fatalf("command echo: %q", got.Command)
			}
		})
	}
}

// TestLocalBuildDataIncludesMachine pins that a successful local build
// response reports the attached target machines so a client can map the
// accepted run id back to the slots the coordinator will run.
func TestLocalBuildDataIncludesMachine(t *testing.T) {
	prevRoot, hadRoot := os.LookupEnv("VCI_ROOT")
	tmp := t.TempDir()
	if err := os.Setenv("VCI_ROOT", tmp); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if hadRoot {
			_ = os.Setenv("VCI_ROOT", prevRoot)
		} else {
			_ = os.Unsetenv("VCI_ROOT")
		}
	})
	// Build a coordinator config with a single machine named "alpha".
	cfgPath := filepath.Join(tmp, "config.toml")
	body := "schema_version = 1\norchestrator = \"self\"\n\n[log_limits]\nstdout_bytes = 4194304\nstderr_bytes = 4194304\n\n[retention]\nmax_bytes = 536870912\n\n[machines.alpha]\n\n[projects.demo]\nmachines = [\"alpha\"]\ncommand = [\"true\"]\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("# demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "vci-cli@example.com"},
		{"config", "user.name", "vci-cli"},
		{"config", "commit.gpgsign", "false"},
		{"add", "-A"},
		{"commit", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = src
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	var out, errOut bytes.Buffer
	code := Run([]string{"build", src}, &out, &errOut)
	if code != 0 {
		t.Logf("stderr: %s", errOut.String())
		t.Fatalf("local build must succeed; stdout=%s", out.String())
	}
	var got Response
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if !got.OK || got.Error != nil {
		t.Fatalf("expected ok envelope, got: %+v", got)
	}
	var data struct {
		RunID   string `json:"run_id"`
		State   string `json:"state"`
		Targets []struct {
			Machine string `json:"machine"`
			State   string `json:"state"`
		} `json:"targets"`
	}
	raw, err := json.Marshal(got.Data)
	if err != nil {
		t.Fatalf("encode data: %v", err)
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("data decode: %v", err)
	}
	if !strings.HasPrefix(data.RunID, "run_") {
		t.Fatalf("missing run_id: %+v", data)
	}
	if len(data.Targets) != 1 || data.Targets[0].Machine != "alpha" {
		t.Fatalf("expected one target machine 'alpha', got %+v", data.Targets)
	}
}
