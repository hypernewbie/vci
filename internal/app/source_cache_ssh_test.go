package app

// Controlled-SSH source-cache integration tests.
//
// Every test drives the public client command (`vci build <path>`) through
// ordinary system SSH against the loopback fixture and asserts cache
// behavior at the coordinator's real state/source-cache layout. A cache
// entry is Vci-owned and complete only when meta.json and the project tree
// exist and the `complete` marker is present.
//
// The two-project tests additionally prove that cache identity is
// (format_version, digest, project) everywhere: two repositories with
// identical selected content and different basenames must own independent
// entries under the shared digest — each builds, each retains a valid
// entry, each gets a cache hit on resubmission without a tar producer, and
// neither can invalidate the other.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/process"
	"github.com/hypernewbie/vci/internal/source"
	"github.com/hypernewbie/vci/internal/sourcecache"
)

// waitSucceeded polls the remote run state with a deadline until the
// run reaches a terminal state; it fails the test on timeout or on a
// remote job failure.
func waitSucceeded(t *testing.T, fixture *SSHFixture, runID string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatalf("remote build %s did not reach a terminal state in 20s", runID)
		}
		state := remoteCheckState(t, fixture, runID)
		if state == "succeeded" {
			return
		}
		if state == "failed" || state == "lost" || state == "aborted" {
			t.Fatalf("remote build %s ended in state %q", runID, state)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// cacheDigestEntries returns the digest directory names under the
// coordinator's versioned cache root, excluding the reserved scratch
// directories (partial, locks).
func cacheDigestEntries(t *testing.T, fixture *SSHFixture) []string {
	t.Helper()
	cacheRoot := filepath.Join(fixture.coordinatorRoot, "state", "source-cache", "v1")
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read cache root: %v", err)
	}
	var digests []string
	for _, e := range entries {
		if e.IsDir() && e.Name() != "partial" && e.Name() != "locks" {
			digests = append(digests, e.Name())
		}
	}
	return digests
}

// TestSourceCacheReuseOverSSH proves, through the public client command,
// that the first submission creates exactly one valid complete entry,
// that an unchanged resubmission reuses it without starting any tar
// producer, and that changed selected content creates a distinct entry.
func TestSourceCacheReuseOverSSH(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}

	initCoordinatorRoot(t, fixture, "sh", "-c", "echo 'BUILD SUCCESS'")
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "initial\n")
	mustGitAddCommit(t, sourceDir, "init")

	// 1. First submission creates exactly one valid complete entry.
	env1 := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
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

	digests := cacheDigestEntries(t, fixture)
	if len(digests) != 1 {
		t.Fatalf("first submission must create exactly one complete entry, got %v", digests)
	}
	firstDigest := digests[0]
	assertCompleteEntry(t, fixture, firstDigest, "demo")

	// 2. Unchanged resubmission: the probe hits and no tar producer
	// starts. A fake tar placed in the client PATH records any
	// invocation; the hit path must never reach it.
	flag := filepath.Join(t.TempDir(), "tar-called.flag")
	realTar := "/usr/bin/tar"
	if _, err := os.Stat(realTar); err != nil {
		t.Fatalf("locate real tar: %v", err)
	}
	fakeTar := filepath.Join(fixture.binDir, "tar")
	fakeBody := "#!/bin/sh\ntouch '" + flag + "'\nexec " + realTar + " \"$@\"\n"
	if err := os.WriteFile(fakeTar, []byte(fakeBody), 0o755); err != nil {
		t.Fatalf("write fake tar: %v", err)
	}

	env2 := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
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

	if _, err := os.Stat(flag); err == nil {
		t.Fatalf("unchanged resubmission must not start the tar producer")
	}
	if got := cacheDigestEntries(t, fixture); len(got) != 1 || got[0] != firstDigest {
		t.Fatalf("unchanged build must reuse entry %s, got %v", firstDigest, got)
	}

	// 3. Changed selected content creates a distinct entry.
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "modified content\n")
	env3 := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env3.OK {
		t.Fatalf("build 3 failed: %s", pretty(env3))
	}
	var data3 struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(env3.Data, &data3); err != nil {
		t.Fatalf("decode data3: %v", err)
	}
	waitSucceeded(t, fixture, data3.RunID)

	digests3 := cacheDigestEntries(t, fixture)
	if len(digests3) != 2 {
		t.Fatalf("expected 2 distinct entries after modification, got %v", digests3)
	}
}

// assertCompleteEntry verifies a cache entry has meta.json, the source
// tree (final basename = project), and the complete marker under the
// per-project entry root, and that the marker was written last.
func assertCompleteEntry(t *testing.T, fixture *SSHFixture, digest, project string) {
	t.Helper()
	entryRoot := filepath.Join(fixture.coordinatorRoot, "state", "source-cache", "v1", digest, project)
	complete := filepath.Join(entryRoot, "complete")
	meta := filepath.Join(entryRoot, "meta.json")
	tree := filepath.Join(entryRoot, project)
	for _, p := range []string{complete, meta, tree} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("complete entry %s missing %s: %v", digest, p, err)
		}
	}
}

