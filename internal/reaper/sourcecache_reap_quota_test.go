package reaper

// Phase 3 — source-cache capacity and scratch reaping.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/config"
	"github.com/hypernewbie/vci/internal/model"
	"github.com/hypernewbie/vci/internal/source"
	"github.com/hypernewbie/vci/internal/sourcecache"
)

// publishOne publishes a small tree under the cache root and returns
// its digest.
func publishOne(t *testing.T, root string, name, content string) string {
	t.Helper()
	src := filepath.Join(t.TempDir())
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := digestOf(t, src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sourcecache.PublishTree(root, digest, "demo", src, 1<<20); err != nil {
		t.Fatalf("publish: %v", err)
	}
	return digest
}

// digestOf computes the canonical snapshot digest of a tree.
func digestOf(t *testing.T, root string) (string, error) {
	t.Helper()
	canonical, err := source.CanonicalizeSnapshot(root)
	if err != nil {
		return "", err
	}
	return source.CanonicalDigest(canonical), nil
}

// TestReapSourceCacheCountsActiveButNeverEvicts proves the reaper
// counts active entries in total capacity while excluding them from
// eviction, so its byte report never hides active bytes.
func TestReapSourceCacheCountsActiveButNeverEvicts(t *testing.T) {
	root := t.TempDir()
	contentA := make([]byte, 2048)
	for i := range contentA {
		contentA[i] = 'a'
	}
	digestA := publishOne(t, root, "a.bin", string(contentA))
	contentB := make([]byte, 2048)
	for i := range contentB {
		contentB[i] = 'b'
	}
	digestB := publishOne(t, root, "b.bin", string(contentB))

	// A is the oldest; hold it active. Quota fits exactly one entry.
	if err := sourcecache.AcquireActiveClaim(root, digestA, "demo", "claim-hold"); err != nil {
		t.Fatal(err)
	}
	defer sourcecache.ReleaseActiveClaim(root, digestA, "demo", "claim-hold")
	if err := sourcecache.UpdateLastUse(root, digestA, "demo", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	removed, scratch, bytes, limit, _, err := ReapSourceCache(root, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("must evict exactly the inactive entry, removed=%d", removed)
	}
	if scratch != 0 {
		t.Fatalf("no scratch expected, got %d", scratch)
	}
	if limit != 2048 {
		t.Fatalf("limit: %d", limit)
	}
	if bytes <= 0 {
		t.Fatalf("retained bytes must count the active entry, got %d", bytes)
	}
	if hit, _, _ := sourcecache.IsHit(root, digestA, "demo"); !hit {
		t.Fatalf("active entry must survive")
	}
	if hit, _, _ := sourcecache.IsHit(root, digestB, "demo"); hit {
		t.Fatalf("inactive entry must be evicted")
	}
}

// TestReapSourceCacheRemovesStaleScratch proves the reaper removes
// stale Vci-owned cache partials and locks older than the documented
// age while leaving fresh scratch alone.
func TestReapSourceCacheRemovesStaleScratch(t *testing.T) {
	root := t.TempDir()
	stalePartial := filepath.Join(root, "v1", "partial", "stale")
	freshPartial := filepath.Join(root, "v1", "partial", "fresh")
	staleLock := filepath.Join(root, "v1", "locks", "stale-lock")
	freshLock := filepath.Join(root, "v1", "locks", "fresh-lock")
	for _, p := range []string{stalePartial, freshPartial} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(staleLock), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{staleLock, freshLock} {
		if err := os.WriteFile(p, []byte("lock"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * time.Hour)
	for _, p := range []string{stalePartial, staleLock} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	_, scratch, _, _, _, err := ReapSourceCache(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if scratch != 2 {
		t.Fatalf("expected 2 stale scratch items removed, got %d", scratch)
	}
	for _, p := range []string{stalePartial, staleLock} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("stale scratch %s must be gone", p)
		}
	}
	for _, p := range []string{freshPartial, freshLock} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("fresh scratch %s must remain", p)
		}
	}
}

// TestReapSourceCacheRejectsOversizeSingle proves an oversize retained
// single is counted as rejected and its bytes stay counted.
func TestReapSourceCacheRejectsOversizeSingle(t *testing.T) {
	root := t.TempDir()
	quota := int64(2048)
	content := make([]byte, quota+1)
	for i := range content {
		content[i] = 'x'
	}
	digest := publishOne(t, root, "big.bin", string(content))
	removed, _, bytes, limit, rejected, err := ReapSourceCache(root, quota)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("oversize single must not be evicted, removed=%d", removed)
	}
	if rejected != 1 {
		t.Fatalf("oversize single must be counted as rejected, got %d", rejected)
	}
	if bytes <= quota {
		t.Fatalf("oversize bytes must remain counted, bytes=%d limit=%d", bytes, limit)
	}
	if hit, _, _ := sourcecache.IsHit(root, digest, "demo"); !hit {
		t.Fatalf("oversize entry must not be silently removed")
	}
}

// TestSourceCacheQuotaFallsBackToDefault proves the reaper reports the
// documented default when the coordinator-owned setting is omitted.
// The default lives in DefaultSourceCacheBytes so a reaper-call
// without an explicit config returns that documented value.
func TestSourceCacheQuotaFallsBackToDefault(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := sourceCacheQuota(model.Layout{Root: root}); got != DefaultSourceCacheBytes {
		t.Fatalf("default quota: want %d got %d", DefaultSourceCacheBytes, got)
	}
}

