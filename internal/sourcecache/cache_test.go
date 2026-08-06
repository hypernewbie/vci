package sourcecache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hypernewbie/vci/internal/source"
)

// treeDigest computes the snapshot digest of a tree.
func treeDigest(t *testing.T, root string) string {
	t.Helper()
	digest, err := source.ComputeSnapshotDigest(root)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	return digest
}

// TestValidDigestShape pins the strict sha256-<64-hex> rule. Anything
// that is not exactly that shape is rejected so the cache key is
// constrained to the production algorithm.
func TestValidDigestShape(t *testing.T) {
	const good = "sha256-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if !ValidDigest(good) {
		t.Fatalf("canonical digest must validate: %q", good)
	}
	cases := []struct {
		name   string
		digest string
		want   bool
	}{
		{"missing-prefix", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", false},
		{"wrong-prefix", "sha512-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", false},
		{"too-short", "sha256-0123456789abcdef", false},
		{"too-long", good + "a", false},
		{"uppercase-hex", "sha256-0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef", false},
		{"non-hex-character", "sha256-0123456789abcdeZ0123456789abcdef0123456789abcdef0123456789abcdef", false},
		{"empty", "", false},
		{"only-prefix", "sha256-", false},
		{"with-path-traversal", "sha256-" + "../" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidDigest(tc.digest); got != tc.want {
				t.Fatalf("ValidDigest(%q)=%v, want %v", tc.digest, got, tc.want)
			}
		})
	}
}

// TestPublishTreeRoundTrip exercises the production publish path:
// create a verified tree under src, call PublishTree, then assert the
// entry is a hit. Before publication the same key is not a hit.
func TestPublishTreeRoundTrip(t *testing.T) {
	root := t.TempDir()
	src := writeTree(t, map[string]string{"hi": "hi\n"})
	digest := treeDigest(t, src)
	if !ValidDigest(digest) {
		t.Fatalf("test digest shape: %q", digest)
	}
	project := "demo"
	if hit, _, err := IsHit(root, digest, project); err != nil || hit {
		t.Fatalf("hit must be false before publication: hit=%v err=%v", hit, err)
	}
	published, err := PublishTree(root, digest, project, src, 1<<20)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !published {
		t.Fatalf("publish must report the entry as new")
	}
	if _, err := os.Stat(EntryMetaPath(root, digest, project)); err != nil {
		t.Fatalf("meta.json missing: %v", err)
	}
	if info, err := os.Stat(EntryCompletePath(root, digest, project)); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("complete marker missing or wrong: %v", info)
	}
	if hit, _, err := IsHit(root, digest, project); err != nil || !hit {
		t.Fatalf("hit must be true after publication: hit=%v err=%v", hit, err)
	}
}