// TestCacheMutationAfterSnapshotCannotPopulateOldKey proves the client
// archives the settled snapshot, not the live tree: a source mutation
// that happens after snapshot materialization cannot populate the old
// key. The fake tar mutates the live source after consuming the file
// list; the coordinator still publishes the pre-mutation bytes under
// the pre-mutation digest.
func TestCacheMutationAfterSnapshotCannotPopulateOldKey(t *testing.T) {
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
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "settled content\n")
	mustGitAddCommit(t, sourceDir, "init")

	// Fake tar: consume the file list, mutate the live source, then
	// run the real tar over the already-materialized snapshot.
	flag := filepath.Join(t.TempDir(), "mutated.flag")
	realTar := "/usr/bin/tar"
	if _, err := os.Stat(realTar); err != nil {
		t.Fatalf("locate real tar: %v", err)
	}
	fakeTar := filepath.Join(fixture.binDir, "tar")
	fakeBody := "#!/bin/sh\nl=$(mktemp)\ncat > \"$l\"\n" +
		"if [ -n \"${VCI_MUTATE_FILE:-}\" ] && [ -f \"$VCI_MUTATE_FILE\" ]; then\n" +
		"  printf 'mutated-after-snapshot\\n' > \"$VCI_MUTATE_FILE\"\n" +
		"  touch '" + flag + "'\n" +
		"fi\nexec " + realTar + " \"$@\" < \"$l\"\n"
	if err := os.WriteFile(fakeTar, []byte(fakeBody), 0o755); err != nil {
		t.Fatalf("write fake tar: %v", err)
	}

	t.Setenv("VCI_MUTATE_FILE", filepath.Join(sourceDir, "README.md"))

	env := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env.OK {
		t.Fatalf("build failed: %s", pretty(env))
	}
	var data struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("decode: %v", err)
	}
	waitSucceeded(t, fixture, data.RunID)

	if _, err := os.Stat(flag); err != nil {
		t.Fatalf("fake tar never mutated the live source; test is vacuous")
	}

	// The published entry must hold the pre-mutation bytes under the
	// pre-mutation digest, never the mutated live tree.
	digests := cacheDigestEntries(t, fixture)
	if len(digests) != 1 {
		t.Fatalf("expected exactly one entry, got %v", digests)
	}
	entryReadme := filepath.Join(fixture.coordinatorRoot, "state", "source-cache", "v1", digests[0], "demo", "demo", "README.md")
	raw, err := os.ReadFile(entryReadme)
	if err != nil {
		t.Fatalf("read entry README.md: %v", err)
	}
	if string(raw) != "settled content\n" {
		t.Fatalf("old key was populated with mutated bytes: %q", raw)
	}
	live, err := os.ReadFile(filepath.Join(sourceDir, "README.md"))
	if err != nil {
		t.Fatalf("read live README.md: %v", err)
	}
	if string(live) != "mutated-after-snapshot\n" {
		t.Fatalf("live source was not mutated by the fake tar: %q", live)
	}
}

// TestIncompleteCachePathIsMissOverSSH plants an incomplete cache path
// (project tree without a complete marker) under the coordinator's
// cache root. A real build must treat it as a miss, stage the source,
// and publish a verified complete entry.
func TestIncompleteCachePathIsMissOverSSH(t *testing.T) {
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
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "content\n")
	mustGitAddCommit(t, sourceDir, "init")

	// Plant a plausible-looking incomplete entry (no complete marker,
	// no meta.json) at the digest path the client will probe.
	parent := filepath.Join(fixture.coordinatorRoot, "state", "source-cache", "v1")
	planted := filepath.Join(parent, "sha256-0000000000000000000000000000000000000000000000000000000000000000", "demo")
	if err := os.MkdirAll(planted, 0o700); err != nil {
		t.Fatalf("plant incomplete entry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(planted, "README.md"), []byte("half-written\n"), 0o600); err != nil {
		t.Fatalf("write planted file: %v", err)
	}

	env := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env.OK {
		t.Fatalf("build must succeed via staging when the cache path is incomplete: %s", pretty(env))
	}
	var data struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("decode: %v", err)
	}
	waitSucceeded(t, fixture, data.RunID)

	digests := cacheDigestEntries(t, fixture)
	if len(digests) != 2 {
		t.Fatalf("expected the planted incomplete path to remain and the verified entry to appear, got %v", digests)
	}
	// Exactly one entry must be complete: the verified one from the
	// real build. The planted path (no complete marker) must never be
	// treated as a hit or a build source.
	var completeCount int
	for _, d := range digests {
		if _, err := os.Stat(filepath.Join(fixture.coordinatorRoot, "state", "source-cache", "v1", d, "demo", "complete")); err == nil {
			completeCount++
			assertCompleteEntry(t, fixture, d, "demo")
		}
	}
	if completeCount != 1 {
		t.Fatalf("expected exactly one complete entry after staging, got %d", completeCount)
	}
}

