package bundlecache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAdmitWritesHitAndMeta(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2025, 6, 1, 10, 30, 0, 0, time.UTC)
	payload := []byte("bundle-bytes")

	admitted, err := Admit(root, "proj", "abc123", payload, Policy{}, now)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if !admitted {
		t.Fatal("Admit: expected admission")
	}
	hit, err := IsHit(root, "proj", "abc123")
	if err != nil {
		t.Fatalf("IsHit: %v", err)
	}
	if !hit {
		t.Fatal("IsHit: complete entry should be a hit")
	}

	raw, err := os.ReadFile(filepath.Join(root, Version, "proj", "abc123", "meta.json"))
	if err != nil {
		t.Fatalf("meta.json: %v", err)
	}
	var m struct {
		Bytes    int64     `json:"bytes"`
		LastUsed time.Time `json:"last_used"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal meta.json: %v", err)
	}
	if m.Bytes != int64(len(payload)) {
		t.Fatalf("meta bytes = %d, want %d", m.Bytes, len(payload))
	}
	if !m.LastUsed.Equal(now) {
		t.Fatalf("meta last_used = %v, want %v", m.LastUsed, now)
	}

	stored, err := os.ReadFile(filepath.Join(root, Version, "proj", "abc123", "bundle"))
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	if string(stored) != string(payload) {
		t.Fatalf("bundle content = %q, want %q", stored, payload)
	}
}

func TestAdmitRejectsOversizedBundle(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2025, 6, 1, 10, 30, 0, 0, time.UTC)

	admitted, err := Admit(root, "proj", "base1", []byte("12345"), Policy{AdmissionBytes: 4}, now)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if admitted {
		t.Fatal("Admit: oversized bundle must not be admitted")
	}
	hit, err := IsHit(root, "proj", "base1")
	if err != nil {
		t.Fatalf("IsHit: %v", err)
	}
	if hit {
		t.Fatal("IsHit: nothing was admitted, so no hit")
	}
}

func TestListReturnsCompleteEntriesSortedByBase(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2025, 6, 1, 10, 30, 0, 0, time.UTC)
	for _, base := range []string{"zzz", "aaa", "mmm"} {
		if _, err := Admit(root, "proj", base, []byte("x"+base), Policy{}, now); err != nil {
			t.Fatalf("Admit %s: %v", base, err)
		}
	}
	// An entry whose complete marker is missing must not be listed.
	if _, err := Admit(root, "proj", "incomplete", []byte("y"), Policy{}, now); err != nil {
		t.Fatalf("Admit incomplete: %v", err)
	}
	if err := os.Remove(filepath.Join(root, Version, "proj", "incomplete", "complete")); err != nil {
		t.Fatalf("remove complete marker: %v", err)
	}

	entries, err := List(root, "proj")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []Entry{
		{Base: "aaa", Bytes: 4},
		{Base: "mmm", Bytes: 4},
		{Base: "zzz", Bytes: 4},
	}
	if len(entries) != len(want) {
		t.Fatalf("List = %d entries, want %d: %+v", len(entries), len(want), entries)
	}
	for i, e := range entries {
		if e.Base != want[i].Base || e.Bytes != want[i].Bytes {
			t.Fatalf("entry %d = %+v, want %+v", i, e, want[i])
		}
		if !e.LastUsed.Equal(now) {
			t.Fatalf("entry %d last_used = %v, want %v", i, e.LastUsed, now)
		}
	}
}

func TestEvictLRUHonorsMaxEntries(t *testing.T) {
	root := t.TempDir()
	t0 := time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC)
	for i, base := range []string{"oldest", "middle", "newest"} {
		if _, err := Admit(root, "proj", base, []byte(base), Policy{}, t0.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatalf("Admit %s: %v", base, err)
		}
	}

	removed, err := EvictLRU(root, "proj", Policy{MaxEntries: 2})
	if err != nil {
		t.Fatalf("EvictLRU: %v", err)
	}
	if removed != 1 {
		t.Fatalf("EvictLRU removed %d, want 1", removed)
	}
	checkHits(t, root, map[string]bool{"oldest": false, "middle": true, "newest": true})
}

func TestEvictLRUHonorsMaxBytes(t *testing.T) {
	root := t.TempDir()
	t0 := time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC)
	order := []string{"aaa", "bbb", "ccc", "ddd"}
	sizes := map[string]int{"aaa": 60, "bbb": 70, "ccc": 80, "ddd": 90}
	for i, base := range order {
		if _, err := Admit(root, "proj", base, make([]byte, sizes[base]), Policy{}, t0.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatalf("Admit %s: %v", base, err)
		}
	}

	// Total is 300 bytes; 200 is the cap, so aaa (60) and bbb (70) go.
	removed, err := EvictLRU(root, "proj", Policy{MaxBytes: 200})
	if err != nil {
		t.Fatalf("EvictLRU: %v", err)
	}
	if removed != 2 {
		t.Fatalf("EvictLRU removed %d, want 2", removed)
	}
	checkHits(t, root, map[string]bool{"aaa": false, "bbb": false, "ccc": true, "ddd": true})
}

func TestEvictLRUNeverRemovesClaimedEntry(t *testing.T) {
	t.Run("claimed oldest survives", func(t *testing.T) {
		root := t.TempDir()
		t0 := time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC)
		for i, base := range []string{"oldest", "newest"} {
			if _, err := Admit(root, "proj", base, []byte(base), Policy{}, t0.Add(time.Duration(i)*time.Hour)); err != nil {
				t.Fatalf("Admit %s: %v", base, err)
			}
		}
		if err := AcquireActiveClaim(root, "proj", "oldest", "claim-1"); err != nil {
			t.Fatalf("AcquireActiveClaim: %v", err)
		}

		removed, err := EvictLRU(root, "proj", Policy{MaxEntries: 1})
		if err != nil {
			t.Fatalf("EvictLRU: %v", err)
		}
		if removed != 1 {
			t.Fatalf("EvictLRU removed %d, want 1", removed)
		}
		checkHits(t, root, map[string]bool{"oldest": true, "newest": false})
	})
	t.Run("all claimed evicts nothing", func(t *testing.T) {
		root := t.TempDir()
		t0 := time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC)
		for i, base := range []string{"oldest", "newest"} {
			if _, err := Admit(root, "proj", base, []byte(base), Policy{}, t0.Add(time.Duration(i)*time.Hour)); err != nil {
				t.Fatalf("Admit %s: %v", base, err)
			}
			if err := AcquireActiveClaim(root, "proj", base, "claim-"+base); err != nil {
				t.Fatalf("AcquireActiveClaim %s: %v", base, err)
			}
		}

		removed, err := EvictLRU(root, "proj", Policy{MaxEntries: 1})
		if err != nil {
			t.Fatalf("EvictLRU: %v", err)
		}
		if removed != 0 {
			t.Fatalf("EvictLRU removed %d, want 0", removed)
		}
		checkHits(t, root, map[string]bool{"oldest": true, "newest": true})
	})
	t.Run("released claim becomes evictable", func(t *testing.T) {
		root := t.TempDir()
		t0 := time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC)
		for i, base := range []string{"oldest", "newest"} {
			if _, err := Admit(root, "proj", base, []byte(base), Policy{}, t0.Add(time.Duration(i)*time.Hour)); err != nil {
				t.Fatalf("Admit %s: %v", base, err)
			}
		}
		if err := AcquireActiveClaim(root, "proj", "oldest", "claim-1"); err != nil {
			t.Fatalf("AcquireActiveClaim: %v", err)
		}
		if err := ReleaseActiveClaim(root, "proj", "oldest", "claim-1"); err != nil {
			t.Fatalf("ReleaseActiveClaim: %v", err)
		}

		removed, err := EvictLRU(root, "proj", Policy{MaxEntries: 1})
		if err != nil {
			t.Fatalf("EvictLRU: %v", err)
		}
		if removed != 1 {
			t.Fatalf("EvictLRU removed %d, want 1", removed)
		}
		checkHits(t, root, map[string]bool{"oldest": false, "newest": true})
	})
}

func TestUpdateLastUseChangesEvictionOrder(t *testing.T) {
	root := t.TempDir()
	t0 := time.Date(2025, 6, 1, 10, 0, 0, 0, time.UTC)
	for i, base := range []string{"older", "newer"} {
		if _, err := Admit(root, "proj", base, []byte(base), Policy{}, t0.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatalf("Admit %s: %v", base, err)
		}
	}
	// Make "older" the most recently used so "newer" becomes the LRU entry.
	if err := UpdateLastUse(root, "proj", "older", t0.Add(2*time.Hour)); err != nil {
		t.Fatalf("UpdateLastUse: %v", err)
	}

	removed, err := EvictLRU(root, "proj", Policy{MaxEntries: 1})
	if err != nil {
		t.Fatalf("EvictLRU: %v", err)
	}
	if removed != 1 {
		t.Fatalf("EvictLRU removed %d, want 1", removed)
	}
	checkHits(t, root, map[string]bool{"older": true, "newer": false})
}

func TestReapStaleRemovesIncompleteEntriesAndOldClaims(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

	// A complete entry must survive reaping.
	if _, err := Admit(root, "proj", "complete1", []byte("data"), Policy{}, now); err != nil {
		t.Fatalf("Admit: %v", err)
	}

	// Incomplete entries: the old one is removed, the fresh one is kept.
	oldIncomplete := filepath.Join(root, Version, "proj", "oldpartial")
	if err := writeIncomplete(oldIncomplete, now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("writeIncomplete old: %v", err)
	}
	freshIncomplete := filepath.Join(root, Version, "proj", "freshpartial")
	if err := writeIncomplete(freshIncomplete, now.Add(-10*time.Minute)); err != nil {
		t.Fatalf("writeIncomplete fresh: %v", err)
	}

	// Claims: the stale marker is removed, the fresh one is kept.
	if err := AcquireActiveClaim(root, "proj", "complete1", "stale-claim"); err != nil {
		t.Fatalf("AcquireActiveClaim stale: %v", err)
	}
	if err := AcquireActiveClaim(root, "proj", "complete1", "fresh-claim"); err != nil {
		t.Fatalf("AcquireActiveClaim fresh: %v", err)
	}
	staleClaim := filepath.Join(root, Version, "proj", "complete1", "claims", "stale-claim")
	if err := os.Chtimes(staleClaim, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatalf("Chtimes stale claim: %v", err)
	}

	removed, err := ReapStale(root, now, time.Hour, 30*time.Minute)
	if err != nil {
		t.Fatalf("ReapStale: %v", err)
	}
	if removed != 2 {
		t.Fatalf("ReapStale removed %d, want 2 (old partial entry + stale claim)", removed)
	}
	if _, err := os.Stat(oldIncomplete); !os.IsNotExist(err) {
		t.Fatal("old incomplete entry still present")
	}
	if _, err := os.Stat(freshIncomplete); err != nil {
		t.Fatalf("fresh incomplete entry was removed: %v", err)
	}
	hit, err := IsHit(root, "proj", "complete1")
	if err != nil {
		t.Fatalf("IsHit: %v", err)
	}
	if !hit {
		t.Fatal("complete entry was removed by ReapStale")
	}
	if _, err := os.Stat(staleClaim); !os.IsNotExist(err) {
		t.Fatal("stale claim marker still present")
	}
	if _, err := os.Stat(filepath.Join(root, Version, "proj", "complete1", "claims", "fresh-claim")); err != nil {
		t.Fatal("fresh claim marker was removed")
	}
}

func TestValidationRejectsBadSegments(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	for _, s := range []string{"", "..", "a/b", `a\b`, "a b", "a?b"} {
		if _, err := Admit(root, s, "base", []byte("x"), Policy{}, now); err == nil {
			t.Errorf("Admit accepted invalid project %q", s)
		}
		if _, err := Admit(root, "proj", s, []byte("x"), Policy{}, now); err == nil {
			t.Errorf("Admit accepted invalid base %q", s)
		}
		if _, err := IsHit(root, s, "base"); err == nil {
			t.Errorf("IsHit accepted invalid project %q", s)
		}
		if _, err := IsHit(root, "proj", s); err == nil {
			t.Errorf("IsHit accepted invalid base %q", s)
		}
	}
	if err := AcquireActiveClaim(root, "proj", "base", "a/b"); err == nil {
		t.Error("AcquireActiveClaim accepted invalid claimID")
	}
	if _, err := List(root, "a/b"); err == nil {
		t.Error("List accepted invalid project")
	}
	if _, err := EvictLRU(root, "a/b", Policy{MaxEntries: 1}); err == nil {
		t.Error("EvictLRU accepted invalid project")
	}
	if err := UpdateLastUse(root, "proj", "..", now); err == nil {
		t.Error("UpdateLastUse accepted invalid base")
	}
}

// writeIncomplete creates an entry dir without the complete marker, with
// bundle and meta.json mtimes set to ts.
func writeIncomplete(entryPath string, ts time.Time) error {
	if err := os.MkdirAll(entryPath, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(entryPath, "bundle"), []byte("partial"), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(entryPath, "meta.json"), []byte(`{"bytes":7,"last_used":"2025-06-01T00:00:00Z"}`), 0o644); err != nil {
		return err
	}
	for _, name := range []string{"bundle", "meta.json"} {
		if err := os.Chtimes(filepath.Join(entryPath, name), ts, ts); err != nil {
			return err
		}
	}
	return nil
}

// checkHits asserts each base's IsHit result.
func checkHits(t *testing.T, root string, want map[string]bool) {
	t.Helper()
	for base, expected := range want {
		hit, err := IsHit(root, "proj", base)
		if err != nil {
			t.Fatalf("IsHit(%s): %v", base, err)
		}
		if hit != expected {
			t.Fatalf("IsHit(%s) = %v, want %v", base, hit, expected)
		}
	}
}