// TestPublishRejectsPreCreatedIncompleteDirectory proves a directory
// already present under the digest parent, without a completion marker
// or meta.json, is not a cache hit. The "test -d" mistake is rejected.
func TestPublishRejectsPreCreatedIncompleteDirectory(t *testing.T) {
	root := t.TempDir()
	src := writeTree(t, map[string]string{"demo": "demo\n"})
	digest := treeDigest(t, src)
	entryPath := EntryPath(root, digest, "demo")
	if err := os.MkdirAll(entryPath, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(entryPath, "demo"), []byte("demo\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	hit, _, err := IsHit(root, digest, "demo")
	if err != nil {
		t.Fatalf("hit: %v", err)
	}
	if hit {
		t.Fatalf("bare directory must not be a hit")
	}
}

// TestPublicationLockBlocksConcurrentSameKey verifies two publishers
// of the same key serialize through the publication lock. The second
// publisher backs off rather than overwriting the first.
func TestPublicationLockBlocksConcurrentSameKey(t *testing.T) {
	root := t.TempDir()
	src := writeTree(t, map[string]string{"demo": "demo\n"})
	digest := treeDigest(t, src)
	project := "demo"
	now := time.Now()
	unlock1, err := PublicationLock(root, digest, project, now, time.Minute)
	if err != nil {
		t.Fatalf("lock1: %v", err)
	}
	defer unlock1()
	if _, err := PublicationLock(root, digest, project, now, time.Minute); err == nil {
		t.Fatalf("second publisher must fail while first holds lock")
	}
}

// TestPublicationLockReapsStale verifies a stale publication lock is
// reclaimed.
func TestPublicationLockReapsStale(t *testing.T) {
	root := t.TempDir()
	src := writeTree(t, map[string]string{"demo": "demo\n"})
	digest := treeDigest(t, src)
	project := "demo"
	if err := os.MkdirAll(filepath.Dir(LockPath(root, digest, project)), 0o700); err != nil {
		t.Fatalf("mkdir locks: %v", err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.WriteFile(LockPath(root, digest, project), []byte(old.UTC().Format(time.RFC3339Nano)), 0o600); err != nil {
		t.Fatalf("write stale lock: %v", err)
	}
	if err := os.Chtimes(LockPath(root, digest, project), old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	unlock, err := PublicationLock(root, digest, project, time.Now(), time.Minute)
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	unlock()
}

// TestActiveClaimProtectsEntry verifies the reaper-side check skips
// entries holding a claim. The new entry's LastUse field is the LRU
// signal; the active claim means the reaper must not evict.
func TestActiveClaimProtectsEntry(t *testing.T) {
	root := t.TempDir()
	src := writeTree(t, map[string]string{"demo": "demo\n"})
	digest := treeDigest(t, src)
	if _, err := PublishTree(root, digest, "demo", src, 1<<20); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := AcquireActiveClaim(root, digest, "demo", "claim-1"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	active, err := ActiveClaimsExist(root, digest, "demo")
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	if !active {
		t.Fatalf("active claim must exist")
	}
	if err := PurgeEntry(root, digest, "demo"); err == nil {
		t.Fatalf("purge must fail while active claim is present")
	}
	ReleaseActiveClaim(root, digest, "demo", "claim-1")
	active, _ = ActiveClaimsExist(root, digest, "demo")
	if active {
		t.Fatalf("active claim must be gone after release")
	}
	if err := PurgeEntry(root, digest, "demo"); err != nil {
		t.Fatalf("purge: %v", err)
	}
}

// TestLastUseOrderingRespectsUpdateLastUse verifies eviction order is
// driven by the recorded last_use metadata, not file mtimes inside
// the project tree.
func TestLastUseOrderingRespectsUpdateLastUse(t *testing.T) {
	root := t.TempDir()
	type fixture struct {
		digest  string
		content string
		lastUse time.Time
	}
	for _, fx := range []fixture{
		{content: "a\n", lastUse: time.Now().Add(-2 * time.Hour)},
		{content: "b\n", lastUse: time.Now().Add(-1 * time.Hour)},
	} {
		src := writeTree(t, map[string]string{"f": fx.content})
		fx.digest = treeDigest(t, src)
		if _, err := PublishTree(root, fx.digest, "demo", src, 1<<20); err != nil {
			t.Fatalf("publish: %v", err)
		}
		// Force the entry's last_use to the desired time.
		raw, err := os.ReadFile(EntryMetaPath(root, fx.digest, "demo"))
		if err != nil {
			t.Fatalf("meta read: %v", err)
		}
		var meta EntryMeta
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatalf("meta decode: %v", err)
		}
		meta.LastUse = fx.lastUse.UTC().Format(time.RFC3339Nano)
		out, _ := json.Marshal(&meta)
		if err := os.WriteFile(EntryMetaPath(root, fx.digest, "demo"), out, 0o600); err != nil {
			t.Fatalf("meta write: %v", err)
		}
		// Touch the project file mtime so it is "newer" than the
		// recorded last_use. The List view must follow the meta
		// last_use, not the file mtime.
		treePath := EntryTreePath(root, fx.digest, "demo")
		_ = filepath.Walk(treePath, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			_ = os.Chtimes(p, time.Now(), time.Now())
			return nil
		})
	}
	items, err := List(root)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(items))
	}
	if !items[0].LastUse.Before(items[1].LastUse) {
		t.Fatalf("list must order by recorded last_use; got %v then %v", items[0].LastUse, items[1].LastUse)
	}
	// UpdateLastUse on item[0] so it becomes the most-recently-used.
	if err := UpdateLastUse(root, items[0].Digest, "demo", time.Now()); err != nil {
		t.Fatalf("update: %v", err)
	}
	items, _ = List(root)
	if !items[1].LastUse.Before(items[0].LastUse) {
		t.Fatalf("update must reorder")
	}
}

// TestSingleEntryLargerThanQuotaRejected pins the configured capacity
// rule: a single entry larger than the configured quota must be
// rejected, not silently retained because it was "first". Eviction
// cannot admit an oversize entry by removing others.
func TestSingleEntryLargerThanQuotaRejected(t *testing.T) {
	root := t.TempDir()
	quota := int64(8 * 1024)
	src := writeTree(t, map[string]string{"data": strings.Repeat("Z", int(quota+1024))})
	digest := treeDigest(t, src)
	_, err := PublishTree(root, digest, "demo", src, quota)
	if err == nil {
		t.Fatalf("oversize entry must be rejected by PublishTree")
	}
	items, listErr := List(root)
	if listErr != nil {
		t.Fatalf("list: %v", listErr)
	}
	if len(items) != 0 {
		t.Fatalf("oversize entry must not appear in list, got %+v", items)
	}
}

// TestReusedClaimIDStillSafe asserts concurrent acquire/release does
// not double-acquire: repeated AcquireActiveClaim is idempotent.
func TestReusedClaimIDStillSafe(t *testing.T) {
	root := t.TempDir()
	src := writeTree(t, map[string]string{"demo": "demo\n"})
	digest := treeDigest(t, src)
	if _, err := PublishTree(root, digest, "demo", src, 1<<20); err != nil {
		t.Fatalf("publish: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := AcquireActiveClaim(root, digest, "demo", "claim-x"); err != nil {
				t.Errorf("acquire: %v", err)
			}
			ReleaseActiveClaim(root, digest, "demo", "claim-x")
		}()
	}
	wg.Wait()
}
