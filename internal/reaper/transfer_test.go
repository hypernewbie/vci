package reaper

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