// TestSourceCacheQuotaHonorsConfiguredValue proves the reaper honors
// the coordinator-owned SourceCacheBytes setting. Client roots that
// attempt to set it are rejected at Validate.
func TestSourceCacheQuotaHonorsConfiguredValue(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cfg := "schema_version = 1\norchestrator = \"self\"\n\n[log_limits]\nstdout_bytes = 4194304\nstderr_bytes = 4194304\n\n[retention]\nmax_bytes = 536870912\nsource_cache_bytes = 8388608\n\n[machines.mac-local]\n\n[projects.demo]\nmachines = [\"mac-local\"]\ncommand = [\"true\"]\n"
	if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := sourceCacheQuota(model.Layout{Root: root}); got != 8388608 {
		t.Fatalf("configured quota: want 8388608 got %d", got)
	}
}

// TestClientCannotSetSourceCacheQuota ensures the role-aware
// validator rejects a client root's attempt to set the source-cache
// quota. A misconfigured client root cannot dictate cache policy.
func TestClientCannotSetSourceCacheQuota(t *testing.T) {
	cfg := config.Defaults()
	cfg.Orchestrator = "remote-host"
	cfg.Retention.MaxBytes = config.DefaultRetention.MaxBytes
	cfg.Retention.SourceCacheBytes = 1024
	if err := config.Validate(cfg); err == nil {
		t.Fatalf("client root setting source-cache quota must be rejected")
	}
}

// TestCoordinatorSourceCacheQuotaAcceptsReasonableValue ensures the
// coordinator validator accepts a reasonable configured quota and
// rejects values below the documented minimum.
func TestCoordinatorSourceCacheQuotaAcceptsReasonableValue(t *testing.T) {
	cfg := config.Defaults()
	cfg.Orchestrator = config.OrchestratorSelf
	cfg.Machines = map[string]config.Machine{"mac-local": {}}
	cfg.Retention.MaxBytes = 1 << 30
	cfg.Retention.SourceCacheBytes = 1 << 20
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("coordinator reasonable quota: %v", err)
	}
	cfg.Retention.SourceCacheBytes = 4
	if err := config.Validate(cfg); err == nil {
		t.Fatalf("coordinator undersized quota must be rejected")
	}
}

// TestReapTransferDirsRemovesStale asserts that staging directories
// matching the vci-source. prefix and older than the threshold are
// removed.
func TestReapTransferDirsRemovesStale(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "vci-source.stale123")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * transferStaleAge)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatal(err)
	}
	removed, err := reapTransferDirs(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("expected 1 removed, got %d", removed)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale transfer dir was not removed: %v", err)
	}
}

// TestReapTransferDirsLeavesFresh asserts that staging directories
// inside the age threshold are not removed.
func TestReapTransferDirsLeavesFresh(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "vci-source.fresh456")
	if err := os.Mkdir(fresh, 0o700); err != nil {
		t.Fatal(err)
	}
	removed, err := reapTransferDirs(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("expected 0 removed, got %d", removed)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh transfer dir was removed: %v", err)
	}
}

// TestReapTransferDirsIgnoresUnrelated asserts that directories without
// the vci-source. prefix are never touched, even when stale.
func TestReapTransferDirsIgnoresUnrelated(t *testing.T) {
	dir := t.TempDir()
	unrelated := filepath.Join(dir, "unrelated-thing")
	if err := os.Mkdir(unrelated, 0o700); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * transferStaleAge)
	if err := os.Chtimes(unrelated, past, past); err != nil {
		t.Fatal(err)
	}
	removed, err := reapTransferDirs(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("unrelated content was removed: %d", removed)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated dir disappeared: %v", err)
	}
}

// TestReapTransferDirsHonoursPrefix asserts the prefix match is exact
// and does not over-match adjacent names.
func TestReapTransferDirsHonoursPrefix(t *testing.T) {
	dir := t.TempDir()
	similar := filepath.Join(dir, "not-vci-source.x")
	if err := os.Mkdir(similar, 0o700); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * transferStaleAge)
	if err := os.Chtimes(similar, past, past); err != nil {
		t.Fatal(err)
	}
	removed, err := reapTransferDirs(dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("near-match was removed: %d", removed)
	}
}

// TestReapTransferDirsPreservesSymlinkSentinelTarget asserts that a staged
// symlink to an external sentinel file leaves the sentinel file, its mode,
// and its content completely intact after reaping.
func TestReapTransferDirsPreservesSymlinkSentinelTarget(t *testing.T) {
	tempDir := t.TempDir()
	sentinelDir := t.TempDir()
	sentinelFile := filepath.Join(sentinelDir, "sentinel.txt")
	if err := os.WriteFile(sentinelFile, []byte("sentinel-data"), 0o644); err != nil {
		t.Fatal(err)
	}

	stale := filepath.Join(tempDir, "vci-source-demo.stale123")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(stale, "sentinel-link")
	if err := os.Symlink(sentinelFile, symlinkPath); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * transferStaleAge)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatal(err)
	}

	removed, err := reapTransferDirs(tempDir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("expected 1 removed, got %d", removed)
	}

	data, err := os.ReadFile(sentinelFile)
	if err != nil {
		t.Fatalf("sentinel file was deleted or unreadable: %v", err)
	}
	if string(data) != "sentinel-data" {
		t.Fatalf("sentinel content altered: %s", data)
	}
	info, err := os.Stat(sentinelFile)
	if err != nil {
		t.Fatalf("stat sentinel file: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("sentinel mode altered: %o", info.Mode().Perm())
	}
}

func TestEnforceEvictsOldestBlob(t *testing.T) {
	l := model.Layout{Root: filepath.Join(t.TempDir(), ".vci")}
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.BlobsDir(), "old"), []byte("old"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.BlobsDir(), "new"), []byte("new"), 0o400); err != nil {
		t.Fatal(err)
	}
	r, err := Enforce(l, config.Retention{MaxBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	if r.RemovedEntries != 1 {
		t.Fatalf("report: %+v", r)
	}
}