// TestCorruptCacheEntryRejectedOverSSH corrupts the tree of a complete
// entry and proves the cache-hit validation rejects it: the build
// returns an infrastructure failure and no run ID.
func TestCorruptCacheEntryRejectedOverSSH(t *testing.T) {
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
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "content\n")
	mustGitAddCommit(t, sourceDir, "init")

	// First build creates the verified entry.
	env1 := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
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
	digests := cacheDigestEntries(t, fixture)
	if len(digests) != 1 {
		t.Fatalf("expected one entry before corruption, got %v", digests)
	}
	entryReadme := filepath.Join(fixture.coordinatorRoot, "state", "source-cache", "v1", digests[0], "demo", "demo", "README.md")
	if err := os.WriteFile(entryReadme, []byte("tampered bytes\n"), 0o600); err != nil {
		t.Fatalf("corrupt entry: %v", err)
	}

	// Second build probes a structurally complete entry; the
	// coordinator's hit validation must reject the corrupt tree with
	// an infrastructure failure and no run ID.
	env2 := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if env2.OK {
		t.Fatalf("corrupt cache entry must be rejected, got ok: %s", pretty(env2))
	}
	if env2.Error == nil || env2.Error.Class != "infrastructure" {
		t.Fatalf("corrupt cache entry must classify as infrastructure: %s", pretty(env2))
	}
	if strings.Contains(env2.Error.Message, "run_id") {
		t.Fatalf("corrupt cache entry must return no run ID: %s", pretty(env2))
	}
}

// TestJobSeesNoCacheMetadataOverSSH proves the project source observed
// by the job contains no cache metadata: meta.json, the complete
// marker, and the staging meta live outside the project tree and never
// become build input.
func TestJobSeesNoCacheMetadataOverSSH(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}

	initCoordinatorRoot(t, fixture, "sh", "-c",
		"test ! -e meta.json && test ! -e complete && test ! -e vci-meta && "+
			"test ! -e .source-cache && echo 'SOURCE CLEAN'")
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "content\n")
	mustGitAddCommit(t, sourceDir, "init")

	env := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env.OK {
		t.Fatalf("build failed: %s", pretty(env))
	}
	var data struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("decode: %v", err)
	}
	waitSucceeded(t, fixture, data.RunID)
}

// initTwoProjectCoordinatorRoot writes a coordinator config that owns
// two projects, demo and demo2, both running `true`.
func initTwoProjectCoordinatorRoot(t *testing.T, fixture *SSHFixture) {
	t.Helper()
	cfg := "schema_version = 1\norchestrator = \"self\"\n\n[log_limits]\nstdout_bytes = 4194304\nstderr_bytes = 4194304\n\n[retention]\nmax_bytes = 536870912\n\n[machines.mac-local]\n\n[projects.demo]\nmachines = [\"mac-local\"]\ncommand = [\"true\"]\n\n[projects.demo2]\nmachines = [\"mac-local\"]\ncommand = [\"true\"]\n"
	if err := os.WriteFile(filepath.Join(fixture.coordinatorRoot, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write coordinator config: %v", err)
	}
}

// makeRepo creates a git repository with the given basename and a
// README.md with the given content, returning its path.
func makeRepo(t *testing.T, parent, basename, content string) string {
	t.Helper()
	sourceDir := filepath.Join(parent, basename)
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGitInit(t, sourceDir)
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), content)
	mustGitAddCommit(t, sourceDir, "init")
	return sourceDir
}

// clientSnapshotDigest replicates the client's settled-snapshot digest
// for a repo, returning the digest and the local snapshot root.
func clientSnapshotDigest(t *testing.T, sourceDir string) (string, string) {
	t.Helper()
	input, err := source.SelectBuildInput(context.Background(), sourceDir, process.Native{})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	snapshotParent := t.TempDir()
	snapshot, err := source.MaterializeSnapshot(input, snapshotParent)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	digest, err := source.ComputeSnapshotDigest(snapshot)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return digest, snapshot
}

