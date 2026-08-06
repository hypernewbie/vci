package sourcecache

// Phase 3 — concurrency, active use, and exact capacity.

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/source"
)

// writeTree creates a source tree with the given name/content pairs.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestPublishTreeConcurrentSameKey proves that concurrent publishers of
// the same key serialize: exactly one publishes, every other competitor
// observes the winner and discards its own partial, and no partial
// scratch survives.
func TestPublishTreeConcurrentSameKey(t *testing.T) {
	root := t.TempDir()
	src := writeTree(t, map[string]string{"README.md": "shared content\n"})
	digest, err := digestOfTree(t, src)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 6
	published := make([]bool, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each worker publishes from its own copy so a partial
			// tree is never shared.
			own := writeTree(t, map[string]string{"README.md": "shared content\n"})
			published[i], errs[i] = PublishTree(root, digest, "demo", own, 1<<20)
		}(i)
	}
	wg.Wait()
	var publishCount int
	for i := range published {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if published[i] {
			publishCount++
		}
	}
	if publishCount != 1 {
		t.Fatalf("exactly one same-key publisher must win, got %d", publishCount)
	}
	hit, _, err := IsHit(root, digest, "demo")
	if err != nil || !hit {
		t.Fatalf("entry must be complete: %v", err)
	}
	// No partial scratch may survive.
	partialRoot := filepath.Join(root, FormatVersion, "partial")
	entries, err := os.ReadDir(partialRoot)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("partial scratch must be discarded after a winner is observed: %v", entries)
	}
}

// TestPublishTreeRejectsOversizeEntry proves admission rejects an
// incoming entry larger than the configured quota and leaves no entry
// behind.
func TestPublishTreeRejectsOversizeEntry(t *testing.T) {
	root := t.TempDir()
	quota := int64(1024)
	big := make([]byte, quota+1)
	for i := range big {
		big[i] = 'x'
	}
	src := writeTree(t, map[string]string{"big.bin": string(big)})
	digest, err := digestOfTree(t, src)
	if err != nil {
		t.Fatal(err)
	}
	_, err = PublishTree(root, digest, "demo", src, quota)
	if !errors.Is(err, ErrAdmissionRejected) {
		t.Fatalf("oversize entry must be rejected with ErrAdmissionRejected, got %v", err)
	}
	if hit, _, _ := IsHit(root, digest, "demo"); hit {
		t.Fatalf("rejected entry must not be a hit")
	}
}

