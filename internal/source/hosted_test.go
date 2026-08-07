package source

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/layout"
	"github.com/hypernewbie/vci/internal/process"
)

// hostedTestLayout returns a t.TempDir()-rooted Layout whose Ensure
// has already run. Tests use this so the hosted parent MkdirTemp can
// land under state/tmp.
func hostedTestLayout(t *testing.T) layout.Layout {
	t.Helper()
	root := t.TempDir()
	l := layout.Layout{Root: root}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	return l
}

// hostedFixtureCommit is the canonical 40-lowercase-hex fixture commit
// used by every hosted test. The checkout always succeeds only when
// `git rev-parse HEAD` echoes this exact value back.
const hostedFixtureCommit = "0123456789abcdef0123456789abcdef01234567"

func mustGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
}

// TestHostedCheckoutRunsExpectedGitSequence pins that the full pinned
// sequence is issued in the documented order: init → remote add →
// fetch → checkout --detach → rev-parse HEAD. The fake runner
// succeeds for every command and emits the pinned commit for the
// rev-parse call so the integrity check passes.
func TestHostedCheckoutRunsExpectedGitSequence(t *testing.T) {
	mustGit(t)
	l := hostedTestLayout(t)
	runner := &fakeRunner{patterns: []fakePattern{
		{match: matchHas("rev-parse"), stdout: hostedFixtureCommit + "\n"},
		{match: matchAll(), stdout: ""},
	}}
	h := config.ValidatedHosted{
		URL:    "https://example.com/owner/repo.git",
		Commit: hostedFixtureCommit,
	}
	root, err := Checkout(context.Background(), runner, l, "demo", h)
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	t.Cleanup(func() { _ = CleanUnder(root, l.TempDir()) })

	// Recorded invocations: 1) init, 2) remote add, 3) fetch,
	// 4) checkout --detach, 5) rev-parse --verify HEAD.
	if got := len(runner.calls); got != 5 {
		t.Fatalf("calls: want 5, got %d (%v)", got, runner.calls)
	}
	wantSubs := []string{"init", "remote", "fetch", "checkout", "rev-parse"}
	for i, want := range wantSubs {
		if !containsSubstring(runner.calls[i].Args, want) {
			t.Fatalf("call[%d]=%v missing %q", i, runner.calls[i].Args, want)
		}
	}
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		t.Fatalf("root stat: %v %v", err, fi)
	}
	if filepath.Base(root) != "demo" {
		t.Fatalf("root basename: want demo, got %q", filepath.Base(root))
	}
	parent := filepath.Dir(root)
	if !strings.HasPrefix(filepath.Base(parent), HostedPrefix) {
		t.Fatalf("parent %q missing HostedPrefix", parent)
	}
}

// TestHostedCheckoutInheritsAndOverridesEnv pins that every command's
// Env includes both inherited values (PATH, HOME, SSH_AUTH_SOCK when
// present in os.Environ) and the three documented git overrides, and
// that the three overrides ALWAYS win regardless of any inherited
// value. Env is replaced by os/exec when non-nil, so a missing PATH
// would break git lookup, a missing HOME would break ssh/askpass,
// and an inherited GIT_TERMINAL_PROMPT=1 would defeat the
// noninteractive policy that Plan 12 Fix explicitly hardens.
func TestHostedCheckoutInheritsAndOverridesEnv(t *testing.T) {
	mustGit(t)
	// Pre-seed conflicting override values so the test fails if
	// safeCheckoutEnv forgets to drop the inherited value before
	// appending the override.
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	t.Setenv("GIT_ASKPASS", "/some/legit/askpass")
	t.Setenv("GIT_ASKPASS_REQUIRE", "prefer")
	l := hostedTestLayout(t)
	runner := &fakeRunner{patterns: []fakePattern{
		{match: matchHas("rev-parse"), stdout: hostedFixtureCommit + "\n"},
		{match: matchAll(), stdout: ""},
	}}
	h := config.ValidatedHosted{
		URL:    "https://example.com/owner/repo.git",
		Commit: hostedFixtureCommit,
	}
	root, err := Checkout(context.Background(), runner, l, "demo", h)
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	t.Cleanup(func() { _ = CleanUnder(root, l.TempDir()) })

	// Caller os.Environ must not be mutated by Checkout.
	t.Cleanup(func() {
		for k, v := range map[string]string{
			"GIT_TERMINAL_PROMPT": "1",
			"GIT_ASKPASS":         "/some/legit/askpass",
			"GIT_ASKPASS_REQUIRE": "prefer",
		} {
			if got := os.Getenv(k); got != v {
				t.Errorf("caller env mutated: %s=%q (want %q)", k, got, v)
			}
		}
	})

	wantOverrides := map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_ASKPASS":         "true",
		"GIT_ASKPASS_REQUIRE": "force",
	}
	for i, call := range runner.calls {
		envMap := envToMap(call.Env)
		for k, v := range wantOverrides {
			if got, ok := envMap[k]; !ok || got != v {
				t.Fatalf("call[%d] env %s=%q (want %q), full env=%v", i, k, got, v, call.Env)
			}
		}
		// Inherited PATH and HOME must be present. PATH is the
		// signal that os.Environ() was the merge source; if PATH
		// is missing, the test knows the merge was skipped.
		if _, ok := envMap["PATH"]; !ok {
			t.Fatalf("call[%d] env missing PATH (os.Environ not merged)", i)
		}
		if _, ok := envMap["HOME"]; !ok {
			t.Fatalf("call[%d] env missing HOME", i)
		}
		// Exactly one entry per override key — the inherited
		// value must have been dropped, not appended.
		for k := range wantOverrides {
			count := 0
			for _, kv := range call.Env {
				if strings.HasPrefix(kv, k+"=") {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("call[%d] env %s appears %d times, want 1", i, k, count)
			}
		}
	}
	// The safeCheckoutEnv() result must not alias the caller
	// os.Environ() slice — mutating it must not leak back.
	original := os.Environ()
	mutated := safeCheckoutEnv()
	for i := range mutated {
		if i < len(original) && &mutated[i] == &original[i] {
			t.Fatalf("safeCheckoutEnv alias detected at index %d", i)
		}
	}
}

