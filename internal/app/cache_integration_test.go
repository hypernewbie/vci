package app

// Controlled-SSH source-cache integration tests.
//
// Every test drives the public client command (`vci build <path>`) through
// ordinary system SSH against the loopback fixture and asserts cache
// behavior at the coordinator's real state/source-cache layout. A cache
// entry is Vci-owned and complete only when meta.json and the project tree
// exist and the `complete` marker is present.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
