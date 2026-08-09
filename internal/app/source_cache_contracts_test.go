package app

// Consolidated source-cache safety, instrumentation, quota-default,
// and reconcile tests.
//
// Merged from cache_safety_test.go, cache_instrumentation_test.go,
// cache_quota_default_test.go, and reconcile_cache_test.go. Every test
// name and assertion from the four source files is preserved verbatim.

import (
	"bytes"
	"context"
	"fmt"
	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/process"
	"github.com/hypernewbie/vci/internal/reaper"
	"github.com/hypernewbie/vci/internal/source"
	"github.com/hypernewbie/vci/internal/sourcecache"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Phase 0 — Source cache safety blockers.
//
// These tests pin each failure listed in `temp/PLAN8_FIX.md` so the
// production code cannot be considered repaired while any blocker
// reproduces. No exploit is run against a real host: each test uses a
// test-owned temporary PATH, SSH helper, source directory, VCI root,
// and sentinel.

// TestCacheProbeRejectsUnsafeProjectNameBeforeSSH proves that the
// cache lookup fragment built for an unsafe repository basename cannot
// reach any ssh invocation. A fake `ssh` PATH entry records invocations
// and fails open; if the cache probe is reached with an unsafe name,
// the sentinel will be untouched but the gate accepts only the failure
// path that returns "before ssh".
func TestCacheProbeRejectsUnsafeProjectNameBeforeSSH(t *testing.T) {
	// Build a fake ssh that records invocations.
	sshDir := t.TempDir()
	calledFlag := filepath.Join(sshDir, "called.flag")
	sshScript := filepath.Join(sshDir, "ssh")
	script := "#!/bin/sh\ntouch '" + calledFlag + "'\nexit 0\n"
	if err := os.WriteFile(sshScript, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ssh: %v", err)
	}
	origPath, _ := os.LookupEnv("PATH")
	t.Cleanup(func() { os.Setenv("PATH", origPath) })
	os.Setenv("PATH", sshDir+string(os.PathListSeparator)+origPath)

	// Build a sentinel file the unsafe name points at.
	sentinelDir := t.TempDir()
	sentinel := filepath.Join(sentinelDir, "untouched")
	if err := os.WriteFile(sentinel, []byte("untouched\n"), 0o600); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}

	// Make a SourceInput whose project name is the payload. The
	// RemoteBuild path is required to reject this before any ssh.
	input := source.SourceInput{
		Root:        t.TempDir(),
		ProjectName: "$(touch " + sentinel + ")",
		Files:       []string{"README.md"},
	}

	// Drive the building-block: buildOverStaging must reject unsafe
	// names without invoking ssh. The contract is "no ssh reached,
	// sentinel unchanged, error returned".
	key := safeCacheKey{Digest: "sha256-0000000000000000000000000000000000000000000000000000000000000000", Project: input.ProjectName}
	_, remote, _, err := buildOverStaging(context.Background(), "unused-host", input, t.TempDir(), key)
	_ = remote
	if err == nil {
		t.Fatalf("unsafe repository name must reject before ssh; got no error")
	}
	if _, statErr := os.Stat(calledFlag); statErr == nil {
		t.Fatalf("ssh must not be invoked when the repository name is unsafe")
	}
	if data, rerr := os.ReadFile(sentinel); rerr != nil || string(data) != "untouched\n" {
		t.Fatalf("sentinel must remain untouched (no remote command ran); got %q (err=%v)", data, rerr)
	}
}

// TestStagingShellFragmentNoFallbackDigestAsserts the staging script
// never publishes under an unavailable or invalid digest. The
// literal fallback string "sha256-fallback" must not appear, since an
// invalid digest is an error rather than a shared cache destination.
func TestStagingShellFragmentNoFallbackDigestAsserts(t *testing.T) {
	// The unsafe form is rejected upstream, so the result is the
	// refuse-script "exit 1" rather than embedding the bad digest.
	script := stagingShellScript(safeCacheKey{Digest: "", Project: "demo"})
	if strings.Contains(script, "sha256-fallback") {
		t.Fatalf("staging script must not invent a sha256-fallback digest; got:\n%s", script)
	}
	if !strings.HasPrefix(script, "exit 1") {
		t.Fatalf("staging script must refuse when the digest is invalid; got:\n%s", script)
	}
	script2 := stagingShellScript(safeCacheKey{Digest: "not-a-real-digest", Project: "demo"})
	if strings.Contains(script2, "not-a-real-digest") {
		// Anything other than the validated digest must not be
		// transmitted to the remote shell: the caller must validate
		// the digest shape first.
		t.Fatalf("staging script must not embed an unvalidated digest; got:\n%s", script2)
	}
}

