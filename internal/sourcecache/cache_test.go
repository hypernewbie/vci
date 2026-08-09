package sourcecache

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

// TestPublicationLockStaleOwnerUnlockKeepsReclaimedLock is the
// stale-owner regression: A acquires the publication lock, A's lock
// ages past maxAge, B reclaims it, and only then does A's deferred
// unlock run. A's late unlock must not delete B's lock: B's lock must
// remain on disk and a third publisher C must still be blocked until B
// itself unlocks. Before the owner-token fix the unlock removed the
// lock path unconditionally, deleting B's lock and letting C publish
// concurrently with B.
func TestPublicationLockStaleOwnerUnlockKeepsReclaimedLock(t *testing.T) {
	root := t.TempDir()
	src := writeTree(t, map[string]string{"demo": "demo\n"})
	digest := treeDigest(t, src)
	project := "demo"
	maxAge := time.Hour
	// A acquires the publication lock.
	unlockA, err := PublicationLock(root, digest, project, time.Now(), maxAge)
	if err != nil {
		t.Fatalf("lock A: %v", err)
	}
	// A's lock ages well past maxAge and becomes stale.
	old := time.Now().Add(-2 * maxAge)
	if err := os.Chtimes(LockPath(root, digest, project), old, old); err != nil {
		t.Fatalf("age A lock: %v", err)
	}
	// B reclaims the stale lock.
	unlockB, err := PublicationLock(root, digest, project, time.Now(), maxAge)
	if err != nil {
		t.Fatalf("lock B: %v", err)
	}
	// A finishes and its stale-owner unlock runs; B's lock must survive.
	unlockA()
	if _, err := os.Stat(LockPath(root, digest, project)); err != nil {
		t.Fatalf("B's lock must survive A's stale unlock: %v", err)
	}
	// C cannot acquire while B still holds the lock.
	if _, err := PublicationLock(root, digest, project, time.Now(), maxAge); err == nil {
		t.Fatalf("C must not acquire while B holds the lock")
	}
	// B's own unlock releases the lock, after which D can acquire.
	unlockB()
	unlockD, err := PublicationLock(root, digest, project, time.Now(), maxAge)
	if err != nil {
		t.Fatalf("lock D after B's release: %v", err)
	}
	unlockD()
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
	if runtime.GOOS == "windows" {
		t.Skip("last_use ordering relies on Unix file time granularity")
	}
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

// TestReleaseActiveClaimRejectsTraversal proves ReleaseActiveClaim
// validates digest, project, and claimID before removing anything, so
// a crafted claimID/project/digest can never delete an external path
// or a sibling claim/entry. Regression: ReleaseActiveClaim used to
// call os.RemoveAll on the unvalidated joined path, so a claimID like
// "claim-good/../claim-victim" deleted a sibling claim, "../victim.txt"
// deleted an entry-root sibling, and a "../.." traversal reached
// outside the cache root entirely.
func TestReleaseActiveClaimRejectsTraversal(t *testing.T) {
	const (
		digest  = "sha256-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
		digest2 = "sha256-ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	)
	root := t.TempDir()
	external := t.TempDir() // sibling of root; must never be touched

	// A legitimate claim for the entry under test.
	if err := AcquireActiveClaim(root, digest, "demo", "claim-good"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	claimGood := ActiveClaimPath(root, digest, "demo", "claim-good")
	if err := os.WriteFile(filepath.Join(claimGood, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatalf("write claim-good marker: %v", err)
	}

	// Victim 1: sibling claim of the same entry that claimID
	// "claim-good/../claim-victim" would resolve to.
	claimVictim := ActiveClaimPath(root, digest, "demo", "claim-victim")
	if err := os.MkdirAll(claimVictim, 0o700); err != nil {
		t.Fatalf("mkdir claim-victim: %v", err)
	}
	if err := os.WriteFile(filepath.Join(claimVictim, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatalf("write claim-victim marker: %v", err)
	}

	// Victim 2: sibling file inside the entry root that claimID
	// "../victim.txt" would resolve to.
	entryVictim := filepath.Join(EntryPath(root, digest, "demo"), "victim.txt")
	if err := os.WriteFile(entryVictim, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write entry victim: %v", err)
	}

	// Victim 3: another project's claim under a second digest that
	// project "../<digest2>" or digest "../<digest2>" would resolve to.
	siblingClaim := ActiveClaimPath(root, digest2, "demo2", "claim-x")
	if err := os.MkdirAll(siblingClaim, 0o700); err != nil {
		t.Fatalf("mkdir sibling claim: %v", err)
	}
	if err := os.WriteFile(filepath.Join(siblingClaim, "keep.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatalf("write sibling claim marker: %v", err)
	}

	// Victim 4: a path outside the cache root. Compute the exact
	// traversal claimID that the unvalidated join would have resolved
	// to, so the assertion pins the real escape target.
	externalSentinel := filepath.Join(external, "sentinel.txt")
	if err := os.WriteFile(externalSentinel, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write external sentinel: %v", err)
	}
	escapeRel, err := filepath.Rel(claimGood, externalSentinel)
	if err != nil {
		t.Fatalf("rel: %v", err)
	}
	if filepath.Join(claimGood, escapeRel) != externalSentinel {
		t.Fatalf("escape rel %q does not resolve to %q", escapeRel, externalSentinel)
	}

	// Malformed inputs must remove nothing.
	for name, tc := range map[string]struct {
		digest, project, claimID string
	}{
		"traversal-claimID-sibling-claim": {digest: digest, project: "demo", claimID: "claim-good/../claim-victim"},
		"traversal-claimID-entry-file":    {digest: digest, project: "demo", claimID: "../victim.txt"},
		"traversal-claimID-external":      {digest: digest, project: "demo", claimID: escapeRel},
		"traversal-project":               {digest: digest, project: "../" + digest2, claimID: "claim-x"},
		"traversal-digest":                {digest: "../" + digest2, project: "demo2", claimID: "claim-x"},
		"invalid-digest":                  {digest: "sha256-zz", project: "demo", claimID: "claim-good"},
		"invalid-project":                 {digest: digest, project: "..", claimID: "claim-good"},
		"invalid-claimID":                 {digest: digest, project: "demo", claimID: ".."},
	} {
		t.Run(name, func(t *testing.T) {
			ReleaseActiveClaim(root, tc.digest, tc.project, tc.claimID)
		})
	}

	// None of the victims may be removed, and the legitimate claim
	// must still be present because no malformed release acted on it.
	for path, what := range map[string]string{
		claimVictim:      "sibling claim",
		entryVictim:      "entry-root sibling file",
		siblingClaim:     "sibling project claim",
		externalSentinel: "external sentinel",
		claimGood:        "legitimate claim",
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s was removed by a malformed release: %v", what, err)
		}
	}

	// A valid release still removes exactly its own claim and nothing
	// more: the sibling claim, entry file, sibling project claim, and
	// external sentinel must all survive.
	ReleaseActiveClaim(root, digest, "demo", "claim-good")
	if _, err := os.Stat(claimGood); !os.IsNotExist(err) {
		t.Fatalf("valid release must remove its own claim dir: %v", err)
	}
	for path, what := range map[string]string{
		claimVictim:      "sibling claim",
		entryVictim:      "entry-root sibling file",
		siblingClaim:     "sibling project claim",
		externalSentinel: "external sentinel",
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("valid release removed %s: %v", what, err)
		}
	}
	if active, err := ActiveClaimsExist(root, digest, "demo"); err != nil || !active {
		t.Fatalf("sibling claim must still make the entry active: active=%v err=%v", active, err)
	}
}

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

// TestPublishTreeEvictsEntryLargerThanSlackButSmallerThanQuota is the
// admission-eviction regression: an existing inactive entry (9KB) that
// is larger than the slack (quota 10KB - incoming 2KB = 8KB) but
// smaller than the total quota must still be evictable. The oversize
// ceiling for admission is the quota, not the slack, so the incoming
// entry is admitted after eviction instead of being rejected.
func TestPublishTreeEvictsEntryLargerThanSlackButSmallerThanQuota(t *testing.T) {
	root := t.TempDir()
	quota := int64(10 * 1024)
	contentExisting := make([]byte, 9*1024)
	for i := range contentExisting {
		contentExisting[i] = 'e'
	}
	srcExisting := writeTree(t, map[string]string{"existing.bin": string(contentExisting)})
	digestExisting, err := digestOfTree(t, srcExisting)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PublishTree(root, digestExisting, "demo", srcExisting, quota); err != nil {
		t.Fatalf("publish existing: %v", err)
	}
	// Incoming 2KB: the slack is 8KB, smaller than the 9KB entry, so
	// with the old single-bound admission the 9KB entry was treated as
	// an oversize single and retained, rejecting the 2KB entry even
	// though evicting it would fit under the quota.
	contentIncoming := make([]byte, 2*1024)
	for i := range contentIncoming {
		contentIncoming[i] = 'i'
	}
	srcIncoming := writeTree(t, map[string]string{"incoming.bin": string(contentIncoming)})
	digestIncoming, err := digestOfTree(t, srcIncoming)
	if err != nil {
		t.Fatal(err)
	}
	published, err := PublishTree(root, digestIncoming, "demo", srcIncoming, quota)
	if err != nil {
		t.Fatalf("publish incoming: %v", err)
	}
	if !published {
		t.Fatalf("incoming entry must be admitted after eviction")
	}
	if hit, _, _ := IsHit(root, digestExisting, "demo"); hit {
		t.Fatalf("existing 9KB entry must be evicted to make room")
	}
	if hit, _, _ := IsHit(root, digestIncoming, "demo"); !hit {
		t.Fatalf("incoming 2KB entry must be a hit after admission")
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

	removed, total, _, err := EnforceQuota(root, quota, quota)
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
