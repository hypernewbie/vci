package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypernewbie/vci/internal/app"
	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/layout"
)

// TestBuildHostedRequiresExactlyTwoArgs pins that `build --hosted
// <project>` is the only valid hosted form; missing project, extra
// args, or positional path mixed with --hosted are usage errors.
func TestBuildHostedRequiresExactlyTwoArgs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `schema_version = 1
orchestrator = "self"

[machines.mac-local]

[projects.demo]
machines = ["mac-local"]
command = ["true"]
`
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := [][]string{
		{"build"},
		{"build", "--hosted"},
		{"build", "--hosted", "demo", "extra"},
		{"build", "/some/path", "--hosted", "demo"},
		{"build", "demo", "--hosted"},
	}
	for _, args := range cases {
		var out, errOut bytes.Buffer
		code := Run(args, &out, &errOut)
		if code == 0 {
			t.Fatalf("expected non-zero exit for %v: %s", args, out.String())
		}
		var resp Response
		if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
			t.Fatalf("%v: not JSON: %v", args, err)
		}
		if resp.Error == nil || resp.Error.Code != "invalid_arguments" {
			t.Fatalf("%v: want invalid_arguments, got %+v", args, resp)
		}
	}
}

// TestBuildHostedNotConfiguredReturnsTypedEnvelope pins that a
// project without hosted_fallback returns
// hosted_fallback_not_configured (configuration, non-retryable).
func TestBuildHostedNotConfiguredReturnsTypedEnvelope(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	l := layout.Layout{Root: root}
	if err := app.Initialize(l); err != nil {
		t.Fatal(err)
	}
	if err := app.AddMachine(l, "mac-local", config.Machine{}); err != nil {
		t.Fatal(err)
	}
	if err := app.AddProject(l, "demo", config.Project{Machines: []string{"mac-local"}, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := Run([]string{"build", "--hosted", "demo"}, &out, &errOut)
	if code == 0 {
		t.Fatalf("missing hosted_fallback must fail: %s", out.String())
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || resp.Error.Code != "hosted_fallback_not_configured" {
		t.Fatalf("want hosted_fallback_not_configured, got %+v", resp)
	}
	if resp.Error.Class != "configuration" || resp.Error.Retryable {
		t.Fatalf("want configuration/non-retryable, got %+v", resp)
	}
}

// TestSetupProjectHostedSetClearsThenClearRoundTrip pins that
// `setup project hosted set` validates, persists, and is readable;
// `clear` removes it without removing the project itself.
func TestSetupProjectHostedSetClearsThenClearRoundTrip(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	l := layout.Layout{Root: root}
	if err := app.Initialize(l); err != nil {
		t.Fatal(err)
	}
	if err := app.AddMachine(l, "mac-local", config.Machine{}); err != nil {
		t.Fatal(err)
	}
	if err := app.AddProject(l, "demo", config.Project{Machines: []string{"mac-local"}, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	commit := "0123456789abcdef0123456789abcdef01234567"
	url := "https://example.com/owner/repo.git"
	var out, errOut bytes.Buffer
	if code := Run([]string{"setup", "project", "hosted", "set", "demo", "--url", url, "--commit", commit}, &out, &errOut); code != 0 {
		t.Fatalf("set: %d %s", code, out.String())
	}
	cfg, err := config.Load(l.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Projects["demo"].HostedFallback.URL != url || cfg.Projects["demo"].HostedFallback.Commit != commit {
		t.Fatalf("round-trip mismatch: %+v", cfg.Projects["demo"].HostedFallback)
	}
	out.Reset()
	if code := Run([]string{"setup", "project", "hosted", "clear", "demo"}, &out, &errOut); code != 0 {
		t.Fatalf("clear: %d %s", code, out.String())
	}
	cfg, _ = config.Load(l.ConfigPath())
	if cfg.Projects["demo"].HostedFallback.URL != "" || cfg.Projects["demo"].HostedFallback.Commit != "" {
		t.Fatalf("clear failed: %+v", cfg.Projects["demo"].HostedFallback)
	}
	// Project must still exist.
	if _, ok := cfg.Projects["demo"]; !ok {
		t.Fatalf("clear removed project")
	}
}

// TestSetupProjectHostedSetRejectsBadCommit pins that an invalid
// commit surfaces as hosted_fallback_invalid in the CLI envelope
// and never touches the config file.
func TestSetupProjectHostedSetRejectsBadCommit(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	l := layout.Layout{Root: root}
	if err := app.Initialize(l); err != nil {
		t.Fatal(err)
	}
	if err := app.AddMachine(l, "mac-local", config.Machine{}); err != nil {
		t.Fatal(err)
	}
	if err := app.AddProject(l, "demo", config.Project{Machines: []string{"mac-local"}, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := Run([]string{"setup", "project", "hosted", "set", "demo", "--url", "https://example.com/o/r.git", "--commit", "main"}, &out, &errOut)
	if code == 0 {
		t.Fatalf("bad commit must fail: %s", out.String())
	}
	var resp Response
	_ = json.Unmarshal(out.Bytes(), &resp)
	if resp.Error == nil || resp.Error.Code != "hosted_fallback_invalid" {
		t.Fatalf("want hosted_fallback_invalid, got %+v", resp)
	}
	cfg, _ := config.Load(l.ConfigPath())
	if cfg.Projects["demo"].HostedFallback.Commit != "" {
		t.Fatalf("bad commit persisted: %+v", cfg.Projects["demo"].HostedFallback)
	}
}

// TestSetupProjectHostedRequiresCoordinator pins that the hosted
// setup subcommands refuse a client root before any mutation.
func TestSetupProjectHostedRequiresCoordinator(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `schema_version = 1
orchestrator = "builder"
`
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := Run([]string{"setup", "project", "hosted", "set", "demo", "--url", "https://example.com/o/r.git", "--commit", "0123456789abcdef0123456789abcdef01234567"}, &out, &errOut)
	if code == 0 {
		t.Fatalf("client root must be refused: %s", out.String())
	}
	if !strings.Contains(out.String(), "client root") && !strings.Contains(out.String(), "orchestrator") {
		t.Fatalf("error must name coordinator refusal: %s", out.String())
	}
}

// TestBuildHostedOnClientProxiesViaRemoteCommand pins that a client
// root's `build --hosted <project>` proxies through RemoteCommand;
// the local source path / coordinator code paths are not invoked.
func TestBuildHostedOnClientProxiesViaRemoteCommand(t *testing.T) {
	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `schema_version = 1
orchestrator = "this-host-does-not-exist.invalid"
`
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := Run([]string{"build", "--hosted", "demo"}, &out, &errOut)
	if code == 0 {
		t.Fatalf("unreachable remote must fail: %s", out.String())
	}
	var resp Response
	_ = json.Unmarshal(out.Bytes(), &resp)
	if resp.Error == nil {
		t.Fatalf("expected error envelope: %s", out.String())
	}
	// The remote target is unreachable; this is an infrastructure
	// failure (SSH, not hosted). The classifier is
	// remote_unavailable.
	if resp.Error.Class != "infrastructure" {
		t.Fatalf("want infrastructure, got %+v", resp)
	}
}