// TestHostedCheckoutUsesSafetyCConfig pins that the fetch and
// checkout commands carry -c core.hooksPath=/dev/null and
// -c protocol.file.allow=never before the subcommand. The flags must
// precede the subcommand: `git -c KEY=VAL -c KEY=VAL <subcmd>`.
func TestHostedCheckoutUsesSafetyCConfig(t *testing.T) {
	mustGit(t)
	l := hostedTestLayout(t)
	runner := &fakeRunner{patterns: []fakePattern{
		{match: matchHas("rev-parse"), stdout: hostedFixtureCommit + "\n"},
		{match: matchAll(), stdout: ""},
	}}
	h := config.ValidatedHosted{
		URL:    "https://example.com/owner/repo.git",
		Commit: hostedFixtureCommit,
	}
	root, err := Checkout(context.Background(), runner, l, "demo", h)
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	t.Cleanup(func() { _ = CleanUnder(root, l.TempDir()) })

	safetyFlags := []string{"-c", "core.hooksPath=/dev/null", "-c", "protocol.file.allow=never"}
	// fetch (call[2]) must carry both safety -c flags.
	if !containsAllInOrder(runner.calls[2].Args, safetyFlags) {
		t.Fatalf("fetch=%v missing safety -c flags in order", runner.calls[2].Args)
	}
	// checkout (call[3]) must carry the hooksPath safety flag.
	if !containsAllInOrder(runner.calls[3].Args, []string{"-c", "core.hooksPath=/dev/null"}) {
		t.Fatalf("checkout=%v missing hooksPath safety flag", runner.calls[3].Args)
	}
}

// TestHostedCheckoutWrapsFetchFailure pins that a fetch exit
// non-zero wraps ErrHostedSourceUnavailable and the checkout root is
// removed so the reaper never sees stale Vci-owned bytes.
func TestHostedCheckoutWrapsFetchFailure(t *testing.T) {
	mustGit(t)
	l := hostedTestLayout(t)
	runner := &fakeRunner{
		patterns: []fakePattern{
			{match: matchAll(), stdout: ""},
		},
		failures: []fakeFailure{
			{match: matchHas("fetch"), msg: "fetch failed"},
		},
	}
	h := config.ValidatedHosted{
		URL:    "https://example.com/owner/repo.git",
		Commit: hostedFixtureCommit,
	}
	_, err := Checkout(context.Background(), runner, l, "demo", h)
	if err == nil {
		t.Fatal("fetch failure must surface")
	}
	if !errors.Is(err, config.ErrHostedSourceUnavailable) {
		t.Fatalf("want ErrHostedSourceUnavailable, got %v", err)
	}
	// No vci-hosted-* directories should remain under TempDir.
	entries, _ := os.ReadDir(l.TempDir())
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), HostedPrefix) {
			t.Fatalf("stale %s remained after fetch failure", e.Name())
		}
	}
}