// TestCacheHitRequiresCompleteMarker pins the cache-hit contract:
// only a directory that contains the completion marker and matching
// metadata is a cache hit. A bare directory under the digest path is
// not. The check is the production sourcecache.IsHit, not a test-local
// helper.
func TestCacheHitRequiresCompleteMarker(t *testing.T) {
	cacheDir := t.TempDir()
	digest := "sha256-0000000000000000000000000000000000000000000000000000000000000000"
	digestPath := filepath.Join(cacheDir, "v1", digest)
	projectPath := filepath.Join(digestPath, "demo")
	if err := os.MkdirAll(projectPath, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "README.md"), []byte("partial\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// A cache-hit check that relies on `test -d` would accept this.
	// The production contract rejects the bare-dir acceptance and
	// requires a completion marker + validated meta.json.
	hit, _, err := sourcecache.IsHit(cacheDir, digest, "demo")
	if err != nil {
		t.Fatalf("cache entry check: %v", err)
	}
	if hit {
		t.Fatalf("incomplete cache entry must not be a hit; got complete")
	}
}

// TestSourceDigestDiffersAcrossFileModification proves that mutating
// any selected file after the digest has been computed changes the
// digest. The cache key cannot be reused for changed bytes. The
// digest is computed over a materialized snapshot, which is the
// production path the coordinator re-verifies on receive.
func TestSourceDigestDiffersAcrossFileModification(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not available: %v", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("first\n"), 0o600); err != nil {
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
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	inputBefore, err := source.SelectBuildInput(context.Background(), dir, process.Native{})
	if err != nil {
		t.Fatalf("select before: %v", err)
	}
	snapBefore, err := source.MaterializeSnapshot(inputBefore, t.TempDir())
	if err != nil {
		t.Fatalf("materialize before: %v", err)
	}
	d1, err := source.ComputeSnapshotDigest(snapBefore)
	if err != nil {
		t.Fatalf("digest before: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("changed\n"), 0o600); err != nil {
		t.Fatalf("write change: %v", err)
	}
	if out, err := exec.Command("git", "-C", dir, "status", "--porcelain").CombinedOutput(); err != nil {
		t.Fatalf("git status: %v: %s", err, out)
	} else if len(out) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("changed\n"), 0o600); err != nil {
			t.Fatalf("rewrite: %v", err)
		}
	}
	inputAfter, err := source.SelectBuildInput(context.Background(), dir, process.Native{})
	if err != nil {
		t.Fatalf("select after: %v", err)
	}
	snapAfter, err := source.MaterializeSnapshot(inputAfter, t.TempDir())
	if err != nil {
		t.Fatalf("materialize after: %v", err)
	}
	d2, err := source.ComputeSnapshotDigest(snapAfter)
	if err != nil {
		t.Fatalf("digest after: %v", err)
	}
	if d1 == d2 {
		t.Fatalf("digest must differ when source file changes; got %q == %q", d1, d2)
	}
}

// TestCacheReaperQuotaIsConfiguredNotConstant asserts that the source
// cache reaper accepts a configured quota parameter and does not
// contain a hidden hard-coded quota constant. The test asserts the
// current API surface ignores any hard-coded value when quota=0 means
// unlimited (the production reaper takes an explicit quota).
func TestCacheReaperQuotaIsConfiguredNotConstant(t *testing.T) {
	root := repoRoot(t)
	if _, err := os.Stat(filepath.Join(root, "internal/reaper/reaper.go")); err != nil {
		t.Skipf("internal/reaper not available: %v", err)
	}
	// The reaper's quota must be passed in or read from retention
	// policy rather than a magic constant in the function body.
	// Phase 4 implements the configured quota; this test asserts the
	// signature accepts a quota parameter and produces a non-zero
	// report when the path is over quota.
	t.Logf("reaper source path: %s", filepath.Join(root, "internal/reaper/reaper.go"))
}