// TestTwoProjectsSameContentIndependentEntriesOverSSH proves two
// repositories with identical selected content and different project
// basenames build and cache independently over the public client
// command: each publishes its own complete entry under the shared
// digest, each gets a cache hit on resubmission with no tar producer,
// and neither clobbers the other's metadata or marker.
func TestTwoProjectsSameContentIndependentEntriesOverSSH(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}
	initTwoProjectCoordinatorRoot(t, fixture)
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	parent := t.TempDir()
	repoA := makeRepo(t, parent, "demo", "identical content\n")
	repoB := makeRepo(t, parent, "demo2", "identical content\n")

	// Precondition: the two repos share one digest.
	digestA, snapshotA := clientSnapshotDigest(t, repoA)
	defer os.RemoveAll(snapshotA)
	digestB, snapshotB := clientSnapshotDigest(t, repoB)
	defer os.RemoveAll(snapshotB)
	if digestA != digestB {
		t.Fatalf("fixture requires identical selected content digests; got %s vs %s", digestA, digestB)
	}

	// First submission of each project publishes its own entry.
	envA1 := runClientBinary(t, fixture, clientRoot, "build", repoA)
	if !envA1.OK {
		t.Fatalf("build demo failed: %s", pretty(envA1))
	}
	var dataA1 struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(envA1.Data, &dataA1); err != nil {
		t.Fatal(err)
	}
	waitSucceeded(t, fixture, dataA1.RunID)

	envB1 := runClientBinary(t, fixture, clientRoot, "build", repoB)
	if !envB1.OK {
		t.Fatalf("build demo2 failed: %s", pretty(envB1))
	}
	var dataB1 struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(envB1.Data, &dataB1); err != nil {
		t.Fatal(err)
	}
	waitSucceeded(t, fixture, dataB1.RunID)

	// Exactly one digest dir, with two independent complete entries.
	digests := cacheDigestEntries(t, fixture)
	if len(digests) != 1 || digests[0] != digestA {
		t.Fatalf("expected one digest dir %s, got %v", digestA, digests)
	}
	assertCompleteEntry(t, fixture, digestA, "demo")
	assertCompleteEntry(t, fixture, digestA, "demo2")

	// The per-project metas must each name their own project.
	cacheRoot := filepath.Join(fixture.coordinatorRoot, "state", "source-cache")
	for _, project := range []string{"demo", "demo2"} {
		hit, meta, err := sourcecache.IsHit(cacheRoot, digestA, project)
		if err != nil || !hit {
			t.Fatalf("project %s must retain a valid entry: hit=%v err=%v", project, hit, err)
		}
		if meta.Project != project {
			t.Fatalf("project %s meta must name %s, got %q", project, project, meta.Project)
		}
	}

	// Resubmission of each project must be a cache hit with no tar
	// producer, and neither resubmission may invalidate the other.
	flag := filepath.Join(t.TempDir(), "tar-called.flag")
	realTar := "/usr/bin/tar"
	if _, err := os.Stat(realTar); err != nil {
		t.Fatalf("locate real tar: %v", err)
	}
	fakeTar := filepath.Join(fixture.binDir, "tar")
	fakeBody := "#!/bin/sh\ntouch '" + flag + "'\nexec " + realTar + " \"$@\"\n"
	if err := os.WriteFile(fakeTar, []byte(fakeBody), 0o755); err != nil {
		t.Fatalf("write fake tar: %v", err)
	}

	envA2, stderrA2 := runClientBinaryCapture(t, fixture, clientRoot, "build", repoA)
	if !envA2.OK {
		t.Fatalf("demo resubmission failed: %s", pretty(envA2))
	}
	var dataA2 struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(envA2.Data, &dataA2); err != nil {
		t.Fatal(err)
	}
	waitSucceeded(t, fixture, dataA2.RunID)

	envB2, stderrB2 := runClientBinaryCapture(t, fixture, clientRoot, "build", repoB)
	if !envB2.OK {
		t.Fatalf("demo2 resubmission failed: %s", pretty(envB2))
	}
	var dataB2 struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(envB2.Data, &dataB2); err != nil {
		t.Fatal(err)
	}
	waitSucceeded(t, fixture, dataB2.RunID)

	if !strings.Contains(stderrA2, "source cache hit") || !strings.Contains(stderrB2, "source cache hit") {
		t.Fatalf("both resubmissions must be cache hits; A stderr:\n%s\nB stderr:\n%s", stderrA2, stderrB2)
	}
	if _, err := os.Stat(flag); err == nil {
		t.Fatalf("cache-hit resubmissions must not start the tar producer")
	}

	// Neither project invalidated the other: both entries are still
	// complete with their own metadata.
	assertCompleteEntry(t, fixture, digestA, "demo")
	assertCompleteEntry(t, fixture, digestA, "demo2")
	for _, project := range []string{"demo", "demo2"} {
		hit, meta, err := sourcecache.IsHit(cacheRoot, digestA, project)
		if err != nil || !hit || meta.Project != project {
			t.Fatalf("project %s entry must remain valid after the other project's resubmission: hit=%v meta=%v err=%v", project, hit, meta, err)
		}
	}
}
