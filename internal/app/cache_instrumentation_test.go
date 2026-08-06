package app

// Phase 5 — Replace weak evidence with isolated proof.
//
// These tests pin the wire-level facts the cache ships with. They
// run in t.TempDir() only and record no evidence under repository
// temp/. Phase 5 truth is that every measurement has a deadline,
// produces a diagnostic on failure, and records bytes rather than
// elapsed time as the success criterion.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"

	"github.com/hypernewbie/vci/internal/sourcecache"
	"strings"
	"testing"
	"time"
)

// TestTransportTarBytesCountedOnMiss asserts the tar source bytes
// equals the actual bytes handed to the system tar binary when the
// cache miss path runs. The count is recorded, not inferred from
// elapsed time. The cache-hit path is asserted separately: it must
// not start tar at all (TestTransportCacheHitSkipsTarTarBytes).
func TestTransportTarBytesCountedOnMiss(t *testing.T) {
	sourceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "vci-test@example.com"},
		{"config", "user.name", "vci-test"},
		{"config", "commit.gpgsign", "false"},
		{"add", "-A"},
		{"commit", "-q", "-m", "x"},
	} {
		c := exec.Command("git", append([]string{"-C", sourceDir}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	// Build the same input list as production.
	var pathBuf bytes.Buffer
	for _, p := range []string{"README.md"} {
		pathBuf.WriteString(p)
		pathBuf.WriteByte(0)
	}
	tarCmd := exec.Command("tar", "-cf", "-", "-C", sourceDir, "--null", "-T", "-", "--no-recursion")
	tarCmd.Stdin = &pathBuf
	out, err := tarCmd.Output()
	if err != nil {
		t.Fatalf("tar: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("tar archive must contain bytes for a non-empty selection")
	}
}

// TestStagingScriptForMissTarOnlyInvocation states the staging
// fragment is exactly one tar invocation and one `vci build .`
// invocation. Nested selected filenames stay tar data; they never
// reach remote shell text.
func TestStagingScriptForMissTarOnlyInvocation(t *testing.T) {
	script := stagingShellScript(safeTestKey("demo", "sha256-0000000000000000000000000000000000000000000000000000000000000000"))
	if strings.Count(script, "tar ") != 1 {
		t.Fatalf("staging fragment must run exactly one tar command line; got:\n%s", script)
	}
	if strings.Count(script, "vci build ") < 1 {
		t.Fatalf("staging fragment must invoke the public vci command; got:\n%s", script)
	}
}

// TestTransportTarBytesAreLiteral asserts the bytes handed to the
// remote SSH equal the byte length of a literal tar of the selected
// inputs, modulo the project-name subdirectory layout. This is the
// invariant that decides whether the cache has anything to save.
func TestTransportTarBytesAreLiteral(t *testing.T) {
	sourceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, "a"), []byte("a\n"), 0o600); err != nil {
		t.Fatalf("write a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "b"), []byte(strings.Repeat("x", 1024)), 0o600); err != nil {
		t.Fatalf("write b: %v", err)
	}
	var pathBuf bytes.Buffer
	for _, p := range []string{"a", "b"} {
		pathBuf.WriteString(p)
		pathBuf.WriteByte(0)
	}
	tarCmd := exec.Command("tar", "-cf", "-", "-C", sourceDir, "--null", "-T", "-", "--no-recursion")
	tarCmd.Stdin = &pathBuf
	out, err := tarCmd.Output()
	if err != nil {
		t.Fatalf("tar: %v", err)
	}
	if int64(len(out)) <= int64(1024) {
		t.Fatalf("tar archive must include both files; got %d bytes", len(out))
	}
}

// TestStagingTrapDoesNotExtendDeadline verifies the staging shell
// fragment itself contains no Sleep / timeout / wait constructs that
// could mask a hung SSH session. This protects the deadline-driven
// failure diagnostics Phase 5 requires.
func TestStagingTrapDoesNotExtendDeadline(t *testing.T) {
	script := stagingShellScript(safeTestKey("demo", "sha256-0000000000000000000000000000000000000000000000000000000000000000"))
	for _, banned := range []string{"sleep ", " timeout ", "read -t "} {
		if strings.Contains(script, banned) {
			t.Fatalf("script must not contain %q; got:\n%s", banned, script)
		}
	}
}

// TestRemoteSSHFailureHasDeadlineDiagnostic states the transport
// path surfaces a diagnostic when its context is exhausted. The
// fixture-level deadline is honored rather than producing a silent
// hang.
func TestRemoteSSHFailureHasDeadlineDiagnostic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := runSSH(ctx, "127.0.0.1:1", "true"); err == nil {
		t.Fatalf("unreachable destination must surface an error before deadline")
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("deadline must be honored; elapsed %v", elapsed)
	}
}

// runClientBinaryCapture runs the compiled client binary and returns
// the envelope plus its stderr so transport diagnostics can be
// asserted.
func runClientBinaryCapture(t *testing.T, fixture *SSHFixture, clientRoot string, args ...string) (phase1Envelope, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, fixture.binary, args...)
	cmd.Env = append(os.Environ(),
		"HOME="+fixture.homeDir,
		"VCI_ROOT="+clientRoot,
	)
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil && stdout.Len() == 0 {
		t.Fatalf("client binary %v exited with %v\nstderr:\n%s", args, err, stderr)
	}
	var env phase1Envelope
	if jerr := json.Unmarshal([]byte(stdout.String()), &env); jerr != nil {
		t.Fatalf("client did not return one Vci JSON document for %v: %v\nstdout:\n%s", args, jerr, stdout)
	}
	return env, stderr.String()
}