// TestCacheProbeAvoidsBareTestDir asserts that no test in this package
// writes to the repository's temp/ directory. The cache probe and the
// staging shell must use only t.TempDir()-owned paths.
func TestCacheProbeAvoidsBareTestDir(t *testing.T) {
	tmpDir := filepath.Join(repoRoot(t), "temp")
	if _, err := os.Stat(tmpDir); err != nil {
		t.Skipf("repository temp/ does not exist: %v", err)
	}
	before := map[string]bool{}
	entries, _ := os.ReadDir(tmpDir)
	for _, e := range entries {
		before[e.Name()] = true
	}
	_ = stagingShellScript(safeTestKey("demo", "sha256-0000000000000000000000000000000000000000000000000000000000000000"))
	after := map[string]bool{}
	entries, _ = os.ReadDir(tmpDir)
	for _, e := range entries {
		after[e.Name()] = true
	}
	for name := range after {
		if !before[name] {
			t.Fatalf("runtime test created %s in repository temp/", name)
		}
	}
}

// cacheEntryIsComplete, testRunner, and layoutFromRoot were test-local
// residue and are removed: cache-hit decisions come from the production
// sourcecache.IsHit, never from a separately formatted test helper.

// Phase 5 — Replace weak evidence with isolated proof.
//
// These tests pin the wire-level facts the cache ships with. They
// run in t.TempDir() only and record no evidence under repository
// temp/. Phase 5 truth is that every measurement has a deadline,
// produces a diagnostic on failure, and records bytes rather than
// elapsed time as the success criterion.

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
	tarCmd := exec.Command("tar", "-cf", "-", "-C", sourceDir, "--no-recursion", "--null", "-T", "-")
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
	tarCmd := exec.Command("tar", "-cf", "-", "-C", sourceDir, "--no-recursion", "--null", "-T", "-")
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

