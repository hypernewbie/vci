package reaper

// Phase 3 — source-cache capacity and scratch reaping.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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
