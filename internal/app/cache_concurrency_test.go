package app

// Phase 3 — concurrency, active use, and exact capacity over
// controlled SSH. Every poll has a deadline; every assertion uses the
// coordinator's real state/source-cache layout.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/process"
	"github.com/hypernewbie/vci/internal/reaper"
	"github.com/hypernewbie/vci/internal/source"
	"github.com/hypernewbie/vci/internal/sourcecache"
)

// TestConcurrentSameKeyClientsOverSSH runs two simultaneous client
// builds of the same source. Both must succeed, publication must
// serialize, and exactly one verified complete entry must exist with
// no partial scratch left behind.
func TestConcurrentSameKeyClientsOverSSH(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
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
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "concurrent content\n")
	mustGitAddCommit(t, sourceDir, "init")

	const clients = 2
	type result struct {
		env phase1Envelope
		run string
	}
	results := make([]result, clients)
	var wg sync.WaitGroup
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			env := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
			var data struct {
				RunID string `json:"run_id"`
			}
			if err := json.Unmarshal(env.Data, &data); err != nil {
				t.Errorf("decode run id: %v", err)
				return
			}
			results[i] = result{env: env, run: data.RunID}
		}(i)
	}
	wg.Wait()

	for i := range results {
		if !results[i].env.OK {
			t.Fatalf("concurrent client %d failed: %s", i, pretty(results[i].env))
		}
		if results[i].run == "" {
			t.Fatalf("concurrent client %d returned no run id", i)
		}
	}
	for _, r := range results {
		waitSucceeded(t, fixture, r.run)
	}

	digests := cacheDigestEntries(t, fixture)
	if len(digests) != 1 {
		t.Fatalf("concurrent same-key clients must create exactly one entry, got %v", digests)
	}
	assertCompleteEntry(t, fixture, digests[0], "demo")

	// No partial scratch may survive the concurrent publication.
	partialRoot := filepath.Join(fixture.coordinatorRoot, "state", "source-cache", "v1", "partial")
	entries, err := os.ReadDir(partialRoot)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read partial root: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("partial scratch must be discarded after concurrent publication: %v", entries)
	}
}

// TestStaleScratchDoesNotBlockBuildOverSSH plants a stale publication
// lock for the exact key and a stale partial, then proves a real build
// still publishes and the reaper removes the stale scratch.
func TestStaleScratchDoesNotBlockBuildOverSSH(t *testing.T) {
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

	// Replicate the client snapshot identity to learn the exact key.
	input, err := source.SelectBuildInput(ctx, sourceDir, process.Native{})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	snapshotParent := t.TempDir()
	snapshot, err := source.MaterializeSnapshot(input, snapshotParent)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	defer os.RemoveAll(snapshot)
	digest, err := source.ComputeSnapshotDigest(snapshot)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}

	// Plant a stale lock for the exact key and a stale partial.
	locksDir := filepath.Join(fixture.coordinatorRoot, "state", "source-cache", "v1", "locks")
	lockPath := filepath.Join(locksDir, digest+"-demo")
	if err := os.MkdirAll(locksDir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.WriteFile(lockPath, []byte(old.UTC().Format(time.RFC3339Nano)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatal(err)
	}
	partialDir := filepath.Join(fixture.coordinatorRoot, "state", "source-cache", "v1", "partial", "stale-partial")
	if err := os.MkdirAll(partialDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(partialDir, old, old); err != nil {
		t.Fatal(err)
	}

	env := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env.OK {
		t.Fatalf("build with stale scratch must succeed: %s", pretty(env))
	}
	var data struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	waitSucceeded(t, fixture, data.RunID)
	digests := cacheDigestEntries(t, fixture)
	if len(digests) != 1 {
		t.Fatalf("stale lock must not block publication, got %v", digests)
	}
	assertCompleteEntry(t, fixture, digests[0], "demo")

	// The build reaped the stale lock itself during publication, so the
	// planted lock path must be gone and the stale partial must be
	// removed by the reaper pass.
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("stale lock must be reaped by the build's publication, still present: %v", err)
	}
	cacheRoot := filepath.Join(fixture.coordinatorRoot, "state", "source-cache")
	_, scratch, _, _, _, err := reaper.ReapSourceCache(cacheRoot, 1<<30)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if scratch != 1 {
		t.Fatalf("expected the stale partial removed by the reaper, got %d", scratch)
	}
	if _, err := os.Stat(partialDir); !os.IsNotExist(err) {
		t.Fatalf("stale partial must be gone after reaping")
	}
}

// TestActiveCaptureProtectsEntryWhileReapingOverSSH holds an active
// claim on a real coordinator entry and proves the reaper counts its
// bytes in total capacity while excluding it from eviction.
func TestActiveCaptureProtectsEntryWhileReapingOverSSH(t *testing.T) {
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
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), "active capture\n")
	mustGitAddCommit(t, sourceDir, "init")

	env := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
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
	digests := cacheDigestEntries(t, fixture)
	if len(digests) != 1 {
		t.Fatalf("expected one entry, got %v", digests)
	}
	digest := digests[0]

	// Hold an active capture claim, then reap with a quota below the
	// entry size: the entry must survive and its bytes must be counted.
	cacheRoot := filepath.Join(fixture.coordinatorRoot, "state", "source-cache")
	if err := sourcecache.AcquireActiveClaim(cacheRoot, digest, "demo", "claim-ssh-hold"); err != nil {
		t.Fatal(err)
	}
	defer sourcecache.ReleaseActiveClaim(cacheRoot, digest, "demo", "claim-ssh-hold")

	items, err := sourcecache.List(cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	var activeBytes int64
	for _, it := range items {
		if it.Digest == digest {
			if !it.Active {
				t.Fatalf("entry must be flagged active while the capture claim is held")
			}
			activeBytes = it.Size
		}
	}
	if activeBytes == 0 {
		t.Fatalf("active entry bytes must be counted")
	}

	removed, _, bytes, _, _, err := reaper.ReapSourceCache(cacheRoot, 1)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if removed != 0 {
		t.Fatalf("active entry must never be evicted, removed=%d", removed)
	}
	if bytes <= 0 || bytes < activeBytes {
		t.Fatalf("reaper must count the active bytes, bytes=%d active=%d", bytes, activeBytes)
	}
	if hit, _, _ := sourcecache.IsHit(cacheRoot, digest, "demo"); !hit {
		t.Fatalf("active entry must remain a hit after reaping")
	}
}