// TestPublishTreeEvictsInactiveLRUBeforeAdmission proves publication
// evicts least-recently-used inactive entries first and rejects when no
// inactive capacity remains.
func TestPublishTreeEvictsInactiveLRUBeforeAdmission(t *testing.T) {
	root := t.TempDir()
	quota := int64(4096)
	// Entry A: ~1KB, published first (oldest last_use).
	contentA := make([]byte, 1024)
	for i := range contentA {
		contentA[i] = 'a'
	}
	srcA := writeTree(t, map[string]string{"a.bin": string(contentA)})
	digestA, err := digestOfTree(t, srcA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PublishTree(root, digestA, "demo", srcA, quota); err != nil {
		t.Fatalf("publish A: %v", err)
	}
	// Entry B: ~1KB.
	contentB := make([]byte, 1024)
	for i := range contentB {
		contentB[i] = 'b'
	}
	srcB := writeTree(t, map[string]string{"b.bin": string(contentB)})
	digestB, err := digestOfTree(t, srcB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PublishTree(root, digestB, "demo", srcB, quota); err != nil {
		t.Fatalf("publish B: %v", err)
	}
	// Entry C: ~2.5KB. Total would exceed quota, so the oldest
	// inactive entry (A) must be evicted before C is admitted.
	contentC := make([]byte, 2560)
	for i := range contentC {
		contentC[i] = 'c'
	}
	srcC := writeTree(t, map[string]string{"c.bin": string(contentC)})
	digestC, err := digestOfTree(t, srcC)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PublishTree(root, digestC, "demo", srcC, quota); err != nil {
		t.Fatalf("publish C with eviction: %v", err)
	}
	for digest, want := range map[string]bool{digestA: false, digestB: true, digestC: true} {
		hit, _, err := IsHit(root, digest, "demo")
		if err != nil {
			t.Fatal(err)
		}
		if hit != want {
			t.Fatalf("entry %s presence: want %v got %v", digest, want, hit)
		}
	}
}

// TestPublishTreeRejectsWhenNoInactiveCapacity proves that when all
// remaining capacity is held by active entries, admission rejects
// instead of over-publishing.
func TestPublishTreeRejectsWhenNoInactiveCapacity(t *testing.T) {
	root := t.TempDir()
	quota := int64(2048)
	contentA := make([]byte, 1024)
	for i := range contentA {
		contentA[i] = 'a'
	}
	srcA := writeTree(t, map[string]string{"a.bin": string(contentA)})
	digestA, err := digestOfTree(t, srcA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PublishTree(root, digestA, "demo", srcA, quota); err != nil {
		t.Fatalf("publish A: %v", err)
	}
	if err := AcquireActiveClaim(root, digestA, "demo", "claim-hold"); err != nil {
		t.Fatal(err)
	}
	defer ReleaseActiveClaim(root, digestA, "demo", "claim-hold")
	// The only entry is active; a second entry of ~1.5KB cannot be
	// admitted because evicting the active entry is forbidden.
	contentB := make([]byte, 1536)
	for i := range contentB {
		contentB[i] = 'b'
	}
	srcB := writeTree(t, map[string]string{"b.bin": string(contentB)})
	digestB, err := digestOfTree(t, srcB)
	if err != nil {
		t.Fatal(err)
	}
	_, err = PublishTree(root, digestB, "demo", srcB, quota)
	if !errors.Is(err, ErrAdmissionRejected) {
		t.Fatalf("must reject when only active capacity exists, got %v", err)
	}
	if hit, _, _ := IsHit(root, digestB, "demo"); hit {
		t.Fatalf("rejected entry must not be a hit")
	}
}

// TestEnforceQuotaCountsActiveButNeverEvictsActive proves active
// entries are counted in total capacity while being excluded from
// eviction.
func TestEnforceQuotaCountsActiveButNeverEvictsActive(t *testing.T) {
	root := t.TempDir()
	quota := int64(1024)
	contentA := make([]byte, 1024)
	for i := range contentA {
		contentA[i] = 'a'
	}
	srcA := writeTree(t, map[string]string{"a.bin": string(contentA)})
	digestA, err := digestOfTree(t, srcA)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PublishTree(root, digestA, "demo", srcA, 1<<20); err != nil {
		t.Fatalf("publish A: %v", err)
	}
	// Hold an active claim on A; then publish B and enforce quota so
	// tight that B alone would need eviction.
	if err := AcquireActiveClaim(root, digestA, "demo", "claim-hold"); err != nil {
		t.Fatal(err)
	}
	defer ReleaseActiveClaim(root, digestA, "demo", "claim-hold")
	contentB := make([]byte, 1024)
	for i := range contentB {
		contentB[i] = 'b'
	}
	srcB := writeTree(t, map[string]string{"b.bin": string(contentB)})
	digestB, err := digestOfTree(t, srcB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PublishTree(root, digestB, "demo", srcB, 1<<20); err != nil {
		t.Fatalf("publish B: %v", err)
	}

	// Force A's last_use to be oldest so it is the first eviction
	// candidate.
	if err := UpdateLastUse(root, digestA, "demo", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	items, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	var totalActive int64
	for _, it := range items {
		if it.Digest == digestA {
			if !it.Active {
				t.Fatalf("entry A must be flagged active")
			}
			totalActive += it.Size
		}
	}
	if totalActive == 0 {
		t.Fatalf("active bytes must be counted in total capacity")
	}

	removed, total, _, err := EnforceQuota(root, quota)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("must evict exactly the inactive entry, removed=%d", removed)
	}
	if hit, _, _ := IsHit(root, digestA, "demo"); !hit {
		t.Fatalf("active entry must never be evicted")
	}
	if hit, _, _ := IsHit(root, digestB, "demo"); hit {
		t.Fatalf("inactive entry must be evicted")
	}
	if total <= 0 {
		t.Fatalf("retained bytes must still count the active entry, total=%d", total)
	}
}

// TestReapStaleScratchRemovesOnlyStale removes stale partials and stale
// locks while preserving fresh ones.
func TestReapStaleScratchRemovesOnlyStale(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	partialRoot := filepath.Join(root, FormatVersion, "partial")
	locksRoot := filepath.Join(root, FormatVersion, "locks")
	if err := os.MkdirAll(partialRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(locksRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	stalePartial := filepath.Join(partialRoot, "stale-partial")
	freshPartial := filepath.Join(partialRoot, "fresh-partial")
	staleLock := filepath.Join(locksRoot, "stale-lock")
	freshLock := filepath.Join(locksRoot, "fresh-lock")
	for _, p := range []string{stalePartial, freshPartial} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{staleLock, freshLock} {
		if err := os.WriteFile(p, []byte("lock"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := now.Add(-2 * time.Hour)
	for _, p := range []string{stalePartial, staleLock} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := ReapStaleScratch(root, now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("expected 2 stale scratch items removed, got %d", removed)
	}
	for _, p := range []string{stalePartial, staleLock} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("stale %s must be gone", p)
		}
	}
	for _, p := range []string{freshPartial, freshLock} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("fresh %s must remain", p)
		}
	}
}

// TestPublishTreeReapsStaleLock proves a stale publication lock does
// not block a new publisher.
func TestPublishTreeReapsStaleLock(t *testing.T) {
	root := t.TempDir()
	src := writeTree(t, map[string]string{"README.md": "content\n"})
	digest, err := digestOfTree(t, src)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := LockPath(root, digest, "demo")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * publicationLockAge)
	if err := os.WriteFile(lockPath, []byte(old.UTC().Format(time.RFC3339Nano)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatal(err)
	}
	published, err := PublishTree(root, digest, "demo", src, 1<<20)
	if err != nil {
		t.Fatalf("publish with stale lock: %v", err)
	}
	if !published {
		t.Fatalf("stale lock must not block publication")
	}
}

// digestOfTree computes the canonical snapshot digest of a tree.
func digestOfTree(t *testing.T, root string) (string, error) {
	t.Helper()
	canonical, err := source.CanonicalizeSnapshot(root)
	if err != nil {
		return "", err
	}
	return source.CanonicalDigest(canonical), nil
}
