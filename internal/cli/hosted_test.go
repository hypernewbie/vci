package cli

// Plan 14 hosted source mode: the `build --hosted <project>` public
// surface, `setup project hosted set|clear`, and the coordinator-only
// hosted_fallback rules. Coordinator roots run the pinned git
// checkout locally (proved end-to-end through a stub `git` in PATH so
// no public network is needed); client roots proxy the command to the
// coordinator over ordinary ssh without producing a run record or
// mutating local config.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hypernewbie/vci/internal/app"
	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
)

// hostedGitStub is a fake `git` binary placed in PATH for the duration
// of one test. It records every invocation and returns the bytes the
// real `git` would produce for each of the documented pinned-checkout
// commands. The stub exists so the full CLI flow runs end-to-end
// without a real remote — Plan 12 forbids public-network in tests
// and the controlled SSH fixture cannot reliably host git-over-SSH
// on every CI host. The stub's recorded calls prove the exact
// pinned sequence and env were honored.
type hostedGitStub struct {
	dir    string
	calls  []string
	commit string
}

func writeHostedGitStub(t *testing.T, commit string) *hostedGitStub {
	t.Helper()
	dir := t.TempDir()
	stub := &hostedGitStub{dir: dir, commit: commit}
	script := filepath.Join(dir, "git")
	body := `#!/bin/sh
echo "git $@" >> "$STUB_CALL_LOG"
checkoutDir=""
# Strip leading -c KEY=VAL pairs and -C <dir> so the case statement
# sees the real subcommand.
while [ $# -gt 0 ]; do
  case "$1" in
    -C) shift; checkoutDir="$1"; shift ;;
    -c) shift 2 ;;
    *) break ;;
  esac
done
case "$1" in
  init)
    # git init -q <dir>
    target="$3"
    mkdir -p "$target/.git"
    echo "ref: refs/heads/main" > "$target/.git/HEAD"
    exit 0
    ;;
  remote)
    exit 0
    ;;
  fetch)
    if [ -n "$checkoutDir" ]; then
      echo "$STUB_COMMIT" > "$checkoutDir/.git/FETCH_HEAD"
    fi
    exit 0
    ;;
  checkout)
    exit 0
    ;;
  rev-parse)
    # git rev-parse --verify HEAD echoes the pinned commit;
    # git rev-parse --show-toplevel echoes the checkout root.
    if [ "$2" = "--verify" ]; then
      echo "$STUB_COMMIT"
    else
      echo "$checkoutDir"
    fi
    exit 0
    ;;
  ls-files)
    exit 0
    ;;
  check-attr)
    exit 0
    ;;
esac
exit 1
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "calls.log")
	if err := os.Setenv("STUB_CALL_LOG", logPath); err != nil {
		t.Fatal(err)
	}
	t.Logf("stub log: %s", logPath)
	if err := os.Setenv("STUB_COMMIT", commit); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Unsetenv("STUB_CALL_LOG")
		_ = os.Unsetenv("STUB_COMMIT")
	})
	previous := os.Getenv("PATH")
	os.Setenv("PATH", dir+string(os.PathListSeparator)+previous)
	t.Cleanup(func() { os.Setenv("PATH", previous) })
	return stub
}

func (s *hostedGitStub) callsFromLog(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.dir, "calls.log"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

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
	l := model.Layout{Root: root}
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
	l := model.Layout{Root: root}
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
	l := model.Layout{Root: root}
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

// TestCLICoordinatorHostedEndToEndWithStubGit pins the public CLI
// path: `vci build --hosted <project>` from a coordinator root
// produces a run record, the additive source_provenance block lands
// in the staged snapshot, and the pinned git sequence is what was
// expected. Uses a stub `git` in PATH so no public network or real
// remote is required.
func TestCLICoordinatorHostedEndToEndWithStubGit(t *testing.T) {
	commit := "0123456789abcdef0123456789abcdef01234567"
	stub := writeHostedGitStub(t, commit)

	root := t.TempDir()
	t.Setenv("VCI_ROOT", root)
	l := model.Layout{Root: root}
	if err := app.Initialize(l); err != nil {
		t.Fatal(err)
	}
	if err := app.AddMachine(l, "mac-local", config.Machine{}); err != nil {
		t.Fatal(err)
	}
	if err := app.AddProject(l, "demo", config.Project{Machines: []string{"mac-local"}, Command: []string{"true"}}); err != nil {
		t.Fatal(err)
	}
	url := "https://example.com/owner/repo.git"
	if err := app.SetHostedFallback(l, "demo", url, commit); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := Run([]string{"build", "--hosted", "demo"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("build --hosted: %d %s", code, out.String())
	}
	var resp Response
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.OK {
		t.Fatalf("not OK: %+v", resp)
	}
	runID, _ := resp.Data.(map[string]any)["run_id"].(string)
	if runID == "" {
		t.Fatalf("no run_id: %s", out.String())
	}
	// The staged record carries an additive source_provenance
	// block. Use `vci check` to read it back; the public envelope
	// returns the saved snapshot.
	var checkBuf, checkErr bytes.Buffer
	if code := Run([]string{"check", runID}, &checkBuf, &checkErr); code != 0 {
		t.Fatalf("check: %d %s", code, checkBuf.String())
	}
	var checkEnv Response
	_ = json.Unmarshal(checkBuf.Bytes(), &checkEnv)
	data, _ := checkEnv.Data.(map[string]any)
	cfgSnap, _ := data["config_snapshot"].(map[string]any)
	if cfgSnap == nil {
		t.Fatalf("no config_snapshot: %+v", data)
	}
	prov, _ := cfgSnap["source_provenance"].(map[string]any)
	if prov == nil {
		t.Fatalf("no source_provenance in snapshot: %+v", cfgSnap)
	}
	if prov["kind"] != "hosted_git" {
		t.Fatalf("kind: %v", prov["kind"])
	}
	if prov["url"] != url {
		t.Fatalf("url: %v", prov["url"])
	}
	if prov["commit"] != commit {
		t.Fatalf("commit: %v", prov["commit"])
	}
	// No checkout path / credential may appear in the snapshot.
	for _, banned := range []string{"checkout_path", "credential", "token", "password"} {
		if _, present := cfgSnap[banned]; present {
			t.Fatalf("%s leaked into snapshot: %v", banned, cfgSnap[banned])
		}
	}
	// The stub recorded the documented git sequence.
	calls := stub.callsFromLog(t)
	joined := strings.Join(calls, "\n")
	wantSubs := []string{"init", "remote add origin", "fetch", "checkout", "rev-parse"}
	for _, want := range wantSubs {
		if !strings.Contains(joined, want) {
			t.Fatalf("stub git sequence missing %q; got: %s", want, joined)
		}
	}
	// No stale vci-hosted-* remains under TempDir after the build
	// has moved past the staging state.
	tmp := l.TempDir()
	_ = os.MkdirAll(tmp, 0o700)
	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "vci-hosted-") {
			t.Fatalf("stale hosted root remained: %s", e.Name())
		}
	}
}

// TestCLIClientHostedProxiesViaRemoteCommand pins that a client root
// forwards `build --hosted <project>` to the coordinator over SSH
// without producing a run record locally. The remote target is
// unreachable so the test asserts only the proxy envelope path.
func TestCLIClientHostedProxiesViaRemoteCommand(t *testing.T) {
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
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil {
		t.Fatalf("expected error envelope: %s", out.String())
	}
	// The remote target is unreachable; this is an infrastructure
	// failure (SSH, not hosted). The classifier is
	// remote_unavailable.
	if resp.Error.Class != "infrastructure" {
		t.Fatalf("want infrastructure, got %+v", resp)
	}
	// No run record was created on the client.
	entries, _ := os.ReadDir(filepath.Join(root, "state", "runs"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "run_") {
			t.Fatalf("client created run record %s", e.Name())
		}
	}
	// No machine / project table on the client.
	cfg2, _ := config.Load(filepath.Join(root, "config.toml"))
	if len(cfg2.Projects) > 0 {
		t.Fatalf("client mutated projects: %+v", cfg2.Projects)
	}
}