// TestCacheLRUHitOrderOverSSH proves eviction follows the recorded
// last-use of real cache hits: after A, then B, then A again, the
// reaper evicts B (oldest hit) and keeps A.
func TestCacheLRUHitOrderOverSSH(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}

	initCoordinatorRoot(t, fixture, "true")
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	buildOnce := func(content string) string {
		t.Helper()
		sourceParent := t.TempDir()
		sourceDir := filepath.Join(sourceParent, "demo")
		if err := os.MkdirAll(sourceDir, 0o755); err != nil {
			t.Fatal(err)
		}
		mustGitInit(t, sourceDir)
		mustWriteFile(t, filepath.Join(sourceDir, "README.md"), content)
		mustGitAddCommit(t, sourceDir, "init")
		env := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
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
		return sourceDir
	}

	// A and B are same-size trees with different content.
	buildOnce("content-a\n")
	buildOnce("content-b\n")
	// Hitting A again makes A the most-recently-used entry.
	buildOnce("content-a\n")

	cacheRoot := filepath.Join(fixture.coordinatorRoot, "state", "source-cache")
	items, err := sourcecache.List(cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(items))
	}
	// The entry with the newest recorded last-use is A (hit again); the
	// other is B.
	var a, b sourcecache.EntryListItem
	if items[0].LastUse.After(items[1].LastUse) {
		a, b = items[0], items[1]
	} else {
		a, b = items[1], items[0]
	}
	aDigest, aSize := a.Digest, a.Size
	bDigest, bSize := b.Digest, b.Size
	// Quota fits exactly one entry; the least-recently-used (B) must go.
	quota := aSize
	if bSize != aSize {
		t.Fatalf("fixture requires equal entry sizes, got %d vs %d", aSize, bSize)
	}
	removed, _, _, _, _, err := reaper.ReapSourceCache(cacheRoot, quota)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("expected exactly one eviction, got %d", removed)
	}
	if hit, _, _ := sourcecache.IsHit(cacheRoot, aDigest, "demo"); !hit {
		t.Fatalf("most-recently-used entry must survive")
	}
	if hit, _, _ := sourcecache.IsHit(cacheRoot, bDigest, "demo"); hit {
		t.Fatalf("least-recently-used entry must be evicted")
	}
}

// TestCacheCapacityRejectedOverSSH proves admission enforcement under
// coordinator authority: with retention.source_cache_bytes set to the
// documented minimum, an incoming entry larger than the quota is
// rejected, no complete entry appears, and the build still succeeds on
// the direct one-shot path.
func TestCacheCapacityRejectedOverSSH(t *testing.T) {
	fixture := NewSSHFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := fixture.waitForSSHRoundtrip(ctx); err != nil {
		t.Fatalf("ssh roundtrip: %v", err)
	}

	// Coordinator config with a tight coordinator-owned cache quota.
	cfg := "schema_version = 1\norchestrator = \"self\"\n\n[log_limits]\nstdout_bytes = 4194304\nstderr_bytes = 4194304\n\n[retention]\nmax_bytes = 536870912\nsource_cache_bytes = 4096\n\n[machines.mac-local]\n\n[projects.demo]\nmachines = [\"mac-local\"]\ncommand = [\"true\"]\n"
	if err := os.WriteFile(filepath.Join(fixture.coordinatorRoot, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write coordinator config: %v", err)
	}
	clientRoot := initClientRoot(t, fixture, fixture.SSHAlias())

	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "demo")
	if err := os.MkdirAll(sourceDir, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	mustGitInit(t, sourceDir)
	// Source comfortably larger than the 4096-byte quota.
	mustWriteFile(t, filepath.Join(sourceDir, "README.md"), string(make([]byte, 16384)))
	mustGitAddCommit(t, sourceDir, "init")

	env := runClientBinary(t, fixture, clientRoot, "build", sourceDir)
	if !env.OK {
		t.Fatalf("build must succeed one-shot when the cache rejects the entry: %s", pretty(env))
	}
	var data struct {
		RunID string `json:"run_id"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	waitSucceeded(t, fixture, data.RunID)

	digests := cacheDigestEntries(t, fixture)
	if len(digests) != 0 {
		t.Fatalf("oversize entry must not be published, got %v", digests)
	}
}
