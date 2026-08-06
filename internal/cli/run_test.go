package cli

import (
	"bytes"
	"encoding/json"
	"os"
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
		{"check-whitespace", "check", []string{"run_has space"}},
		{"abort-whitespace", "abort", []string{"run_has space"}},
		{"internal-run-whitespace", "internal-run", []string{"run_has space"}},
		{"check-empty-after-prefix", "check", []string{"run_"}},
		{"abort-empty-after-prefix", "abort", []string{"run_"}},
		{"internal-run-empty-after-prefix", "internal-run", []string{"run_"}},
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