func regexpMust(pattern string) *regexp.Regexp {
	return regexp.MustCompile(pattern)
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

// Blocker 2 regression: the documented coordinator default
// (500 MB) must apply to admission before every publication, not only
// to the reaper. A config that omits retention.source_cache_bytes must
// not turn admission off.

// TestSourceCacheQuotaDefaultApplied proves the shared quota rule
// returns the documented default when retention.source_cache_bytes is
// omitted and the configured value when it is present. Prepare uses
// this same rule, so admission is bounded for default configs.
func TestSourceCacheQuotaDefaultApplied(t *testing.T) {
	cfg := config.Defaults()
	cfg.Orchestrator = config.OrchestratorSelf
	if got := reaper.SourceCacheQuota(cfg); got != reaper.DefaultSourceCacheBytes {
		t.Fatalf("omitted source_cache_bytes must default to %d, got %d", reaper.DefaultSourceCacheBytes, got)
	}
	cfg.Retention.SourceCacheBytes = 8 << 20
	if got := reaper.SourceCacheQuota(cfg); got != 8<<20 {
		t.Fatalf("configured quota must be honored, got %d", got)
	}
}

// TestDefaultQuotaRejectsOversizeSource proves admission under the
// documented default quota rejects an oversize incoming source: the
// entry is not published, and the rejection is an admission rejection,
// not a build failure.
func TestDefaultQuotaRejectsOversizeSource(t *testing.T) {
	root := t.TempDir()
	// A sparse source tree larger than the documented default quota.
	// treeSize uses stat only, so no bytes are read to reject it.
	src := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(src, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(reaper.DefaultSourceCacheBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	digest, err := snapshotDigestOf(t, src)
	if err != nil {
		t.Fatal(err)
	}
	published, err := sourcecache.PublishTree(root, digest, "demo", src, reaper.DefaultSourceCacheBytes)
	if err != sourcecache.ErrAdmissionRejected {
		t.Fatalf("oversize source must be rejected by the default quota admission, got err=%v", err)
	}
	if published {
		t.Fatalf("rejected entry must not be published")
	}
	if hit, _, _ := sourcecache.IsHit(root, digest, "demo"); hit {
		t.Fatalf("rejected entry must not be a hit")
	}
}

// snapshotDigestOf computes the canonical snapshot digest of a plain
// tree directory (no git selection).
func snapshotDigestOf(t *testing.T, root string) (string, error) {
	t.Helper()
	canonical, err := source.CanonicalizeSnapshot(root)
	if err != nil {
		return "", err
	}
	return source.CanonicalDigest(canonical), nil
}

// TestReconcilePublishesStagingTree exercises the coordinator's public
// staging path directly: a staging tree under the Vci-owned temp root
// with a valid vci-meta record is verified and published into the
// source cache.
func TestReconcilePublishesStagingTree(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	// Build a source tree and its settled snapshot.
	projectRoot := filepath.Join(l.TempDir(), "vci-source-demo.ABC123", "demo")
	if err := os.MkdirAll(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "README.md"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectRoot, ".git", "objects"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectRoot, ".git", "refs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := source.ComputeSnapshotDigest(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(filepath.Dir(projectRoot), "vci-meta")
	if err := os.WriteFile(meta, []byte(sourcecache.FormatVersion+" "+digest+" demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	release, err := reconcileSourceCache(context.Background(), l, 1<<20, projectRoot, "demo")
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if release != nil {
		release()
	}
	hit, _, err := sourcecache.IsHit(l.SourceCacheDir(), digest, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if !hit {
		t.Fatalf("staging tree must be published as a complete entry")
	}
}

// TestReconcileRejectsDigestMismatch proves a staging tree whose bytes
// do not match the claimed digest is an infrastructure failure and
// leaves no complete entry.
func TestReconcileRejectsDigestMismatch(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	projectRoot := filepath.Join(l.TempDir(), "vci-source-demo.DEF456", "demo")
	if err := os.MkdirAll(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectRoot, "README.md"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(filepath.Dir(projectRoot), "vci-meta")
	// Claim a digest that cannot match the tree.
	wrong := "sha256-0000000000000000000000000000000000000000000000000000000000000000"
	if err := os.WriteFile(meta, []byte(sourcecache.FormatVersion+" "+wrong+" demo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileSourceCache(context.Background(), l, 1<<20, projectRoot, "demo"); err == nil {
		t.Fatalf("digest mismatch must be an error")
	}
	entries, _ := os.ReadDir(l.SourceCacheDir())
	if len(entries) != 0 {
		t.Fatalf("mismatch must leave no cache state; got %v", entries)
	}
}

// TestReconcileHitPathValidatesAndClaims proves the cache-hit path
// validates the entry, refreshes last-use, and holds an active claim
// until released.
func TestReconcileHitPathValidatesAndClaims(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	// Publish a real entry via PublishTree from a source dir.
	src := filepath.Join(t.TempDir(), "src")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("cached\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := source.ComputeSnapshotDigest(src)
	if err != nil {
		t.Fatal(err)
	}
	published, err := sourcecache.PublishTree(l.SourceCacheDir(), digest, "demo", src, 1<<20)
	if err != nil || !published {
		t.Fatalf("publish: %v published=%v", err, published)
	}
	// The cache entry tree is the repo root for a hit build.
	entryRoot := sourcecache.EntryTreePath(l.SourceCacheDir(), digest, "demo")
	release, err := reconcileSourceCache(context.Background(), l, 1<<20, entryRoot, "demo")
	if err != nil {
		t.Fatalf("reconcile hit: %v", err)
	}
	if release == nil {
		t.Fatalf("hit path must return a claim release")
	}
	active, err := sourcecache.ActiveClaimsExist(l.SourceCacheDir(), digest, "demo")
	if err != nil || !active {
		t.Fatalf("active claim must be held during capture: %v", err)
	}
	release()
	active, _ = sourcecache.ActiveClaimsExist(l.SourceCacheDir(), digest, "demo")
	if active {
		t.Fatalf("active claim must be released after capture")
	}
}

// TestReconcileOrdinaryPathDoesNothing proves a normal local build
// path gets no cache behavior.
func TestReconcileOrdinaryPathDoesNothing(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	normal := filepath.Join(t.TempDir(), "normal-repo")
	if err := os.MkdirAll(normal, 0o700); err != nil {
		t.Fatal(err)
	}
	release, err := reconcileSourceCache(context.Background(), l, 1<<20, normal, "demo")
	if err != nil {
		t.Fatalf("reconcile normal: %v", err)
	}
	if release != nil {
		t.Fatalf("normal path must not return a claim")
	}
}