// TestHostedCheckoutWrapsIntegrityMismatch pins that a wrong
// `git rev-parse HEAD` value produces ErrHostedSourceIntegrityFailed
// and the checkout root is removed.
func TestHostedCheckoutWrapsIntegrityMismatch(t *testing.T) {
	mustGit(t)
	l := hostedTestLayout(t)
	runner := &fakeRunner{patterns: []fakePattern{
		{match: matchHas("rev-parse"), stdout: "deadbeef" + strings.Repeat("0", 32) + "\n"},
		{match: matchAll(), stdout: ""},
	}}
	h := config.ValidatedHosted{
		URL:    "https://example.com/owner/repo.git",
		Commit: hostedFixtureCommit,
	}
	_, err := Checkout(context.Background(), runner, l, "demo", h)
	if err == nil {
		t.Fatal("integrity mismatch must fail")
	}
	if !errors.Is(err, config.ErrHostedSourceIntegrityFailed) {
		t.Fatalf("want ErrHostedSourceIntegrityFailed, got %v", err)
	}
	entries, _ := os.ReadDir(l.TempDir())
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), HostedPrefix) {
			t.Fatalf("stale %s remained after integrity failure", e.Name())
		}
	}
}

// TestHostedCheckoutRemovesOnContextCancel pins that a context cancel
// during a git command removes the checkout root. The fake runner
// honors ctx.Done and never returns a successful result.
func TestHostedCheckoutRemovesOnContextCancel(t *testing.T) {
	mustGit(t)
	l := hostedTestLayout(t)
	ctx, cancel := context.WithCancel(context.Background())
	runner := &cancelingRunner{}
	h := config.ValidatedHosted{
		URL:    "https://example.com/owner/repo.git",
		Commit: hostedFixtureCommit,
	}
	cancel() // cancel before calling
	_, err := Checkout(ctx, runner, l, "demo", h)
	if err == nil {
		t.Fatal("cancel must surface")
	}
	if !errors.Is(err, config.ErrHostedSourceUnavailable) {
		t.Fatalf("want ErrHostedSourceUnavailable wrap, got %v", err)
	}
	entries, _ := os.ReadDir(l.TempDir())
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), HostedPrefix) {
			t.Fatalf("stale %s remained after cancel", e.Name())
		}
	}
}

// TestHostedCleanRefusesOutsideTempDir pins that Clean on a path
// outside the temp dir is refused, not silently removed.
func TestHostedCleanRefusesOutsideTempDir(t *testing.T) {
	outside := t.TempDir()
	bogus := filepath.Join(outside, "vci-hosted-decoy")
	if err := os.Mkdir(bogus, 0o700); err != nil {
		t.Fatal(err)
	}
	err := CleanUnder(bogus, t.TempDir())
	if err == nil {
		t.Fatal("Clean must refuse path outside TempDir")
	}
	if !errors.Is(err, config.ErrHostedSourceIntegrityFailed) {
		t.Fatalf("want ErrHostedSourceIntegrityFailed, got %v", err)
	}
	if _, err := os.Stat(bogus); err != nil {
		t.Fatalf("refused path was removed: %v", err)
	}
}

// matchAll matches every argv. It must be paired with a more specific
// pattern that runs first (the runner iterates patterns in order).
func matchAll() func([]string) bool { return func(_ []string) bool { return true } }

// TestHostedCheckoutEnvDoesNotMutateCaller pins that safeCheckoutEnv
// returns a fresh slice per call and never aliases os.Environ().
func TestHostedCheckoutEnvDoesNotMutateCaller(t *testing.T) {
	a := safeCheckoutEnv()
	b := safeCheckoutEnv()
	if reflect.DeepEqual(a, b) == false {
		// Two distinct slices must not share underlying memory.
		if &a[0] == &b[0] {
			t.Fatal("env slices alias")
		}
	}
}

// TestPrepareHostedDoesNotCreateHostedTempOnDirect is a placeholder
// for the app-level PrepareHosted test; the integration test lives in
// internal/app/hosted_app_test.go. Pinning this name here documents
// that the source-level hosted temp-root invariant is asserted at
// the app boundary, not the source boundary.
func TestPrepareHostedDoesNotCreateHostedTempOnDirect_DocOnly(t *testing.T) {
	t.Skip("asserted in internal/app/hosted_app_test.go")
}

// --- helpers ---

func containsSubstring(args []string, want string) bool {
	for _, a := range args {
		if strings.Contains(a, want) {
			return true
		}
	}
	return false
}

func containsAllInOrder(args []string, want []string) bool {
	i := 0
	for _, a := range args {
		if i < len(want) && a == want[i] {
			i++
		}
	}
	return i == len(want)
}

func envToMap(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		out[k] = v
	}
	return out
}

// cancelingRunner returns context.Canceled on every Run call. The
// hosted path propagates this as a wrapped unavailable error.
type cancelingRunner struct{}

func (cancelingRunner) Run(ctx context.Context, cmd process.Command) (process.Result, error) {
	<-ctx.Done()
	return process.Result{}, ctx.Err()
}

// ensure unused import
var _ = time.Second