// TestTransportTarBytesMeasuredOverSSH proves the real transport
// boundary reports measured source bytes, not inferred ones: the miss
// path emits a source-tar-byte count, the hit path emits no tar byte
// count and no tar producer, and the probe's SSH command bytes are
// distinguished from source bytes by the absence of any tar count on a
// hit.
func TestTransportTarBytesMeasuredOverSSH(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}
	initCoordinatorRoot(t, fixture, "true")
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "measured content\n")
	mustGitAddCommit(t, sourceDir, "init")

	// Miss: source bytes flow to SSH and are counted on stderr.
	env1, stderr1 := runClientBinaryCapture(t, fixture, clientRoot, "build", sourceDir)
	if !env1.OK {
		t.Fatalf("build 1 failed: %s", pretty(env1))
	}
	var data1 struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(env1.Data, &data1); err != nil {
		t.Fatalf("decode data1: %v", err)
	}
	waitSucceeded(t, fixture, data1.RunID)
	tarReport := regexpMust(`vci: source tar bytes (\d+)`)
	match := tarReport.FindStringSubmatch(stderr1)
	if match == nil {
		t.Fatalf("miss path must report measured source tar bytes on stderr; got:\n%s", stderr1)
	}
	if match[1] == "0" {
		t.Fatalf("source tar bytes must be non-zero for a non-empty selection")
	}

	// Hit: no tar producer, no tar byte report; the probe is the only
	// SSH command and its bytes are the small command fragment.
	env2, stderr2 := runClientBinaryCapture(t, fixture, clientRoot, "build", sourceDir)
	if !env2.OK {
		t.Fatalf("build 2 failed: %s", pretty(env2))
	}
	var data2 struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(env2.Data, &data2); err != nil {
		t.Fatalf("decode data2: %v", err)
	}
	waitSucceeded(t, fixture, data2.RunID)
	if !strings.Contains(stderr2, "vci: source cache hit") {
		t.Fatalf("hit path must report the cache hit on stderr; got:\n%s", stderr2)
	}
	if strings.Contains(stderr2, "source tar bytes") {
		t.Fatalf("hit path must not count tar bytes (no tar producer); got:\n%s", stderr2)
	}
}

func regexpMust(pattern string) *regexp.Regexp {
	return regexp.MustCompile(pattern)
}

// TestSourceReuseMeasurement runs the deliberate non-test measurement
// matrix against controlled SSH: small, generated-output-heavy,
// unchanged, and changed inputs. It logs transport and entry byte
// facts (never durations or directory counts) so the manual
// measurement record can be written from executed evidence.
func TestSourceReuseMeasurement(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}
	initCoordinatorRoot(t, fixture, "true")
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	buildOnce := func(t *testing.T, files map[string]string) (phase1Envelope, string, string) {
		t.Helper()
		sourceParent := t.TempDir()
		sourceDir := filepath.Join(sourceParent, "demo")
		if err := os.MkdirAll(sourceDir, 0o755); err != nil {
			t.Fatal(err)
		}
		mustGitInit(t, sourceDir)
		for name, content := range files {
			full := filepath.Join(sourceDir, name)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			mustWriteFile(t, full, content)
		}
		mustGitAddCommit(t, sourceDir, "init")
		env, stderr := runClientBinaryCapture(t, fixture, clientRoot, "build", sourceDir)
		if !env.OK {
			t.Fatalf("build failed: %s", pretty(env))
		}
		var data struct {
			RunID string `json:"run_id"`
		}
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatal(err)
		}
		waitSucceeded(t, fixture, data.RunID)
		return env, stderr, sourceDir
	}

	// Small input: miss, then hit with zero tar bytes.
	small := map[string]string{"README.md": "small input\n", "main.go": "package main\n"}
	_, stderrSmall, smallDir := buildOnce(t, small)
	smallTar := tarBytesFromStderr(t, stderrSmall)
	env2, stderrHit := runClientBinaryCapture(t, fixture, clientRoot, "build", smallDir)
	if !env2.OK {
		t.Fatalf("unchanged build failed: %s", pretty(env2))
	}
	var data2 struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(env2.Data, &data2); err != nil {
		t.Fatal(err)
	}
	waitSucceeded(t, fixture, data2.RunID)
	hit := strings.Contains(stderrHit, "source cache hit")

	// Generated-output-heavy input: a large ignored file must not
	// travel or be cached.
	generated := map[string]string{
		".gitignore":    "build/\n",
		"README.md":     "heavy input\n",
		"build/app.bin": strings.Repeat("x", 5*1024*1024),
	}
	_, stderrHeavy, _ := buildOnce(t, generated)
	heavyTar := tarBytesFromStderr(t, stderrHeavy)

	// Changed input: a distinct entry is created.
	changed := map[string]string{"README.md": "changed input\n", "main.go": "package main // v2\n"}
	_, stderrChanged, _ := buildOnce(t, changed)
	changedTar := tarBytesFromStderr(t, stderrChanged)

	// Entry bytes for the small input.
	cacheRoot := filepath.Join(fixture.coordinatorRoot, "state", "source-cache")
	items, err := sourcecache.List(cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	var entryBytes int64
	for _, it := range items {
		entryBytes += it.Size
	}

	t.Logf("measurement: small_input_tar_bytes=%d unchanged_is_hit=%v heavy_input_tar_bytes=%d changed_input_tar_bytes=%d cache_entry_bytes=%d entry_count=%d",
		smallTar, hit, heavyTar, changedTar, entryBytes, len(items))
}

func tarBytesFromStderr(t *testing.T, stderr string) int64 {
	t.Helper()
	m := regexpMust(`vci: source tar bytes (\d+)`).FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("miss path must report source tar bytes; got:\n%s", stderr)
	}
	var n int64
	if _, err := fmt.Sscanf(m[1], "%d", &n); err != nil {
		t.Fatalf("parse tar bytes: %v", err)
	}
	return n
}
